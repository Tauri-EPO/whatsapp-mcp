"""Agent notes about media files, kept in notes.db next to messages.db.

The bridge owns messages.db and the MCP server only reads it; anything the
agent wants to remember about a file (summary, tags, keep/disposable, a
transcript) needs a home of its own. notes.db is that home: a single table
keyed by the WhatsApp content hash (sha256 hex) rather than by message, so the
same file forwarded into three chats has one note and the note survives the
cached bytes being purged.

Notes are only readable and writable for hashes the agent can see, meaning a
message row carrying that hash exists in a chat allowed by
WHATSAPP_ALLOWED_CHATS. Unknown hashes and hashes that exist only in denied
chats are both reported as not_found, so a note never confirms that a file
exists somewhere the agent may not look.
"""

from __future__ import annotations

import os
import re
import sqlite3
from datetime import UTC, datetime
from typing import Any

import whatsapp
from errors import ToolError
from whatsapp import CHAT_POLICY

NOTES_DB_NAME = "notes.db"
MAX_VALUE_BYTES = 64 * 1024
MAX_KEY_LEN = 64
MAX_SEARCH_LIMIT = 200
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")

SCHEMA = """
CREATE TABLE IF NOT EXISTS media_notes (
    sha256 TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (sha256, key)
);
"""


def notes_db_path() -> str:
    """notes.db lives in the store directory, next to messages.db."""
    return os.path.join(os.path.dirname(os.path.abspath(whatsapp.MESSAGES_DB_PATH)), NOTES_DB_NAME)


def _connect(create: bool) -> sqlite3.Connection | None:
    """Open notes.db; with create=False a missing file yields None instead of an empty database."""
    path = notes_db_path()
    if not create and not os.path.exists(path):
        return None
    conn = sqlite3.connect(path, timeout=whatsapp.SQLITE_BUSY_TIMEOUT_S)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript(SCHEMA)
    return conn


def normalize_sha256(value: str) -> str:
    sha = (value or "").strip().lower()
    if not _SHA256_RE.match(sha):
        raise ToolError("invalid_argument", "sha256 must be the 64-character hex hash from list_media or list_messages")
    return sha


def _normalize_key(value: str) -> str:
    key = (value or "").strip()
    if not key or len(key) > MAX_KEY_LEN:
        raise ToolError(
            "invalid_argument", f"key must be 1..{MAX_KEY_LEN} characters (summary, tags, keep, transcript...)"
        )
    return key


def visible_hashes(hashes: list[str]) -> set[str]:
    """The subset of hashes that appear on at least one message in an allowed chat."""
    wanted = sorted({h for h in hashes if h})
    if not wanted:
        return set()
    clauses = [f"lower(hex(file_sha256)) IN ({','.join('?' * len(wanted))})"]
    params: list[Any] = list(wanted)
    if CHAT_POLICY.restricted:
        clause, clause_params = CHAT_POLICY.sql_clause("chat_jid")
        clauses.append(clause)
        params.extend(clause_params)
    try:
        conn = whatsapp._connect_messages_db()
        try:
            rows = conn.execute(
                f"SELECT DISTINCT lower(hex(file_sha256)) FROM messages WHERE {' AND '.join(clauses)}", params
            ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc
    return {row[0] for row in rows}


def _messages_for_hash(sha256: str) -> list[dict[str, Any]]:
    clauses = ["lower(hex(m.file_sha256)) = ?"]
    params: list[Any] = [sha256]
    if CHAT_POLICY.restricted:
        clause, clause_params = CHAT_POLICY.sql_clause("m.chat_jid")
        clauses.append(clause)
        params.extend(clause_params)
    try:
        conn = whatsapp._connect_messages_db()
        try:
            rows = conn.execute(
                f"""
                SELECT m.id, m.chat_jid, c.name, m.timestamp, m.media_type, m.filename, m.file_length
                FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid
                WHERE {" AND ".join(clauses)}
                ORDER BY m.timestamp DESC, m.id
                """,
                params,
            ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc
    return [
        {
            "message_id": r[0],
            "chat_jid": r[1],
            "chat_name": r[2],
            "timestamp": datetime.fromisoformat(r[3]).isoformat() if r[3] else None,
            "media_type": r[4],
            "filename": r[5] or None,
            "bytes": int(r[6]) if r[6] else None,
        }
        for r in rows
    ]


def _require_visible(sha256: str) -> list[dict[str, Any]]:
    messages = _messages_for_hash(sha256)
    if not messages:
        raise ToolError("not_found", "no media with this sha256 in the chats you can see")
    return messages


def fetch_notes(hashes: list[str]) -> dict[str, dict[str, str]]:
    """Notes for many hashes at once: {sha256: {key: value}}. Missing notes.db means no notes."""
    wanted = sorted({h for h in hashes if h})
    if not wanted:
        return {}
    conn = _connect(create=False)
    if conn is None:
        return {}
    try:
        rows = conn.execute(
            f"SELECT sha256, key, value FROM media_notes WHERE sha256 IN ({','.join('?' * len(wanted))})", wanted
        ).fetchall()
    finally:
        conn.close()
    out: dict[str, dict[str, str]] = {}
    for sha, key, value in rows:
        out.setdefault(sha, {})[key] = value
    return out


def annotate_media(sha256: str, key: str, value: str = "") -> dict[str, Any]:
    """Set (or, with an empty value, delete) one note on a visible hash."""
    sha = normalize_sha256(sha256)
    key = _normalize_key(key)
    value = value if value is not None else ""
    if len(value.encode("utf-8")) > MAX_VALUE_BYTES:
        raise ToolError("invalid_argument", f"value exceeds {MAX_VALUE_BYTES} bytes; store a summary, not the file")
    _require_visible(sha)
    now = datetime.now(UTC).replace(microsecond=0).isoformat()
    conn = _connect(create=True)
    assert conn is not None
    try:
        if value.strip() == "":
            deleted = conn.execute("DELETE FROM media_notes WHERE sha256 = ? AND key = ?", (sha, key)).rowcount
            conn.commit()
            return {"success": True, "sha256": sha, "key": key, "deleted": deleted > 0}
        conn.execute(
            """
            INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, ?, ?, ?)
            ON CONFLICT(sha256, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
            """,
            (sha, key, value, now),
        )
        conn.commit()
    finally:
        conn.close()
    return {"success": True, "sha256": sha, "key": key, "value": value, "updated_at": now}


def get_media_notes(sha256: str) -> dict[str, Any]:
    """Every note on a visible hash plus the messages that carry the file."""
    sha = normalize_sha256(sha256)
    messages = _require_visible(sha)
    notes: dict[str, dict[str, str]] = {}
    conn = _connect(create=False)
    if conn is not None:
        try:
            for key, value, updated_at in conn.execute(
                "SELECT key, value, updated_at FROM media_notes WHERE sha256 = ? ORDER BY key", (sha,)
            ):
                notes[key] = {"value": value, "updated_at": updated_at}
        finally:
            conn.close()
    return {"sha256": sha, "notes": notes, "messages": messages}


def search_media_notes(query: str, key: str | None = None, limit: int = 50) -> list[dict[str, Any]]:
    """Substring search over note values (and keys) for hashes the agent can see."""
    needle = (query or "").strip()
    if not needle:
        raise ToolError("invalid_argument", "query must not be empty")
    limit = max(1, min(int(limit), MAX_SEARCH_LIMIT))
    conn = _connect(create=False)
    if conn is None:
        return []
    clauses = ["(instr(lower(value), lower(?)) > 0 OR instr(value, ?) > 0)"]
    params: list[Any] = [needle, needle]
    if key:
        clauses.append("key = ?")
        params.append(_normalize_key(key))
    try:
        rows = conn.execute(
            f"SELECT sha256, key, value, updated_at FROM media_notes WHERE {' AND '.join(clauses)} ORDER BY updated_at DESC",
            params,
        ).fetchall()
    finally:
        conn.close()
    allowed = visible_hashes([r[0] for r in rows])
    return [
        {"sha256": sha, "key": k, "value": v, "updated_at": updated} for sha, k, v, updated in rows if sha in allowed
    ][:limit]
