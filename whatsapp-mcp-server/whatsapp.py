import base64
import json
import logging
import os
import os.path
import re
import sqlite3
import threading
import time
from dataclasses import dataclass
from datetime import datetime
from typing import Any

import httpx

import audio
from chat_policy import load_chat_policy
from errors import ToolError

# All diagnostics go through logging (stderr). Never use print here: on the stdio
# transport stdout is the MCP protocol channel and stray output breaks it.
logger = logging.getLogger("whatsapp_mcp")

# Configuration via environment variables with sensible defaults. The bridge's
# WHATSAPP_STORE_DIR, when set, locates both databases; the explicit *_DB_PATH
# variables still win.
_DEFAULT_BRIDGE_STORE_DIR = (os.getenv("WHATSAPP_STORE_DIR") or "").strip() or os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "whatsapp-bridge", "store"
)
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


@dataclass
class PageResult:
    """One page of a list tool: items plus how to fetch the next page."""

    items: list[dict[str, Any]]
    next_cursor: str | None
    has_more: bool

    def to_dict(self) -> dict[str, Any]:
        return {"items": self.items, "next_cursor": self.next_cursor, "has_more": self.has_more}


def encode_cursor(payload: dict[str, Any]) -> str:
    """Opaque, URL-safe cursor. Callers pass it back verbatim."""
    raw = json.dumps(payload, separators=(",", ":"), sort_keys=True).encode("utf-8")
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def decode_cursor(cursor: str | None, expected_kind: str) -> dict[str, Any] | None:
    """Decode a cursor produced by encode_cursor; None when absent."""
    if not cursor:
        return None
    try:
        padded = cursor + "=" * (-len(cursor) % 4)
        payload = json.loads(base64.urlsafe_b64decode(padded.encode("ascii")).decode("utf-8"))
    except (ValueError, UnicodeDecodeError) as exc:
        raise ToolError("invalid_argument", "cursor is not valid; pass next_cursor from the previous page") from exc
    if not isinstance(payload, dict) or payload.get("k") != expected_kind:
        raise ToolError(
            "invalid_argument", f"cursor does not belong to {expected_kind}; pass next_cursor from the previous page"
        )
    return payload


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


# Schema probes (does chats.last_read_time exist? is messages_fts usable?) ran
# on every call. The answer only changes when the bridge migrates the file, so
# cache it per database path and invalidate when the file's mtime/size move.
_schema_cache: dict[tuple[str, str], tuple[tuple[float, int], Any]] = {}
_schema_cache_lock = threading.Lock()


def _db_signature(path: str) -> tuple[float, int]:
    try:
        st = os.stat(path)
        return (st.st_mtime, st.st_size)
    except OSError:
        return (0.0, 0)


def _schema_memo(kind: str, path: str, compute):
    sig = _db_signature(path)
    with _schema_cache_lock:
        hit = _schema_cache.get((kind, path))
        if hit is not None and hit[0] == sig:
            return hit[1]
    value = compute()
    with _schema_cache_lock:
        _schema_cache[(kind, path)] = (sig, value)
    return value


def _reset_schema_cache() -> None:
    with _schema_cache_lock:
        _schema_cache.clear()


def _fts_available(conn: sqlite3.Connection) -> bool:
    """True when messages_fts exists and this SQLite build can read it (memoised per file)."""
    return _schema_memo("fts", MESSAGES_DB_PATH, lambda: _fts_available_uncached(conn))


def _fts_available_uncached(conn: sqlite3.Connection) -> bool:
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


def _bridge_timeout(default: float = 30.0) -> float:
    """Per-call timeout for bridge REST requests (WHATSAPP_BRIDGE_TIMEOUT_S, default 30 s)."""
    raw = os.getenv("WHATSAPP_BRIDGE_TIMEOUT_S", "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        return default
    return value if value > 0 else default


# Uploads and downloads may wait on WhatsApp (media-retry asks the sender's phone).
BRIDGE_MEDIA_TIMEOUT_S = 120.0
BRIDGE_CONNECT_RETRIES = 2
BRIDGE_RETRY_BACKOFF_S = 0.5


class _BridgeHTTP:
    """The httpx client behind ``get``/``post``, created on first use.

    httpx is already the MCP SDK's HTTP stack, so the bridge client rides on it
    instead of a second library. Tests monkeypatch ``bridge_http.get`` /
    ``bridge_http.post`` with fakes returning objects that have ``status_code``,
    ``json()`` and ``text``.
    """

    def __init__(self) -> None:
        self._client: httpx.Client | None = None

    def _client_or_new(self) -> httpx.Client:
        if self._client is None:
            # No redirects: the bridge never redirects, and following one could
            # replay a POST (with the bearer token) to an unexpected host.
            self._client = httpx.Client(follow_redirects=False)
        return self._client

    def get(self, url: str, **kwargs: Any) -> httpx.Response:
        return self._client_or_new().get(url, **kwargs)

    def post(self, url: str, **kwargs: Any) -> httpx.Response:
        return self._client_or_new().post(url, **kwargs)


bridge_http = _BridgeHTTP()


def _bridge_request(method: str, path: str, *, timeout: float | None = None, **kwargs):
    """Call the bridge REST API with a timeout and a short retry on connection errors.

    Only errors raised before any bytes reach the bridge (connection refused,
    reset, connect timeout) are retried, so a POST is never delivered twice; a
    read timeout surfaces immediately. ``bridge_http.post``/``bridge_http.get``
    are looked up at call time so tests can monkeypatch them.
    """
    url = f"{WHATSAPP_API_BASE_URL}{path}"
    kwargs.setdefault("headers", _bridge_headers())
    kwargs["timeout"] = timeout if timeout is not None else _bridge_timeout()
    fn = bridge_http.get if method.upper() == "GET" else bridge_http.post
    for attempt in range(BRIDGE_CONNECT_RETRIES + 1):
        try:
            return fn(url, **kwargs)
        except (httpx.ConnectError, httpx.ConnectTimeout) as exc:
            if attempt >= BRIDGE_CONNECT_RETRIES:
                raise ToolError("bridge_unavailable", f"bridge unreachable at {WHATSAPP_API_BASE_URL}: {exc}") from exc
            delay = BRIDGE_RETRY_BACKOFF_S * (2**attempt)
            logger.warning("Bridge unreachable (%s), retrying in %.1fs: %s", path, delay, exc)
            time.sleep(delay)
        except httpx.HTTPError as exc:
            raise ToolError("bridge_unavailable", f"bridge request failed ({path}): {exc}") from exc
    raise AssertionError("unreachable")


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
    # name; other media get a generated `<type>_<timestamp>_<id>.<ext>`).
    # Rows written before target_message_id existed also carry the reaction /
    # poll-vote target here; _target_id() handles both.
    filename: str | None = None
    # For reactions and poll votes: the message they refer to.
    target_message_id: str | None = None
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
    # Media size and WhatsApp content hash (hex). The hash identifies the same
    # file across forwards and keys the agent's media notes (see media_inventory).
    bytes: int | None = None
    sha256: str | None = None


# One column list and one mapper for every query that yields Message rows.
# Every column added here is available to all readers; positional indexing
# elsewhere is a bug.
MESSAGE_COLUMNS = (
    "messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, "
    "messages.chat_jid, messages.id, messages.media_type, messages.quoted_message_id, messages.filename, "
    "messages.deleted_at, messages.view_once, messages.target_message_id, "
    "messages.file_length, lower(hex(messages.file_sha256))"
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
        target,
        file_length,
        sha256,
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
        target_message_id=target,
        bytes=int(file_length) if file_length else None,
        sha256=sha256 or None,
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


def _is_pointer_row(message: Message) -> bool:
    """Reactions and poll votes point at another message instead of carrying media."""
    return message.media_type in ("reaction", "poll_vote")


def _target_id(message: Message) -> str | None:
    """Target message ID for pointer rows; falls back to the legacy filename slot."""
    if not _is_pointer_row(message):
        return None
    return message.target_message_id or message.filename or None


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
        "filename": (message.filename or None) if message.media_type and not _is_pointer_row(message) else None,
        "target_message_id": _target_id(message),
        "reaction_to_message_id": (_target_id(message) if message.media_type == "reaction" else None),
        "poll_message_id": (_target_id(message) if message.media_type == "poll_vote" else None),
        "quoted_message_id": message.quoted_message_id,
        "deleted_at": message.deleted_at.isoformat() if message.deleted_at else None,
        "view_once": message.view_once,
        "bytes": message.bytes if message.media_type and not _is_pointer_row(message) else None,
        "sha256": message.sha256 if message.media_type and not _is_pointer_row(message) else None,
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
    has_column = _schema_memo(
        "chats.last_read_time",
        MESSAGES_DB_PATH,
        lambda: "last_read_time" in {row[1] for row in cursor.execute("PRAGMA table_info(chats)").fetchall()},
    )
    return f"{table_alias}.last_read_time" if has_column else "NULL"


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


# Sender-name and alias resolution is called once per returned message and used
# to open one to three SQLite connections each time (messages.db, then
# whatsapp.db for the LID map and again for contacts). Names change rarely, so
# results are cached per process for NAME_CACHE_TTL_S; tests reset the cache.
NAME_CACHE_TTL_S = 300.0
_name_cache: dict[tuple[str, str], tuple[Any, float]] = {}
_name_cache_lock = threading.Lock()


def _cache_get(kind: str, key: str) -> tuple[bool, Any]:
    with _name_cache_lock:
        hit = _name_cache.get((kind, key))
        if hit is None:
            return False, None
        value, expires = hit
        if expires < time.monotonic():
            del _name_cache[(kind, key)]
            return False, None
        return True, value


def _cache_put(kind: str, key: str, value: Any) -> Any:
    with _name_cache_lock:
        _name_cache[(kind, key)] = (value, time.monotonic() + NAME_CACHE_TTL_S)
    return value


def _reset_name_cache() -> None:
    """Drop cached sender names and aliases (tests, or after a contact sync)."""
    with _name_cache_lock:
        _name_cache.clear()


def _sender_aliases(value: str) -> list[str]:
    hit, cached = _cache_get("aliases", value)
    if hit:
        return list(cached)
    return list(_cache_put("aliases", value, _sender_aliases_uncached(value)))


def _sender_aliases_uncached(value: str) -> list[str]:
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
    try:
        conn = _connect_whatsmeow_db()
        cursor = conn.cursor()
        if jid_suffix in ("lid", ""):
            cursor.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? LIMIT 1", (jid_prefix,))
            row = cursor.fetchone()
            if row and row[0]:
                lookup_jid = row[0] + "@s.whatsapp.net"
            elif jid_suffix == "lid":
                # Definitely a LID but not in the map — can't resolve
                return None
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
    hit, cached = _cache_get("name", sender_jid)
    if hit:
        return cached
    return _cache_put("name", sender_jid, _get_sender_name_uncached(sender_jid))


def _get_sender_name_uncached(sender_jid: str) -> str:
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # Exact match on the JID as stored, then on the other spellings of the
        # same number (bare, phone JID, LID JID). No LIKE '%number%': it scanned
        # the table and could match an unrelated JID containing the digits.
        bare = sender_jid.split("@")[0] if "@" in sender_jid else sender_jid
        candidates = [sender_jid, bare, f"{bare}@s.whatsapp.net", f"{bare}@lid"]
        cursor.execute(
            f"""
            SELECT name
            FROM chats
            WHERE jid IN ({",".join("?" for _ in candidates)})
              AND name IS NOT NULL AND name != ''
            ORDER BY CASE WHEN jid = ? THEN 0 ELSE 1 END
            LIMIT 1
        """,
            (*candidates, sender_jid),
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
    unread_only: bool = False,
) -> list[dict[str, Any]]:
    """Items of one page of list_messages_page (kept for callers that want a plain list)."""
    return list_messages_page(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after,
        sort_by=sort_by,
        include_deleted=include_deleted,
        unread_only=unread_only,
    ).items


def list_messages_page(
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
    unread_only: bool = False,
    cursor: str | None = None,
) -> PageResult:
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
        unread_only: Only inbound messages newer than their chat's read marker
            (chats.last_read_time, as reported by any linked device). Chats with no
            marker count as entirely unread.

        cursor: Opaque next_cursor from the previous page (keyset pagination).
            When given, page is ignored. Relevance sort falls back to an offset
            carried inside the cursor.

    Returns:
        PageResult: items (hits plus context rows), next_cursor, has_more.
    """
    cursor_state = decode_cursor(cursor, "messages")
    if cursor_state is not None and cursor_state.get("s") != sort_by:
        raise ToolError("invalid_argument", "cursor was created with a different sort_by")
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

        if unread_only:
            read_marker = _last_read_time_select(cursor, "chats")
            where_clauses.append(
                f"messages.is_from_me = 0 AND ({read_marker} IS NULL OR messages.timestamp > {read_marker})"
            )

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

        # Sorting and pagination. Keyset on (timestamp, id) for the time orders;
        # relevance (bm25) has no stable key, so its cursor carries an offset.
        keyset = sort_by != "relevance" or not use_fts
        offset = page * limit
        if cursor_state is not None:
            if keyset and "t" in cursor_state:
                cmp = ">" if sort_by == "oldest" else "<"
                where_clauses.append(
                    f"(messages.timestamp {cmp} ? OR (messages.timestamp = ? AND messages.id {cmp} ?))"
                )
                params.extend([cursor_state["t"], cursor_state["t"], cursor_state["i"]])
                offset = 0
            else:
                offset = int(cursor_state.get("o", 0))
            # where_clauses may have been closed already; rebuild the WHERE part
            query_parts = [part for part in query_parts if not part.startswith("WHERE ")]
            if where_clauses:
                query_parts.append("WHERE " + " AND ".join(where_clauses))
        if sort_by == "relevance" and use_fts:
            query_parts.append(f"ORDER BY bm25({MESSAGES_FTS_TABLE}), messages.timestamp DESC")
        else:
            order = "ASC" if sort_by == "oldest" else "DESC"
            query_parts.append(f"ORDER BY messages.timestamp {order}, messages.id {order}")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit + 1, offset])

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
        has_more = len(messages) > limit
        messages = messages[:limit]

        next_cursor = None
        if has_more and messages:
            last = messages[-1]
            if keyset:
                next_cursor = encode_cursor({"k": "messages", "s": sort_by, "t": last[0], "i": last[6]})
            else:
                next_cursor = encode_cursor({"k": "messages", "s": sort_by, "o": offset + limit})

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

            return PageResult([msg_to_dict(msg) for msg in messages_with_context], next_cursor, has_more)

        # Return messages without context
        return PageResult([msg_to_dict(msg) for msg in result], next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
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
            raise ToolError("not_found", f"Message with ID {message_id}{where} not found")
        target_message = _row_to_message(msg_data)
        if denied := _policy_denied(target_message.chat_jid):
            raise ToolError("denied", denied)
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
    """Items of one page of list_chats_page."""
    return list_chats_page(
        query=query, limit=limit, page=page, include_last_message=include_last_message, sort_by=sort_by
    ).items


def list_chats_page(
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
    cursor: str | None = None,
) -> PageResult:
    """Get chats matching the specified criteria.

    Returns:
        List of chat dictionaries with jid, name, is_group, last_message, etc.
    """
    cursor_state = decode_cursor(cursor, "chats")
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

        if cursor_state is not None and cursor_state.get("s") != sort_by:
            raise ToolError("invalid_argument", "cursor was created with a different sort_by")
        offset = page * limit
        if cursor_state is not None:
            offset = 0
            if sort_by == "last_active":
                # NULL last_message_time sorts last in DESC order; keyset skips
                # past the (time, jid) of the previous page's last row.
                # Rows with a NULL time sort after every dated row, so after a
                # dated cursor they are still ahead of us.
                where_clauses.append(
                    "(chats.last_message_time < ? OR (chats.last_message_time = ? AND chats.jid > ?) OR chats.last_message_time IS NULL)"
                    if cursor_state.get("t") is not None
                    else "(chats.last_message_time IS NULL AND chats.jid > ?)"
                )
                params.extend(
                    [cursor_state["t"], cursor_state["t"], cursor_state["j"]]
                    if cursor_state.get("t") is not None
                    else [cursor_state["j"]]
                )
            else:
                where_clauses.append("(chats.name > ? OR (chats.name = ? AND chats.jid > ?))")
                params.extend([cursor_state["n"], cursor_state["n"], cursor_state["j"]])
            query_parts = [part for part in query_parts if not part.startswith("WHERE ")]
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Add sorting (jid as the tie-breaker so the keyset is total)
        order_by = (
            "chats.last_message_time DESC, chats.jid ASC"
            if sort_by == "last_active"
            else "chats.name ASC, chats.jid ASC"
        )
        query_parts.append(f"ORDER BY {order_by}")

        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit + 1, offset])

        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()
        has_more = len(chats) > limit
        chats = chats[:limit]
        next_cursor = None
        if has_more and chats:
            last = chats[-1]
            state = {"k": "chats", "s": sort_by, "j": last[0]}
            if sort_by == "last_active":
                state["t"] = last[2]
            else:
                state["n"] = last[1]
            next_cursor = encode_cursor(state)

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

        return PageResult(result, next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
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
    """Items of one page of get_contact_chats_page."""
    return get_contact_chats_page(jid, limit=limit, page=page).items


def get_contact_chats_page(jid: str, limit: int = 20, page: int = 0, cursor: str | None = None) -> PageResult:
    """Get all chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    cursor_state = decode_cursor(cursor, "contact_chats")
    try:
        offset = page * limit
        keyset_clause, keyset_params = "", []
        if cursor_state is not None:
            offset = 0
            if cursor_state.get("t") is not None:
                keyset_clause = "AND (c.last_message_time < ? OR (c.last_message_time = ? AND c.jid > ?) OR c.last_message_time IS NULL)"
                keyset_params = [cursor_state["t"], cursor_state["t"], cursor_state["j"]]
            else:
                keyset_clause = "AND (c.last_message_time IS NULL AND c.jid > ?)"
                keyset_params = [cursor_state["j"]]
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
            ) OR c.jid IN ({placeholders})) AND {policy_clause} {keyset_clause}
            ORDER BY c.last_message_time DESC, c.jid ASC
            LIMIT ? OFFSET ?
        """,
            (*aliases, *aliases, *policy_params, *keyset_params, limit + 1, offset),
        )

        chats = cursor.fetchall()
        has_more = len(chats) > limit
        chats = chats[:limit]
        next_cursor = None
        if has_more and chats:
            next_cursor = encode_cursor({"k": "contact_chats", "t": chats[-1][2], "j": chats[-1][0]})

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

        return PageResult(result, next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
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
            _require_allowed(jid)
            return None

        return msg_to_dict(_row_to_message(msg_data))

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> dict[str, Any] | None:
    """Get chat metadata by JID.

    Returns:
        Chat dictionary or None if not found
    """
    try:
        _require_allowed(chat_jid)
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
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def _direct_chat_candidates(value: str) -> tuple[str, str, str]:
    """Exact JID spellings for a contact: the input as given, phone JID, LID JID.

    The old lookup used ``jid LIKE '%number%'`` with no ORDER BY: a full scan
    that could return an unrelated chat whose JID merely contains the digits.
    """
    raw = (value or "").strip()
    bare = raw.split("@", 1)[0].lstrip("+").replace(" ", "").replace("-", "")
    return raw, f"{bare}@s.whatsapp.net", f"{bare}@lid"


def get_direct_chat_by_contact(sender_phone_number: str) -> dict[str, Any] | None:
    """Get chat metadata by sender phone number (exact match on the number's JID forms)."""
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
            WHERE c.jid IN (?, ?, ?) AND {policy_clause}
            ORDER BY CASE WHEN c.jid = ? THEN 0 WHEN c.jid LIKE '%@s.whatsapp.net' THEN 1 ELSE 2 END
            LIMIT 1
        """,
            (*_direct_chat_candidates(sender_phone_number), sender_phone_number, *policy_params),
        )

        chat_data = cursor.fetchone()

        if not chat_data:
            for candidate in _direct_chat_candidates(sender_phone_number):
                _require_allowed(candidate)
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
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def _bridge_error_code(status: int) -> str:
    """Map a bridge HTTP status to an error code."""
    if status == 400:
        return "invalid_argument"
    if status == 403:
        return "denied"
    if status == 404:
        return "not_found"
    if status == 401:
        return "internal"  # our own token was rejected: configuration, not the caller's fault
    if status >= 500:
        return "bridge_unavailable"
    return "internal"


def _bridge_json(response) -> dict[str, Any]:
    """Decode a bridge response; raise ToolError for HTTP or application failures.

    The bridge answers 200 + {"success": true, ...} on success, 200 +
    {"success": false, "message"} or {"ok": false, "error"} for application
    failures, and 4xx/5xx (JSON or plain text) otherwise.
    """
    try:
        payload = response.json()
    except (json.JSONDecodeError, ValueError):
        payload = {}
    if not isinstance(payload, dict):
        payload = {}
    message = payload.get("message") or payload.get("error") or (getattr(response, "text", "") or "").strip()[:300]
    if response.status_code != 200:
        raise ToolError(
            _bridge_error_code(response.status_code), message or f"bridge answered HTTP {response.status_code}"
        )
    if "success" in payload and not payload.get("success"):
        raise ToolError("internal", message or "bridge reported failure")
    if "ok" in payload and not payload.get("ok"):
        raise ToolError("internal", message or "bridge reported failure")
    return payload


def _sent_info(result: dict[str, Any]) -> dict[str, Any]:
    """message_id / chat_jid / timestamp of a message the bridge just sent (when reported)."""
    return {k: result[k] for k in ("message_id", "chat_jid", "timestamp") if result.get(k)}


def _require_allowed(jid: str | None) -> None:
    if denied := _policy_denied(jid):
        raise ToolError("denied", denied)


def send_message(
    recipient: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
) -> tuple[bool, str, dict[str, Any]]:
    """Send a text message. Returns (True, status, {message_id, chat_jid, timestamp}) or raises ToolError."""
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(recipient)
    payload: dict[str, Any] = {"recipient": recipient, "message": message}
    if quoted_message_id:
        payload["quoted_message_id"] = quoted_message_id
        payload["quoted_sender_jid"] = quoted_sender_jid
        payload["quoted_content"] = quoted_content
    if mentions:
        payload["mentions"] = mentions
    result = _bridge_json(_bridge_request("POST", "/send", json=payload))
    return True, result.get("message", "Message sent"), _sent_info(result)


def send_file(recipient: str, media_path: str, caption: str = "") -> tuple[bool, str, dict[str, Any]]:
    """Send a media file (image, video, document) with an optional caption.

    The bridge populates the WA media-message Caption field from `message`, so
    passing both in one /api/send call produces a single attachment-with-caption
    message instead of two separate messages.
    """
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    if not media_path:
        raise ToolError("invalid_argument", "media_path must be provided")
    _require_allowed(recipient)
    if not os.path.isfile(media_path):
        raise ToolError("not_found", f"Media file not found: {media_path}")
    payload = {"recipient": recipient, "media_path": media_path}
    if caption:
        payload["message"] = caption
    result = _bridge_json(_bridge_request("POST", "/send", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
    return True, result.get("message", "File sent"), _sent_info(result)


def send_audio_message(recipient: str, media_path: str) -> tuple[bool, str, dict[str, Any]]:
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    if not media_path:
        raise ToolError("invalid_argument", "media_path must be provided")
    _require_allowed(recipient)
    if not os.path.isfile(media_path):
        raise ToolError("not_found", f"Media file not found: {media_path}")
    if not media_path.endswith(".ogg"):
        try:
            media_path = audio.convert_to_opus_ogg_temp(media_path)
        except Exception as e:
            raise ToolError("internal", f"Error converting file to opus ogg (is ffmpeg installed?): {e}") from e
    payload = {"recipient": recipient, "media_path": media_path}
    result = _bridge_json(_bridge_request("POST", "/send", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
    return True, result.get("message", "Audio sent"), _sent_info(result)


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
        (True, status) or raises ToolError.
    """
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    if not message_id:
        raise ToolError("invalid_argument", "message_id must be provided")
    _require_allowed(recipient)
    payload: dict[str, Any] = {
        "recipient": recipient,
        "message_id": message_id,
        "emoji": emoji,
        "from_me": from_me,
        "sender_jid": sender_jid,
    }
    _bridge_json(_bridge_request("POST", "/react", json=payload))
    return True, "Reaction sent" if emoji else "Reaction removed"


def get_group_members(group_jid: str) -> dict[str, Any]:
    """List the participants of a group via the bridge (live query to WhatsApp).

    Returns {"success": true, ...} with group metadata and a "members" list of
    {jid, phone_number, lid, name, is_admin, is_super_admin}; raises ToolError.
    """
    group_jid = (group_jid or "").strip()
    if not group_jid.endswith("@g.us"):
        raise ToolError("invalid_argument", f"Not a group JID: {group_jid!r} (expected ...@g.us)")
    _require_allowed(group_jid)
    payload = _bridge_json(_bridge_request("GET", "/group/members", params={"jid": group_jid}))
    payload.setdefault("members", [])
    for member in payload["members"]:
        member["display"] = member.get("name") or member.get("phone_number") or member.get("jid")
    return payload


def get_poll_results(message_id: str, chat_jid: str) -> dict[str, Any]:
    """Tally of a native WhatsApp poll seen by the bridge (question, options, votes)."""
    message_id, chat_jid = (message_id or "").strip(), (chat_jid or "").strip()
    if not message_id or not chat_jid:
        raise ToolError("invalid_argument", "chat_jid and message_id are required")
    _require_allowed(chat_jid)
    return _bridge_json(_bridge_request("GET", "/poll", params={"message_id": message_id, "chat_jid": chat_jid}))


def delete_message(chat_jid: str, message_id: str, for_everyone: bool = False) -> tuple[bool, str]:
    """Revoke a sent message for everyone, or remove it from the local archive only."""
    chat_jid, message_id = (chat_jid or "").strip(), (message_id or "").strip()
    if not chat_jid or not message_id:
        raise ToolError("invalid_argument", "chat_jid and message_id are required")
    _require_allowed(chat_jid)
    payload = _bridge_json(
        _bridge_request(
            "POST",
            "/delete",
            json={"chat_jid": chat_jid, "message_id": message_id, "for_everyone": bool(for_everyone)},
        )
    )
    return True, payload.get("message") or "Deleted"


PURGE_MEDIA_TYPES = ("image", "video", "audio", "document", "sticker")


def purge_media(
    items: list[dict[str, str]] | None = None,
    chat_jid: str = "",
    older_than_days: int = 0,
    min_bytes: int = 0,
    media_type: str = "",
    dry_run: bool = True,
) -> dict[str, Any]:
    """Ask the bridge to drop cached media bytes (rows untouched); dry run unless told otherwise."""
    chat_jid = (chat_jid or "").strip()
    media_type = (media_type or "").strip()
    normalized: list[dict[str, str]] = []
    for item in items or []:
        if not isinstance(item, dict):
            raise ToolError("invalid_argument", "items must be a list of {message_id, chat_jid}")
        message_id = str(item.get("message_id") or "").strip()
        item_chat = str(item.get("chat_jid") or "").strip()
        if not message_id or not item_chat:
            raise ToolError("invalid_argument", "each item needs message_id and chat_jid")
        _require_allowed(item_chat)
        normalized.append({"message_id": message_id, "chat_jid": item_chat})
    if not normalized and not chat_jid and not older_than_days and not min_bytes and not media_type:
        raise ToolError(
            "invalid_argument", "Provide items, or at least one of chat_jid / older_than_days / min_bytes / media_type"
        )
    if media_type and media_type not in PURGE_MEDIA_TYPES:
        raise ToolError("invalid_argument", f"media_type must be one of {', '.join(PURGE_MEDIA_TYPES)}")
    if older_than_days < 0 or min_bytes < 0:
        raise ToolError("invalid_argument", "older_than_days and min_bytes must not be negative")
    if chat_jid:
        _require_allowed(chat_jid)
    body: dict[str, Any] = {"dry_run": bool(dry_run)}
    if normalized:
        body["items"] = normalized
    else:
        if chat_jid:
            body["chat_jid"] = chat_jid
        if older_than_days:
            body["older_than_days"] = int(older_than_days)
        if min_bytes:
            body["min_bytes"] = int(min_bytes)
        if media_type:
            body["media_type"] = media_type
    payload = _bridge_json(_bridge_request("POST", "/media/purge", json=body))
    return {
        "success": True,
        "dry_run": bool(payload.get("dry_run", dry_run)),
        "message": payload.get("message") or "",
        "matched": int(payload.get("matched") or 0),
        "purged_files": int(payload.get("purged_files") or 0),
        "purged_bytes": int(payload.get("purged_bytes") or 0),
        "truncated": bool(payload.get("truncated", False)),
        "items": payload.get("items") or [],
    }


def mark_messages_read(
    message_ids: list[str],
    chat_jid: str,
    sender_jid: str = "",
    timestamp: str | None = None,
) -> tuple[bool, str]:
    """Mark selected messages as read through the WhatsApp bridge."""
    normalized_ids = [message_id.strip() for message_id in (message_ids or [])]
    if not normalized_ids or any(not message_id for message_id in normalized_ids):
        raise ToolError("invalid_argument", "At least one non-empty message ID must be provided")
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(chat_jid)
    if chat_jid.endswith("@g.us") and not sender_jid:
        raise ToolError("invalid_argument", "sender_jid must be provided for group read receipts")
    payload: dict[str, Any] = {"message_ids": normalized_ids, "chat_jid": chat_jid}
    if sender_jid:
        payload["sender_jid"] = sender_jid
    if timestamp:
        payload["timestamp"] = timestamp
    result = _bridge_json(_bridge_request("POST", "/mark-read", json=payload))
    return True, result.get("message", "Marked as read")


def download_media(message_id: str, chat_jid: str) -> str | None:
    """Download media from a message and return the local file path.

    Raises ToolError when the chat is denied, the bridge is unreachable or the
    bridge reports a failure; returns None only when the bridge answered
    success without a path.
    """
    _require_allowed(chat_jid)
    payload = {"message_id": message_id, "chat_jid": chat_jid}
    result = _bridge_json(_bridge_request("POST", "/download", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
    path = result.get("path")
    if path:
        logger.info("Media downloaded successfully: %s", path)
    return path


def bridge_status() -> dict[str, Any]:
    """Health, readiness and build identity of the bridge in one call.

    Never raises for a bridge that is down: returns ok=false with the reason,
    so the agent can tell "bridge unreachable" from "nothing matched".
    """
    status: dict[str, Any] = {"ok": False, "bridge_url": WHATSAPP_API_BASE_URL}
    try:
        health = _bridge_request("GET", "/health", timeout=10)
    except ToolError as exc:
        status["reason"] = exc.message
        return status
    try:
        body = health.json() if health.status_code == 200 else {}
    except (json.JSONDecodeError, ValueError):
        body = {}
    if health.status_code != 200 or not isinstance(body, dict):
        status["reason"] = f"/api/health answered HTTP {health.status_code}"
        return status
    status.update(
        {
            "status": body.get("status"),
            "connected": bool(body.get("connected")),
            "paired": bool(body.get("paired")),
            "uptime_seconds": body.get("uptime_seconds"),
            "store_bytes": body.get("store_bytes"),
            "media_bytes": body.get("media_bytes"),
            "media_files": body.get("media_files"),
        }
    )
    status["ok"] = status["connected"] and status["paired"]
    if not status["ok"]:
        status["reason"] = (
            "bridge is up but not paired: scan the QR code in its log"
            if not status["paired"]
            else "bridge is paired but disconnected from WhatsApp; it reconnects automatically"
        )
    try:
        version = _bridge_request("GET", "/version", timeout=10)
        if version.status_code == 200:
            info = version.json()
            if isinstance(info, dict):
                status["version"] = {k: info.get(k) for k in ("version", "commit", "go", "whatsmeow", "fts5")}
    except (ToolError, json.JSONDecodeError, ValueError):
        pass
    return status


def list_unread(limit_chats: int = 20, limit_per_chat: int = 5, since: str | None = None) -> dict[str, Any]:
    """Chats with unread inbound messages, each with its newest unread rows.

    "Unread" means inbound (is_from_me = 0) and newer than the chat's read
    marker (chats.last_read_time, as reported by any linked device); chats
    with no marker count as entirely unread. Ordered by most recent unread
    message. Honours WHATSAPP_ALLOWED_CHATS.
    """
    limit_chats = max(1, min(int(limit_chats), 100))
    limit_per_chat = max(1, min(int(limit_per_chat), 50))
    since_ts: datetime | None = None
    if since:
        try:
            since_ts = datetime.fromisoformat(since)
        except ValueError as exc:
            raise ToolError("invalid_argument", f"since must be ISO-8601, got {since!r}") from exc
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        read_marker = _last_read_time_select(cursor, "chats")
        policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
        pointer_filter = "(messages.media_type IS NULL OR messages.media_type NOT IN ('reaction', 'poll_vote'))"
        params: list[Any] = list(policy_params)
        since_clause = ""
        if since_ts is not None:
            since_clause = "AND messages.timestamp > ?"
            params.append(since_ts)
        params.append(limit_chats)
        cursor.execute(
            f"""
            SELECT chats.jid, chats.name, {read_marker} AS last_read_time,
                   COUNT(*) AS unread_count, MAX(messages.timestamp) AS latest_unread
            FROM messages
            JOIN chats ON chats.jid = messages.chat_jid
            WHERE messages.is_from_me = 0
              AND ({read_marker} IS NULL OR messages.timestamp > {read_marker})
              AND messages.deleted_at IS NULL
              AND {pointer_filter}
              AND {policy_clause}
              {since_clause}
            GROUP BY chats.jid
            ORDER BY latest_unread DESC
            LIMIT ?
            """,
            tuple(params),
        )
        groups = cursor.fetchall()
        chats: list[dict[str, Any]] = []
        total = 0
        for jid, name, last_read, count, latest in groups:
            cursor.execute(
                f"""
                SELECT {MESSAGE_COLUMNS}
                FROM messages JOIN chats ON messages.chat_jid = chats.jid
                WHERE messages.chat_jid = ? AND messages.is_from_me = 0
                  AND ({read_marker} IS NULL OR messages.timestamp > {read_marker})
                  AND messages.deleted_at IS NULL
                  AND {pointer_filter}
                ORDER BY messages.timestamp DESC, messages.id DESC
                LIMIT ?
                """,
                (jid, limit_per_chat),
            )
            rows = cursor.fetchall()
            total += int(count)
            chats.append(
                {
                    "chat_jid": jid,
                    "chat_name": name,
                    "is_group": jid.endswith("@g.us"),
                    "unread_count": int(count),
                    "latest_unread": latest,
                    "last_read_time": last_read,
                    "messages": [msg_to_dict(_row_to_message(row)) for row in reversed(rows)],
                }
            )
        return {"chats": chats, "total_unread": total, "chats_with_unread": len(chats)}
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


# --- group management (bridge group_manage.go) ---------------------------------


def _group_jid(value: str) -> str:
    jid = (value or "").strip()
    if not jid.endswith("@g.us"):
        raise ToolError("invalid_argument", f"Not a group JID: {jid!r} (expected ...@g.us)")
    _require_allowed(jid)
    return jid


def manage_group_participants(group_jid: str, action: str, participants: list[str]) -> dict[str, Any]:
    """add / remove / promote / demote participants (phone numbers or JIDs)."""
    jid = _group_jid(group_jid)
    action = (action or "").strip().lower()
    if action not in ("add", "remove", "promote", "demote"):
        raise ToolError("invalid_argument", "action must be one of add, remove, promote, demote")
    cleaned = [str(p).strip() for p in (participants or []) if str(p).strip()]
    if not cleaned:
        raise ToolError("invalid_argument", "participants must list at least one phone number or JID")
    return _bridge_json(
        _bridge_request(
            "POST", "/group/participants", json={"group_jid": jid, "action": action, "participants": cleaned}
        )
    )


def update_group(group_jid: str, name: str | None = None, description: str | None = None) -> dict[str, Any]:
    """Change the group's subject (name) and/or description."""
    jid = _group_jid(group_jid)
    body: dict[str, Any] = {"group_jid": jid}
    if name is not None:
        body["name"] = name
    if description is not None:
        body["description"] = description
    if len(body) == 1:
        raise ToolError("invalid_argument", "provide name and/or description")
    return _bridge_json(_bridge_request("POST", "/group/subject", json=body))


def get_group_invite_link(group_jid: str, reset: bool = False) -> dict[str, Any]:
    """The group's invite link; reset=True revokes the old one first."""
    jid = _group_jid(group_jid)
    return _bridge_json(_bridge_request("POST", "/group/invite", json={"group_jid": jid, "reset": bool(reset)}))


def leave_group(group_jid: str) -> dict[str, Any]:
    """Leave the group (irreversible without a new invite)."""
    jid = _group_jid(group_jid)
    return _bridge_json(_bridge_request("POST", "/group/leave", json={"group_jid": jid}))


def send_typing(chat_jid: str, is_typing: bool = True) -> dict[str, Any]:
    """Show (or clear) the 'typing…' presence in a chat."""
    target = (chat_jid or "").strip()
    if not target:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(target)
    return _bridge_json(_bridge_request("POST", "/typing", json={"recipient": target, "is_typing": bool(is_typing)}))


def edit_message(chat_jid: str, message_id: str, text: str) -> dict[str, Any]:
    """Edit an own message (WhatsApp accepts edits for ~15 minutes after sending)."""
    chat_jid, message_id = (chat_jid or "").strip(), (message_id or "").strip()
    if not chat_jid or not message_id:
        raise ToolError("invalid_argument", "chat_jid and message_id are required")
    if not (text or "").strip():
        raise ToolError("invalid_argument", "text must not be empty")
    _require_allowed(chat_jid)
    return _bridge_json(
        _bridge_request("POST", "/edit", json={"chat_jid": chat_jid, "message_id": message_id, "text": text})
    )


def forward_message(chat_jid: str, message_id: str, to_chat_jid: str) -> dict[str, Any]:
    """Re-send a stored message (text or cached media with caption) to another chat."""
    chat_jid, message_id, to = (chat_jid or "").strip(), (message_id or "").strip(), (to_chat_jid or "").strip()
    if not chat_jid or not message_id or not to:
        raise ToolError("invalid_argument", "chat_jid, message_id and to_chat_jid are required")
    _require_allowed(chat_jid)
    _require_allowed(to)
    return _bridge_json(
        _bridge_request(
            "POST",
            "/forward",
            json={"chat_jid": chat_jid, "message_id": message_id, "to_chat_jid": to},
            timeout=BRIDGE_MEDIA_TIMEOUT_S,
        )
    )
