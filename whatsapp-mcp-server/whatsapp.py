import json
import logging
import os
import os.path
import re
import sqlite3
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import requests

import audio
from chat_policy import load_chat_policy

# All diagnostics go through logging (stderr). Never use print here: on the stdio
# transport stdout is the MCP protocol channel and stray output breaks it.
logger = logging.getLogger("whatsapp_mcp")

# Configuration via environment variables with sensible defaults
_DEFAULT_BRIDGE_STORE_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "whatsapp-bridge", "store")
MESSAGES_DB_PATH = os.getenv(
    "WHATSAPP_DB_PATH",
    os.path.join(_DEFAULT_BRIDGE_STORE_DIR, "messages.db"),
)
WHATSMEOW_DB_PATH = os.getenv(
    "WHATSMEOW_DB_PATH",
    os.path.join(_DEFAULT_BRIDGE_STORE_DIR, "whatsapp.db"),
)
WHATSAPP_API_BASE_URL = os.getenv("WHATSAPP_API_URL", "http://localhost:8080/api")

_BRIDGE_TOKEN_PATH = os.path.join(os.path.dirname(WHATSMEOW_DB_PATH), ".bridge-token")

# The bridge opens messages.db in WAL mode, so reads never block on its writes;
# the timeout covers the brief exclusive locks WAL still needs (checkpoints,
# schema changes) instead of surfacing "database is locked" to the agent.
SQLITE_BUSY_TIMEOUT_S = 5.0


# Conversation allow-list (WHATSAPP_ALLOWED_CHATS). Read tools filter to these
# chats, write tools refuse anything else. Unrestricted when unset.
CHAT_POLICY = load_chat_policy()


def _policy_denied(jid: str | None) -> str | None:
    """Denial message when the policy blocks jid, else None."""
    if CHAT_POLICY.allows(jid):
        return None
    return CHAT_POLICY.denial_message(jid)


def _connect_messages_db() -> sqlite3.Connection:
    return sqlite3.connect(MESSAGES_DB_PATH, timeout=SQLITE_BUSY_TIMEOUT_S)


def _connect_whatsmeow_db() -> sqlite3.Connection:
    return sqlite3.connect(WHATSMEOW_DB_PATH, timeout=SQLITE_BUSY_TIMEOUT_S)


# --- Full-text search -------------------------------------------------------
#
# The bridge owns an FTS5 index over messages.content (messages_fts, unicode61
# tokenizer with diacritics removed; see whatsapp-bridge/fts.go). When it is
# present, list_messages(query=...) uses MATCH: accent-insensitive, whole-word,
# with AND / OR / NOT / "phrase" / prefix* operators and BM25 relevance. When it
# is absent (bridge built without the sqlite_fts5 tag) the old substring scan
# is used, so search never breaks — it is just slower and less precise.

MESSAGES_FTS_TABLE = "messages_fts"

# unicode61 splits only on non-word characters, so scripts without spaces
# (Han, kana, Thai...) tokenize as one blob and MATCH would miss substrings.
# Those queries stay on the substring scan.
_UNSEGMENTED_SCRIPT_RE = re.compile(r"[\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff\u0e00-\u0e7f]")
_FTS_TOKEN_RE = re.compile(r"\S+")


def _fts_available(conn: sqlite3.Connection) -> bool:
    """True when messages_fts exists and this SQLite build can read it."""
    try:
        row = conn.execute(
            "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", (MESSAGES_FTS_TABLE,)
        ).fetchone()
        if not row or row[0] == 0:
            return False
        conn.execute(f"SELECT rowid FROM {MESSAGES_FTS_TABLE} LIMIT 0")
        return True
    except sqlite3.Error:
        return False


def _fts_query_kind(query: str) -> str:
    """'fts' when the query can go to the index, 'substring' otherwise."""
    if not query or not query.strip():
        return "substring"
    if _UNSEGMENTED_SCRIPT_RE.search(query):
        return "substring"
    return "fts"


def _fts_quote_tokens(query: str) -> str:
    """Escape a free-text query so FTS5 treats every token literally (implicit AND).

    Used as the retry when the raw query is not valid FTS5 syntax: agents pass
    arbitrary user text, and characters like ( ) - * or bare AND/OR/NOT are
    operators. A trailing * on a token is kept so plain prefix searches survive.
    """
    parts = []
    for token in _FTS_TOKEN_RE.findall(query):
        prefix = token.endswith("*") and len(token) > 1
        core = token[:-1] if prefix else token
        core = core.replace('"', '""')
        if not core:
            continue
        parts.append(f'"{core}"' + ("*" if prefix else ""))
    return " ".join(parts)


def _read_bridge_token() -> str | None:
    env = os.getenv("WHATSAPP_BRIDGE_TOKEN", "").strip()
    if env:
        return env
    try:
        with open(_BRIDGE_TOKEN_PATH, encoding="utf-8") as fh:
            value = fh.read().strip()
            return value or None
    except FileNotFoundError:
        return None
    except OSError:
        return None


def _bridge_headers() -> dict[str, str]:
    token = _read_bridge_token()
    if not token:
        return {}
    return {"Authorization": f"Bearer {token}"}


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: str | None = None
    media_type: str | None = None
    # Bridge-side filename of the media (documents keep the sender's original
    # name; other media get a generated `<type>_<timestamp>_<id>.<ext>`). For
    # media_type == "reaction" the bridge reuses this column for the reacted-to
    # message ID, exposed to callers as `reaction_to_message_id` instead.
    filename: str | None = None
    # ID of the message this one is replying to (NULL for non-replies).
    quoted_message_id: str | None = None
    # Set when the message was revoked ("delete for everyone"), by the sender or
    # by us. Content, media and filename are kept on purpose: this archive is
    # the account owner's copy. Only delete_message(for_everyone=False) removes
    # a row.
    deleted_at: datetime | None = None
    # True for WhatsApp "view once" media. The archive keeps a copy; the phone's
    # single viewing is unaffected because the bridge never sends the view receipt.
    view_once: bool = False


# One column list and one mapper for every query that yields Message rows.
# Every column added here is available to all readers; positional indexing
# elsewhere is a bug.
MESSAGE_COLUMNS = (
    "messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, "
    "messages.chat_jid, messages.id, messages.media_type, messages.quoted_message_id, messages.filename, "
    "messages.deleted_at, messages.view_once"
)


def _row_to_message(row: tuple) -> Message:
    """Build a Message from a row selected with MESSAGE_COLUMNS (in that order)."""
    (
        timestamp,
        sender,
        chat_name,
        content,
        is_from_me,
        chat_jid,
        msg_id,
        media_type,
        quoted_id,
        filename,
        deleted,
        view_once,
    ) = row
    return Message(
        timestamp=datetime.fromisoformat(timestamp),
        sender=sender,
        chat_name=chat_name,
        content=content,
        is_from_me=is_from_me,
        chat_jid=chat_jid,
        id=msg_id,
        media_type=media_type,
        quoted_message_id=quoted_id,
        filename=filename,
        deleted_at=datetime.fromisoformat(deleted) if deleted else None,
        view_once=bool(view_once),
    )


@dataclass
class Chat:
    jid: str
    name: str | None
    last_message_time: datetime | None
    last_message: str | None = None
    last_sender: str | None = None
    last_is_from_me: bool | None = None
    # Bridge read marker (chats.last_read_time): how far we have read this
    # chat, from read receipts and history-sync backfill. NULL when the
    # bridge has never seen a read for the chat, or predates the column.
    last_read_time: datetime | None = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

    @property
    def unread(self) -> bool:
        """Whether the chat's last message is inbound and unread by us.

        With a read marker this is genuine unread — a chat read on the phone
        or another linked device is not reported. Without one (older bridge,
        or a chat WhatsApp never reported a read for) it degrades to the old
        heuristic: unread if the last message is inbound.

        A missing last-message row (`last_is_from_me is None`) cannot establish
        direction — protocol/unsupported events can advance last_message_time
        without storing a message — so those chats are not reported as unread.
        """
        if self.last_message_time is None or self.last_is_from_me is None:
            return False
        if self.last_is_from_me:
            return False
        if self.last_read_time is None:
            return True
        return self.last_message_time > self.last_read_time


@dataclass
class Contact:
    phone_number: str
    name: str | None
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: list[Message]
    after: list[Message]


def msg_to_dict(message: Message, include_sender_name: bool = True) -> dict[str, Any]:
    """Convert a Message dataclass to a dictionary for JSON serialization."""
    # Extract phone number from JID (e.g., "1234567890@s.whatsapp.net" -> "1234567890")
    sender_phone = message.sender.split("@")[0] if "@" in message.sender else message.sender

    sender_name = None
    sender_display = None
    if include_sender_name:
        if message.is_from_me:
            sender_name = "Me"
            sender_display = "Me"
        else:
            resolved_name = get_sender_name(message.sender)
            # Check if we got an actual name (not just the JID back)
            if resolved_name and resolved_name != message.sender and resolved_name != sender_phone:
                sender_name = resolved_name
                sender_display = f"{resolved_name} ({sender_phone})"
            else:
                sender_name = sender_phone
                sender_display = sender_phone

    return {
        "id": message.id,
        "timestamp": message.timestamp.isoformat(),
        "sender_jid": message.sender,
        "sender_phone": sender_phone,
        "sender_name": sender_name,
        "sender_display": sender_display,  # "Name (phone)" or just phone if no name
        "content": message.content,
        "is_from_me": message.is_from_me,
        "chat_jid": message.chat_jid,
        "chat_name": message.chat_name,
        "media_type": message.media_type,
        "filename": (message.filename or None)
        if message.media_type and message.media_type not in ("reaction", "poll_vote")
        else None,
        "reaction_to_message_id": (message.filename if message.media_type == "reaction" else None),
        "poll_message_id": (message.filename if message.media_type == "poll_vote" else None),
        "quoted_message_id": message.quoted_message_id,
        "deleted_at": message.deleted_at.isoformat() if message.deleted_at else None,
        "view_once": message.view_once,
    }


def chat_to_dict(chat: "Chat") -> dict[str, Any]:
    """Convert a Chat dataclass to a dictionary for JSON serialization."""
    return {
        "jid": chat.jid,
        "name": chat.name,
        "is_group": chat.is_group,
        "last_message_time": chat.last_message_time.isoformat() if chat.last_message_time else None,
        "last_message": chat.last_message,
        "last_sender": chat.last_sender,
        "last_is_from_me": chat.last_is_from_me,
        "last_read_time": chat.last_read_time.isoformat() if chat.last_read_time else None,
        "unread": chat.unread,
    }


def contact_to_dict(contact: "Contact") -> dict[str, Any]:
    """Convert a Contact dataclass to a dictionary for JSON serialization."""
    return {"phone_number": contact.phone_number, "name": contact.name, "jid": contact.jid}


def _last_read_time_select(cursor: sqlite3.Cursor, table_alias: str) -> str:
    """SELECT expression for chats.last_read_time, or a NULL literal.

    The bridge adds the column through its own migration, so a messages.db
    written by an older bridge doesn't have it yet. Reads must keep working
    against such a store — those chats simply report last_read_time = None.
    """
    columns = {row[1] for row in cursor.execute("PRAGMA table_info(chats)").fetchall()}
    return f"{table_alias}.last_read_time" if "last_read_time" in columns else "NULL"


def _last_message_join(chat_alias: str, msg_alias: str) -> str:
    """Deterministic single-row join to the chat's latest message.

    Multiple messages can share last_message_time (history sync is second-
    resolution). Joining solely on timestamp would duplicate chat rows and
    make last_is_from_me / unread non-deterministic; pick one id as tie-break.
    """
    return f"""
            LEFT JOIN messages {msg_alias} ON {chat_alias}.jid = {msg_alias}.chat_jid
                AND {msg_alias}.id = (
                    SELECT m.id FROM messages m
                    WHERE m.chat_jid = {chat_alias}.jid
                      AND m.timestamp = {chat_alias}.last_message_time
                    ORDER BY m.id DESC
                    LIMIT 1
                )
    """


def _sender_aliases(value: str) -> list[str]:
    # messages.sender is written inconsistently: the same contact may appear as
    # bare phone ("13232432100"), full phone JID ("13232432100@s.whatsapp.net"),
    # bare LID ("231241139937355"), or full LID JID ("231241139937355@lid").
    # whatsmeow_lid_map (whatsapp.db) maps pn<->lid; we emit all four forms so
    # an IN-based filter catches every row regardless of which form was stored.
    bare = value.split("@", 1)[0]
    pn: str | None = None
    lid: str | None = None
    if os.path.isfile(WHATSMEOW_DB_PATH):
        try:
            conn = _connect_whatsmeow_db()
            try:
                row = conn.execute("SELECT lid FROM whatsmeow_lid_map WHERE pn = ?", (bare,)).fetchone()
                if row:
                    pn, lid = bare, row[0]
                else:
                    row = conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ?", (bare,)).fetchone()
                    if row:
                        lid, pn = bare, row[0]
            finally:
                conn.close()
        except sqlite3.Error:
            pass

    aliases: list[str] = []
    if pn:
        aliases += [pn, f"{pn}@s.whatsapp.net"]
    if lid:
        aliases += [lid, f"{lid}@lid"]
    if not aliases:
        # No mapping found; emit the bare form plus both possible suffixes so
        # we still match whichever form the bridge happened to store.
        aliases = [bare, f"{bare}@s.whatsapp.net", f"{bare}@lid"]
    return aliases


def _resolve_lid_to_phone(lid_or_jid: str) -> str | None:
    """Resolve a WhatsApp LID (linked device identifier) to a phone number.

    WhatsApp's newer protocol uses opaque LIDs (e.g. '35047067385985') as sender
    identifiers instead of phone numbers. The whatsmeow_lid_map table maps these
    back to real phone numbers.

    Returns the phone number if found, None otherwise.
    """
    if not os.path.exists(WHATSMEOW_DB_PATH):
        return None
    # Extract the numeric part from JID-style strings (e.g. '35047067385985@lid')
    lid = lid_or_jid.split("@")[0] if "@" in lid_or_jid else lid_or_jid
    try:
        conn = _connect_whatsmeow_db()
        cursor = conn.cursor()
        cursor.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", (lid,))
        row = cursor.fetchone()
        return row[0] if row else None
    except sqlite3.Error:
        return None
    finally:
        if "conn" in locals():
            conn.close()


def _resolve_name_from_whatsmeow(jid: str) -> str | None:
    """Look up a contact name from whatsmeow's contact store (whatsapp.db).

    Handles both standard JIDs (12345@s.whatsapp.net) and LIDs (opaque numeric
    identifiers used by WhatsApp's linked device protocol). LIDs are first
    resolved to phone numbers via whatsmeow_lid_map, then looked up in contacts.

    Falls back gracefully if the DB or table doesn't exist.
    """
    if not os.path.exists(WHATSMEOW_DB_PATH):
        return None

    lookup_jid = jid
    jid_prefix = jid.split("@")[0] if "@" in jid else jid
    jid_suffix = jid.split("@")[1] if "@" in jid else ""

    # If this is a LID (@lid suffix) or a raw number, try LID map first.
    # LIDs overlap in length with phone numbers (12-15 digits) so we always
    # attempt LID resolution and fall through to direct contact lookup if not found.
    if jid_suffix in ("lid", ""):
        phone = _resolve_lid_to_phone(jid_prefix)
        if phone:
            lookup_jid = phone + "@s.whatsapp.net"
        elif jid_suffix == "lid":
            # Definitely a LID but not in the map — can't resolve
            return None

    try:
        conn = _connect_whatsmeow_db()
        cursor = conn.cursor()
        # whatsmeow_contacts columns: our_jid, their_jid, first_name, full_name, push_name, business_name
        cursor.execute(
            "SELECT full_name, push_name, first_name, business_name FROM whatsmeow_contacts WHERE their_jid = ? LIMIT 1",
            (lookup_jid,),
        )
        row = cursor.fetchone()
        if row:
            # Prefer full_name, then push_name, then first_name, then business_name
            return row[0] or row[1] or row[2] or row[3] or None
        return None
    except sqlite3.Error:
        return None
    finally:
        if "conn" in locals():
            conn.close()


def get_sender_name(sender_jid: str) -> str:
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # First try matching by exact JID
        cursor.execute(
            """
            SELECT name
            FROM chats
            WHERE jid = ?
            LIMIT 1
        """,
            (sender_jid,),
        )

        result = cursor.fetchone()

        # If no result, try looking for the number within JIDs
        if not result:
            # Extract the phone number part if it's a JID
            if "@" in sender_jid:
                phone_part = sender_jid.split("@")[0]
            else:
                phone_part = sender_jid

            cursor.execute(
                """
                SELECT name
                FROM chats
                WHERE jid LIKE ?
                LIMIT 1
            """,
                (f"%{phone_part}%",),
            )

            result = cursor.fetchone()

        if result and result[0] and not result[0].replace("+", "").isdigit():
            return result[0]

        # Fall back to whatsmeow contact store
        whatsmeow_name = _resolve_name_from_whatsmeow(sender_jid)
        if whatsmeow_name:
            return whatsmeow_name

        # Try with @s.whatsapp.net suffix if bare number
        if "@" not in sender_jid:
            whatsmeow_name = _resolve_name_from_whatsmeow(sender_jid + "@s.whatsapp.net")
            if whatsmeow_name:
                return whatsmeow_name

        return sender_jid

    except sqlite3.Error as e:
        logger.error("Database error while getting sender name: %s", e)
        return sender_jid
    finally:
        if "conn" in locals():
            conn.close()


def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    content_prefix = ""
    if hasattr(message, "media_type") and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        logger.warning("Error formatting message: %s", e)
    return output


def format_messages_list(messages: list[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    for message in messages:
        output += format_message(message, show_chat_info)
    return output


# SQLite's default parameter limit is 999 on older builds; 3 params per hit.
_CONTEXT_HITS_PER_QUERY = 200


def _fetch_context_windows(
    cursor: sqlite3.Cursor,
    hits: list[Message],
    before: int,
    after: int,
    include_deleted: bool = True,
) -> dict[tuple[str, str], tuple[list[Message], list[Message]]]:
    """Fetch the before/after window for many hits in one query per batch.

    Returns {(id, chat_jid): (before_msgs newest-first, after_msgs oldest-first)}.
    Window membership is per chat and ranked with ROW_NUMBER(), so the cost is one
    statement per _CONTEXT_HITS_PER_QUERY hits instead of two per hit.
    """
    windows: dict[tuple[str, str], tuple[list[Message], list[Message]]] = {
        (hit.id, hit.chat_jid): ([], []) for hit in hits
    }
    if not hits or (before <= 0 and after <= 0):
        return windows
    deleted_filter = "" if include_deleted else "AND messages.deleted_at IS NULL"
    for start in range(0, len(hits), _CONTEXT_HITS_PER_QUERY):
        batch = hits[start : start + _CONTEXT_HITS_PER_QUERY]
        values = ",".join("(?, ?, ?)" for _ in batch)
        hit_params: list[Any] = []
        for hit in batch:
            hit_params.extend([hit.id, hit.chat_jid, hit.timestamp.isoformat()])
        sql = f"""
            WITH hits(id, chat_jid, ts) AS (VALUES {values})
            SELECT * FROM (
                SELECT {MESSAGE_COLUMNS}, hits.id AS hit_id, 'before' AS side,
                       ROW_NUMBER() OVER (
                           PARTITION BY hits.id, hits.chat_jid
                           ORDER BY messages.timestamp DESC, messages.id DESC
                       ) AS rn
                FROM hits
                JOIN messages ON messages.chat_jid = hits.chat_jid AND messages.timestamp < hits.ts
                JOIN chats ON messages.chat_jid = chats.jid
                WHERE 1=1 {deleted_filter}
            ) WHERE rn <= ?
            UNION ALL
            SELECT * FROM (
                SELECT {MESSAGE_COLUMNS}, hits.id AS hit_id, 'after' AS side,
                       ROW_NUMBER() OVER (
                           PARTITION BY hits.id, hits.chat_jid
                           ORDER BY messages.timestamp ASC, messages.id ASC
                       ) AS rn
                FROM hits
                JOIN messages ON messages.chat_jid = hits.chat_jid AND messages.timestamp > hits.ts
                JOIN chats ON messages.chat_jid = chats.jid
                WHERE 1=1 {deleted_filter}
            ) WHERE rn <= ?
            ORDER BY hit_id, side, rn
        """
        cursor.execute(sql, (*hit_params, before, after))
        width = len(MESSAGE_COLUMNS.split(","))
        for row in cursor.fetchall():
            message = _row_to_message(row[:width])
            hit_id, side = row[width], row[width + 1]
            key = (hit_id, message.chat_jid)
            if key not in windows:
                continue
            windows[key][0 if side == "before" else 1].append(message)
    return windows


def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | None = None,
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
    sort_by: str = "newest",
    include_deleted: bool = True,
) -> list[dict[str, Any]]:
    """Get messages matching the specified criteria with optional context.

    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content. With the bridge's
            FTS5 index this is accent-insensitive and word-based, and supports
            AND / OR / NOT, "exact phrase" and prefix* operators; without it a
            plain substring match is used.
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
        sort_by: Sort order - "newest" (default), "oldest" for chronological ordering, or
            "relevance" (best match first; only meaningful with query and the FTS index)
        include_deleted: Keep revoked messages in the result (default True; they carry
            deleted_at and their original content). False hides them.

    Returns:
        List of message dictionaries with id, timestamp, sender, content, etc.
    """
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        use_fts = bool(query) and _fts_query_kind(query) == "fts" and _fts_available(conn)

        # Build base query
        query_parts = [f"SELECT {MESSAGE_COLUMNS} FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        if use_fts:
            query_parts.append(f"JOIN {MESSAGES_FTS_TABLE} ON {MESSAGES_FTS_TABLE}.rowid = messages.rowid")
        where_clauses = []
        params = []

        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")

            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")

            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            aliases = _sender_aliases(sender_phone_number)
            placeholders = ",".join("?" * len(aliases))
            where_clauses.append(f"messages.sender IN ({placeholders})")
            params.extend(aliases)

        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)

        if CHAT_POLICY.restricted:
            clause, clause_params = CHAT_POLICY.sql_clause("messages.chat_jid")
            where_clauses.append(clause)
            params.extend(clause_params)

        if not include_deleted:
            where_clauses.append("messages.deleted_at IS NULL")

        match_param_index = None
        if query and use_fts:
            where_clauses.append(f"{MESSAGES_FTS_TABLE} MATCH ?")
            match_param_index = len(params)
            params.append(query)
        elif query:
            # SQLite's LOWER() only handles ASCII, so LIKE LOWER(...) silently
            # excludes Unicode matches. instr() on the raw column preserves them.
            where_clauses.append("(instr(LOWER(messages.content), LOWER(?)) > 0 OR instr(messages.content, ?) > 0)")
            params.extend([query, query])

        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Add sorting and pagination
        offset = page * limit
        if sort_by == "relevance" and use_fts:
            query_parts.append(f"ORDER BY bm25({MESSAGES_FTS_TABLE}), messages.timestamp DESC")
        else:
            order = "ASC" if sort_by == "oldest" else "DESC"
            query_parts.append(f"ORDER BY messages.timestamp {order}")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])

        sql = " ".join(query_parts)
        try:
            cursor.execute(sql, tuple(params))
        except sqlite3.OperationalError:
            if match_param_index is None:
                raise
            # Raw text was not valid FTS5 syntax (operator characters, unbalanced
            # quotes...). Retry with every token quoted so it is matched literally.
            params[match_param_index] = _fts_quote_tokens(query)
            cursor.execute(sql, tuple(params))
        messages = cursor.fetchall()

        result = [_row_to_message(msg) for msg in messages]

        if include_context and result:
            # One query per batch of hits (not two per hit); dedupe on (id, chat_jid)
            # because message IDs repeat across chats (forwards, broadcasts).
            windows = _fetch_context_windows(cursor, result, context_before, context_after, include_deleted)
            seen: set[tuple[str, str]] = set()
            messages_with_context: list[Message] = []

            def _add(message: Message) -> None:
                key = (message.id, message.chat_jid)
                if key not in seen:
                    seen.add(key)
                    messages_with_context.append(message)

            for msg in result:
                before_msgs, after_msgs = windows[(msg.id, msg.chat_jid)]
                # Windows are fetched nearest-first; emit them in reading order.
                for ctx_msg in reversed(before_msgs):
                    _add(ctx_msg)
                _add(msg)
                for ctx_msg in after_msgs:
                    _add(ctx_msg)

            return [msg_to_dict(msg) for msg in messages_with_context]

        # Return messages without context
        return [msg_to_dict(msg) for msg in result]

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []
    finally:
        if "conn" in locals():
            conn.close()


def get_message_context(
    message_id: str, before: int = 5, after: int = 5, chat_jid: str | None = None, include_deleted: bool = True
) -> MessageContext:
    """Get context around a specific message.

    The messages table is keyed by (id, chat_jid): the same WhatsApp message ID
    can legitimately exist in several chats (forwards, broadcasts). Passing
    chat_jid makes the lookup a primary-key hit and removes the ambiguity;
    without it the most recent row with that ID is used.
    """
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # Get the target message first
        select = f"SELECT {MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid"
        if chat_jid:
            cursor.execute(select + " WHERE messages.id = ? AND messages.chat_jid = ?", (message_id, chat_jid))
        else:
            cursor.execute(select + " WHERE messages.id = ? ORDER BY messages.timestamp DESC LIMIT 1", (message_id,))
        msg_data = cursor.fetchone()

        if not msg_data:
            where = f" in chat {chat_jid}" if chat_jid else ""
            raise ValueError(f"Message with ID {message_id}{where} not found")
        target_message = _row_to_message(msg_data)
        if denied := _policy_denied(target_message.chat_jid):
            raise ValueError(denied)
        deleted_filter = "" if include_deleted else "AND messages.deleted_at IS NULL"

        # Get messages before
        cursor.execute(
            f"""
            SELECT {MESSAGE_COLUMNS}
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ? {deleted_filter}
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """,
            (target_message.chat_jid, msg_data[0], before),
        )

        before_messages = [_row_to_message(msg) for msg in cursor.fetchall()]

        # Get messages after
        cursor.execute(
            f"""
            SELECT {MESSAGE_COLUMNS}
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ? {deleted_filter}
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """,
            (target_message.chat_jid, msg_data[0], after),
        )

        after_messages = [_row_to_message(msg) for msg in cursor.fetchall()]

        return MessageContext(message=target_message, before=before_messages, after=after_messages)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise
    finally:
        if "conn" in locals():
            conn.close()


def list_chats(
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> list[dict[str, Any]]:
    """Get chats matching the specified criteria.

    Returns:
        List of chat dictionaries with jid, name, is_group, last_message, etc.
    """
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # The last message is always joined — is_from_me feeds the unread
        # flag — but its content is only selected when asked for. The columns
        # are referenced by tuple index downstream, so the result shape stays
        # constant across the branch.
        if include_last_message:
            last_message_select = "messages.content as last_message, messages.sender as last_sender"
        else:
            last_message_select = "NULL as last_message, NULL as last_sender"

        query_parts = [
            f"""
            SELECT
                chats.jid,
                chats.name,
                chats.last_message_time,
                {last_message_select},
                messages.is_from_me as last_is_from_me,
                {_last_read_time_select(cursor, "chats")}
            FROM chats
            {_last_message_join("chats", "messages")}
        """
        ]

        where_clauses = []
        params = []

        if query:
            # instr() on the raw column matches Unicode; LOWER()+LIKE only covers ASCII.
            where_clauses.append(
                "(instr(LOWER(chats.name), LOWER(?)) > 0 OR instr(chats.name, ?) > 0 OR chats.jid LIKE ?)"
            )
            params.extend([query, query, f"%{query}%"])

        if CHAT_POLICY.restricted:
            clause, clause_params = CHAT_POLICY.sql_clause("chats.jid")
            where_clauses.append(clause)
            params.extend(clause_params)

        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")

        # Add pagination
        offset = (page) * limit
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])

        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()

        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
                last_read_time=datetime.fromisoformat(chat_data[6]) if chat_data[6] else None,
            )
            result.append(chat_to_dict(chat))

        return result

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []
    finally:
        if "conn" in locals():
            conn.close()


def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search contacts by name or phone number.

    Searches both the messages.db chats table and whatsmeow's contact store
    (whatsapp.db) to find contacts. Results are deduplicated by JID.
    """
    seen_jids: set[str] = set()
    result: list[dict[str, Any]] = []
    # JIDs are all ASCII so LIKE is safe; names use instr() because SQLite's
    # LOWER() only folds case for ASCII and would drop Unicode matches.
    jid_pattern = "%" + query + "%"

    # 1) Search messages.db chats table (existing behavior)
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        cursor.execute(
            """
            SELECT DISTINCT jid, name
            FROM chats
            WHERE
                (instr(LOWER(name), LOWER(?)) > 0 OR instr(name, ?) > 0 OR jid LIKE ?)
                AND jid NOT LIKE '%@g.us'
            ORDER BY name, jid
            LIMIT 50
        """,
            (query, query, jid_pattern),
        )
        for jid, name in cursor.fetchall():
            if jid not in seen_jids:
                seen_jids.add(jid)
                contact = Contact(phone_number=jid.split("@")[0], name=name, jid=jid)
                result.append(contact_to_dict(contact))
    except sqlite3.Error as e:
        logger.error("Database error (messages.db): %s", e)
    finally:
        if "conn" in locals():
            conn.close()

    # 2) Search whatsmeow contact store (whatsapp.db)
    if os.path.exists(WHATSMEOW_DB_PATH):
        try:
            conn2 = _connect_whatsmeow_db()
            cursor2 = conn2.cursor()
            cursor2.execute(
                """
                SELECT their_jid, full_name, push_name, first_name, business_name
                FROM whatsmeow_contacts
                WHERE
                    instr(LOWER(full_name), LOWER(?)) > 0 OR instr(full_name, ?) > 0
                    OR instr(LOWER(push_name), LOWER(?)) > 0 OR instr(push_name, ?) > 0
                    OR instr(LOWER(first_name), LOWER(?)) > 0 OR instr(first_name, ?) > 0
                    OR instr(LOWER(business_name), LOWER(?)) > 0 OR instr(business_name, ?) > 0
                    OR their_jid LIKE ?
                LIMIT 50
            """,
                (query, query, query, query, query, query, query, query, jid_pattern),
            )
            for their_jid, full_name, push_name, first_name, business_name in cursor2.fetchall():
                if their_jid not in seen_jids:
                    seen_jids.add(their_jid)
                    name = full_name or push_name or first_name or business_name or ""
                    contact = Contact(phone_number=their_jid.split("@")[0], name=name, jid=their_jid)
                    result.append(contact_to_dict(contact))
        except sqlite3.Error as e:
            logger.error("Database error (whatsapp.db): %s", e)
        finally:
            if "conn2" in locals():
                conn2.close()

    return result


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> list[dict[str, Any]]:
    """Get all chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        policy_clause, policy_params = CHAT_POLICY.sql_clause("c.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()

        aliases = _sender_aliases(jid)
        placeholders = ",".join("?" * len(aliases))
        cursor.execute(
            f"""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                last_msg.content as last_message,
                last_msg.sender as last_sender,
                last_msg.is_from_me as last_is_from_me,
                {_last_read_time_select(cursor, "c")}
            FROM chats c
            {_last_message_join("c", "last_msg")}
            WHERE (EXISTS (
                SELECT 1
                FROM messages contact_msg
                WHERE contact_msg.chat_jid = c.jid
                    AND contact_msg.sender IN ({placeholders})
            ) OR c.jid = ?) AND {policy_clause}
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """,
            (*aliases, jid, *policy_params, limit, page * limit),
        )

        chats = cursor.fetchall()

        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
                last_read_time=datetime.fromisoformat(chat_data[6]) if chat_data[6] else None,
            )
            result.append(chat_to_dict(chat))

        return result

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []
    finally:
        if "conn" in locals():
            conn.close()


def get_last_interaction(jid: str) -> dict[str, Any] | None:
    """Get most recent message involving the contact.

    Args:
        jid: The JID of the contact to search for

    Returns:
        Message dictionary or None if no messages found
    """
    try:
        policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()

        aliases = _sender_aliases(jid)
        placeholders = ",".join("?" * len(aliases))
        cursor.execute(
            f"""
            SELECT {MESSAGE_COLUMNS}
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE (messages.sender IN ({placeholders}) OR chats.jid = ?) AND {policy_clause}
            ORDER BY messages.timestamp DESC
            LIMIT 1
        """,
            (*aliases, jid, *policy_params),
        )

        msg_data = cursor.fetchone()

        if not msg_data:
            return None

        return msg_to_dict(_row_to_message(msg_data))

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None
    finally:
        if "conn" in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> dict[str, Any] | None:
    """Get chat metadata by JID.

    Returns:
        Chat dictionary or None if not found
    """
    try:
        if not CHAT_POLICY.allows(chat_jid):
            return None
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # See list_chats: the last message is always joined for is_from_me,
        # and the result tuple shape stays stable across the branch.
        if include_last_message:
            last_message_select = "m.content as last_message, m.sender as last_sender"
        else:
            last_message_select = "NULL as last_message, NULL as last_sender"

        query = f"""
            SELECT
                c.jid,
                c.name,
                c.last_message_time,
                {last_message_select},
                m.is_from_me as last_is_from_me,
                {_last_read_time_select(cursor, "c")}
            FROM chats c
            {_last_message_join("c", "m")}
            WHERE c.jid = ?
        """

        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        chat = Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=datetime.fromisoformat(chat_data[6]) if chat_data[6] else None,
        )
        return chat_to_dict(chat)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None
    finally:
        if "conn" in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> dict[str, Any] | None:
    """Get chat metadata by sender phone number."""
    try:
        policy_clause, policy_params = CHAT_POLICY.sql_clause("c.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()

        cursor.execute(
            f"""
            SELECT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me,
                {_last_read_time_select(cursor, "c")}
            FROM chats c
            {_last_message_join("c", "m")}
            WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us' AND {policy_clause}
            LIMIT 1
        """,
            (f"%{sender_phone_number}%", *policy_params),
        )

        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        chat = Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=datetime.fromisoformat(chat_data[6]) if chat_data[6] else None,
        )
        return chat_to_dict(chat)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None
    finally:
        if "conn" in locals():
            conn.close()


def send_message(
    recipient: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
) -> tuple[bool, str]:
    if denied := _policy_denied(recipient):
        return False, denied
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload: dict[str, Any] = {
            "recipient": recipient,
            "message": message,
        }
        if quoted_message_id:
            payload["quoted_message_id"] = quoted_message_id
            payload["quoted_sender_jid"] = quoted_sender_jid
            payload["quoted_content"] = quoted_content
        if mentions:
            payload["mentions"] = mentions

        response = requests.post(url, json=payload, headers=_bridge_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_file(recipient: str, media_path: str, caption: str = "") -> tuple[bool, str]:
    """Send a media file (image, video, document) with an optional caption.

    The bridge populates the WA media-message Caption field from `message`, so
    passing both in one /api/send call produces a single attachment-with-caption
    message instead of two separate messages.
    """
    if denied := _policy_denied(recipient):
        return False, denied
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {"recipient": recipient, "media_path": media_path}
        if caption:
            payload["message"] = caption

        response = requests.post(url, json=payload, headers=_bridge_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_audio_message(recipient: str, media_path: str) -> tuple[bool, str]:
    if denied := _policy_denied(recipient):
        return False, denied
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {"recipient": recipient, "media_path": media_path}

        response = requests.post(url, json=payload, headers=_bridge_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def send_reaction(
    recipient: str,
    message_id: str,
    emoji: str,
    from_me: bool = False,
    sender_jid: str = "",
) -> tuple[bool, str]:
    """Send (or remove) a reaction to a WhatsApp message.

    Args:
        recipient: The chat JID the message belongs to (phone JID or group JID).
        message_id: The ID of the message to react to.
        emoji: The reaction emoji. Pass an empty string to remove an existing reaction.
        from_me: Whether the original message was sent by the current user.
        sender_jid: JID of the original message sender (required for group messages
                    when from_me is False so the bridge can build the correct key).

    Returns:
        Tuple of (success, status_message).
    """
    if denied := _policy_denied(recipient):
        return False, denied
    try:
        if not recipient:
            return False, "Recipient must be provided"
        if not message_id:
            return False, "Message ID must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/react"
        payload: dict[str, Any] = {
            "recipient": recipient,
            "message_id": message_id,
            "emoji": emoji,
            "from_me": from_me,
            "sender_jid": sender_jid,
        }

        response = requests.post(url, json=payload, headers=_bridge_headers())

        if response.status_code == 200:
            result = response.json()
            if result.get("ok"):
                return True, "Reaction sent"
            return False, result.get("error", "Unknown error")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def get_group_members(group_jid: str) -> dict[str, Any]:
    """List the participants of a group via the bridge (live query to WhatsApp).

    Returns {"success": bool, "message": str, ...} with group metadata and a
    "members" list of {jid, phone_number, lid, name, is_admin, is_super_admin}.
    """
    group_jid = (group_jid or "").strip()
    if not group_jid.endswith("@g.us"):
        return {"success": False, "message": f"Not a group JID: {group_jid!r} (expected ...@g.us)"}
    if denied := _policy_denied(group_jid):
        return {"success": False, "message": denied}
    try:
        response = requests.get(
            f"{WHATSAPP_API_BASE_URL}/group/members", params={"jid": group_jid}, headers=_bridge_headers(), timeout=30
        )
        try:
            payload = response.json()
        except (json.JSONDecodeError, ValueError):
            payload = {}
        if response.status_code != 200 or not payload.get("success"):
            message = payload.get("message") or f"HTTP {response.status_code}: {response.text[:200]}"
            return {"success": False, "message": message}
        payload.setdefault("members", [])
        for member in payload["members"]:
            member["display"] = member.get("name") or member.get("phone_number") or member.get("jid")
        return payload
    except requests.RequestException as exc:
        return {"success": False, "message": f"Bridge request failed: {exc}"}


def get_poll_results(message_id: str, chat_jid: str) -> dict[str, Any]:
    """Tally of a native WhatsApp poll seen by the bridge (question, options, votes)."""
    message_id, chat_jid = (message_id or "").strip(), (chat_jid or "").strip()
    if not message_id or not chat_jid:
        return {"success": False, "message": "message_id and chat_jid are required"}
    if denied := _policy_denied(chat_jid):
        return {"success": False, "message": denied}
    try:
        response = requests.get(
            f"{WHATSAPP_API_BASE_URL}/poll",
            params={"message_id": message_id, "chat_jid": chat_jid},
            headers=_bridge_headers(),
            timeout=30,
        )
        try:
            payload = response.json()
        except (json.JSONDecodeError, ValueError):
            payload = {}
        if response.status_code != 200 or not payload.get("success"):
            return {"success": False, "message": payload.get("message") or f"HTTP {response.status_code}"}
        return payload
    except requests.RequestException as exc:
        return {"success": False, "message": f"Bridge request failed: {exc}"}


def delete_message(chat_jid: str, message_id: str, for_everyone: bool = False) -> tuple[bool, str]:
    """Revoke a sent message for everyone, or remove it from the local archive only."""
    chat_jid, message_id = (chat_jid or "").strip(), (message_id or "").strip()
    if not chat_jid or not message_id:
        return False, "chat_jid and message_id are required"
    if denied := _policy_denied(chat_jid):
        return False, denied
    try:
        response = requests.post(
            f"{WHATSAPP_API_BASE_URL}/delete",
            json={"chat_jid": chat_jid, "message_id": message_id, "for_everyone": bool(for_everyone)},
            headers=_bridge_headers(),
            timeout=30,
        )
        try:
            payload = response.json()
        except (json.JSONDecodeError, ValueError):
            payload = {}
        message = payload.get("message") or f"HTTP {response.status_code}: {response.text[:200]}"
        return bool(payload.get("success")) and response.status_code == 200, message
    except requests.RequestException as exc:
        return False, f"Bridge request failed: {exc}"


def mark_messages_read(
    message_ids: list[str],
    chat_jid: str,
    sender_jid: str = "",
    timestamp: str | None = None,
) -> tuple[bool, str]:
    """Mark selected messages as read through the WhatsApp bridge."""
    if denied := _policy_denied(chat_jid):
        return False, denied
    try:
        normalized_ids = [message_id.strip() for message_id in message_ids]
        if not normalized_ids or any(not message_id for message_id in normalized_ids):
            return False, "At least one non-empty message ID must be provided"
        if not chat_jid:
            return False, "Chat JID must be provided"
        if chat_jid.endswith("@g.us") and not sender_jid:
            return False, "Sender JID must be provided for group read receipts"

        payload: dict[str, Any] = {
            "message_ids": normalized_ids,
            "chat_jid": chat_jid,
        }
        if sender_jid:
            payload["sender_jid"] = sender_jid
        if timestamp:
            payload["timestamp"] = timestamp

        response = requests.post(
            f"{WHATSAPP_API_BASE_URL}/mark-read",
            json=payload,
            headers=_bridge_headers(),
        )

        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def download_media(message_id: str, chat_jid: str) -> str | None:
    """Download media from a message and return the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        The local file path if download was successful, None otherwise
    """
    if denied := _policy_denied(chat_jid):
        logger.warning("Download refused: %s", denied)
        return None
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {"message_id": message_id, "chat_jid": chat_jid}

        response = requests.post(url, json=payload, headers=_bridge_headers())

        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                logger.info("Media downloaded successfully: %s", path)
                return path
            else:
                logger.warning("Download failed: %s", result.get("message", "Unknown error"))
                return None
        else:
            logger.warning("Download error: HTTP %s - %s", response.status_code, response.text)
            return None

    except requests.RequestException as e:
        logger.error("Request error: %s", e)
        return None
    except json.JSONDecodeError:
        logger.error("Error parsing response: %s", response.text)
        return None
    except Exception as e:
        logger.exception("Unexpected error during download: %s", e)
        return None
