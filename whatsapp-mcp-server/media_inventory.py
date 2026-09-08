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
import threading
import time
from collections import OrderedDict
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import media_notes
import whatsapp
from errors import ToolError
from whatsapp import (
    CHAT_POLICY,
    PageResult,
    decode_cursor,
    encode_cursor,
    page_number,
    page_size,
    parse_db_time,
    timestamp_bound,
)

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


def cached_message_id(name: str) -> str | None:
    """The message id a cached filename carries, or None when it is not one.

    Filenames are ``<type>_<date>_<time>_<id>[.ext]``; the id is what follows
    the third underscore, minus the extension (fixed per type; documents take
    the sender's, and files cached before that change have none). Message IDs
    never contain a dot, so splitting the extension is safe for every shape.
    A half-written download (``.part``) carries no readable bytes yet.
    """
    if name.endswith(".part"):
        return None
    parts = name.split("_", 3)
    if len(parts) != 4 or parts[0] not in MEDIA_TYPES:
        return None
    return os.path.splitext(parts[3])[0]


def scan_chat_cache(chat_jid: str) -> dict[str, CachedFile]:
    """Map message id -> cached file for one chat directory.

    One stat per file, so this is for callers that want every entry (the store
    totals). A single message is answered by ``lookup_cached_name``, a page by
    ``_CacheIndex``. A missing or unreadable directory means nothing is cached.
    """
    found: dict[str, CachedFile] = {}
    try:
        with os.scandir(chat_media_dir(chat_jid)) as entries:
            for entry in entries:
                message_id = cached_message_id(entry.name)
                if message_id is None or not entry.is_file():
                    continue
                try:
                    size = entry.stat().st_size
                except OSError:
                    continue
                found[message_id] = CachedFile(entry.name, size)
    except OSError:
        return {}
    return found


def lookup_cached_name(chat_jid: str, message_id: str) -> str | None:
    """The filename cached for one message, or None when nothing is.

    The names come from the directory entries, which cost no syscall of their
    own: one directory read and no stat at all, whatever the chat holds.
    Building the whole map to answer for one message meant a stat per file the
    chat ever received, on every lookup and again after every fetched file
    (issue #318). Not memoised: the caller is about to open the bytes, or to
    decide whether to pay the bridge for them.

    The directory is read to the end and the last match wins, which is the rule
    the maps above follow: one message can have two files (a document cached
    before the extension was kept, and its re-download), and a listing and a
    read of the same message must not answer with different bytes.
    """
    found: str | None = None
    try:
        with os.scandir(chat_media_dir(chat_jid)) as entries:
            for entry in entries:
                if cached_message_id(entry.name) == message_id and entry.is_file():
                    found = entry.name
    except OSError:
        return None
    return found


def list_chat_names(chat_jid: str) -> dict[str, str]:
    """Map message id -> cached filename for one chat, without stat-ing anything.

    The names alone answer "which message is this file", and reading them costs
    one directory read whatever the chat holds. What a file *is* — still there,
    how many bytes — is a stat, and the callers that need it take it one file at
    a time (``_stat_cached``).
    """
    names: dict[str, str] = {}
    try:
        with os.scandir(chat_media_dir(chat_jid)) as entries:
            for entry in entries:
                message_id = cached_message_id(entry.name)
                if message_id is not None and entry.is_file():
                    names[message_id] = entry.name
    except OSError:
        return {}
    return names


def _stat_cached(chat_jid: str, name: str) -> CachedFile | None:
    """One named file of a chat directory, or None when it is gone or unreadable."""
    try:
        return CachedFile(name, os.stat(os.path.join(chat_media_dir(chat_jid), name)).st_size)
    except OSError:
        return None


# How long a directory listing is reused across calls, and how many chats keep
# one. Paging a listing re-reads the same directories call after call, so the
# names are kept for the few seconds that takes; the mtime of the directory
# drops the entry as soon as anything is written there (an arrival, a purge, a
# retention sweep). Only names are kept, never "this file exists": every row of
# a page is stat-ed through the name, so a file that is gone is reported as not
# cached whatever the memo still says. The bound the memo can cost is therefore
# one-sided and small — a file that arrived in the last CACHE_TTL_S seconds
# under a directory mtime too coarse to separate it from the read can be listed
# as not cached for that long.
CACHE_TTL_S = 5.0
CACHE_MAX_CHATS = 16

_names: OrderedDict[str, tuple[int, float, dict[str, str]]] = OrderedDict()
_names_lock = threading.Lock()


def _dir_mtime_ns(chat_jid: str) -> int:
    """The chat directory's mtime, or 0 when it does not exist."""
    try:
        return os.stat(chat_media_dir(chat_jid)).st_mtime_ns
    except OSError:
        return 0


def cached_names(chat_jid: str) -> dict[str, str]:
    """``list_chat_names`` memoised per process, bounded by the mtime and CACHE_TTL_S."""
    mtime, now = _dir_mtime_ns(chat_jid), time.monotonic()
    with _names_lock:
        entry = _names.get(chat_jid)
        if entry is not None and entry[0] == mtime and now - entry[1] < CACHE_TTL_S:
            _names.move_to_end(chat_jid)
            return entry[2]
    listed = list_chat_names(chat_jid)
    with _names_lock:
        # Expired entries can never be served again: drop them instead of
        # holding one map per file of the last CACHE_MAX_CHATS chats forever.
        for jid in [j for j, (_, at, _map) in _names.items() if now - at >= CACHE_TTL_S]:
            del _names[jid]
        _names[chat_jid] = (mtime, now, listed)
        _names.move_to_end(chat_jid)
        while len(_names) > CACHE_MAX_CHATS:
            _names.popitem(last=False)
    return listed


def forget_cached_names(chat_jid: str | None = None) -> None:
    """Drop the memoised listing of one chat, or of all of them."""
    with _names_lock:
        if chat_jid is None:
            _names.clear()
        else:
            _names.pop(chat_jid, None)


class _CacheIndex:
    """The cached file of each row of one page.

    One directory read per chat, reused across pages, and one stat per row —
    not one stat per file the chat ever received (issue #318). The stat is what
    answers ``cached``, so nothing the memo names is claimed to be readable
    without the filesystem saying so on this call.
    """

    def __init__(self) -> None:
        self._by_chat: dict[str, dict[str, str]] = {}

    def lookup(self, chat_jid: str, message_id: str) -> CachedFile | None:
        if chat_jid not in self._by_chat:
            self._by_chat[chat_jid] = cached_names(chat_jid)
        name = self._by_chat[chat_jid].get(message_id)
        return _stat_cached(chat_jid, name) if name else None


def _media_filters(
    chat_jid: str | Sequence[str] | None,
    media_type: str | None,
    after: str | None,
    before: str | None,
    min_bytes: int | None,
    column_prefix: str,
    exclude_chat_jid: str | Sequence[str] | None = None,
) -> tuple[list[str], list[Any]]:
    clauses: list[str] = [f"{column_prefix}media_type IN ({','.join('?' * len(MEDIA_TYPES))})"]
    params: list[Any] = list(MEDIA_TYPES)
    if chats := whatsapp.chat_jid_filter(chat_jid):
        clauses.append(whatsapp._chat_jid_clause(f"{column_prefix}chat_jid", chats))
        params.extend(chats)
    if excluded := whatsapp.chat_jid_filter(exclude_chat_jid, "exclude_chat_jid", require_allowed=False):
        clauses.append(whatsapp._chat_jid_clause(f"{column_prefix}chat_jid", excluded, negated=True))
        params.extend(excluded)
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
    """An after/before argument as a normalised timestamp bound (issue #253)."""
    try:
        return timestamp_bound(datetime.fromisoformat(value))
    except ValueError as exc:
        raise ToolError("invalid_argument", f"{name} must be an ISO-8601 timestamp") from exc


def _notes_exists_clause(conn: sqlite3.Connection, has_notes: bool) -> str | None:
    """SQL predicate for "the agent has annotated this file", or None when it can never match.

    notes.db is a separate database, so it is attached to the read connection
    for the call instead of pulling every annotated hash into the query as
    parameters. No notes.db (or no table in it yet) means nothing is annotated:
    has_notes=True then matches nothing and has_notes=False matches everything.
    """
    annotated = None
    if os.path.exists(media_notes.notes_db_path()):
        conn.execute("ATTACH DATABASE ? AS notesdb", (media_notes.notes_db_path(),))
        annotated = conn.execute(
            "SELECT 1 FROM notesdb.sqlite_master WHERE type = 'table' AND name = 'media_notes'"
        ).fetchone()
    if not annotated:
        return None if has_notes else "1"
    exists = "EXISTS (SELECT 1 FROM notesdb.media_notes n WHERE n.sha256 = lower(hex(m.file_sha256)))"
    return exists if has_notes else f"NOT {exists}"


ORDER_BY = {
    "size": "m.file_length DESC, m.timestamp DESC",
    "date": "m.timestamp DESC",
    "copies": "copies DESC, m.file_length DESC, m.timestamp DESC",
}

# The display columns of one inventory row, in the order _row_to_item unpacks them.
_ITEM_COLUMNS = """m.id, m.chat_jid, c.name, m.sender, m.timestamp, m.is_from_me, m.media_type, m.filename,
                       m.file_length, lower(hex(m.file_sha256)), m.deleted_at"""


def _page_sql(clauses: list[str], order: str, copies_clauses: list[str] | None = None) -> str:
    """The page query, with or without the archive-wide copies aggregate.

    Sorting by ``copies`` needs the aggregate to order the rows, so it keeps the
    CTE. The date and size sorts do not: they select the page first and look the
    counts up for its hashes afterwards (``_copies_for_hashes``), so the page no
    longer pays for every other hash in the archive (issue #317). Then the last
    column is the raw hash instead of the two counts. What the page still costs
    is its own select: seeked when the filters and the sort match an index
    (chat and time), a sort of the filtered rows when they do not (by size).
    """
    if copies_clauses is None:
        return f"""
            SELECT {_ITEM_COLUMNS}, m.file_sha256
            FROM messages m
            LEFT JOIN chats c ON c.jid = m.chat_jid
            WHERE {" AND ".join(clauses)}
            ORDER BY {order}, m.id, m.chat_jid
            LIMIT ? OFFSET ?
        """
    return f"""
        WITH copies AS (
            SELECT file_sha256, COUNT(*) AS copies, COUNT(DISTINCT chat_jid) AS copies_in
            FROM messages
            WHERE file_sha256 IS NOT NULL AND {" AND ".join(copies_clauses)}
            GROUP BY file_sha256
        )
        SELECT {_ITEM_COLUMNS}, COALESCE(copies.copies, 1), COALESCE(copies.copies_in, 1)
        FROM messages m
        LEFT JOIN chats c ON c.jid = m.chat_jid
        LEFT JOIN copies ON copies.file_sha256 = m.file_sha256
        WHERE {" AND ".join(clauses)}
        ORDER BY {order}, m.id, m.chat_jid
        LIMIT ? OFFSET ?
    """


def _copies_for_hashes(
    conn: sqlite3.Connection, hashes: list[bytes], copies_clauses: list[str], copies_params: list[Any]
) -> dict[bytes, tuple[int, int]]:
    """Copy and chat counts for the page's hashes, counted over the whole visible archive.

    Same meaning as the CTE the ``copies`` sort joins — every policy-visible
    media row carrying that hash, in any chat, whatever the page was filtered
    on — computed for at most one page of hashes. ``file_sha256`` is indexed
    (``idx_messages_file_sha256``, partial on NOT NULL), so the work follows the
    duplicates of this page instead of the size of the archive.
    """
    if not hashes:
        return {}
    sql = f"""
        SELECT file_sha256, COUNT(*), COUNT(DISTINCT chat_jid)
        FROM messages
        WHERE file_sha256 IS NOT NULL AND file_sha256 IN ({",".join("?" * len(hashes))})
          AND {" AND ".join(copies_clauses)}
        GROUP BY file_sha256
    """
    return {row[0]: (int(row[1]), int(row[2])) for row in conn.execute(sql, (*hashes, *copies_params))}


def list_media_page(
    chat_jid: str | Sequence[str] | None = None,
    media_type: str | None = None,
    after: str | None = None,
    before: str | None = None,
    min_bytes: int | None = None,
    has_notes: bool | None = None,
    sort: str = "size",
    limit: int = 50,
    page: int = 0,
    cursor: str | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
) -> PageResult:
    """One page of media rows with size, hash, copy counts and cache state."""
    if sort not in SORTS:
        raise ToolError("invalid_argument", f"sort must be one of {', '.join(SORTS)}")
    limit = page_size(limit, MAX_LIMIT)
    page = page_number(page)  # checked even when a cursor overrides it
    state = decode_cursor(cursor, "media")
    offset = int(state["o"]) if state else page * limit

    clauses, params = _media_filters(chat_jid, media_type, after, before, min_bytes, "m.", exclude_chat_jid)
    copies_clauses, copies_params = _media_filters(None, None, None, None, None, "")
    order = ORDER_BY[sort]
    rows: list[Any] = []
    has_more = False
    try:
        conn = whatsapp._connect_messages_db()
        try:
            if has_notes is not None:
                notes_clause = _notes_exists_clause(conn, has_notes)
                if notes_clause is None:
                    return PageResult([], None, False)
                clauses.append(notes_clause)
            if sort == "copies":
                sql = _page_sql(clauses, order, copies_clauses)
                rows = conn.execute(sql, (*copies_params, *params, limit + 1, offset)).fetchall()
                has_more = len(rows) > limit
                rows = rows[:limit]
            else:
                sql = _page_sql(clauses, order)
                page_rows = conn.execute(sql, (*params, limit + 1, offset)).fetchall()
                has_more = len(page_rows) > limit
                page_rows = page_rows[:limit]
                counts = _copies_for_hashes(
                    conn,
                    sorted({row[11] for row in page_rows if row[11] is not None}),
                    copies_clauses,
                    copies_params,
                )
                # A row whose hash is NULL (or somehow unaccounted for) is its own single copy.
                rows = [(*row[:11], *counts.get(row[11], (1, 1))) for row in page_rows]
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc

    cache = _CacheIndex()
    notes = media_notes.fetch_notes([row[9] for row in rows])
    items = [_row_to_item(row, cache, notes) for row in rows]
    next_cursor = encode_cursor({"k": "media", "o": offset + limit}) if has_more else None
    return PageResult(items, next_cursor, has_more)


def _row_to_item(row: tuple, cache: _CacheIndex, notes: dict[str, dict[str, str]]) -> dict[str, Any]:
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
        "timestamp": parse_db_time(timestamp).isoformat() if timestamp else None,
        "media_type": media_type,
        "filename": filename or None,
        "bytes": int(file_length) if file_length else None,
        "sha256": sha256 or None,
        "cached": cached is not None,
        "cached_bytes": cached.bytes if cached else None,
        "cached_file": cached.name if cached else None,
        "copies": int(copies),
        "copies_in": int(copies_in),
        "deleted_at": parse_db_time(deleted_at).isoformat() if deleted_at else None,
        "notes": notes.get(sha256 or "", {}),
        "has_notes": bool(notes.get(sha256 or "")),
    }


def media_stats(
    chat_jid: str | Sequence[str] | None = None, exclude_chat_jid: str | Sequence[str] | None = None
) -> dict[str, Any]:
    """Totals by chat and by media type, from the rows and from the cache directories."""
    clauses, params = _media_filters(chat_jid, None, None, None, None, "m.", exclude_chat_jid)
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
