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

import logging
import os
import re
import sqlite3
from datetime import UTC, datetime
from typing import Any

import whatsapp
from errors import ToolError
from whatsapp import CHAT_POLICY

logger = logging.getLogger(__name__)

NOTES_DB_NAME = "notes.db"
# Keys the tools write themselves; everything else (summary, tags, keep, ...) is
# the agent's own free-form vocabulary. See docs/TOOLS.md.
TRANSCRIPT_KEY = "transcript"
TRANSCRIPT_LANG_KEY = "transcript_lang"
TRANSCRIPT_BACKEND_KEY = "transcript_backend"
# Why a file has no transcript, written by the background worker
# (transcribe_worker.py) so it stops retrying a file whisper cannot read.
TRANSCRIPT_ERROR_KEY = "transcript_error"
MAX_VALUE_BYTES = 64 * 1024
MAX_KEY_LEN = 64
MAX_SEARCH_LIMIT = 200
# Transcript hits one message query may union in. This is a workload bound, not
# the old parameter bound (the hits are staged in a temp table now, see
# whatsapp._stage_transcript_hits): the index ranks them, so the cap keeps the
# best few thousand instead of truncating an arbitrary set. A word that occurs
# in more voice notes than this is a listing, not a search — search_media_notes.
MAX_TRANSCRIPT_MATCHES = 5000
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")

SCHEMA = """
CREATE TABLE IF NOT EXISTS media_notes (
    sha256 TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (sha256, key)
);
CREATE TABLE IF NOT EXISTS notes_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
"""

# --- Transcript index -------------------------------------------------------
#
# list_messages(query=...) has to reach what was *said* in a voice note, and the
# text only exists here: messages.content is empty for audio, so the bridge's
# messages_fts never sees it. A substring scan of every transcript note answered
# that at first, but it read the whole table per query, could not rank its hits
# and had to be capped so the matching hashes still fit in one SQL statement.
#
# transcripts_fts is the same idea as messages_fts, one database down: an FTS5
# index the MCP server owns end to end (the bridge never opens notes.db, so the
# "no FTS triggers from Python" rule of messages.db does not apply). Same
# tokenizer as whatsapp-bridge/fts.go, so "orcamento" finds "orçamento" and
# "ana" does not match "semana" whether the word was written or spoken, and
# bm25 gives audio hits a score that can be compared with the message ones.
#
# It is a plain (not external-content) table: media_notes rows are tiny and the
# write path is one voice note at a time, so keeping the text twice is cheaper
# than the trigger machinery — and a build without FTS5 then degrades to the
# substring scan instead of breaking every write.
TRANSCRIPTS_FTS_TABLE = "transcripts_fts"
# One statement on purpose: executescript() would commit the note write this
# runs inside, so the note and its index entry would stop being atomic.
_FTS_SCHEMA = (
    "CREATE VIRTUAL TABLE IF NOT EXISTS transcripts_fts "
    "USING fts5(sha256 UNINDEXED, text, tokenize='unicode61 remove_diacritics 2')"
)
# Bumping this rebuilds the index from the notes on the next use.
_FTS_VERSION_KEY = "transcripts_fts_version"
_FTS_VERSION = "1"
_fts_warned = False


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


def _rebuild_transcripts_fts(conn: sqlite3.Connection) -> None:
    """Fill the index from the transcript notes and mark it current.

    Runs once on a store whose notes predate the index, and again whenever
    _FTS_VERSION moves. The marker is written in the same transaction, so a
    rebuild that dies halfway is simply repeated on the next use.
    """
    conn.execute(f"DELETE FROM {TRANSCRIPTS_FTS_TABLE}")
    conn.execute(
        f"INSERT INTO {TRANSCRIPTS_FTS_TABLE} (sha256, text) SELECT sha256, value FROM media_notes WHERE key = ?",
        (TRANSCRIPT_KEY,),
    )
    conn.execute(
        "INSERT INTO notes_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
        (_FTS_VERSION_KEY, _FTS_VERSION),
    )


def _ensure_transcripts_fts(conn: sqlite3.Connection) -> bool:
    """True when the transcript index is present and current on this connection.

    False means this SQLite build has no fts5 (or the index could not be
    created): every caller then falls back to the substring scan, so search
    keeps working, unranked.
    """
    global _fts_warned
    try:
        conn.execute(_FTS_SCHEMA)
        row = conn.execute("SELECT value FROM notes_meta WHERE key = ?", (_FTS_VERSION_KEY,)).fetchone()
        if row is None or row[0] != _FTS_VERSION:
            _rebuild_transcripts_fts(conn)
    except sqlite3.Error as exc:
        # First time loudly, then quietly: a locked database repeats, and the
        # fallback is correct, only slower and unranked.
        logger.log(
            logging.DEBUG if _fts_warned else logging.WARNING,
            "transcript index unavailable, falling back to substring search: %s",
            exc,
        )
        _fts_warned = True
        return False
    return True


def _index_transcript(conn: sqlite3.Connection, sha256: str, value: str) -> None:
    """Mirror one transcript note into the index (an empty value removes it).

    Runs inside the caller's write transaction, so the note and its index entry
    commit together. When the index cannot be touched at all (no fts5, or a
    database busy long enough to time out) the version marker goes with it: the
    index is then stale, and dropping the marker is what makes the next use
    rebuild it instead of answering from it. If even that fails, the exception
    leaves annotate_media's transaction unfinished and the note is not written.
    """
    if not _ensure_transcripts_fts(conn):
        conn.execute("DELETE FROM notes_meta WHERE key = ?", (_FTS_VERSION_KEY,))
        return
    # sha256 is UNINDEXED, so this scans the index; transcripts are written one
    # voice note at a time, and the alternative (a rowid map) is a second table
    # to keep in sync.
    conn.execute(f"DELETE FROM {TRANSCRIPTS_FTS_TABLE} WHERE sha256 = ?", (sha256,))
    if value:
        conn.execute(f"INSERT INTO {TRANSCRIPTS_FTS_TABLE} (sha256, text) VALUES (?, ?)", (sha256, value))


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
    # Compare the blob, not lower(hex(...)): that is what idx_messages_file_sha256 indexes.
    clauses = [f"file_sha256 IN ({','.join('?' * len(wanted))})"]
    params: list[Any] = [bytes.fromhex(h) for h in wanted]
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
    clauses = ["m.file_sha256 = ?"]
    params: list[Any] = [bytes.fromhex(sha256)]
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
            if key == TRANSCRIPT_KEY:
                _index_transcript(conn, sha, "")
            conn.commit()
            return {"success": True, "sha256": sha, "key": key, "deleted": deleted > 0}
        conn.execute(
            """
            INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, ?, ?, ?)
            ON CONFLICT(sha256, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
            """,
            (sha, key, value, now),
        )
        # Every transcript write lands here (transcribe_audio, the ingest worker
        # and an agent writing the key by hand), so this is the one hook the
        # index needs.
        if key == TRANSCRIPT_KEY:
            _index_transcript(conn, sha, value)
        conn.commit()
    finally:
        conn.close()
    return {"success": True, "sha256": sha, "key": key, "value": value, "updated_at": now}


def store_transcript(sha256: str, result: dict[str, Any]) -> None:
    """Write one transcription result under the transcript keys, clearing a past failure.

    Shared by the transcribe_audio tool and the background worker so both spell
    the cache the same way. Raises ToolError when the hash is not visible.
    """
    annotate_media(sha256, TRANSCRIPT_KEY, result["text"])
    for key, value in ((TRANSCRIPT_LANG_KEY, result.get("language")), (TRANSCRIPT_BACKEND_KEY, result.get("backend"))):
        if value:
            annotate_media(sha256, key, str(value))
    # An empty value deletes: a file that transcribes now is no longer failing.
    annotate_media(sha256, TRANSCRIPT_ERROR_KEY, "")


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


def _match_transcripts(conn: sqlite3.Connection, needle: str, limit: int) -> list[tuple[str, float]]:
    """Index hits for ``needle`` as (sha256, bm25 score), best match first."""
    sql = (
        f"SELECT sha256, bm25({TRANSCRIPTS_FTS_TABLE}) AS score FROM {TRANSCRIPTS_FTS_TABLE} "
        f"WHERE {TRANSCRIPTS_FTS_TABLE} MATCH ? ORDER BY score LIMIT ?"
    )
    bound = max(1, int(limit))
    try:
        rows = conn.execute(sql, (needle, bound)).fetchall()
    except sqlite3.OperationalError:
        # Raw text was not valid FTS5 syntax (operator characters, unbalanced
        # quotes...); retry with every token quoted, as the message side does.
        rows = conn.execute(sql, (whatsapp._fts_quote_tokens(needle), bound)).fetchall()
    return [(sha, float(score)) for sha, score in rows if _SHA256_RE.match(sha or "")]


def _substring_transcripts(conn: sqlite3.Connection, needle: str, limit: int) -> list[tuple[str, float]]:
    """Fallback for what the index cannot serve; unranked, hence a score of 0.0."""
    rows = conn.execute(
        "SELECT sha256 FROM media_notes WHERE key = ? "
        "AND (instr(lower(value), lower(?)) > 0 OR instr(value, ?) > 0) "
        "ORDER BY updated_at DESC LIMIT ?",
        (TRANSCRIPT_KEY, needle, needle, max(1, int(limit))),
    ).fetchall()
    return [(row[0], 0.0) for row in rows if _SHA256_RE.match(row[0] or "")]


def transcript_matches(query: str, limit: int = MAX_TRANSCRIPT_MATCHES) -> list[tuple[str, float]]:
    """Hashes whose stored transcript matches ``query``, as (sha256, score) pairs.

    This is how ``list_messages(query=...)`` reaches spoken words: ``messages_fts``
    is the bridge's index over ``messages.content`` and knows nothing about
    notes.db, so the message query unions in the rows carrying one of these
    hashes. The score is the FTS5 bm25 value (negative, lower is a better match)
    so the caller can rank audio hits together with the message ones; the
    substring fallback cannot rank and reports 0.0, which puts its hits after
    every ranked one. A missing notes.db, an unreadable one, or an empty query
    all mean "no transcript matched": search degrades to content-only, it never
    fails.
    """
    needle = (query or "").strip()
    if not needle:
        return []
    conn = _connect(create=False)
    if conn is None:
        return []
    try:
        if whatsapp._fts_query_kind(needle) == "fts" and _ensure_transcripts_fts(conn):
            # A rebuild may have happened; nothing else on this connection will
            # commit it.
            conn.commit()
            return _match_transcripts(conn, needle, limit)
        return _substring_transcripts(conn, needle, limit)
    except sqlite3.Error:
        return []
    finally:
        conn.close()


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
