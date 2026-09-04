"""Read-only media inventory over messages.db and the bridge's media cache.

The bridge writes every media message row with file_length and file_sha256
(the WhatsApp content hash) and caches the bytes under
``<store>/<chat_jid>/<type>_<yyyymmdd_hhmmss>_<message id>[.ext]``. This module
turns that into something the agent can reason about: what is heavy, what is
the same file forwarded into several chats (same sha256), and what is actually
cached on disk right now. Nothing here writes; purge and notes live elsewhere
(issues #97, #98).
"""

from __future__ import annotations

import os
import sqlite3
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import whatsapp
from errors import ToolError
from whatsapp import CHAT_POLICY, PageResult, decode_cursor, encode_cursor

# Rows that carry a downloadable file. Pointer rows (reaction, poll_vote) and
# text never appear in the inventory.
MEDIA_TYPES = ("image", "video", "audio", "document", "sticker")
SORTS = ("size", "date", "copies")
MAX_LIMIT = 200


def media_root() -> str:
    """Directory holding the per-chat media folders: where messages.db lives."""
    return os.path.dirname(os.path.abspath(whatsapp.MESSAGES_DB_PATH))


def chat_media_dir(chat_jid: str) -> str:
    """The bridge maps ':' (device suffix) to '_' in directory names."""
    return os.path.join(media_root(), chat_jid.replace(":", "_"))


@dataclass
class CachedFile:
    name: str
    bytes: int


def scan_chat_cache(chat_jid: str) -> dict[str, CachedFile]:
    """Map message id -> cached file for one chat directory.

    Filenames are ``<type>_<date>_<time>_<id>[.ext]``; the id is what follows
    the third underscore, minus the extension the bridge appends for its type.
    Documents keep no extension (the original name lives in messages.filename).
    A missing or unreadable directory means nothing is cached.
    """
    found: dict[str, CachedFile] = {}
    try:
        with os.scandir(chat_media_dir(chat_jid)) as entries:
            for entry in entries:
                if not entry.is_file() or entry.name.endswith(".part"):
                    continue
                parts = entry.name.split("_", 3)
                if len(parts) != 4 or parts[0] not in MEDIA_TYPES:
                    continue
                tail = parts[3]
                if parts[0] != "document":
                    tail = os.path.splitext(tail)[0]
                try:
                    size = entry.stat().st_size
                except OSError:
                    continue
                found[tail] = CachedFile(entry.name, size)
    except OSError:
        return {}
    return found


class _CacheIndex:
    """Lazily scans one directory per chat for the duration of a call."""

    def __init__(self) -> None:
        self._by_chat: dict[str, dict[str, CachedFile]] = {}

    def lookup(self, chat_jid: str, message_id: str) -> CachedFile | None:
        if chat_jid not in self._by_chat:
            self._by_chat[chat_jid] = scan_chat_cache(chat_jid)
        return self._by_chat[chat_jid].get(message_id)


def _media_filters(
    chat_jid: str | None,
    media_type: str | None,
    after: str | None,
    before: str | None,
    min_bytes: int | None,
    column_prefix: str,
) -> tuple[list[str], list[Any]]:
    clauses: list[str] = [f"{column_prefix}media_type IN ({','.join('?' * len(MEDIA_TYPES))})"]
    params: list[Any] = list(MEDIA_TYPES)
    if chat_jid:
        clauses.append(f"{column_prefix}chat_jid = ?")
        params.append(chat_jid)
    if media_type:
        if media_type not in MEDIA_TYPES:
            raise ToolError("invalid_argument", f"media_type must be one of {', '.join(MEDIA_TYPES)}")
        clauses.append(f"{column_prefix}media_type = ?")
        params.append(media_type)
    if after:
        clauses.append(f"{column_prefix}timestamp >= ?")
        params.append(_iso(after, "after"))
    if before:
        clauses.append(f"{column_prefix}timestamp <= ?")
        params.append(_iso(before, "before"))
    if min_bytes:
        clauses.append(f"{column_prefix}file_length >= ?")
        params.append(int(min_bytes))
    if CHAT_POLICY.restricted:
        clause, clause_params = CHAT_POLICY.sql_clause(f"{column_prefix}chat_jid")
        clauses.append(clause)
        params.extend(clause_params)
    return clauses, params


def _iso(value: str, name: str) -> str:
    try:
        return datetime.fromisoformat(value).isoformat(sep=" ")
    except ValueError as exc:
        raise ToolError("invalid_argument", f"{name} must be an ISO-8601 timestamp") from exc


def list_media_page(
    chat_jid: str | None = None,
    media_type: str | None = None,
    after: str | None = None,
    before: str | None = None,
    min_bytes: int | None = None,
    sort: str = "size",
    limit: int = 50,
    page: int = 0,
    cursor: str | None = None,
) -> PageResult:
    """One page of media rows with size, hash, copy counts and cache state."""
    if sort not in SORTS:
        raise ToolError("invalid_argument", f"sort must be one of {', '.join(SORTS)}")
    if chat_jid:
        whatsapp._require_allowed(chat_jid)
    limit = max(1, min(int(limit), MAX_LIMIT))
    state = decode_cursor(cursor, "media")
    offset = int(state["o"]) if state else max(0, int(page)) * limit

    clauses, params = _media_filters(chat_jid, media_type, after, before, min_bytes, "m.")
    copies_clauses, copies_params = _media_filters(None, None, None, None, None, "")
    order = {
        "size": "m.file_length DESC, m.timestamp DESC",
        "date": "m.timestamp DESC",
        "copies": "copies DESC, m.file_length DESC, m.timestamp DESC",
    }[sort]
    sql = f"""
        WITH copies AS (
            SELECT file_sha256, COUNT(*) AS copies, COUNT(DISTINCT chat_jid) AS copies_in
            FROM messages
            WHERE file_sha256 IS NOT NULL AND {" AND ".join(copies_clauses)}
            GROUP BY file_sha256
        )
        SELECT m.id, m.chat_jid, c.name, m.sender, m.timestamp, m.is_from_me, m.media_type, m.filename,
               m.file_length, lower(hex(m.file_sha256)), m.deleted_at,
               COALESCE(copies.copies, 1), COALESCE(copies.copies_in, 1)
        FROM messages m
        LEFT JOIN chats c ON c.jid = m.chat_jid
        LEFT JOIN copies ON copies.file_sha256 = m.file_sha256
        WHERE {" AND ".join(clauses)}
        ORDER BY {order}, m.id
        LIMIT ? OFFSET ?
    """
    try:
        conn = whatsapp._connect_messages_db()
        try:
            rows = conn.execute(sql, (*copies_params, *params, limit + 1, offset)).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc

    has_more = len(rows) > limit
    rows = rows[:limit]
    cache = _CacheIndex()
    items = [_row_to_item(row, cache) for row in rows]
    next_cursor = encode_cursor({"k": "media", "o": offset + limit}) if has_more else None
    return PageResult(items, next_cursor, has_more)


def _row_to_item(row: tuple, cache: _CacheIndex) -> dict[str, Any]:
    (
        msg_id,
        chat_jid,
        chat_name,
        sender,
        timestamp,
        is_from_me,
        media_type,
        filename,
        file_length,
        sha256,
        deleted_at,
        copies,
        copies_in,
    ) = row
    cached = cache.lookup(chat_jid, msg_id)
    return {
        "message_id": msg_id,
        "chat_jid": chat_jid,
        "chat_name": chat_name,
        "sender_jid": sender,
        "is_from_me": bool(is_from_me),
        "timestamp": datetime.fromisoformat(timestamp).isoformat() if timestamp else None,
        "media_type": media_type,
        "filename": filename or None,
        "bytes": int(file_length) if file_length else None,
        "sha256": sha256 or None,
        "cached": cached is not None,
        "cached_bytes": cached.bytes if cached else None,
        "cached_file": cached.name if cached else None,
        "copies": int(copies),
        "copies_in": int(copies_in),
        "deleted_at": datetime.fromisoformat(deleted_at).isoformat() if deleted_at else None,
    }


def media_stats(chat_jid: str | None = None) -> dict[str, Any]:
    """Totals by chat and by media type, from the rows and from the cache directories."""
    if chat_jid:
        whatsapp._require_allowed(chat_jid)
    clauses, params = _media_filters(chat_jid, None, None, None, None, "m.")
    where = " AND ".join(clauses)
    try:
        conn = whatsapp._connect_messages_db()
        try:
            by_chat_rows = conn.execute(
                f"""
                SELECT m.chat_jid, c.name, COUNT(*), COALESCE(SUM(m.file_length), 0),
                       COUNT(DISTINCT m.file_sha256)
                FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid
                WHERE {where}
                GROUP BY m.chat_jid ORDER BY SUM(m.file_length) DESC
                """,
                params,
            ).fetchall()
            by_type_rows = conn.execute(
                f"""
                SELECT m.media_type, COUNT(*), COALESCE(SUM(m.file_length), 0)
                FROM messages m WHERE {where}
                GROUP BY m.media_type ORDER BY SUM(m.file_length) DESC
                """,
                params,
            ).fetchall()
            duplicates = conn.execute(
                f"""
                SELECT COUNT(*), COALESCE(SUM(extra_bytes), 0) FROM (
                    SELECT (COUNT(*) - 1) * MAX(m.file_length) AS extra_bytes
                    FROM messages m WHERE {where} AND m.file_sha256 IS NOT NULL
                    GROUP BY m.file_sha256 HAVING COUNT(*) > 1
                )
                """,
                params,
            ).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc

    by_chat = []
    cached_files_total = cached_bytes_total = 0
    for jid, name, count, total_bytes, distinct in by_chat_rows:
        cached = scan_chat_cache(jid)
        cached_bytes = sum(f.bytes for f in cached.values())
        cached_files_total += len(cached)
        cached_bytes_total += cached_bytes
        by_chat.append(
            {
                "chat_jid": jid,
                "chat_name": name,
                "files": int(count),
                "distinct_files": int(distinct),
                "bytes": int(total_bytes),
                "cached_files": len(cached),
                "cached_bytes": cached_bytes,
            }
        )
    by_type = [{"media_type": t, "files": int(n), "bytes": int(b)} for t, n, b in by_type_rows]
    return {
        "chat_jid": chat_jid or None,
        "media_root": media_root(),
        "total": {
            "files": sum(c["files"] for c in by_chat),
            "bytes": sum(c["bytes"] for c in by_chat),
            "cached_files": cached_files_total,
            "cached_bytes": cached_bytes_total,
            "duplicate_groups": int(duplicates[0]) if duplicates else 0,
            "duplicate_bytes": int(duplicates[1]) if duplicates else 0,
        },
        "by_chat": by_chat,
        "by_type": by_type,
    }
