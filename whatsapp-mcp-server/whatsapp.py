import base64
import hashlib
import json
import logging
import math
import os
import os.path
import pathlib
import re
import sqlite3
import threading
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from itertools import islice
from typing import Any, NamedTuple

import httpx

import audio
import endpoint_cert
import media_upload
import transcribe
from chat_policy import DEFAULT_USER_SERVER, load_chat_policy, normalize_chat_entry, validate_chat_target
from errors import MEDIA_REFUSED_CODE, ToolError
from phone import br_mobile_alternate, br_national_mobile_alternate, normalize_recipient, phone_digits
from untrusted import sanitize_name

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
    validate_chat_target(jid)
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


# Page-size ceilings, one per listing: the numbers the tool docstrings and
# docs/TOOLS.md quote. page_size() clamps to them.
MESSAGES_MAX_LIMIT = 500
CHATS_MAX_LIMIT = 200
CONTACT_CHATS_MAX_LIMIT = 200
UNANSWERED_MAX_LIMIT = 200
UNREAD_MAX_CHATS = 100
UNREAD_MAX_PER_CHAT = 50


def page_size(limit: Any, maximum: int, name: str = "limit") -> int:
    """One page size for every listing tool: an integer in 1..maximum.

    Asking for more rows than a tool serves is a reasonable request, so the
    upper bound stays a clamp. Asking for none is not, and the listings read it
    two different wrong ways (issue #309). Each binds `limit + 1` to see whether
    a next page exists, so limit=-1 became `LIMIT 0`: an empty page whose
    has_more (0 > -1) is true, which a walk on has_more never escapes. From
    limit=-2 down the bound is negative, which SQLite reads as "no limit", so
    the whole table was read and then sliced away. Neither is a page, so the
    value is refused with the range named.
    """
    try:
        value = int(limit)
    except (OverflowError, TypeError, ValueError) as exc:
        raise ToolError(
            "invalid_argument", f"{name} must be an integer between 1 and {maximum}, got {limit!r}"
        ) from exc
    if value < 1:
        raise ToolError("invalid_argument", f"{name} must be between 1 and {maximum}, got {value}")
    return min(value, maximum)


def page_number(page: Any, name: str = "page") -> int:
    """A page index: 0 or more.

    A negative one is not page "minus something": SQLite reads a negative OFFSET
    as zero, so it would silently serve page 0 under another page's name.
    """
    try:
        value = int(page)
    except (OverflowError, TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"{name} must be an integer of 0 or more, got {page!r}") from exc
    if value < 0:
        raise ToolError("invalid_argument", f"{name} must be 0 or more, got {value}")
    return value


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


def _read_only_uri(path: str) -> str:
    """``file:`` URI that opens ``path`` read-only, whatever characters the path holds.

    ``Path.as_uri`` percent-encodes spaces, ``?`` and ``#`` and turns a Windows
    ``C:\\dir with space\\x.db`` into ``file:///C:/dir%20with%20space/x.db``;
    pasting the raw path after ``file:`` would cut it at the first ``?`` or ``#``.
    """
    return pathlib.Path(os.path.abspath(path)).as_uri() + "?mode=ro"


def _connect_read_only(path: str) -> sqlite3.Connection:
    """A connection that cannot write to the database at ``path`` and never creates it.

    ``mode=ro`` and not ``immutable=1``: a WAL database the bridge has open must
    be read through its ``-wal``/``-shm`` files, or reads would miss everything
    not yet checkpointed. With the bridge stopped the reader recovers the log
    and creates ``-shm`` next to the file, which needs a directory it can write:
    on a read-only mount or as another user it only works while the bridge has
    the database open (docs/DOCKER.md, "The store read-only in the MCP
    container"). An ATTACHed
    database does *not* inherit the flag; :func:`attach_notes_read_only` attaches
    notes.db with its own ``mode=ro``, because every join here only reads it and
    its writers open it on their own connection.
    """
    return sqlite3.connect(_read_only_uri(path), timeout=SQLITE_BUSY_TIMEOUT_S, uri=True)


class MessagesDbNotFoundError(ToolError, sqlite3.OperationalError):
    """messages.db is not where this server was told to look.

    Both things on purpose: a tool that lets it escape answers with the failure
    envelope (a ToolError), while the helpers written to tolerate an archive
    they cannot read (``except sqlite3.Error``: the read-receipt routing, the
    sender-namespace lookup) keep tolerating it, as they did when a missing
    file was an empty one.
    """


def attach_notes_read_only(conn: sqlite3.Connection, path: str) -> None:
    """``ATTACH`` notes.db to a read connection as ``notesdb``, read-only.

    The file: URI form is honoured because the connections come from
    :func:`_connect_read_only` (``uri=True``); on any other connection SQLite
    would take the URI for a file name, so pass only connections from
    :func:`_connect_messages_db`. A join through it can read the agent's notes
    but never write them.
    """
    conn.execute("ATTACH DATABASE ? AS notesdb", (_read_only_uri(path),))


def _connect_messages_db() -> sqlite3.Connection:
    """messages.db, read-only (the bridge owns it). A wrong path is an error, not an empty database."""
    if not os.path.isfile(MESSAGES_DB_PATH):
        # Opening it read-write would have created an empty file here, and every
        # tool would then answer "no such table" or an empty list.
        raise MessagesDbNotFoundError(
            "internal",
            f"messages.db not found at {os.path.abspath(MESSAGES_DB_PATH)}: the bridge has not created it "
            "there, or the path is wrong. The path comes from WHATSAPP_DB_PATH, or from "
            "WHATSAPP_STORE_DIR/messages.db when that is unset",
        )
    return _connect_read_only(MESSAGES_DB_PATH)


def _connect_whatsmeow_db() -> sqlite3.Connection:
    """whatsapp.db, read-only: whatsmeow's session store is opaque to this server.

    A missing file raises ``sqlite3.OperationalError``, as an unreadable one
    always has; every caller that tolerates an absent phone book catches
    ``sqlite3.Error`` (or checks ``os.path.isfile`` first).
    """
    return _connect_read_only(WHATSMEOW_DB_PATH)


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
# cache it per database path and SQLite schema version. WAL-only migrations
# may leave the main file mtime/size untouched; the reader must observe them.
_schema_cache: dict[tuple[str, str], tuple[tuple[float, int, int], Any]] = {}
_schema_cache_lock = threading.Lock()


def _db_signature(path: str) -> tuple[float, int]:
    try:
        st = os.stat(path)
        return (st.st_mtime, st.st_size)
    except OSError:
        return (0.0, 0)


def _schema_memo(kind: str, path: str, compute, *, schema: sqlite3.Connection | sqlite3.Cursor):
    sig = (*_db_signature(path), schema.execute("PRAGMA main.schema_version").fetchone()[0])
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
    return _schema_memo("fts", MESSAGES_DB_PATH, lambda: _fts_available_uncached(conn), schema=conn)


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

    This is httpx, the project's own dependency (the MCP SDK brings a different
    client, httpx2; see pyproject.toml). Tests monkeypatch ``bridge_http.get`` /
    ``bridge_http.post`` with fakes returning objects that have ``status_code``,
    ``json()`` and ``text``.
    """

    def __init__(self) -> None:
        self._client: httpx.Client | None = None

    def _client_or_new(self) -> httpx.Client:
        if self._client is None:
            # No redirects: the bridge never redirects, and following one could
            # replay a POST (with the bearer token) to an unexpected host.
            # Bridge credentials must not reach an inherited HTTP proxy.
            self._client = httpx.Client(follow_redirects=False, trust_env=False)
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


MEDIA_TYPES = ("image", "video", "audio", "document", "sticker")


def _location_fields(raw: str | None) -> dict[str, Any] | None:
    if not raw or len(raw) > 65536:
        return None
    try:
        value = json.loads(raw)
    except (ValueError, TypeError):
        return None
    if not isinstance(value, dict) or type(value.get("live")) is not bool:
        return None
    result: dict[str, Any] = {"live": value["live"]}
    for key in ("name", "address", "url", "comment"):
        if isinstance(value.get(key), str):
            result[key] = value[key]
    for key, low, high in (
        ("latitude", -90, 90),
        ("longitude", -180, 180),
        ("speed_mps", 0, 3.4028235e38),
        ("bearing_degrees", 0, 360),
        ("accuracy_meters", 0, 4294967295),
        ("sequence", 0, 9223372036854775807),
        ("time_offset_seconds", 0, 4294967295),
    ):
        number = value.get(key)
        if key in ("accuracy_meters", "bearing_degrees", "sequence", "time_offset_seconds") and type(number) is not int:
            continue
        if (
            isinstance(number, (int, float))
            and not isinstance(number, bool)
            and low <= number <= high
            and math.isfinite(number)
        ):
            result[key] = number
    return result


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    # Which namespace `sender` belongs to, as the bridge recorded it when it
    # stored the row: "s.whatsapp.net", "lid", or None when it never knew —
    # rows written before the column existed, and senders that are not user
    # JIDs at all (issue #375).
    sender_server: str | None = None
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
    # Native location fields; NULL on old text-only rows, never reconstructed.
    location: dict[str, Any] | None = None


# One column list and one mapper for every query that yields Message rows.
# Every column added here is available to all readers; positional indexing
# elsewhere is a bug.
MESSAGE_COLUMNS = (
    "messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, "
    "messages.chat_jid, messages.id, messages.media_type, messages.quoted_message_id, messages.filename, "
    "messages.deleted_at, messages.view_once, messages.target_message_id, "
    "messages.file_length, lower(hex(messages.file_sha256)), messages.sender_server, messages.location"
)


def message_columns(cursor: sqlite3.Cursor) -> str:
    """MESSAGE_COLUMNS as this store can answer it.

    Older bridges may lack sender_server or location. Select NULL for those
    fields so all readers keep the same projection and old archives remain
    readable without the MCP server changing the bridge-owned schema.
    """
    has_column = _schema_memo(
        "messages.sender_server",
        MESSAGES_DB_PATH,
        # Iterated, not fetchall()'d: this runs on the caller's cursor, and
        # export.py's guard against buffering a whole archive watches for it.
        lambda: "sender_server" in {row[1] for row in cursor.execute("PRAGMA table_info(messages)")},
        schema=cursor,
    )
    columns = MESSAGE_COLUMNS if has_column else MESSAGE_COLUMNS.replace("messages.sender_server", "NULL")
    has_location = _schema_memo(
        "messages.location",
        MESSAGES_DB_PATH,
        lambda: "location" in {row[1] for row in cursor.execute("PRAGMA table_info(messages)")},
        schema=cursor,
    )
    return columns if has_location else columns.replace("messages.location", "NULL")


def _row_to_message(row: tuple) -> Message:
    """Build a Message from a row selected with MESSAGE_COLUMNS (in that order)."""
    if len(row) == 16:  # legacy callers that materialized the old projection
        row = (*row, None)
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
        sender_server,
        location,
    ) = row
    return Message(
        timestamp=parse_db_time(timestamp),
        sender=sender,
        sender_server=sender_server or None,
        chat_name=chat_name,
        content=content,
        # SQLite hands the BOOLEAN column back as 0/1; the dataclass (and the
        # JSON an agent reads) says bool, and omit_nulls drops `false`, not 0.
        is_from_me=bool(is_from_me),
        chat_jid=chat_jid,
        id=msg_id,
        media_type=media_type,
        quoted_message_id=quoted_id,
        filename=filename,
        deleted_at=parse_db_time(deleted) if deleted else None,
        view_once=bool(view_once),
        target_message_id=target,
        bytes=int(file_length) if file_length is not None else None,
        sha256=sha256 or None,
        location=_location_fields(location),
    )


# The status feed. WhatsApp files every contact's status post under this one
# JID, so the archive holds it as a chat whose stored name is whoever posted
# last — a phone number that changes with the feed (issue #379). It is a chat
# for reading (`list_messages(chat_jid="status@broadcast")` returns the posts)
# and never a conversation waiting for a reply.
STATUS_BROADCAST_JID = "status@broadcast"
STATUS_CHAT_NAME = "Status updates"


def chat_display_name(jid: str, stored: str | None) -> str | None:
    """The name a listing shows for a chat, before the phone book is consulted.

    Only the status feed differs from what the archive stored, and it differs
    for every reader: a label that changed with the last poster would be a
    different chat on every page (issue #379).
    """
    return STATUS_CHAT_NAME if jid == STATUS_BROADCAST_JID else stored


def _chat_name_expr(alias: str = "chats") -> str:
    """`chat_display_name` in SQL, for the statements that sort and search on the name.

    The status feed is listed under a name this server chose, so ordering and
    `query` have to read that name too — otherwise `list_chats` shows "Status
    updates" and files it under a phone number nobody can search for. The two
    constants are module-level, never user input, so they are inlined.
    """
    return f"CASE WHEN {alias}.jid = '{STATUS_BROADCAST_JID}' THEN '{STATUS_CHAT_NAME}' ELSE {alias}.name END"


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
    # Whether a stored message row backs last_message / last_sender /
    # last_is_from_me. False means the chat has no rows in `messages` at all
    # (history never synced, or every row was pruned), which is why the three
    # last_* fields are null — not "the last message could not be matched".
    has_messages: bool = False
    # Where `name` comes from: "chat" = stored in messages.db (the WhatsApp
    # chat title or group subject), "contacts" = your phone book
    # (whatsmeow_contacts) because the stored name was missing or just the
    # number, "push" = the name the contact gave themselves, because the phone
    # book had nothing either, "jid" = nothing better than the number exists,
    # "system" = this server named the chat itself (the status feed, #379).
    name_source: str = "chat"
    # The name the contact gave themselves, whatever `name` ended up being.
    # A cached snapshot of unknown age (whatsmeow_contacts has no timestamp).
    push_name: str | None = None
    # Every spelling this row speaks for when a listing collapsed the phone JID
    # and the `@lid` of one person into it (issue #337): [phone JID, LID JID],
    # `jid` being the first of them. Empty on a chat WhatsApp knows one way only.
    aliases: list[str] = field(default_factory=list)

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

    @property
    def is_status(self) -> bool:
        """The status feed, the one chat that is nobody's conversation (issue #379).

        `status@broadcast` collects everyone's status posts under a single JID.
        It is not a group and not direct, and no one is waiting for a reply in
        it, so the triage listings drop it and this flag names it for the rest.
        """
        return self.jid == STATUS_BROADCAST_JID

    @property
    def unread(self) -> bool:
        """Whether the chat's last message is inbound and unread by us.

        With a read marker this is genuine unread — a chat read on the phone
        or another linked device is not reported. Without one (older bridge,
        or a chat WhatsApp never reported a read for) it degrades to the old
        heuristic: unread if the last message is inbound.

        A chat with no stored messages (`has_messages` false, so
        `last_is_from_me is None`) has no direction to go on — history sync can
        list a chat without storing a row, and older bridges advanced
        last_message_time for events they did not store —
        so it is not reported as unread. Read `has_messages` to tell that
        "nothing is waiting" from "we cannot tell".
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
    phone_number: str | None
    name: str | None
    jid: str
    lid: str | None = None
    # The name the contact gave themselves (whatsmeow_contacts.push_name), kept
    # beside `name` instead of collapsed into it (#280).
    push_name: str | None = None


@dataclass
class MessageContext:
    message: Message
    before: list[Message]
    after: list[Message]


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


class SenderIdentity(NamedTuple):
    """Which namespace an identifier belongs to, once the LID map has spoken.

    The bridge stores `messages.sender` as the bare user part: phone digits when
    the LID resolved at write time, bare LID digits otherwise. `phone` is a real
    phone number and never a LID; `lid` is the anonymous link-ID. A LID keeps
    `phone` only when `whatsmeow_lid_map` knows the number behind it (#281).

    Rows written since #375 carry `messages.sender_server`, which says the
    namespace outright; the length heuristic below only decides for the rows
    that predate it.
    """

    phone: str | None
    lid: str | None


# E.164 allows 15 digits at most, so a longer bare identifier cannot be a phone
# number and is a LID on sight. At 15 and below the two overlap, and only the
# LID map tells them apart: a bare number it does not know stays a phone number,
# which is the deliberate limit of this classification — an unmapped 15-digit
# LID is indistinguishable from a (rare, but legal) 15-digit number, so it is
# reported as one rather than guessed away. It is the fallback for legacy rows;
# what the bridge recorded (`sender_server`) wins wherever it is present.
MAX_PHONE_DIGITS = 15

# The two namespace values messages.sender_server holds
# (whatsapp-bridge/sender_namespace.go); DEFAULT_USER_SERVER is the other one.
LID_SERVER = "lid"


def _sender_identities(
    values: Sequence[str], servers: Mapping[str, str | None] | None = None
) -> dict[str, SenderIdentity]:
    """Classify a batch of stored senders / identifiers in one LID-map query.

    `servers` maps a value to the namespace the bridge recorded for it
    (`messages.sender_server`); it settles the classification and the heuristic
    is left to decide only for the values it has nothing for.

    Results are cached per value for NAME_CACHE_TTL_S like sender names, so a
    page with repeated senders — or the next page of the same chat — is free.
    """
    recorded = servers or {}
    result: dict[str, SenderIdentity] = {}
    pending: dict[str, str] = {}  # value -> bare digits still to look up
    lid_namespace: set[str] = set()  # values a namespace already calls a LID
    # The cache key carries the namespace the value was classified under, so a
    # legacy row and a row the bridge stamped can never serve each other's
    # answer for the same digits.
    keys = {value: f"{value.partition('@')[2] or recorded.get(value) or ''}|{value}" for value in values}
    for value in values:
        if value in result or value in pending:
            continue
        hit, cached = _cache_get("identity", keys[value])
        if hit:
            result[value] = cached
            continue
        bare, _, server = value.partition("@")
        namespace = server or recorded.get(value) or ""
        if namespace == LID_SERVER:
            # A LID, whether the JID said so or the bridge recorded it. The map
            # is still queried below for the number behind it.
            pending[value] = bare
            lid_namespace.add(value)
        elif namespace == DEFAULT_USER_SERVER:
            result[value] = _cache_put("identity", keys[value], SenderIdentity(bare, None))
        elif bare.isdigit() and server == "":
            # A bare number says nothing on its own and no row recorded its
            # namespace: the LID map and the length limit decide below.
            pending[value] = bare
        else:
            # A JID of some other namespace, or something we cannot classify at
            # all (an empty sender, a group JID): it stays where it always was.
            result[value] = _cache_put("identity", keys[value], SenderIdentity(bare or value, None))
    if not pending:
        return result

    pn_by_lid: dict[str, str] = {}
    known_lids: set[str] = set()
    map_read = False
    if os.path.isfile(WHATSMEOW_DB_PATH):
        try:
            conn = _connect_whatsmeow_db()
            try:
                for chunk in _in_chunks(sorted(set(pending.values()))):
                    rows = conn.execute(
                        f"SELECT lid, pn FROM whatsmeow_lid_map WHERE lid IN ({_placeholders(chunk)})", chunk
                    ).fetchall()
                    for lid, pn in rows:
                        known_lids.add(lid)
                        if pn:
                            pn_by_lid[lid] = pn
                map_read = True
            finally:
                conn.close()
        except sqlite3.Error as e:
            # A missing or locked contact store is not an error for the caller:
            # the sender is classified by shape alone.
            logger.debug("sender-identity batch failed: %s", e)

    for value, bare in pending.items():
        is_lid = value in lid_namespace or bare in known_lids or len(bare) > MAX_PHONE_DIGITS
        identity = SenderIdentity(pn_by_lid.get(bare), bare) if is_lid else SenderIdentity(bare, None)
        result[value] = identity
        # Only an answer the map actually gave is cached. A classification made
        # while whatsapp.db was missing, locked or broken is a guess, and
        # caching it would report a LID as a phone number for the next five
        # minutes — the bug this exists to prevent (#281). Guessing costs no
        # I/O, so recomputing it on the next call is cheap — and a LID the
        # bridge recorded still has a number to find once the map is readable.
        if map_read:
            _cache_put("identity", keys[value], identity)
    return result


def sender_identity(value: str, server: str | None = None) -> SenderIdentity:
    """Phone/LID namespace of one sender or identifier (see :class:`SenderIdentity`).

    `server` is the namespace the bridge recorded for it, when the caller has a
    row to read it from (`messages.sender_server`).
    """
    return _sender_identities([value], {value: server} if server else None)[value]


# Bare identifiers of this length are the ambiguous ones: over MAX_PHONE_DIGITS
# nothing can be a phone number, and no real archive holds one longer than 13,
# so 14 and 15 digits are where an unmapped LID passes for a number. The bridge
# backfill draws the same line (whatsapp-bridge/sender_namespace.go).
AMBIGUOUS_LID_DIGITS = (14, 15)


def stored_sender_namespace(bare: str) -> str | None:
    """The namespace messages.db recorded for a bare sender, when a row has one.

    A tool is handed an identifier with no namespace attached, but the archive
    usually knows which one it is: the bridge stamps it on every row it stores
    (`messages.sender_server`, #375).
    """
    if not bare or "@" in bare:
        return None
    hit, cached = _cache_get("sender_server", bare)
    if hit:
        return cached
    if not os.path.isfile(MESSAGES_DB_PATH):
        # Asked before the bridge ever ran: connecting would create the archive.
        return None
    namespace = None
    try:
        conn = _connect_messages_db()
        try:
            row = conn.execute(
                "SELECT sender_server FROM messages WHERE sender = ? AND sender_server IS NOT NULL LIMIT 1",
                (bare,),
            ).fetchone()
            namespace = row[0] if row else None
        finally:
            conn.close()
    except sqlite3.Error as e:
        # An unreadable archive is not an error for the caller: the identifier
        # is classified by the LID map and its shape instead.
        logger.debug("sender-namespace lookup failed: %s", e)
        return None
    return _cache_put("sender_server", bare, namespace)


def unknown_lid_digits(bare: str) -> bool:
    """Is this bare 14-15 digit identifier a LID nothing in the deployment knows?

    Callers use it after the LID map and the chats table have come up empty: at
    that length a LID and an E.164 number have the same shape, and an identifier
    that no chat, no stored sender and no phone-book entry has ever carried as a
    number is a LID whose mapping was never learned (#375).

    Every source has to have answered for that to hold, so a phone book that
    cannot be read says "no" rather than "no entry" — a classification made
    against a missing or locked whatsapp.db is a guess (#281), and guessing
    towards a LID would rename a real number on the strength of a lock.
    """
    if not bare.isdigit() or len(bare) not in AMBIGUOUS_LID_DIGITS:
        return False
    if stored_sender_namespace(bare) == DEFAULT_USER_SERVER:
        return False
    if not os.path.isfile(WHATSMEOW_DB_PATH):
        return False
    try:
        conn = _connect_whatsmeow_db()
        try:
            # The phone book is keyed by full JID, so a row under the phone
            # spelling is the phone book saying this is a number.
            known = conn.execute(
                "SELECT 1 FROM whatsmeow_contacts WHERE their_jid = ? LIMIT 1",
                (f"{bare}@{DEFAULT_USER_SERVER}",),
            ).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as e:
        logger.debug("phone-book check for %s failed: %s", bare, e)
        return False
    return known is None


def _is_pointer_row(message: Message) -> bool:
    """Reactions and poll votes point at another message instead of carrying media."""
    return message.media_type in ("reaction", "poll_vote")


def _target_id(message: Message) -> str | None:
    """Target message ID for pointer rows; falls back to the legacy filename slot."""
    if not _is_pointer_row(message):
        return None
    return message.target_message_id or message.filename or None


def fetch_media_notes(messages: Sequence[Message]) -> dict[str, dict[str, str]]:
    """Agent notes for every media hash in a batch of messages: {sha256: {key: value}}.

    One query for a whole page instead of one per row. media_notes imports this
    module, so the import lives here rather than at the top.
    """
    from media_notes import fetch_notes

    hashes = [m.sha256 for m in messages if m.sha256 and m.media_type in MEDIA_TYPES]
    return fetch_notes(hashes) if hashes else {}


def fetch_sender_identities(messages: Sequence[Message]) -> dict[str, SenderIdentity]:
    """Sender namespaces for a batch of messages: {stored sender: SenderIdentity}.

    One LID-map query for a whole page instead of one per row; pass the result
    to msg_to_dict alongside the notes. Each sender is classified with the
    namespace its row recorded, so only the rows that predate the column fall
    back to the length heuristic.
    """
    senders = [message.sender for message in messages if message.sender]
    servers: dict[str, str | None] = {}
    for message in messages:
        # Rows disagree only when one of them predates sender_server, so a
        # recorded namespace wins over a missing one whatever the row order.
        if message.sender and message.sender_server and not servers.get(message.sender):
            servers[message.sender] = message.sender_server
    return _sender_identities(senders, servers)


def attach_message_notes(rows: Sequence[dict[str, Any]]) -> None:
    """Add `message_notes` to the rows that have one, in a single query.

    Notes about the *message* (why it matters, that it was answered by voice),
    which is not the same thing as `notes`: that one belongs to the file a media
    row carries and is keyed by its sha256. Rows nobody annotated are left
    without the key rather than carrying an empty mapping — a page of a hundred
    messages would otherwise pay for a hundred of them.
    """
    from notes import attach_notes

    attach_notes(
        rows,
        "message",
        lambda row: f"{row.get('chat_jid') or ''}/{row.get('id') or ''}",
        field="message_notes",
        only_when_present=True,
    )


def msgs_to_dicts(messages: Sequence[Message], include_sender_name: bool = True) -> list[dict[str, Any]]:
    """Convert a page of messages, looking their media and message notes up in one query each."""
    notes = fetch_media_notes(messages)
    identities = fetch_sender_identities(messages)
    if include_sender_name:
        prefetch_sender_names(messages)
    rows = [msg_to_dict(message, include_sender_name, notes, identities) for message in messages]
    attach_message_notes(rows)
    return rows


def media_notes_for_message(chat_jid: str, message_id: str) -> dict[str, Any]:
    """{"sha256", "notes"} for one media message; empty notes when the row has no hash."""
    try:
        conn = _connect_messages_db()
        try:
            row = conn.execute(
                "SELECT lower(hex(file_sha256)) FROM messages WHERE id = ? AND chat_jid = ?",
                (message_id, chat_jid),
            ).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    sha256 = (row[0] if row else None) or None
    if not sha256:
        return {"sha256": None, "notes": {}}
    from media_notes import fetch_notes

    return {"sha256": sha256, "notes": fetch_notes([sha256]).get(sha256, {})}


def msg_to_dict(
    message: Message,
    include_sender_name: bool = True,
    notes: dict[str, dict[str, str]] | None = None,
    identities: dict[str, SenderIdentity] | None = None,
) -> dict[str, Any]:
    """Convert a Message dataclass to a dictionary for JSON serialization.

    Media rows carry the agent's notes for their sha256 (`{}` when the file has
    never been annotated). Pass `notes` from fetch_media_notes and `identities`
    from fetch_sender_identities, and call prefetch_sender_names once for the
    page, to convert it with a single query each; without them a row costs one
    lookup.
    """
    # The two identifier namespaces stay apart: `sender_phone` is a phone number
    # or nothing, `sender_lid` the anonymous link-ID (#281). Which one a row is
    # comes from the row itself when the bridge recorded it (#375), and from the
    # LID map and the length limit for rows that predate the column.
    # `sender_jid` keeps the value the bridge stored, whichever form that was.
    bare = message.sender.split("@", 1)[0]
    identity = (identities or {}).get(message.sender) or sender_identity(message.sender, message.sender_server)
    sender_phone = identity.phone
    sender_lid = identity.lid

    sender_name = None
    sender_display = None
    sender_push_name = None
    if include_sender_name:
        if message.is_from_me:
            sender_name = "Me"
            sender_display = "Me"
        else:
            # The name the sender gave themselves, whatever `sender_name` ends
            # up being: a cached snapshot of unknown age, not a live claim
            # (#280). Served from the cache prefetch_sender_names filled.
            sender_push_name = _contact_profiles([message.sender]).get(message.sender, _NO_PROFILE).push_name
            # What to show when nobody has a name for this sender. An
            # unresolved LID has no phone number to fall back on, and echoing
            # its digits as a name would invent a contact called "1171581...".
            label = sender_phone if sender_phone is not None else f"{sender_lid}@lid"
            resolved_name = get_sender_name(message.sender)
            # Check if we got an actual name (not just the JID back)
            if resolved_name and resolved_name not in (message.sender, bare, sender_phone):
                sender_name = resolved_name
                sender_display = f"{resolved_name} ({label})"
            else:
                sender_name = sender_phone
                sender_display = label

    is_media = message.media_type in MEDIA_TYPES
    sha256 = message.sha256 if is_media else None

    row: dict[str, Any] = {
        "id": message.id,
        "timestamp": message.timestamp.isoformat(),
        "sender_jid": message.sender,
        "sender_phone": sender_phone,
        "sender_lid": sender_lid,
        "sender_name": sender_name,
        "sender_push_name": sender_push_name,  # what they call themselves, when known
        "sender_display": sender_display,  # "Name (phone)" or just phone if no name
        "content": message.content,
        "is_from_me": message.is_from_me,
        "chat_jid": message.chat_jid,
        # The chat this row belongs to, named as the chat listings name it: the
        # status feed stores the last poster's number, which describes neither
        # the chat nor this message (issue #379).
        "chat_name": chat_display_name(message.chat_jid, message.chat_name),
        "media_type": message.media_type,
        "location": message.location if message.media_type == "location" else None,
        "filename": (message.filename or None) if is_media else None,
        "target_message_id": _target_id(message),
        "reaction_to_message_id": (_target_id(message) if message.media_type == "reaction" else None),
        "poll_message_id": (_target_id(message) if message.media_type == "poll_vote" else None),
        "quoted_message_id": message.quoted_message_id,
        "deleted_at": message.deleted_at.isoformat() if message.deleted_at else None,
        "view_once": message.view_once,
        "bytes": message.bytes if is_media else None,
        "sha256": sha256,
    }
    if sha256:
        page_notes = notes if notes is not None else fetch_media_notes([message])
        row["notes"] = page_notes.get(sha256, {})
    return row


def chat_to_dict(chat: "Chat") -> dict[str, Any]:
    """Convert a Chat dataclass to a dictionary for JSON serialization.

    `aliases` is only on the rows that carry one — a merged phone/LID pair
    (issue #337) — the way `notes` is only on the rows that have notes.
    `is_status` follows the same rule: it marks the one status feed row
    (issue #379) instead of adding `false` to every chat in a listing.
    """
    row = {
        "jid": chat.jid,
        "name": chat.name,
        "push_name": chat.push_name,
        "name_source": chat.name_source,
        "is_group": chat.is_group,
        "last_message_time": chat.last_message_time.isoformat() if chat.last_message_time else None,
        "last_message": chat.last_message,
        "last_sender": chat.last_sender,
        "last_is_from_me": chat.last_is_from_me,
        "last_read_time": chat.last_read_time.isoformat() if chat.last_read_time else None,
        "has_messages": chat.has_messages,
        "unread": chat.unread,
    }
    if chat.aliases:
        row["aliases"] = list(chat.aliases)
    if chat.is_status:
        row["is_status"] = True
    return row


def contact_to_dict(contact: "Contact") -> dict[str, Any]:
    """Convert a Contact dataclass to a dictionary for JSON serialization.

    `phone_number` and `lid` are the two identifier namespaces get_contact
    reports, never each other (#281): a contact WhatsApp only knows
    anonymously has `phone_number: null` unless the LID map resolves it.
    `push_name` is what the contact calls themselves, beside the `name` this
    account knows them by (#280).
    """
    return {
        "phone_number": contact.phone_number,
        "lid": contact.lid,
        "name": contact.name,
        "push_name": contact.push_name,
        "jid": contact.jid,
    }


# --- response shaping (compact reads) -----------------------------------------

# The valid `fields` names are the keys the converters above actually emit, read
# off one empty row each, plus the keys added conditionally afterwards (`notes`
# for media rows, `transcript` for include_transcripts, `content_truncated` for
# max_content_chars). Deriving them here keeps one list instead of a hand-copied
# one that drifts the next time a column is added.
MESSAGE_FIELDS: tuple[str, ...] = (
    *msg_to_dict(
        Message(timestamp=datetime.min, sender="", content="", is_from_me=False, chat_jid="", id=""),
        include_sender_name=False,
    ),
    "notes",
    "message_notes",
    "transcript",
    "content_truncated",
)
# "notes" is on every chat row, whichever listing produced it.
_CHAT_ROW_FIELDS: tuple[str, ...] = (
    *chat_to_dict(Chat(jid="", name=None, last_message_time=None)),
    "notes",
)
# Two tuples rather than one: list_chats / get_chat can truncate last_message,
# list_unanswered cannot but reports how long the chat has been waiting. A name
# accepted and then always absent from the row is the silent no-op `fields`
# exists to avoid, so each tool validates against the keys its rows can carry.
# Both collapse a phone/LID pair (issues #337, #366), so "aliases" is on both.
# "is_status" is only on the chat listings: list_unanswered never returns the
# status feed (issue #379), so the name would be a column of nothing there.
CHAT_FIELDS: tuple[str, ...] = (*_CHAT_ROW_FIELDS, "content_truncated", "aliases", "is_status")
UNANSWERED_FIELDS: tuple[str, ...] = (*_CHAT_ROW_FIELDS, "last_inbound_time", "age_hours", "aliases")

# The three keys include_group_mentions=True adds. They are only valid names
# when that flag is on: naming `mention` without it must be the error every
# other unknown field is, not a column of nothing.
UNANSWERED_MENTION_FIELDS: tuple[str, ...] = (*UNANSWERED_FIELDS, "mention", "mention_message_id", "mention_time")


def _is_empty(value: Any) -> bool:
    """Values omit_nulls drops: null, false, empty text, empty notes.

    `is False` rather than falsiness on purpose — `bytes: 0` and `age_hours: 0`
    are facts, not absences.
    """
    return value is None or value is False or value == "" or value == {}


def validate_fields(fields: Sequence[str] | None, known: Sequence[str] = MESSAGE_FIELDS) -> list[str] | None:
    """Normalise a `fields` projection; unknown names raise invalid_argument."""
    if fields is None:
        return None
    if isinstance(fields, str):
        raise ToolError("invalid_argument", "fields must be a list of field names, not a string")
    selected = [str(name).strip() for name in fields if str(name).strip()]
    if not selected:
        raise ToolError("invalid_argument", f"fields must name at least one of: {', '.join(sorted(known))}")
    unknown = [name for name in selected if name not in known]
    if unknown:
        raise ToolError(
            "invalid_argument",
            f"unknown field(s) {', '.join(sorted(set(unknown)))}; valid names: {', '.join(sorted(known))}",
        )
    return selected


def shape_rows(
    rows: list[dict[str, Any]],
    fields: Sequence[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
    known: Sequence[str] = MESSAGE_FIELDS,
    content_key: str = "content",
) -> list[dict[str, Any]]:
    """Compact already-converted rows: truncate the long text, project, drop empties.

    One helper for every bulk read, applied after msg_to_dict / chat_to_dict so
    the shaping never has to know how a row was built. `content_key` names the
    long text on these rows — `content` on message rows, `last_message` on chat
    rows — while the flag it raises stays `content_truncated` either way, so a
    caller reads one key whatever it listed.

    Order matters: truncation runs first (so a projection on the text field still
    gets the shortened text) and `content_truncated` survives a projection that
    did not ask for it, because losing that flag would make the truncated text
    look complete.
    """
    selected = validate_fields(fields, known)
    limit = int(max_content_chars) if max_content_chars is not None else None
    if limit is not None and limit < 1:
        raise ToolError("invalid_argument", f"max_content_chars must be 1 or more, got {max_content_chars!r}")
    if selected is None and not omit_nulls and limit is None:
        return rows

    shaped: list[dict[str, Any]] = []
    for row in rows:
        out = dict(row)
        content = out.get(content_key)
        if limit is not None and isinstance(content, str) and len(content) > limit:
            out[content_key] = content[:limit]
            out["content_truncated"] = True
        if selected is not None:
            keep = [*selected, "content_truncated"] if out.get("content_truncated") else selected
            out = {name: out[name] for name in keep if name in out}
        if omit_nulls:
            out = {name: value for name, value in out.items() if not _is_empty(value)}
        shaped.append(out)
    return shaped


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
        schema=cursor,
    )
    return f"{table_alias}.last_read_time" if has_column else "NULL"


def _has_mentions_column(cursor: sqlite3.Cursor) -> bool:
    """Whether the bridge that wrote this store records messages.mentions."""
    return _schema_memo(
        "messages.mentions",
        MESSAGES_DB_PATH,
        lambda: "mentions" in {row[1] for row in cursor.execute("PRAGMA table_info(messages)").fetchall()},
        schema=cursor,
    )


def mentions_me_predicate(cursor: sqlite3.Cursor, alias: str) -> tuple[str, list[Any]]:
    """SQL for "this message addressed the account this bridge is logged in as".

    The bridge stores the mentioned users as they arrive: the LID user for a
    LID mention, the phone user for a phone one (bridge mentions.go). Matching
    both spellings of our own identity here is what makes that write cheap, and
    the commas around the column are what stop 5511 from matching 5511999.
    """
    if not _has_mentions_column(cursor):
        raise ToolError(
            "bridge_unavailable",
            "this archive was written by a bridge that does not record mentions yet "
            "(messages.mentions is missing); upgrade the bridge, which backfills the column on startup",
        )
    owner = owner_identity()
    users = [user for user in (owner.get("phone"), owner.get("lid")) if user]
    if not users:
        raise ToolError(
            "bridge_unavailable",
            "the bridge did not report this account's phone or LID, so a mention of it cannot be recognised; "
            "call bridge_status",
        )
    matches = " OR ".join([f"(',' || {alias}.mentions || ',') LIKE ?"] * len(users))
    return f"({alias}.mentions IS NOT NULL AND ({matches}))", [f"%,{user},%" for user in users]


def _spoken_filter(alias: str) -> str:
    """Rows that count as somebody speaking in the chat.

    Reactions and poll votes are pointer rows attached to another message, and
    a revoked message is not something anyone said any more. Both would
    otherwise decide who "spoke last" (list_unanswered) or count as unread
    (list_unread).
    """
    return (
        f"{alias}.deleted_at IS NULL "
        f"AND ({alias}.media_type IS NULL OR {alias}.media_type NOT IN ('reaction', 'poll_vote'))"
    )


# A conversation that ends on "ok" or a sticker is closed, not waiting. The list
# is short and literal on purpose: a heuristic that guesses is worse than one an
# operator can read, and docs/TOOLS.md publishes exactly these words.
CLOSING_MESSAGES: tuple[str, ...] = ("ok", "obrigado", "obrigada", "valeu", "blz", "thanks", "👍", "🙏")


def _own_mention_stripped(alias: str) -> tuple[str, list[str]]:
    """(SQL for the row's text without this account's own `@…`, its placeholders).

    WhatsApp writes a mention into the text as `@<lid-or-phone>`, so the message
    that reads "ok @you" is stored as "ok @158…" and matches none of the closing
    words. This account's own two spellings are removed before the comparison —
    only ours, so "ok @someone-else @me" keeps a word we did not write.

    Only the local store is asked (`owner_identity_local`): this runs on a
    read-only path, which must never wait on the bridge, and the answer only
    sharpens a word list. On a store that cannot say who we are — not paired
    yet, whatsmeow.db not mounted — the raw column is compared, as it was
    before issue #411: fewer closing messages recognised, no error on a read
    that was not about mentions in the first place.
    """
    text: str = f"{alias}.content"
    params: list[str] = []
    owner = owner_identity_local()
    if owner is None:
        logger.debug("closing messages: this account is unknown here, own mentions left in the text")
        return text, params
    for user in (owner.get("phone"), owner.get("lid")):
        if user:
            text = f"replace({text}, ?, '')"
            params.append(f"@{user}")
    return text, params


def _closing_message_clause(alias: str) -> tuple[str, list[str]]:
    """Rows that acknowledge rather than ask: a sticker, or one of CLOSING_MESSAGES.

    Reactions and poll votes are already excluded by `_spoken_filter`. Trailing
    ".", "!" and spaces are ignored so "ok!" reads the same as "ok" (and so does
    what is left of "ok @you!" once the mention is removed); SQLite's lower() is
    ASCII-only, which is all these words need and leaves the emoji untouched.

    Both columns are nullable and the caller negates this clause, so `IS` and
    COALESCE keep it two-valued: `NOT NULL` is NULL, which would drop every plain
    text message instead of keeping it.

    The words are compared with the text this account's own `@…` has been taken
    out of (`_own_mention_stripped`), for every caller alike: the ordinary
    "closing last message" rule reads "ok @you" the same way the mention stream
    does (issue #411), which is also what keeps the two streams disjoint.
    """
    text, strip_params = _own_mention_stripped(alias)
    placeholders = ",".join("?" * len(CLOSING_MESSAGES))
    clause = f"({alias}.media_type IS 'sticker' OR ({alias}.media_type IS NOT 'location' AND rtrim(lower(trim(COALESCE({text}, ''))), '.! ') IN ({placeholders})))"
    return clause, [*strip_params, *CLOSING_MESSAGES]


def _last_message_join(chat_alias: str, msg_alias: str, spoken_only: bool = False, chat_match: str = "") -> str:
    """Deterministic single-row join to the chat's newest stored message.

    The row is picked by ordering the chat's messages, never by matching
    `chats.last_message_time`: that marker was advanced by older bridges for
    protocol and unsupported events that stored no message row (the bridge now
    moves it only with a stored row), and history sync writes the
    conversation's own second-resolution timestamp, so an equality join left the last_* fields
    NULL for a large share of chats (issue #218).

    Multiple messages can share a timestamp, so `id DESC` is the tie-break —
    joining on the timestamp alone would duplicate chat rows and make
    last_is_from_me / unread non-deterministic. The correlated subquery is
    driven by idx_messages_chat_timestamp and only runs for the rows the outer
    LIMIT actually returns.

    `spoken_only` skips reactions, poll votes and revoked messages, for callers
    that ask "who spoke last" rather than "what is the last row".

    `chat_match` is an AND-ready predicate on `chat_jid` for a caller that reads
    a merged phone/LID pair (`ChatTwins.both_rows`, issue #366): the newest
    message is then the newest of the two rows. The chat is bound as well as the
    id there, because one pair can hold the same message id under both
    spellings — history sync writing it under the phone JID and the live event
    under the `@lid` — and matching the id alone would return the person twice,
    which is the duplicate this is meant to remove.
    """
    spoken = f"AND {_spoken_filter('m')}" if spoken_only else ""
    match = chat_match or f"= {chat_alias}.jid"

    def newest(column: str) -> str:
        return (
            f"SELECT m.{column} FROM messages m WHERE m.chat_jid {match} {spoken}"
            " ORDER BY m.timestamp DESC, m.id DESC LIMIT 1"
        )

    of_the_pair = f"AND {msg_alias}.chat_jid = ({newest('chat_jid')})" if chat_match else ""
    return f"""
            LEFT JOIN messages {msg_alias} ON {msg_alias}.chat_jid {match}
                AND {msg_alias}.id = ({newest("id")})
                {of_the_pair}
    """


def _sender_aliases(value: str, both_spellings: bool = True) -> list[str]:
    key = value if both_spellings else f"{value}|one"
    hit, cached = _cache_get("aliases", key)
    if hit:
        return list(cached)
    return list(_cache_put("aliases", key, _sender_aliases_uncached(value, both_spellings)))


def _sender_aliases_uncached(value: str, both_spellings: bool = True) -> list[str]:
    # messages.sender is written inconsistently: the same contact may appear as
    # bare phone ("12025550101"), full phone JID ("12025550101@s.whatsapp.net"),
    # bare LID ("100000000000006"), or full LID JID ("100000000000006@lid").
    # whatsmeow_lid_map (whatsapp.db) maps pn<->lid; we emit all four forms so
    # an IN-based filter catches every row regardless of which form was stored.
    #
    # A Brazilian mobile has a second spelling, with or without the ninth digit
    # (issue #475, `phone.py`), and WhatsApp registered only one of them: the
    # map is asked for both and both are emitted, so every tool that resolves
    # a contact through here finds it whichever way the number was written, and
    # whichever identifier named the contact. The digits are taken as given: no
    # separator is dropped here, this set also decides what a filter reads.
    # `both_spellings=False` is the answer from before the second spelling, for
    # the callers that key their own rows by the JID (notes.py).
    bare, _, server = value.partition("@")

    def spellings_of(number: str) -> list[str]:
        other = br_mobile_alternate(number) if both_spellings else None
        return [number, other] if other else [number]

    # A LID is not a phone number: only the number the map gives for it has
    # a second spelling.
    numbers = [] if server == LID_SERVER else spellings_of(bare)
    lids: dict[str, str] = {}  # number -> the LID the map pairs it with
    if os.path.isfile(WHATSMEOW_DB_PATH):
        try:
            conn = _connect_whatsmeow_db()
            try:

                def lids_of(candidates: list[str]) -> dict[str, str]:
                    found: dict[str, str] = {}
                    for number in candidates:
                        row = conn.execute("SELECT lid FROM whatsmeow_lid_map WHERE pn = ?", (number,)).fetchone()
                        if row:
                            found[number] = row[0]
                    return found

                lids = lids_of(numbers)
                if not lids:
                    row = conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ?", (bare,)).fetchone()
                    if row:
                        # Every spelling is asked, not the first that answers:
                        # the set must not depend on which one was typed.
                        numbers = spellings_of(row[0])
                        lids = {row[0]: bare, **lids_of(numbers[1:])}
            finally:
                conn.close()
        except sqlite3.Error:
            pass

    aliases: list[str] = []
    for number in numbers:
        if number in lids:
            aliases += [number, f"{number}@s.whatsapp.net", lids[number], f"{lids[number]}@lid"]
    if not aliases:
        # No mapping found; emit the bare form plus both possible suffixes so
        # we still match whichever form the bridge happened to store.
        aliases = [bare, f"{bare}@s.whatsapp.net", f"{bare}@lid"]
    for number in numbers:
        aliases += [alias for alias in (number, f"{number}@s.whatsapp.net") if alias not in aliases]
    return aliases


# SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999; page sizes stay far
# below that, but chunking keeps a large get_contact_chats page safe.
_SQL_IN_CHUNK = 400


def _is_placeholder_name(name: str | None) -> bool:
    """True when a name identifies nobody: empty, or just the phone number."""
    if not name:
        return True
    return name.strip().lstrip("+").replace(" ", "").replace("-", "").isdigit()


class ContactProfile(NamedTuple):
    """What whatsmeow's contact store knows about one person.

    Two different facts, kept apart (#280): `name` is the label to show, with
    the precedence this archive has always used (full_name → push_name →
    first_name → business_name), and `push_name` is the name the *contact* gave
    themselves — the owner's phone book and a third party's self-description
    answer different questions. `source` says which of the two `name` came from:
    "contacts" (the phone book) or "push" (their own name, because the phone
    book had nothing).

    whatsmeow_contacts has no timestamp, so a push name is a cached snapshot of
    unknown age: present it as "recorded as", never as "is".
    """

    name: str | None
    push_name: str | None
    source: str  # "contacts" | "push" | "" when nothing is known


_NO_PROFILE = ContactProfile(None, None, "")


def _contact_names(jids: list[str]) -> dict[str, str]:
    """Phone-book names for a batch of user JIDs — {jid: name} for known ones."""
    return {jid: profile.name for jid, profile in _contact_profiles(jids).items() if profile.name}


def _contact_profiles(jids: list[str]) -> dict[str, ContactProfile]:
    """Phone-book entries for a batch of user JIDs — {jid: ContactProfile}.

    One pass for a whole page of chats instead of a lookup per chat: at most
    two queries against whatsapp.db (the LID map, then whatsmeow_contacts).
    Results, including "this JID has no entry", are cached for NAME_CACHE_TTL_S
    so paging back and forth costs nothing.
    """
    wanted = {jid for jid in jids if jid and not jid.endswith("@g.us")}
    resolved: dict[str, ContactProfile] = {}
    missing: list[str] = []
    for jid in sorted(wanted):
        hit, cached = _cache_get("contact_profile", jid)
        if not hit:
            missing.append(jid)
        elif cached != _NO_PROFILE:
            resolved[jid] = cached
    if not missing or not os.path.isfile(WHATSMEOW_DB_PATH):
        return resolved

    try:
        conn = _connect_whatsmeow_db()
        try:
            resolved.update(_contact_names_uncached(conn, missing))
        finally:
            conn.close()
    except sqlite3.Error as e:
        # A missing/locked contact store is not an error for the caller: the
        # chat keeps whatever name messages.db had.
        logger.debug("contact-name batch failed: %s", e)
    return resolved


def contact_profile(jid: str) -> ContactProfile:
    """Phone-book entry for one JID (see :class:`ContactProfile`); cached."""
    return _contact_profiles([jid]).get(jid, _NO_PROFILE)


def prefetch_sender_names(messages: Sequence[Message]) -> None:
    """Prime the phone-book cache for a page of messages.

    One batched lookup instead of one per row: msg_to_dict then reads each
    sender's name and push name straight from the cache.
    """
    _contact_profiles([message.sender for message in messages if message.sender and not message.is_from_me])


def _contact_names_uncached(conn: sqlite3.Connection, jids: list[str]) -> dict[str, ContactProfile]:
    """The two-query core of _contact_profiles; also fills the name cache."""
    # whatsmeow_contacts is keyed by the phone JID, so LIDs (and bare numbers,
    # which may be either form) go through whatsmeow_lid_map first.
    lookup: dict[str, str] = {}
    lid_jids = []
    for jid in jids:
        prefix, _, suffix = jid.partition("@")
        if suffix in ("lid", ""):
            lid_jids.append(jid)
        else:
            lookup[jid] = jid

    if lid_jids:
        pn_by_lid: dict[str, str] = {}
        for chunk in _in_chunks([jid.partition("@")[0] for jid in lid_jids]):
            rows = conn.execute(
                f"SELECT lid, pn FROM whatsmeow_lid_map WHERE lid IN ({_placeholders(chunk)})", chunk
            ).fetchall()
            pn_by_lid.update({lid: pn for lid, pn in rows if pn})
        for jid in lid_jids:
            prefix, _, suffix = jid.partition("@")
            pn = pn_by_lid.get(prefix)
            if pn:
                lookup[jid] = f"{pn}@s.whatsapp.net"
            elif suffix == "":
                # Not in the map: a bare number may still be a phone number.
                lookup[jid] = f"{prefix}@s.whatsapp.net"

    profiles_by_contact: dict[str, ContactProfile] = {}
    for chunk in _in_chunks(sorted(set(lookup.values()))):
        rows = conn.execute(
            "SELECT their_jid, full_name, push_name, first_name, business_name "
            f"FROM whatsmeow_contacts WHERE their_jid IN ({_placeholders(chunk)})",
            chunk,
        ).fetchall()
        for their_jid, full_name, push_name, first_name, business_name in rows:
            # A stored name that is just the number is no better than the JID,
            # and that is true field by field: a phone book entry saved as the
            # number must not hide the name the contact gave themselves.
            full, push, first, business = (
                None if _is_placeholder_name(value) else value
                for value in (full_name, push_name, first_name, business_name)
            )
            name = full or push or first or business
            if name and their_jid not in profiles_by_contact:
                source = "push" if name == push and not full else "contacts"
                profiles_by_contact[their_jid] = ContactProfile(name, push, source)

    found: dict[str, ContactProfile] = {}
    for jid in jids:
        profile = profiles_by_contact.get(lookup.get(jid, ""), _NO_PROFILE)
        _cache_put("contact_profile", jid, profile)
        if profile != _NO_PROFILE:
            found[jid] = profile
    return found


def _placeholders(values: list[str]) -> str:
    return ",".join("?" for _ in values)


def _in_chunks(values: list[str], size: int = _SQL_IN_CHUNK) -> list[list[str]]:
    return [values[i : i + size] for i in range(0, len(values), size)]


def lid_map_counterparts(users: Sequence[str]) -> dict[str, str]:
    """{bare user part: the same person's user part in the other namespace}, in one query.

    The batched form of what `_sender_aliases` answers one value at a time: a
    filter that has to resolve a few thousand stored JIDs must not open a
    connection per value. Both directions are returned; users the LID map has
    not paired are absent, and a missing or locked whatsapp.db yields {} rather
    than an error — a caller that cannot see the pairing degrades to the single
    spelling it was given.
    """
    wanted = sorted({user for user in users if user})
    if not wanted or not os.path.isfile(WHATSMEOW_DB_PATH):
        return {}
    pairs: dict[str, str] = {}
    try:
        conn = _connect_whatsmeow_db()
        try:
            for chunk in _in_chunks(wanted):
                marks = _placeholders(chunk)
                rows = conn.execute(
                    f"SELECT lid, pn FROM whatsmeow_lid_map WHERE lid IN ({marks}) OR pn IN ({marks})",
                    [*chunk, *chunk],
                ).fetchall()
                for lid, pn in rows:
                    # Equal parts are the map repeating one identifier, not a pair.
                    if lid and pn and lid != pn:
                        pairs[lid], pairs[pn] = pn, lid
        finally:
            conn.close()
    except sqlite3.Error as e:
        logger.debug("lid-map batch failed: %s", e)
    return pairs


def _cached_lid_counterparts(users: Sequence[str]) -> dict[str, str]:
    """`lid_map_counterparts` behind the name cache: one query for the users not seen yet.

    Every chat listing asks this (`_chat_twins`), and a pairing changes about as
    often as a contact name, so it shares the five-minute cache the names use: a
    warm page opens whatsapp.db no more than the name resolution already does.
    Users the map does not pair are cached as such, or an archive full of
    unmapped LIDs would re-ask on every call.
    """
    pairs: dict[str, str] = {}
    unseen: list[str] = []
    for user in users:
        hit, cached = _cache_get("lid_pair", user)
        if not hit:
            unseen.append(user)
        elif cached:
            pairs[user] = cached
    if unseen:
        found = lid_map_counterparts(unseen)
        for user in unseen:
            if counterpart := _cache_put("lid_pair", user, found.get(user)):
                pairs[user] = counterpart
    return pairs


@dataclass(frozen=True)
class _Twin:
    """The twin a listing row absorbed, seen from the row that lists."""

    absorbed: str  # the chats row this one now speaks for
    jid: str  # the spelling the merged row is reported under (the phone JID)
    aliases: list[str]  # both spellings, phone first
    name: str | None  # the absorbed row's name
    last_read_time: datetime | None
    hidden_names: tuple[str, ...] = ()


@dataclass(frozen=True)
class ChatTwins:
    """The phone/LID chat pairs of one store, and how a listing collapses them.

    A contact WhatsApp knows both ways owns two rows in `chats` — one keyed by
    the phone JID, one by the `@lid` — so every listing used to return that
    person twice with their messages split between the rows (issue #337). Of a
    pair, exactly one row *lists*: the one holding the newest message. Every
    `last_*` field of the merged row is read off that row alone, `unread` and
    the sort key included, so a listing stays ordered by the timestamp it
    prints. What the hidden row still contributes is what belongs to the person
    rather than to the row: the phone spelling `jid` is reported under, both in
    `aliases`, its name when the listing row has none, and its read marker when
    that one is newer (a conversation read under one spelling is read under
    both).

    Only what the LID map pairs is merged: a `@lid` chat it does not know, or
    one whose phone twin has no row in this store, keeps listing on its own.
    """

    by_listed: dict[str, _Twin]  # listing row jid -> the twin it absorbed
    listed_for: dict[str, str]  # hidden row jid -> the row listing instead

    @property
    def active(self) -> bool:
        return bool(self.by_listed)

    def listing_jid(self, jid: str) -> str:
        """The row that lists for this spelling, so either spelling finds the merged chat."""
        return self.listed_for.get(jid, jid)

    def merged_row(self, jid: str) -> _Twin | None:
        """What the row `jid` absorbed, or None when it stands for itself alone."""
        return self.by_listed.get(jid)

    def hidden_clause(self, column: str) -> tuple[str, list[str]]:
        """`column NOT IN (…)`: the rows another row already reports.

        Chunked like every other IN-list here, so the predicate stays inside
        SQLite's variable limit however many pairs the store holds.
        """
        if not self.listed_for:
            return "1=1", []
        hidden = sorted(self.listed_for)
        parts = [_chat_jid_clause(column, chunk, negated=True) for chunk in _in_chunks(hidden)]
        return "(" + " AND ".join(parts) + ")", hidden

    def matching(self, query: str) -> list[str]:
        """Listing rows whose hidden twin answers a `list_chats` query.

        The search runs against the `chats` row, and the twin's name and JID are
        folded in only afterwards, so without this a merged row would be
        unfindable by the very name and spelling it reports.
        """
        needle = query.casefold()
        return sorted(
            listed
            for listed, twin in self.by_listed.items()
            if any(needle in name.casefold() for name in twin.hidden_names or (twin.name or "",))
            or any(needle in jid.casefold() for jid in twin.aliases if jid != listed)
        )

    def absorbing(self, spelling: str) -> list[str]:
        """Listing rows whose hidden twin is stored under this number.

        A JID is compared whole, digits as a substring: the two extra shapes a
        `list_chats` query takes when it is a phone number (`_jid_search_patterns`).
        """
        return sorted(
            listed
            for listed, twin in self.by_listed.items()
            if any((jid == spelling if "@" in spelling else spelling in jid) for jid in twin.aliases if jid != listed)
        )

    def cte(self, name: str = "chat_twin") -> tuple[str, list[str]]:
        """`name(jid, twin_jid, listed_jid, third_jid)` for a WITH clause.

        The empty form keeps one query shape on a store with no pairs, the way
        get_contact_chats_page's `member_of` does for an older schema.
        """
        header = f"{name}(jid, twin_jid, listed_jid, third_jid)"
        rows: list[str] = []
        params: list[str] = []
        for listed, twin in sorted(self.by_listed.items()):
            for jid in twin.aliases:
                other = twin.absorbed if jid == listed else listed
                third = next((member for member in twin.aliases if member not in (jid, other)), None)
                rows.append("(?, ?, ?, ?)" if third else "(?, ?, ?, NULL)")
                params += [jid, other, listed]
                if third:
                    params.append(third)
        if not rows:
            return f"{header} AS (SELECT NULL, NULL, NULL, NULL WHERE 0)", []
        return f"{header} AS (VALUES {', '.join(rows)})", params

    def both_rows(self, alias: str = "tw") -> tuple[str, str, str, list[str]]:
        """(WITH prefix, join, `chat_jid` predicate, params) for the messages of both rows.

        The hidden row is out of `chats`, so a query counting or probing a
        chat's messages has to name it explicitly or it reads half the
        conversation.
        """
        if not self.active:
            return "", "", "= chats.jid", []
        cte, params = self.cte()
        return (
            f"WITH {cte} ",
            f"LEFT JOIN chat_twin {alias} ON {alias}.jid = chats.jid",
            self.message_members("chats.jid", alias),
            params,
        )

    def message_members(self, column: str, alias: str = "tw") -> str:
        """Retain the two-row fast path; a phone pair plus LID has three members."""
        if not any(len(twin.aliases) > 2 for twin in self.by_listed.values()):
            return f"IN ({column}, {alias}.twin_jid)"
        return f"IN ({column}, {alias}.twin_jid, {alias}.third_jid)"

    def merge(self, chats: Sequence[Chat]) -> None:
        """Relabel and fill in every row that absorbed a twin.

        Runs before `_apply_name_fallback`, so the phone book is consulted for
        the spelling the merged row is reported under.
        """
        for chat in chats:
            twin = self.by_listed.get(chat.jid)
            if twin is None:
                continue
            chat.jid = twin.jid
            chat.aliases = list(twin.aliases)
            chat.last_read_time = _later(chat.last_read_time, twin.last_read_time)
            if _is_placeholder_name(chat.name) and not _is_placeholder_name(twin.name):
                chat.name = twin.name


NO_CHAT_TWINS = ChatTwins({}, {})


def _later(left: datetime | None, right: datetime | None) -> datetime | None:
    """The newer of two optional timestamps."""
    if left is None or right is None:
        return left or right
    return max(left, right)


def _paired_lid_chats(cur: sqlite3.Cursor) -> dict[str, str]:
    """{`@lid` chat jid: the phone JID of the same person}, cached for the listings.

    Which spellings exist and which the LID map pairs changes about as often as
    a contact name, so the scan for `@lid` chats and the map lookup share the
    five-minute name cache: a listing on a warm cache pays for the rows of the
    pairs it found, not for a scan of `chats`. Nothing here reads the
    allow-list — the caller applies that per call, so a policy change is not
    something a cached answer can outlive.
    """
    hit, cached = _cache_get("lid_chats", MESSAGES_DB_PATH)
    if hit:
        return cached
    lid_jids = [row[0] for row in cur.execute("SELECT jid FROM chats WHERE jid LIKE '%@lid'").fetchall()]
    counterparts = _cached_lid_counterparts([jid.split("@", 1)[0] for jid in lid_jids]) if lid_jids else {}
    paired = {
        lid_jid: f"{counterparts[lid_jid.split('@', 1)[0]]}@{DEFAULT_USER_SERVER}"
        for lid_jid in lid_jids
        if lid_jid.split("@", 1)[0] in counterparts
    }
    return _cache_put("lid_chats", MESSAGES_DB_PATH, paired)


def _paired_phone_chats(cur: sqlite3.Cursor) -> dict[str, str]:
    """Stored Brazilian ninth-digit pairs, cached without their policy decision."""
    hit, cached = _cache_get("phone_chats", MESSAGES_DB_PATH)
    if hit:
        return cached
    stored = {row[0] for row in cur.execute("SELECT jid FROM chats WHERE jid LIKE '55%@s.whatsapp.net'")}
    pairs = {
        jid: other
        for jid in stored
        if (other := other_phone_spelling(jid)) and other in stored and len(jid) > len(other)
    }
    return _cache_put("phone_chats", MESSAGES_DB_PATH, pairs)


# Far more pairs than an account ever has, and the ceiling of the runtime budget
# below. Past the cap the quietest pairs simply stay unmerged, as they were
# before #337, rather than a listing failing.
CHAT_TWIN_MAX_PAIRS = 2000


def _chat_twin_cap(cur: sqlite3.Cursor) -> int:
    """How many pairs can be merged without crossing SQLite's parameter limit.

    `cte()` binds six parameters per pair and `hidden_clause` one more. A
    three-member identity uses twelve plus two, charged as two pairs; the
    current policy is deducted separately, and the reserve covers the other
    bindings (the filter, the keyset, the limit). SQLite has allowed 32 766 parameters
    since 3.32 (2020) and 999 before it; the limit is asked for rather than
    assumed, so an interpreter carrying an old SQLite merges the busiest ~128
    pairs instead of answering "too many SQL variables".
    """
    try:
        budget = cur.connection.getlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER)
    except (AttributeError, sqlite3.Error):  # pragma: no cover - very old runtimes
        budget = 999
    _, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    return max(0, min(CHAT_TWIN_MAX_PAIRS, (budget - len(policy_params) - 100) // 7))


def _chat_twins(cur: sqlite3.Cursor, only: Sequence[str] | None = None, *, phone_pairs: bool = True) -> ChatTwins:
    """Stored Brazilian phone pairs and confirmed phone/LID pairs (issues #479, #337).

    Two statements on the way to a listing once the pair scan is warm: the rows
    of the paired chats, and the newest message of each — both on indexed
    columns. Both pair scans are cached for five minutes. `only`
    narrows the work to the pairs touching those JIDs, which is all a by-JID
    lookup needs: a listing pays for every pair because it has to hide them all.

    A pair needs both spellings admitted by `WHATSAPP_ALLOWED_CHATS`: an
    allow-list naming one of the two keeps hiding exactly what it hid before,
    rather than having the other half of the conversation folded into it.
    """
    paired = {**(_paired_phone_chats(cur) if phone_pairs else {}), **_paired_lid_chats(cur)}
    wanted = set(only) if only is not None else None
    if wanted is not None:
        # Resolve the connected phone/alternate/LID component before narrowing
        # rows: asking for one spelling must find the same three-row chat.
        while True:
            expanded = wanted | {jid for pair in paired.items() if wanted.intersection(pair) for jid in pair}
            if expanded == wanted:
                break
            wanted = expanded
    candidates = {
        lid_jid: phone_jid
        for lid_jid, phone_jid in paired.items()
        if (wanted is None or lid_jid in wanted or phone_jid in wanted)
        and CHAT_POLICY.allows(lid_jid)
        and CHAT_POLICY.allows(phone_jid)
    }
    if not candidates:
        return NO_CHAT_TWINS

    members = sorted({*candidates, *candidates.values()})
    read_time = _last_read_time_select(cur, "chats")
    rows: dict[str, tuple[str | None, str | None, str | None]] = {}
    for chunk in _in_chunks(members):
        for jid, name, last_time, last_read in cur.execute(
            f"SELECT jid, name, last_message_time, {read_time} FROM chats WHERE jid IN ({_placeholders(chunk)})",
            chunk,
        ).fetchall():
            rows[jid] = (name, last_time, last_read)
    newest: dict[str, str] = {}
    for chunk in _in_chunks([jid for jid in members if jid in rows]):
        for jid, stamp in cur.execute(
            f"SELECT chat_jid, MAX(timestamp) FROM messages WHERE chat_jid IN ({_placeholders(chunk)})"
            " GROUP BY chat_jid",
            chunk,
        ).fetchall():
            newest[jid] = stamp

    def holds_more(jid: str) -> tuple[str, str]:
        # Newest stored message first, the chat's own marker as the tie-break:
        # every last_* field on the merged row comes from the row that wins.
        return (newest.get(jid) or "", rows[jid][1] or "")

    # Only pairs with a row on both sides, busiest first: what the cap drops is
    # the quietest conversations, which is also what a page rarely reaches.
    both_stored = [
        (lid_jid, phone_jid) for lid_jid, phone_jid in candidates.items() if lid_jid in rows and phone_jid in rows
    ]
    both_stored.sort(key=lambda pair: max(holds_more(pair[0]), holds_more(pair[1])), reverse=True)
    cap = _chat_twin_cap(cur)
    if len(both_stored) > cap:
        # Once per cache generation, not once per listing.
        if not _cache_get("twin_cap", MESSAGES_DB_PATH)[0]:
            _cache_put("twin_cap", MESSAGES_DB_PATH, True)
            logger.warning(
                "chat listings: %d identity pairs, only %d of them merged; the quietest list under both spellings",
                len(both_stored),
                cap,
            )
        both_stored = both_stored[:cap]

    by_listed: dict[str, _Twin] = {}
    listed_for: dict[str, str] = {}
    # Only `whatsmeow_lid_map.lid` is unique, so two LIDs can name one number.
    # Merging both would report one JID on two rows, which is worse than the
    # duplicate #337 is about: the busiest pair wins and the rest stay apart.
    groups: dict[str, set[str]] = {}
    group_for: dict[str, str] = {}
    # Join the two phone rows first, then at most one LID per identity. This
    # preserves the existing busiest-LID rule without splitting a phone pair.
    for left, right in sorted(both_stored, key=lambda pair: pair[0].endswith("@lid")):
        key = group_for.get(right, right)
        members_of_group = groups.setdefault(key, {right})
        if left.endswith("@lid") and any(jid.endswith("@lid") for jid in members_of_group):
            continue
        members_of_group.add(left)
        group_for[left] = group_for[right] = key
    for group in groups.values():
        phones = sorted((jid for jid in group if jid.endswith("@s.whatsapp.net")), key=lambda jid: (len(jid), jid))
        phone_jid = phones[0]
        aliases = [*phones, *sorted(group - set(phones))]
        listed = max(group, key=lambda jid: (holds_more(jid), jid == phone_jid))
        hidden_rows = [jid for jid in aliases if jid != listed]
        hidden = next((jid for jid in hidden_rows if not _is_placeholder_name(rows[jid][0])), hidden_rows[0])
        name = rows[hidden][0]
        last_reads = [parse_db_time(stamp) for jid in hidden_rows if (stamp := rows[jid][2])]
        by_listed[listed] = _Twin(
            absorbed=hidden,
            jid=phone_jid,
            aliases=aliases,
            name=name,
            last_read_time=max(last_reads) if last_reads else None,
            hidden_names=tuple(name for jid in hidden_rows if (name := rows[jid][0])),
        )
        listed_for.update({jid: listed for jid in hidden_rows})
    return ChatTwins(by_listed, listed_for) if by_listed else NO_CHAT_TWINS


def _apply_name_fallback(chats: list[Chat], *, phone_aliases: bool = True) -> None:
    """Fill name/push_name/name_source from the phone book for a page of chats.

    `chats.name` is what WhatsApp pushed for the conversation; for a large share
    of direct chats that is empty or the bare number even when the contact is in
    the phone book (issue #230). `push_name` — the name the contact gave
    themselves — is attached whatever `name` ended up being, because it answers
    a different question (#280). Resolution is batched over the whole page.

    The status feed is named here rather than by the phone book: WhatsApp stores
    it under whoever posted last, so the row would otherwise read as a chat with
    that person (issue #379).
    """
    direct = [chat for chat in chats if not chat.is_group and not chat.is_status]
    lookup = _chat_contact_profiles if phone_aliases else _contact_profiles
    profiles = lookup([chat.jid for chat in direct]) if direct else {}
    for chat in chats:
        if chat.is_status:
            chat.name = chat_display_name(chat.jid, chat.name)
            chat.name_source = "system"
            chat.push_name = None
            continue
        profile = profiles.get(chat.jid, _NO_PROFILE)
        chat.push_name = profile.push_name
        if not _is_placeholder_name(chat.name):
            chat.name_source = "chat"
            continue
        if profile.name:
            chat.name = profile.name
            chat.name_source = profile.source or "contacts"
        else:
            chat.name_source = "jid"


def get_sender_name(sender_jid: str) -> str:
    hit, cached = _cache_get("name", sender_jid)
    if hit:
        return cached
    return _cache_put("name", sender_jid, _get_sender_name_uncached(sender_jid))


def _get_sender_name_uncached(sender_jid: str) -> str:
    """chats.name for the sender, else the phone book, else the JID back."""
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # Exact match on the JID as stored, then on the other spellings of the
        # same number (bare, phone JID, LID JID). No LIKE '%number%': it scanned
        # the table and could match an unrelated JID containing the digits.
        bare = sender_jid.split("@")[0] if "@" in sender_jid else sender_jid
        candidates = [sender_jid, bare, f"{bare}@s.whatsapp.net", f"{bare}@lid"]
        if "@" in sender_jid and CHAT_POLICY.restricted:
            # Under an allow-list a full JID names its own row and no other:
            # the same digits under the other server may be a chat the list
            # does not name, and get_contact would hand its name out (issue
            # #466). A bare sender, which is how a message row stores one,
            # still does not say which namespace it is in.
            candidates = [sender_jid]
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

        if result and not _is_placeholder_name(result[0]):
            return result[0]

    except sqlite3.Error as e:
        logger.error("Database error while getting sender name: %s", e)
        return sender_jid
    finally:
        if "conn" in locals():
            conn.close()

    # The phone book, through the same batched lookup a page of chats uses
    # (_contact_names, #245): one code path for LID resolution and name
    # precedence, and one cache, so a sender already resolved for a chat row
    # costs nothing here (issue #257).
    return _contact_names([sender_jid]).get(sender_jid) or sender_jid


# SQLite's default parameter limit is 999 on older builds; 3 params per hit
# plus the neighbour count.
_CONTEXT_HITS_PER_QUERY = 300


def _context_side_sql(values: str, newest_first: bool, include_deleted: bool, columns: str) -> str:
    """One direction of the context window for a batch of hits.

    The neighbours are chosen inside a correlated subquery that seeks
    idx_messages_chat_timestamp from the hit's timestamp and stops at LIMIT ?,
    so the work is the window, not the chat; the outer query then fetches those
    rows by rowid (`messages` is an ordinary rowid table: its primary key is the
    composite (id, chat_jid), not INTEGER PRIMARY KEY). Ranking the whole chat
    history with ROW_NUMBER() and filtering afterwards read every row on that
    side of the hit, so the cost grew with the archive (issue #312).

    The joins are CROSS JOINs to pin the loop order: nothing constrains
    `messages` to `hits` except that subquery, so on a store where ANALYZE has
    run the planner would otherwise drive from `chats`, build an automatic index
    on messages.chat_jid and re-run the subquery once per (chat message x hit)
    — measured at 90 s for one 300-hit batch over 3 x 30k messages, against 5 ms
    with the order below. CROSS JOIN is SQLite's documented way to say "keep this
    nesting"; it changes the plan, never the result.
    """
    direction = "DESC" if newest_first else "ASC"
    comparison = "<" if newest_first else ">"
    deleted_filter = "" if include_deleted else "AND neighbour.deleted_at IS NULL"
    return f"""
        WITH hits(hit_idx, chat_jid, ts) AS (VALUES {values})
        SELECT {columns}, hits.hit_idx
        FROM hits
        CROSS JOIN messages ON messages.rowid IN (
            SELECT neighbour.rowid
            FROM messages AS neighbour
            WHERE neighbour.chat_jid = hits.chat_jid
              AND neighbour.timestamp {comparison} hits.ts
              {deleted_filter}
            ORDER BY neighbour.timestamp {direction}, neighbour.id {direction}
            LIMIT ?
        )
        CROSS JOIN chats ON chats.jid = messages.chat_jid
        ORDER BY hits.hit_idx, messages.timestamp {direction}, messages.id {direction}
    """


def _fetch_context_windows(
    cursor: sqlite3.Cursor,
    hits: list[Message],
    before: int,
    after: int,
    include_deleted: bool = True,
) -> dict[tuple[str, str], tuple[list[Message], list[Message]]]:
    """Fetch the before/after window for many hits, one query per direction and batch.

    Returns {(id, chat_jid): (before_msgs newest-first, after_msgs oldest-first)}.
    A hit is identified by its position in the batch, so the same message ID in
    two chats (forwards, gotcha 2) keeps two windows — and a hit repeated in the
    list is asked for once, or its window would be collected twice.
    """
    windows: dict[tuple[str, str], tuple[list[Message], list[Message]]] = {}
    anchors: list[Message] = []
    for hit in hits:
        key = (hit.id, hit.chat_jid)
        if key not in windows:
            windows[key] = ([], [])
            anchors.append(hit)
    if not anchors or (before <= 0 and after <= 0):
        return windows
    columns = message_columns(cursor)
    width = len(MESSAGE_COLUMNS.split(","))
    for start in range(0, len(anchors), _CONTEXT_HITS_PER_QUERY):
        batch = anchors[start : start + _CONTEXT_HITS_PER_QUERY]
        values = ",".join("(?, ?, ?)" for _ in batch)
        hit_params: list[Any] = []
        for index, hit in enumerate(batch):
            # The anchor is compared against the raw column, so it has to be
            # spelled the way the column is (timestamp_bound), not re-rendered
            # by isoformat().
            hit_params.extend([index, hit.chat_jid, timestamp_bound(hit.timestamp)])
        for newest_first, count in ((True, before), (False, after)):
            if count <= 0:
                continue
            cursor.execute(_context_side_sql(values, newest_first, include_deleted, columns), (*hit_params, count))
            side = 0 if newest_first else 1
            for row in cursor.fetchall():
                hit = batch[row[width]]
                windows[(hit.id, hit.chat_jid)][side].append(_row_to_message(row[:width]))
    return windows


# Media rows the archive actually stores a file for. `reaction` and `poll_vote`
# also live in messages.media_type but are pointer rows (gotcha 3), never media.
POINTER_MEDIA_TYPES = ("reaction", "poll_vote")

# A direct conversation is one-to-one with another person: a phone JID or the
# same person's LID alias (gotcha 1). Every other server is either a fan-out
# surface — `@g.us` groups, `@broadcast` lists, `@newsletter` channels — or not
# a person at all: `@bot` chats (Meta AI and friends) answer on their own and
# nobody is waiting there for a reply, which is what `exclude_groups` is asked
# for (issue #274). So the flag keeps this allow-list instead of denying `@g.us`
# alone (issue #256), and a server WhatsApp adds later is dropped until someone
# decides it is direct. The `is_group` field of a chat is unaffected: it still
# means `@g.us` and nothing else.
DIRECT_JID_SUFFIXES = ("@s.whatsapp.net", "@lid")


def _direct_only_clause(column: str) -> str:
    """SQL predicate keeping direct (one-to-one) conversations only.

    The suffixes are module constants, never user input, so they are inlined
    rather than bound.
    """
    return "(" + " OR ".join(f"{column} LIKE '%{suffix}'" for suffix in DIRECT_JID_SUFFIXES) + ")"


# What a caller writes when they meant a list: "a@s.whatsapp.net,b@g.us" (or the
# same with a semicolon or a space). A JID holds none of these, so any of them
# inside one entry is the mistake, not a chat that happens not to exist.
_JID_SEPARATORS = re.compile(r"[,;\s]")


def _chat_jid_aliases(jid: str) -> list[str]:
    """Every spelling of one chat JID the archive may hold (gotcha 1).

    A direct conversation is stored under the phone JID or under the same
    person's `@lid`, depending on when the row was written, so filtering on the
    form the caller happened to know would miss half the chat. Group, broadcast,
    newsletter and bot servers have one spelling and are returned as given.
    """
    normalized = normalize_chat_entry(jid)
    user, _, server = normalized.rpartition("@")
    if f"@{server}" not in DIRECT_JID_SUFFIXES:
        return [normalized]
    # _sender_aliases also emits the bare user forms, which the chat_jid column
    # never holds; keep the JIDs. It is given the whole JID: only a phone JID
    # has a second spelling (issue #475), a LID never does.
    return [alias for alias in _sender_aliases(normalized) if "@" in alias]


def chat_jid_filter(
    value: str | Sequence[str] | None, argument: str = "chat_jid", require_allowed: bool = True
) -> list[str]:
    """One chat filter argument as the list of JIDs to bind, aliases expanded.

    Accepts a single JID (or bare phone number) and a list of them; a string
    holding several JIDs joined by a separator is refused rather than filtered
    on literally, which used to answer an empty page — indistinguishable from
    "nothing happened in those chats" (issue #289).
    """
    if value is None:
        return []
    listed = not isinstance(value, str)
    raw = [value] if isinstance(value, str) else [str(item) for item in value]
    entries = [item.strip() for item in raw if str(item).strip()]
    if listed and not entries:
        raise ToolError("invalid_argument", f"{argument} was an empty list; omit it to match every allowed chat")

    aliases: list[str] = []
    for entry in entries:
        _validate_chat_filter_entry(entry, argument)
        if require_allowed:
            _require_readable(entry)
        for alias in _chat_jid_aliases(entry):
            if alias not in aliases:
                aliases.append(alias)
    return aliases


def _chat_jid_clause(column: str, jids: list[str], negated: bool = False) -> str:
    """`column IN (?, ...)`, or its negation. SQLite seeks the index for one value too."""
    return f"{column} {'NOT ' if negated else ''}IN ({','.join('?' * len(jids))})"


# Every time column the bridge writes holds one spelling: "YYYY-MM-DD
# HH:MM:SS+00:00" — UTC, seconds, fixed width (store_time.go, and the schema
# note in docs/ARCHITECTURE.md). Same offset on every row is what makes a plain
# string comparison a comparison of instants, so a bound renders the same way
# and binds against the raw column: the timestamp indexes serve the range again
# instead of only the ORDER BY, which the substring expression this replaced
# could not do (issues #253, #257, #270).
#
# Earlier releases let the SQLite driver render the value and stamped the
# writing machine's local offset on the row (older stores also hold Go's
# time.Time.String() form). The bridge rewrites those on startup, so they only
# survive in a store it has not reopened yet; parse_db_time still reads them.
_GO_STRING_TIME = re.compile(r"^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?) ([+-]\d{4}) \S+$")


def timestamp_bound(moment: datetime) -> str:
    """A datetime as a bound in the stored spelling: UTC, to the second.

    An offset-aware bound is converted to UTC, so the same instant expressed in
    any zone selects the same rows; a naive one is read as UTC, which is what
    the archive holds and what every tool result reports.
    """
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=UTC)
    return moment.astimezone(UTC).strftime("%Y-%m-%d %H:%M:%S+00:00")


def parse_db_time(stored: str) -> datetime:
    """A stored timestamp as an aware datetime, in any spelling the bridge wrote.

    The canonical form and the ISO-8601 variants go through fromisoformat; Go's
    time.Time.String() form ("2026-01-09 18:00:00 +0000 UTC", optionally with a
    monotonic " m=+..." suffix) needs the regexp. A value with no offset is read
    as UTC, the convention SQLite's own date functions use. Raises ValueError
    when the text is not a timestamp at all.
    """
    text = stored.strip()
    monotonic = text.find(" m=")
    if monotonic > 0:
        text = text[:monotonic].strip()
    try:
        moment = datetime.fromisoformat(text)
    except ValueError:
        match = _GO_STRING_TIME.match(text)
        if match is None:
            raise ValueError(f"unrecognised timestamp {stored!r}") from None
        offset = match.group(2)
        moment = datetime.fromisoformat(f"{match.group(1)}{offset[:3]}:{offset[3:]}")
    return moment if moment.tzinfo is not None else moment.replace(tzinfo=UTC)


@dataclass(frozen=True)
class MessageFilters:
    """The WHERE predicates every messages query shares.

    One place to build them so list_messages, message_stats and any future
    aggregate answer the same question for the same arguments. `build()`
    validates the combination and returns (clauses, params) in matching order.
    """

    after: str | None = None
    before: str | None = None
    sender_phone_number: str | None = None
    chat_jid: str | Sequence[str] | None = None
    exclude_chat_jid: str | Sequence[str] | None = None
    from_me: bool | None = None
    has_media: bool | None = None
    media_type: str | None = None
    exclude_groups: bool = False
    include_deleted: bool = True
    unread_only: bool = False
    mentions_me: bool = False

    def build(self, cur: sqlite3.Cursor) -> tuple[list[str], list[Any]]:
        _validate_filter_identifiers(self.chat_jid, self.exclude_chat_jid, self.sender_phone_number)
        clauses: list[str] = []
        params: list[Any] = []

        if self.after:
            clauses.append("messages.timestamp > ?")
            params.append(_parse_filter_date(self.after, "after"))

        if self.before:
            clauses.append("messages.timestamp < ?")
            params.append(_parse_filter_date(self.before, "before"))

        if self.sender_phone_number:
            aliases = _sender_aliases(self.sender_phone_number)
            clauses.append(f"messages.sender IN ({','.join('?' * len(aliases))})")
            params.extend(aliases)

        if chats := chat_jid_filter(self.chat_jid):
            clauses.append(_chat_jid_clause("messages.chat_jid", chats))
            params.extend(chats)

        # An exclusion only ever removes rows the caller could already read, so
        # it is not checked against the allow-list: naming a chat this server
        # cannot see is a no-op, not an escalation.
        if excluded := chat_jid_filter(self.exclude_chat_jid, "exclude_chat_jid", require_allowed=False):
            clauses.append(_chat_jid_clause("messages.chat_jid", excluded, negated=True))
            params.extend(excluded)

        if self.exclude_groups:
            clauses.append(_direct_only_clause("messages.chat_jid"))

        clause, clause_params = CHAT_POLICY.sql_clause("messages.chat_jid")
        clauses.append(clause)
        params.extend(clause_params)

        if not self.include_deleted:
            clauses.append("messages.deleted_at IS NULL")

        from_me = self._resolve_direction()
        if from_me is not None:
            clauses.append("messages.is_from_me = ?")
            params.append(1 if from_me else 0)

        if self.unread_only:
            read_marker = _last_read_time_select(cur, "chats")
            clauses.append(f"({read_marker} IS NULL OR messages.timestamp > {read_marker})")

        media_clause, media_params = self._media_predicate()
        if media_clause:
            clauses.append(media_clause)
            params.extend(media_params)

        if self.mentions_me:
            mention_clause, mention_params = mentions_me_predicate(cur, "messages")
            clauses.append(mention_clause)
            params.extend(mention_params)

        return clauses, params

    def _resolve_direction(self) -> bool | None:
        """from_me as a tri-state, with unread_only implying inbound only."""
        if self.unread_only:
            if self.from_me:
                raise ToolError(
                    "invalid_argument",
                    "unread_only only ever matches inbound messages; from_me=True cannot match. Drop one of the two.",
                )
            return False
        return self.from_me

    def _media_predicate(self) -> tuple[str | None, list[Any]]:
        media_type = (self.media_type or "").strip() or None
        if media_type is not None and media_type not in (*MEDIA_TYPES, "location"):
            raise ToolError("invalid_argument", f"media_type must be one of {', '.join((*MEDIA_TYPES, 'location'))}")
        if media_type is not None:
            if media_type == "location" and self.has_media is True:
                raise ToolError("invalid_argument", "locations have no downloadable media; use has_media=False")
            if media_type != "location" and self.has_media is False:
                raise ToolError("invalid_argument", "has_media=False cannot be combined with a media_type")
            return "messages.media_type = ?", [media_type]
        if self.has_media is None:
            return None, []
        placeholders = ",".join("?" * len(MEDIA_TYPES))
        if self.has_media:
            return f"messages.media_type IN ({placeholders})", list(MEDIA_TYPES)
        return f"(messages.media_type IS NULL OR messages.media_type NOT IN ({placeholders}))", list(MEDIA_TYPES)


_SUBSTRING_PREDICATE = "(instr(LOWER(messages.content), LOWER(?)) > 0 OR instr(messages.content, ?) > 0)"


@dataclass(frozen=True)
class QueryPredicate:
    """How one `query=` is turned into SQL.

    `join` is glued into the FROM clause (its parameters come first, as SQL
    reads them), clause/params go into the WHERE list. `relevance_order` is the
    ORDER BY that ranks the hits, or None when this query cannot be ranked and
    sort_by="relevance" has to fall back to newest-first. `match_index` points
    at the MATCH text inside `join_params + params` so a query that is not valid
    FTS5 syntax can be retried quoted (`_execute_message_sql`).
    """

    clause: str | None
    params: list[Any]
    join: str = ""
    join_params: list[Any] = field(default_factory=list)
    relevance_order: str | None = None
    match_index: int | None = None

    def bind(self, filter_params: list[Any]) -> tuple[list[Any], int | None]:
        """The parameters in SQL order (join, filters, predicate) and where MATCH sits."""
        params = [*self.join_params, *filter_params, *self.params]
        if self.match_index is None:
            return params, None
        if self.match_index < len(self.join_params):
            return params, self.match_index
        return params, self.match_index + len(filter_params)


# Transcript hits are staged in a TEMP table rather than bound one parameter per
# hash: the old form capped them at a few hundred files to stay under SQLite's
# parameter limit, and had nowhere to put the score. TEMP lives in the
# connection's own scratch database, so this never writes to the bridge-owned
# messages.db and dies with the connection.
#
# Every hit is staged, in batches. The message query is what decides which of
# them survive the chat, time and allow-list filters, so a cap here (there used
# to be one, 5000 hashes) truncated the candidate set *before* the scope was
# known: a chat whose only matching voice note fell outside the cap counted
# zero. The batch bounds what is held in memory at once, not the answer.
_TRANSCRIPT_HITS_TABLE = "transcript_hits"
_TRANSCRIPT_STAGE_BATCH = 1000


def _stage_transcript_hits(conn: sqlite3.Connection, query: str) -> bool:
    """Materialise the (sha256, score) transcript matches for `query`; False when there are none.

    Voice notes carry no `content`, so neither messages_fts nor instr() can ever
    find what was said in them; the text lives in notes.db, written by
    `transcribe_audio` or by the TRANSCRIBE_ON_INGEST worker and indexed there
    (media_notes.transcripts_fts). notes.db is the MCP server's own database, so
    the union with the message hits happens here.
    """
    from media_notes import iter_transcript_matches

    hits = iter_transcript_matches(query)
    staged = 0
    try:
        # Drop first, whatever happens next: no path may leave an earlier query's
        # hashes behind for this one to read.
        conn.execute(f"DROP TABLE IF EXISTS temp.{_TRANSCRIPT_HITS_TABLE}")
        while batch := list(islice(hits, _TRANSCRIPT_STAGE_BATCH)):
            if not staged:
                conn.execute(f"CREATE TEMP TABLE {_TRANSCRIPT_HITS_TABLE} (sha BLOB PRIMARY KEY, score REAL NOT NULL)")
            conn.executemany(
                f"INSERT OR REPLACE INTO {_TRANSCRIPT_HITS_TABLE} (sha, score) VALUES (?, ?)",
                [(bytes.fromhex(sha), score) for sha, score in batch],
            )
            staged += len(batch)
        if staged:
            conn.commit()
    except sqlite3.Error as exc:
        # No scratch space (a read-only temp dir...), or notes.db failing while
        # its rows are read: the audio side is dropped whole rather than left
        # half-staged, and the search answers from the message text alone.
        logger.warning("could not stage transcript hits, searching message text only: %s", exc)
        try:
            conn.execute(f"DROP TABLE IF EXISTS temp.{_TRANSCRIPT_HITS_TABLE}")
            conn.commit()
        except sqlite3.Error:
            pass
        return False
    finally:
        hits.close()
    return staged > 0


# Message hits and audio hits as one ranked (rowid, score) set. FTS5 refuses
# MATCH inside an OR, so the two sides are unioned in a subquery instead and
# joined on rowid; MIN() keeps the better score when a message matches on both.
# The scores come from two indexes, so ordering across them is an approximation
# — the same tokenizer and the same bm25 weights, over different corpora.
_RANKED_UNION_JOIN = (
    "JOIN (SELECT rid, MIN(score) AS score FROM ("
    f"SELECT rowid AS rid, bm25({MESSAGES_FTS_TABLE}) AS score FROM {MESSAGES_FTS_TABLE} "
    f"WHERE {MESSAGES_FTS_TABLE} MATCH ? "
    "UNION ALL "
    f"SELECT m.rowid, t.score FROM messages m JOIN {_TRANSCRIPT_HITS_TABLE} t ON t.sha = m.file_sha256"
    ") GROUP BY rid) hits ON hits.rid = messages.rowid"
)

# The store key (id, chat_jid) breaks the remaining ties so the offset a
# relevance cursor carries keeps pointing at the same row between pages, even
# when the same forwarded id sits in several chats at the same second.
_RELEVANCE_TIEBREAK = "messages.timestamp DESC, messages.id DESC, messages.chat_jid DESC"


def _query_predicate(conn: sqlite3.Connection, query: str | None) -> QueryPredicate:
    """Content-search predicate shared by the message list and count queries.

    The FTS5 index serves the query when it exists and the text is
    index-friendly; otherwise instr() on the raw column, because SQLite's
    LOWER() is ASCII-only and LIKE LOWER(...) would silently drop Unicode
    matches. Either way, messages whose transcript matches are unioned in.
    """
    if not query:
        return QueryPredicate(None, [])
    transcripts = _stage_transcript_hits(conn, query)
    if _fts_query_kind(query) == "fts" and _fts_available(conn):
        if not transcripts:
            return QueryPredicate(
                f"{MESSAGES_FTS_TABLE} MATCH ?",
                [query],
                join=f"JOIN {MESSAGES_FTS_TABLE} ON {MESSAGES_FTS_TABLE}.rowid = messages.rowid",
                relevance_order=f"bm25({MESSAGES_FTS_TABLE}), {_RELEVANCE_TIEBREAK}",
                match_index=0,
            )
        return QueryPredicate(
            None,
            [],
            join=_RANKED_UNION_JOIN,
            join_params=[query],
            relevance_order=f"hits.score, {_RELEVANCE_TIEBREAK}",
            match_index=0,
        )
    if not transcripts:
        return QueryPredicate(_SUBSTRING_PREDICATE, [query, query])
    # No index to rank with: the set is right, the order stays newest-first.
    return QueryPredicate(
        f"({_SUBSTRING_PREDICATE} OR messages.file_sha256 IN (SELECT sha FROM {_TRANSCRIPT_HITS_TABLE}))",
        [query, query],
    )


def _execute_message_sql(
    cur: sqlite3.Cursor, sql: str, params: list[Any], match_param_index: int | None, query: str | None
) -> None:
    """Execute a message query, retrying an FTS MATCH whose raw text is not FTS5 syntax."""
    try:
        cur.execute(sql, tuple(params))
    except sqlite3.OperationalError:
        if match_param_index is None:
            raise
        # Raw text was not valid FTS5 syntax (operator characters, unbalanced
        # quotes...). Retry with every token quoted so it is matched literally.
        params[match_param_index] = _fts_quote_tokens(query or "")
        cur.execute(sql, tuple(params))


def count_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    query: str | None = None,
    include_deleted: bool = True,
    unread_only: bool = False,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    mentions_me: bool = False,
) -> int:
    """How many messages match the list_messages filters, without returning any row."""
    # Resolve "who am I" before opening the database: it is a local read in the
    # normal case but can fall back to the bridge over HTTP, and a cursor must
    # never be held open across that.
    if mentions_me:
        owner_identity()
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        predicate = _query_predicate(conn, query)
        where_clauses, params = MessageFilters(
            after=after,
            before=before,
            sender_phone_number=sender_phone_number,
            chat_jid=chat_jid,
            exclude_chat_jid=exclude_chat_jid,
            from_me=from_me,
            has_media=has_media,
            media_type=media_type,
            exclude_groups=exclude_groups,
            include_deleted=include_deleted,
            unread_only=unread_only,
            mentions_me=mentions_me,
        ).build(cur)
        if predicate.clause:
            where_clauses.append(predicate.clause)
        params, match_param_index = predicate.bind(params)
        join = f" {predicate.join}" if predicate.join else ""
        where = f" WHERE {' AND '.join(where_clauses)}" if where_clauses else ""
        sql = f"SELECT COUNT(*) FROM messages JOIN chats ON messages.chat_jid = chats.jid{join}{where}"
        _execute_message_sql(cur, sql, params, match_param_index, query)
        row = cur.fetchone()
        return int(row[0]) if row else 0
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def _parse_filter_date(value: str, field: str) -> str:
    """An after/before argument as a normalised timestamp bound (`timestamp_bound`)."""
    try:
        return timestamp_bound(datetime.fromisoformat(value))
    except ValueError:
        raise ValueError(f"Invalid date format for '{field}': {value}. Please use ISO-8601 format.") from None


def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
    sort_by: str = "newest",
    include_deleted: bool = True,
    unread_only: bool = False,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    mentions_me: bool = False,
) -> list[dict[str, Any]]:
    """Items of one page of list_messages_page (kept for callers that want a plain list)."""
    return list_messages_page(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after,
        sort_by=sort_by,
        include_deleted=include_deleted,
        unread_only=unread_only,
        from_me=from_me,
        has_media=has_media,
        media_type=media_type,
        exclude_groups=exclude_groups,
        mentions_me=mentions_me,
    ).items


def list_messages_page(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
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
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    mentions_me: bool = False,
) -> PageResult:
    """Get messages matching the specified criteria with optional context.

    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: One chat JID, or a list of them, to filter messages by chat
        exclude_chat_jid: One chat JID, or a list of them, to drop from the result
        query: Optional search term to filter messages by content. With the bridge's
            FTS5 index this is accent-insensitive and word-based, and supports
            AND / OR / NOT, "exact phrase" and prefix* operators; without it a
            plain substring match is used. Messages whose stored transcript
            matches are unioned in (through the MCP server's own index over
            notes.db), so a voice note somebody transcribed is searchable by
            what was said in it and is ranked with the written hits.
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
        sort_by: Sort order - "newest" (default), "oldest" for chronological ordering, or
            "relevance" (best match first, written and spoken hits ranked together;
            only meaningful with query and the FTS index)
        include_deleted: Keep revoked messages in the result (default True; they carry
            deleted_at and their original content). False hides them.
        unread_only: Only inbound messages newer than their chat's read marker
            (chats.last_read_time, as reported by any linked device). Chats with no
            marker count as entirely unread.
        from_me: True for messages you sent, False for inbound only, None for both
        has_media: True for messages carrying a file, False for text-only
        media_type: A file kind (implies has_media=True), or location (no file)
        exclude_groups: Keep direct conversations only (@s.whatsapp.net / @lid),
            dropping @g.us groups, @broadcast lists, @newsletter channels and
            @bot chats
        mentions_me: Only messages that addressed this account with a WhatsApp
            mention (messages.mentions, matched against both the phone and the
            LID spelling of the owner, which the bridge reports on /api/me)

        cursor: Opaque next_cursor from the previous page (keyset pagination).
            When given, page is ignored. Relevance sort falls back to an offset
            carried inside the cursor.

    Returns:
        PageResult: items (hits plus context rows), next_cursor, has_more.
    """
    _validate_filter_identifiers(chat_jid, exclude_chat_jid, sender_phone_number)
    limit = page_size(limit, MESSAGES_MAX_LIMIT)
    page = page_number(page)
    cursor_state = decode_cursor(cursor, "messages")
    if cursor_state is not None and cursor_state.get("s") != sort_by:
        raise ToolError("invalid_argument", "cursor was created with a different sort_by")
    # Resolve "who am I" before opening the database: it is a local read in the
    # normal case but can fall back to the bridge over HTTP, and a cursor must
    # never be held open across that.
    if mentions_me:
        owner_identity()
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()

        predicate = _query_predicate(conn, query)
        ranked = predicate.relevance_order is not None

        # Build base query
        query_parts = [f"SELECT {message_columns(cur)} FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        if predicate.join:
            query_parts.append(predicate.join)
        where_clauses, params = MessageFilters(
            after=after,
            before=before,
            sender_phone_number=sender_phone_number,
            chat_jid=chat_jid,
            exclude_chat_jid=exclude_chat_jid,
            from_me=from_me,
            has_media=has_media,
            media_type=media_type,
            exclude_groups=exclude_groups,
            include_deleted=include_deleted,
            unread_only=unread_only,
            mentions_me=mentions_me,
        ).build(cur)

        if predicate.clause:
            where_clauses.append(predicate.clause)
        params, match_param_index = predicate.bind(params)

        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Sorting and pagination. Keyset on the store key (timestamp, id,
        # chat_jid) for the time orders — an id alone repeats across chats
        # (forwards, broadcasts), so (timestamp, id) is not a total order and
        # the tied rows would be paged over. Relevance (bm25) has no stable key,
        # so its cursor carries an offset.
        keyset = sort_by != "relevance" or not ranked
        offset = page * limit
        if cursor_state is not None:
            if keyset and "t" in cursor_state:
                cmp = ">" if sort_by == "oldest" else "<"
                if "c" in cursor_state:
                    # Row-value comparison (SQLite >= 3.15): one lexicographic
                    # seek that still uses idx_messages_timestamp for its first
                    # term.
                    where_clauses.append(f"(messages.timestamp, messages.id, messages.chat_jid) {cmp} (?, ?, ?)")
                    params.extend([cursor_state["t"], cursor_state["i"], cursor_state["c"]])
                else:
                    # A cursor issued before chat_jid joined the key: resume it
                    # on the old two-part seek, which is exact in both
                    # directions except for the rows tied on (timestamp, id) in
                    # another chat — those stay skipped once, as they were
                    # before this fix. The cursor this page returns carries the
                    # full key, so the walk heals after one page.
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
        if sort_by == "relevance" and predicate.relevance_order:
            query_parts.append(f"ORDER BY {predicate.relevance_order}")
        else:
            order = "ASC" if sort_by == "oldest" else "DESC"
            query_parts.append(f"ORDER BY messages.timestamp {order}, messages.id {order}, messages.chat_jid {order}")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit + 1, offset])

        _execute_message_sql(cur, " ".join(query_parts), params, match_param_index, query)
        messages = cur.fetchall()
        has_more = len(messages) > limit
        messages = messages[:limit]

        next_cursor = None
        if has_more and messages:
            last = messages[-1]
            if keyset:
                next_cursor = encode_cursor({"k": "messages", "s": sort_by, "t": last[0], "i": last[6], "c": last[5]})
            else:
                next_cursor = encode_cursor({"k": "messages", "s": sort_by, "o": offset + limit})

        result = [_row_to_message(msg) for msg in messages]

        if include_context and result:
            # One query per batch of hits (not two per hit); dedupe on (id, chat_jid)
            # because message IDs repeat across chats (forwards, broadcasts).
            windows = _fetch_context_windows(cur, result, context_before, context_after, include_deleted)
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

            return PageResult(msgs_to_dicts(messages_with_context), next_cursor, has_more)

        # Return messages without context
        return PageResult(msgs_to_dicts(result), next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


MESSAGE_STATS_GROUPINGS = ("chat", "day", "month", "sender")
MAX_STATS_BUCKETS = 500

# Bucket key per grouping. Dates are cut out of the stored ISO-8601 string
# rather than passed through strftime(): the bridge writes timestamps in the
# form SQLite's date functions accept, but substr() also survives a trailing
# zone offset and costs no conversion.
_STATS_GROUP_SQL = {
    "chat": "messages.chat_jid",
    "day": "substr(messages.timestamp, 1, 10)",
    "month": "substr(messages.timestamp, 1, 7)",
    "sender": "messages.sender",
}

# messages, from_me, inbound, media, first_timestamp, last_timestamp.
_STATS_AGGREGATES = (
    "COUNT(*), "
    "SUM(CASE WHEN messages.is_from_me THEN 1 ELSE 0 END), "
    "SUM(CASE WHEN messages.is_from_me THEN 0 ELSE 1 END), "
    f"SUM(CASE WHEN messages.media_type IN ({','.join('?' * len(MEDIA_TYPES))}) THEN 1 ELSE 0 END), "
    "MIN(messages.timestamp), MAX(messages.timestamp)"
)


def _stats_row(row: tuple[Any, ...]) -> dict[str, Any]:
    count, from_me, inbound, media, first_ts, last_ts = row
    return {
        "messages": int(count or 0),
        "from_me": int(from_me or 0),
        "inbound": int(inbound or 0),
        "media": int(media or 0),
        "first_timestamp": first_ts,
        "last_timestamp": last_ts,
    }


def message_stats(
    group_by: str = "chat",
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    after: str | None = None,
    before: str | None = None,
    limit: int = 100,
    sender_phone_number: str | None = None,
    query: str | None = None,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    include_deleted: bool = True,
    unread_only: bool = False,
    mentions_me: bool = False,
) -> dict[str, Any]:
    """Aggregate message counts per chat, day, month or sender.

    Same predicates as list_messages (MessageFilters and _query_predicate), so
    the same arguments describe the same set of rows; the totals row covers every
    matching message, while `buckets` is capped and ordered by count descending.
    A `query` is counted here exactly as it would be listed there — the ranking
    is what an aggregate drops, not the set of hits.
    """
    _validate_filter_identifiers(chat_jid, exclude_chat_jid, sender_phone_number)
    if group_by not in MESSAGE_STATS_GROUPINGS:
        raise ToolError("invalid_argument", f"group_by must be one of {', '.join(MESSAGE_STATS_GROUPINGS)}")
    limit = page_size(limit, MAX_STATS_BUCKETS)
    bucket_sql = _STATS_GROUP_SQL[group_by]
    # Resolve "who am I" before opening the database: it is a local read in the
    # normal case but can fall back to the bridge over HTTP, and a cursor must
    # never be held open across that.
    if mentions_me:
        owner_identity()

    twins = NO_CHAT_TWINS
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        predicate = _query_predicate(conn, query)
        clauses, filter_params = MessageFilters(
            after=after,
            before=before,
            sender_phone_number=sender_phone_number,
            chat_jid=chat_jid,
            exclude_chat_jid=exclude_chat_jid,
            from_me=from_me,
            has_media=has_media,
            media_type=media_type,
            exclude_groups=exclude_groups,
            include_deleted=include_deleted,
            unread_only=unread_only,
            mentions_me=mentions_me,
        ).build(cur)
        if predicate.clause:
            clauses.append(predicate.clause)
        bound, match_index = predicate.bind(filter_params)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        join = f" {predicate.join}" if predicate.join else ""
        # A phone/LID pair is one person's messages, split over two chat rows
        # (issue #366): the bucket key is the row that lists for the pair, so
        # the counts of the two are summed instead of ranked against each other.
        if group_by == "chat":
            twins = _chat_twins(cur)
        prefix, twin_params, twin_join = "", [], ""
        if twins.active:
            cte, twin_params = twins.cte()
            prefix = f"WITH {cte} "
        base = f"FROM messages JOIN chats ON messages.chat_jid = chats.jid{twin_join}{join} {where}"
        # The WITH clause binds first, then the SUM(media) placeholders in the
        # SELECT list, ahead of the search join and the WHERE.
        params = [*twin_params, *MEDIA_TYPES, *bound]
        if match_index is not None:
            match_index += len(twin_params) + len(MEDIA_TYPES)

        # The name of the row that lists, never MIN() over a merged pair's two
        # names — that would file a person under whichever of them sorts first,
        # a bare number included. The other name is the fallback below, the
        # precedence every merged row follows.
        label = "MIN(chats.name)" if group_by == "chat" else "NULL"
        if twins.active:
            # Resolve identities after grouping physical chats, rather than
            # joining the VALUES mapping for every archived message.
            prefix = (
                f"WITH {cte}, physical(chat_jid, name, n, outgoing, inbound, media, first_ts, last_ts) AS ("
                f"SELECT messages.chat_jid, MIN(chats.name), {_STATS_AGGREGATES} {base} GROUP BY messages.chat_jid) "
            )
            bucket_sql = "COALESCE(tw.listed_jid, p.chat_jid)"
            label = f"MIN(CASE WHEN p.chat_jid = {bucket_sql} THEN p.name END)"
            base = "FROM physical p LEFT JOIN chat_twin tw ON tw.jid = p.chat_jid"
            aggregates = "SUM(p.n), SUM(p.outgoing), SUM(p.inbound), SUM(p.media), MIN(p.first_ts), MAX(p.last_ts)"
            count_sql = "SUM(p.n)"
        else:
            aggregates, count_sql = _STATS_AGGREGATES, "COUNT(*)"
        _execute_message_sql(
            cur,
            f"{prefix}SELECT {bucket_sql}, {label}, {aggregates} {base} "
            f"GROUP BY {bucket_sql} ORDER BY {count_sql} DESC, {bucket_sql} DESC LIMIT ?",
            [*params, limit],
            match_index,
            query,
        )
        rows = cur.fetchall()
        _execute_message_sql(
            cur,
            f"{prefix}SELECT COUNT(DISTINCT {bucket_sql}), {aggregates} {base}",
            list(params),
            match_index,
            query,
        )
        total_row = cur.fetchone() or (0, 0, 0, 0, 0, None, None)
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()

    buckets: list[dict[str, Any]] = []
    for key, chat_name, *aggregates in rows:
        bucket: dict[str, Any] = {"key": key, **_stats_row(tuple(aggregates))}
        if group_by == "chat":
            # Reported under the phone spelling, as every merged row is.
            twin = twins.merged_row(key)
            if twin is not None:
                bucket["key"] = key = twin.jid
                if _is_placeholder_name(chat_name) and not _is_placeholder_name(twin.name):
                    chat_name = twin.name
            # The status feed is counted like any other chat and labelled like
            # everywhere else, so a bucket and a chat row name the same thing (#379).
            bucket["label"] = chat_display_name(key, chat_name) or key
        elif group_by == "sender":
            bucket["label"] = get_sender_name(key) if key else None
        buckets.append(bucket)

    total = {"buckets": int(total_row[0] or 0), **_stats_row(tuple(total_row[1:]))}
    return {
        "group_by": group_by,
        "buckets": buckets,
        "total": total,
        "truncated": total["buckets"] > len(buckets),
    }


def get_message_context(
    message_id: str, before: int = 5, after: int = 5, chat_jid: str | None = None, include_deleted: bool = True
) -> MessageContext:
    """Get context around a specific message.

    The messages table is keyed by (id, chat_jid): the same WhatsApp message ID
    can legitimately exist in several chats (forwards, broadcasts). Passing
    chat_jid makes the lookup a primary-key hit and removes the ambiguity;
    without it the most recent row with that ID is used.
    """
    if chat_jid:
        _require_allowed(chat_jid)
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()

        # Get the target message first
        select = f"SELECT {message_columns(cursor)} FROM messages JOIN chats ON messages.chat_jid = chats.jid"
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
            SELECT {message_columns(cursor)}
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
            SELECT {message_columns(cursor)}
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


def _chat_contact_profiles(jids: list[str], conn: sqlite3.Connection | None = None) -> dict[str, ContactProfile]:
    """Display profiles; alternate-phone fallback is authorized per call, never cached under the alias."""
    aliases = {
        jid: other
        for jid in jids
        if (other := other_phone_spelling(jid)) and CHAT_POLICY.allows(jid) and CHAT_POLICY.allows(other)
    }
    wanted = list(dict.fromkeys([*jids, *aliases.values()]))
    all_profiles = _contact_names_uncached(conn, wanted) if conn is not None else _contact_profiles(wanted)
    profiles = {jid: all_profiles[jid] for jid in jids if jid in all_profiles}
    profiles.update(
        {
            jid: all_profiles[other]
            for jid, other in aliases.items()
            if not profiles.get(jid, _NO_PROFILE).name and other in all_profiles
        }
    )
    return profiles


def _matching_contact_chat_names(cur: sqlite3.Cursor, query: str) -> list[str]:
    """Placeholder chats findable by exactly the phone-book name they display."""
    if not os.path.isfile(WHATSMEOW_DB_PATH):
        return []
    try:
        conn = _connect_whatsmeow_db()
        try:
            fields = ("full_name", "push_name", "first_name", "business_name")
            match = " OR ".join(f"instr(LOWER({field}), LOWER(?)) > 0 OR instr({field}, ?) > 0" for field in fields)
            hits = {
                row[0] for row in conn.execute(f"SELECT their_jid FROM whatsmeow_contacts WHERE {match}", [query] * 8)
            }
            if not hits:
                return []
            candidates = hits | {other for jid in hits if (other := other_phone_spelling(jid))}
            candidates.update(lid for lid, phone in _paired_lid_chats(cur).items() if phone in hits)
            policy, policy_params = CHAT_POLICY.sql_clause("jid")
            placeholders: list[str] = []
            remaining = cur.connection.getlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER) - len(policy_params)
            for chunk in _in_chunks(sorted(candidates), max(1, min(_SQL_IN_CHUNK, remaining))):
                placeholders.extend(
                    jid
                    for jid, name in cur.execute(
                        f"SELECT jid, name FROM chats WHERE jid IN ({_placeholders(chunk)}) AND ({policy}) AND {_direct_only_clause('jid')}",
                        [*chunk, *policy_params],
                    )
                    if _is_placeholder_name(name)
                )
            profiles = _chat_contact_profiles(placeholders, conn)
            needle = query.casefold()
            return [
                jid
                for jid in placeholders
                if (name := profiles.get(jid, _NO_PROFILE).name) and needle in name.casefold()
            ]
        finally:
            conn.close()
    except sqlite3.Error as exc:
        logger.debug("contact-name search unavailable: %s", exc)
        return []


def _chat_filter(
    query: str | None, twins: ChatTwins = NO_CHAT_TWINS, cur: sqlite3.Cursor | None = None
) -> tuple[list[str], list[Any]]:
    """WHERE clauses selecting the chats a caller may see: name/JID search plus the allow-list.

    Shared by list_chats_page and count_chats so the count is taken over exactly
    the rows the page would return — the collapsed phone/LID twins (issue #337)
    included, which is why the same `twins` has to reach both.
    """
    clauses: list[str] = []
    params: list[Any] = []
    if twins.active:
        clause, hidden = twins.hidden_clause("chats.jid")
        clauses.append(clause)
        params.extend(hidden)
    if query:
        # instr() on the raw column matches Unicode; LOWER()+LIKE only covers ASCII.
        # The searched name is the one the row shows (#379), so the status feed
        # answers to "Status updates" and not to the last poster's number.
        name = _chat_name_expr()
        jid_patterns, other_spellings = _jid_search_patterns(query)
        clause = f"instr(LOWER({name}), LOWER(?)) > 0 OR instr({name}, ?) > 0 OR {_like_any('chats.jid', jid_patterns)}"
        params.extend([query, query, *jid_patterns])
        # A merged row answers to its twin's name and spelling too, and those
        # are folded in only after this runs.
        matched = set(twins.matching(query))
        if cur is not None:
            matched.update(twins.listing_jid(jid) for jid in _matching_contact_chat_names(cur, query))
        for spelling in other_spellings:
            matched.update(twins.absorbing(spelling))
        if matched := sorted(matched):
            if cur is not None:
                # A common contact name may match more JIDs than one SQLite
                # statement can bind. Like triage's temp table, this belongs
                # only to this read connection; neither archive is written.
                cur.execute("CREATE TEMP TABLE IF NOT EXISTS chat_name_matches (jid TEXT PRIMARY KEY)")
                cur.execute("DELETE FROM temp.chat_name_matches")
                cur.executemany("INSERT INTO temp.chat_name_matches VALUES (?)", [(jid,) for jid in matched])
                clause += " OR chats.jid IN (SELECT jid FROM temp.chat_name_matches)"
            else:
                clause = f"{clause} OR {_chat_jid_clause('chats.jid', matched)}"
                params.extend(matched)
        clauses.append(f"({clause})")
    clause, clause_params = CHAT_POLICY.sql_clause("chats.jid")
    clauses.append(clause)
    params.extend(clause_params)
    return clauses, params


def count_chats(query: str | None = None) -> int:
    """How many chats match the list_chats filter, without returning any of them."""
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        clauses, params = _chat_filter(query, _chat_twins(cur), cur)
        where = f" WHERE {' AND '.join(clauses)}" if clauses else ""
        cur.execute(f"SELECT COUNT(*) FROM chats{where}", tuple(params))
        row = cur.fetchone()
        return int(row[0]) if row else 0
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
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

    A contact WhatsApp knows under both a phone JID and a `@lid` is one row
    here, not two: the pair is collapsed before the page is cut, so paging is
    over the listing rows and a merged chat cannot straddle a page boundary
    (issue #337, `ChatTwins`).

    Returns:
        List of chat dictionaries with jid, name, is_group, last_message, etc.
    """
    limit = page_size(limit, CHATS_MAX_LIMIT)
    page = page_number(page)
    cursor_state = decode_cursor(cursor, "chats")
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        twins = _chat_twins(cur)

        # The last message is always joined — is_from_me feeds the unread
        # flag — but its content is only selected when asked for. The columns
        # are referenced by tuple index downstream, so the result shape stays
        # constant across the branch.
        if include_last_message:
            last_message_select = "messages.content as last_message, messages.sender as last_sender"
        else:
            last_message_select = "NULL as last_message, NULL as last_sender"

        # Sorting by name reads the name the row will *show*: for a merged pair
        # that can be the twin's, and the `@lid` row that usually lists carries
        # none, so ordering on the raw column would file the person under "".
        # For the status feed it is the label this server gives it (#379).
        prefix, twin_join, sort_name, twin_params = "", "", _chat_name_expr(), []
        if twins.active and sort_by != "last_active":
            conn.create_function("placeholder_chat_name", 1, _is_placeholder_name, deterministic=True)
            cte, twin_params = twins.cte()
            prefix = f"WITH {cte} "
            twin_join = "LEFT JOIN chat_twin tw ON tw.jid = chats.jid LEFT JOIN chats twin ON twin.jid = tw.twin_jid"
            sort_name = (
                f"CASE WHEN placeholder_chat_name({_chat_name_expr()}) AND NOT placeholder_chat_name(twin.name) "
                f"THEN twin.name ELSE {_chat_name_expr()} END"
            )

        query_parts = [
            f"""
            {prefix}SELECT
                chats.jid,
                chats.name,
                chats.last_message_time,
                {last_message_select},
                messages.is_from_me as last_is_from_me,
                {_last_read_time_select(cur, "chats")},
                messages.id IS NOT NULL as has_messages,
                {sort_name} as sort_name
            FROM chats
            {twin_join}
            {_last_message_join("chats", "messages")}
        """
        ]

        where_clauses, filter_params = _chat_filter(query, twins, cur)
        params = [*twin_params, *filter_params]

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
                where_clauses.append(f"({sort_name} > ? OR ({sort_name} = ? AND chats.jid > ?))")
                params.extend([cursor_state["n"], cursor_state["n"], cursor_state["j"]])
            query_parts = [part for part in query_parts if not part.startswith("WHERE ")]
            query_parts.append("WHERE " + " AND ".join(where_clauses))

        # Add sorting (jid as the tie-breaker so the keyset is total)
        order_by = (
            "chats.last_message_time DESC, chats.jid ASC"
            if sort_by == "last_active"
            else f"{sort_name} ASC, chats.jid ASC"
        )
        query_parts.append(f"ORDER BY {order_by}")

        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit + 1, offset])

        cur.execute(" ".join(query_parts), tuple(params))
        chats = cur.fetchall()
        has_more = len(chats) > limit
        chats = chats[:limit]
        next_cursor = None
        if has_more and chats:
            last = chats[-1]
            state = {"k": "chats", "s": sort_by, "j": last[0]}
            if sort_by == "last_active":
                state["t"] = last[2]
            else:
                state["n"] = last[8]  # the value ORDER BY used, twin name included
            next_cursor = encode_cursor(state)

        page_chats = [
            Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
                last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
                has_messages=bool(chat_data[7]),
            )
            for chat_data in chats
        ]
        twins.merge(page_chats)
        _apply_name_fallback(page_chats)

        return PageResult([chat_to_dict(chat) for chat in page_chats], next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def _matched_field(query: str, candidates: list[tuple[str, str | None]]) -> str | None:
    """Which of the searched fields actually contains the query, first one wins.

    Reported beside every hit so a caller can tell "this is the name I saved"
    from "this is the name they gave themselves" from "the digits matched".
    `None` means nothing in the searched fields contains the query literally,
    which happens when a wildcard (`%`, `_`) matched only the JID pattern.
    """
    needle = query.casefold()
    for label, value in candidates:
        if value and needle in value.casefold():
            return label
    return None


def _like_any(column: str, patterns: list[str]) -> str:
    """`column LIKE ?` once per pattern, OR-ed; the patterns themselves are bound."""
    return "(" + " OR ".join(f"{column} LIKE ?" for _ in patterns) + ")"


def _phone_spellings(value: str) -> tuple[str | None, str | None]:
    """What a phone-number input also stands for (issue #444, `phone.py`).

    Its digits, and the phone JID of the other spelling when it is a Brazilian
    mobile; each is None when there is none. The input is the number however
    it was typed, or its phone JID. A LID is not a phone number, so an input
    naming one has neither.
    """
    user, _, server = (value or "").strip().partition("@")
    if server not in ("", DEFAULT_USER_SERVER):
        return None, None
    digits = phone_digits(user)
    alternate = br_mobile_alternate(digits) if digits else None
    return digits, f"{alternate}@{DEFAULT_USER_SERVER}" if alternate else None


def other_phone_spelling(jid: str) -> str | None:
    """The phone JID of a Brazilian mobile's other spelling, or None.

    Strict where `_phone_spellings` is tolerant: the input is a phone JID or
    bare digits, written the way a stored JID or an allow-list entry is. No
    separator is dropped, because this one also decides who may read
    (`_require_readable`): a formatted number must not be admitted or refused
    depending on which spelling the list happens to name.
    """
    user, _, server = normalize_chat_entry(jid).rpartition("@")
    if server != DEFAULT_USER_SERVER:
        return None
    alternate = br_mobile_alternate(user)
    return f"{alternate}@{DEFAULT_USER_SERVER}" if alternate else None


def lid_jids_of_contact(jid: str) -> list[str]:
    """The LID JIDs the map pairs with a phone number or phone JID, in either spelling (`_lid_jids_of_number`)."""
    digits, alternate = _phone_spellings(jid)
    return _lid_jids_of_number(digits, alternate)


def canonical_note_jids(jids: Sequence[str]) -> dict[str, str]:
    """Deterministic note keys: Brazilian mobiles always have the ninth digit.

    This depends only on spelling and confirmed LID mappings, never on which
    chat or contact rows happen to exist. Unmapped LIDs remain LIDs; foreign
    numbers and landlines keep their normalized spelling. LID queries are batched.
    Authorization remains notes' separate any-alias check on every operation.
    """
    raw = {normalize_chat_entry(jid) for jid in jids}
    lids = [jid.partition("@")[0] for jid in raw if jid.endswith("@lid")]
    counterparts = {}
    if lids and os.path.isfile(WHATSMEOW_DB_PATH):
        try:
            conn = _connect_whatsmeow_db()
            try:
                for chunk in _in_chunks(sorted(set(lids))):
                    for lid, pn in conn.execute(
                        f"SELECT lid, pn FROM whatsmeow_lid_map WHERE lid IN ({_placeholders(chunk)})", chunk
                    ):
                        if pn and lid != pn:
                            counterparts[lid] = pn
            finally:
                conn.close()
        except sqlite3.Error:
            pass
    result = {}
    for jid in raw:
        phone = (
            f"{counterparts[jid.partition('@')[0]]}@{DEFAULT_USER_SERVER}"
            if jid.endswith("@lid") and jid.partition("@")[0] in counterparts
            else jid
        )
        other = other_phone_spelling(phone)
        result[jid] = max((phone, other), key=len) if other else phone
    return result


def note_jid_spellings(jids: Sequence[str], canonical: dict[str, str] | None = None) -> dict[str, list[str]]:
    """Complete confirmed note aliases in batches, including every mapped LID."""
    canonical = canonical_note_jids(jids) if canonical is None else canonical
    members = {}
    for jid in {normalize_chat_entry(jid) for jid in jids}:
        phone = canonical[jid]
        members[jid] = {value for value in (jid, phone, other_phone_spelling(phone)) if value}
    phones = {
        member.partition("@")[0]
        for aliases in members.values()
        for member in aliases
        if member.endswith("@s.whatsapp.net")
    }
    mapped: dict[str, set[str]] = {}
    if phones and os.path.isfile(WHATSMEOW_DB_PATH):
        try:
            conn = _connect_whatsmeow_db()
            try:
                for chunk in _in_chunks(sorted(phones)):
                    for lid, pn in conn.execute(
                        f"SELECT lid, pn FROM whatsmeow_lid_map WHERE pn IN ({_placeholders(chunk)})", chunk
                    ):
                        if lid and pn and lid != pn:
                            mapped.setdefault(pn, set()).add(f"{lid}@lid")
            finally:
                conn.close()
        except sqlite3.Error:
            pass
    for aliases in members.values():
        for member in list(aliases):
            if member.endswith("@s.whatsapp.net"):
                aliases.update(mapped.get(member.partition("@")[0], set()))
    return {jid: sorted(aliases) for jid, aliases in members.items()}


def phone_book_spelling(jid: str) -> str:
    """The spelling of a Brazilian mobile to answer about when it has no chat of its own.

    The phone book knows one spelling only, so the one it can name wins. The
    name lookup carries no allow-list clause of its own, which makes this the
    place that keeps it honest: a spelling the list does not name is never
    looked up, and a contact admitted through its other spelling
    (`_require_readable`) is answered about under the spelling the list names.
    """
    other = other_phone_spelling(jid)
    if other is None or not CHAT_POLICY.allows(other):
        return jid
    if not CHAT_POLICY.allows(jid):
        return other
    if get_sender_name(jid) == jid and get_sender_name(other) != other:
        return other
    return jid


def _jid_search_patterns(query: str, *, national_mobile: bool = False) -> tuple[list[str], list[str]]:
    """The LIKE patterns a search binds against a JID column, and what the extra ones stand for.

    The query as a substring, as before. A query that is a phone number also
    matches with its separators dropped, and a Brazilian mobile under its
    other spelling (issue #444): that one as the whole phone JID, never as a
    substring of somebody else's. A chat WhatsApp keeps under a LID answers to
    the whole number the LID map pairs it with (issue #465), as that LID's
    whole JID. The second list holds those extra spellings, for a caller that
    reports which field matched.
    """
    patterns = ["%" + query + "%"]
    digits, alternate = _phone_spellings(query)
    national = br_national_mobile_alternate(digits) if national_mobile and digits and "@" not in query else None
    national_jid = f"{national}@{DEFAULT_USER_SERVER}" if national else None
    typed_digits = digits if digits and digits != query and "@" not in query else None
    if typed_digits:
        patterns.append("%" + typed_digits + "%")
    if alternate:
        patterns.append(alternate)
    if national_jid:
        patterns.append(national_jid)
    lid_jids = _lid_jids_of_number(digits, alternate or national_jid)
    return [*patterns, *lid_jids], [
        spelling for spelling in (typed_digits, alternate, national_jid, *lid_jids) if spelling
    ]


def _lid_jids_of_number(digits: str | None, alternate: str | None) -> list[str]:
    """The LID JIDs the LID map pairs with a phone number, in either spelling of a Brazilian mobile.

    The whole number, never a fragment of one: the map holds everybody ever
    seen in a group, so a fragment would match hundreds of LIDs that have no
    chat. Every LID of the number comes back (only `lid` is unique in the map),
    and both lookups that follow a number to its LID ask here, so they agree.
    One read of whatsapp.db, uncached, and [] when it cannot be read: the
    lookup then finds what it found before.
    """
    numbers = [number for number in (digits, alternate.partition("@")[0] if alternate else None) if number]
    if not numbers or not os.path.isfile(WHATSMEOW_DB_PATH):
        return []
    try:
        conn = _connect_whatsmeow_db()
        try:
            rows = conn.execute(
                f"SELECT lid FROM whatsmeow_lid_map WHERE pn IN ({_placeholders(numbers)}) ORDER BY lid", numbers
            ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error as e:
        logger.debug("lid-map lookup failed: %s", e)
        return []
    return [f"{lid}@{LID_SERVER}" for (lid,) in rows if lid]


def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search contacts by name or phone number.

    Searches both the messages.db chats table and whatsmeow's contact store
    (whatsapp.db) to find contacts. Results are deduplicated by JID, and each
    one says which field the query matched (#280), because a contact saved as
    "Z Dave" is found by the name they gave themselves.

    Groups are not contacts, and neither is the status feed: `status@broadcast`
    is stored under the number of whoever posted last, so searching that number
    used to answer with a "contact" whose phone number was the word "status"
    (issue #379).

    A phone-number query is matched as digits however it was typed, and a full
    Brazilian mobile also finds the contact stored under its other spelling,
    with or without the ninth digit (issue #444, `phone.py`). A contact
    WhatsApp only keeps under a LID is found by the number the LID map gives
    for it (issue #465), and says so with `matched: "phone_number"`.
    """
    seen_jids: set[str] = set()
    # (jid, name, push name if this hit carried one, fields the query could
    # have matched in reporting order)
    found: list[tuple[str, str | None, str | None, list[tuple[str, str | None]]]] = []
    # JIDs are all ASCII so LIKE is safe; names use instr() because SQLite's
    # LOWER() only folds case for ASCII and would drop Unicode matches.
    jid_patterns, other_spellings = _jid_search_patterns(query, national_mobile=True)

    # 1) Search messages.db chats table (existing behavior)
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        cursor.execute(
            f"""
            SELECT DISTINCT jid, name
            FROM chats
            WHERE
                (instr(LOWER(name), LOWER(?)) > 0 OR instr(name, ?) > 0 OR {_like_any("jid", jid_patterns)})
                AND jid NOT LIKE '%@g.us'
                AND jid <> '{STATUS_BROADCAST_JID}'
            ORDER BY name, jid
            LIMIT 50
        """,
            (query, query, *jid_patterns),
        )
        for jid, name in cursor.fetchall():
            if jid not in seen_jids:
                seen_jids.add(jid)
                # No push name on a chats-table row; the phone book below has it.
                found.append((jid, name, None, [("name", name)]))
    except MessagesDbNotFoundError:
        raise  # a wrong path is the caller's answer, not an empty contact list
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
                f"""
                SELECT their_jid, full_name, push_name, first_name, business_name
                FROM whatsmeow_contacts
                WHERE
                    instr(LOWER(full_name), LOWER(?)) > 0 OR instr(full_name, ?) > 0
                    OR instr(LOWER(push_name), LOWER(?)) > 0 OR instr(push_name, ?) > 0
                    OR instr(LOWER(first_name), LOWER(?)) > 0 OR instr(first_name, ?) > 0
                    OR instr(LOWER(business_name), LOWER(?)) > 0 OR instr(business_name, ?) > 0
                    OR {_like_any("their_jid", jid_patterns)}
                LIMIT 50
            """,
                (query, query, query, query, query, query, query, query, *jid_patterns),
            )
            for their_jid, full_name, push_name, first_name, business_name in cursor2.fetchall():
                if their_jid not in seen_jids:
                    seen_jids.add(their_jid)
                    name = full_name or push_name or first_name or business_name or ""
                    fields = [
                        ("full_name", full_name),
                        ("push_name", push_name),
                        ("first_name", first_name),
                        ("business_name", business_name),
                    ]
                    push = None if _is_placeholder_name(push_name) else push_name
                    found.append((their_jid, name, push, fields))
        except sqlite3.Error as e:
            logger.error("Database error (whatsapp.db): %s", e)
        finally:
            if "conn2" in locals():
                conn2.close()

    # Two batched passes for the whole result: the LID map, so a contact
    # WhatsApp only knows anonymously is reported as a LID instead of as a
    # phone number (#281), and the phone book, for the push name of a hit that
    # came from the chats table (#280).
    jids = [jid for jid, _, _, _ in found]
    identities = _sender_identities(jids)
    profiles = _contact_profiles([jid for jid, _, push, _ in found if push is None])
    rows: list[dict[str, Any]] = []
    for jid, name, push, fields in found:
        # A whatsmeow hit already carries its own push name; only a chats-table
        # hit needs the phone book, and only its own entry — never one the LID
        # map happened to point at.
        push_name = push if push is not None else profiles.get(jid, _NO_PROFILE).push_name
        candidates = [*fields, ("push_name", push_name), ("jid", jid)]
        contact = Contact(
            phone_number=identities[jid].phone,
            lid=identities[jid].lid,
            name=name,
            push_name=push_name,
            jid=jid,
        )
        led_by_number = jid.endswith(f"@{LID_SERVER}") and jid in other_spellings
        if led_by_number and f"{identities[jid].phone}@{DEFAULT_USER_SERVER}" in seen_jids:
            # The number led to this LID row and to the phone row of the same
            # contact: one hit, the phone one.
            continue
        matched = _matched_field(query, candidates)
        if matched is None and led_by_number:
            # Nothing in the JID holds the digits: its phone number does.
            matched = "phone_number"
        elif matched is None and any(spelling in jid for spelling in other_spellings):
            matched = "jid"
        rows.append({**contact_to_dict(contact), "matched": matched})
    if not rows:
        return rows
    try:
        conn = _connect_messages_db()
        try:
            twins = _chat_twins(conn.cursor(), only=[row["jid"] for row in rows])
        finally:
            conn.close()
    except sqlite3.Error:
        return rows
    collapsed: list[dict[str, Any]] = []
    hits: dict[str, dict[str, Any]] = {}
    for row in rows:
        twin = twins.merged_row(twins.listing_jid(row["jid"]))
        if twin is None or sum(jid.endswith("@s.whatsapp.net") for jid in twin.aliases) < 2:
            collapsed.append(row)
            continue
        canonical = twin.jid
        if canonical in hits:
            hit = hits[canonical]
            for key in ("name", "push_name", "lid", "matched"):
                if (
                    key == "name" and _is_placeholder_name(hit.get(key)) and not _is_placeholder_name(row.get(key))
                ) or (not hit.get(key) and row.get(key)):
                    hit[key] = row[key]
            continue
        hit = {**row, "jid": canonical, "phone_number": canonical.partition("@")[0], "aliases": list(twin.aliases)}
        hits[canonical] = hit
        collapsed.append(hit)
    return collapsed


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> list[dict[str, Any]]:
    """Items of one page of get_contact_chats_page."""
    return get_contact_chats_page(jid, limit=limit, page=page).items


def _group_members_available(cur: sqlite3.Cursor) -> bool:
    """True when the bridge that wrote this store keeps a group_members table.

    The table is created by the bridge's own migration, so a messages.db
    written by an older bridge does not have it. Reads must keep working
    against such a store — group memberships are simply not reported until the
    bridge has run once.

    Deliberately not memoised through `_schema_memo`, unlike the FTS and
    last_read_time probes. That cache is keyed on the file's mtime and size,
    and the bridge writes in WAL mode: right after an upgrade the CREATE TABLE
    sits in messages.db-wal and the main file looks untouched, so a cached
    "False" would hide every membership until a checkpoint — hours on a quiet
    account. This is one indexed lookup in sqlite_master on a connection that
    is already open.
    """
    return bool(cur.execute("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'group_members'").fetchone())


def _membership_flag(spoke: bool, member: bool) -> str | None:
    """How the contact is attached to a chat: by talking, by belonging, or both."""
    if spoke and member:
        return "both"
    if member:
        return "member"
    if spoke:
        return "spoke"
    return None


def get_contact_chats_page(jid: str, limit: int = 20, page: int = 0, cursor: str | None = None) -> PageResult:
    """Every chat the contact is attached to: the ones they talk in and the groups they belong to.

    A join over `messages` alone only finds conversations where the contact has
    *spoken*, which silently omits the groups they are a member of but have
    never posted in (issue #288). The bridge caches each group's roster in
    `group_members`, so those groups are unioned in here and every row says
    which of the two it is: `membership` is "spoke", "member" or "both".
    `is_admin` and `roster_seen_at` come from a roster fetch and are null when
    the membership is only known from a join event or a message — those feeds
    do not know the admin flags and have not seen the whole group.

    Ordering puts conversations first (most recent message first), then the
    memberships (most recently confirmed membership first), so the answer to
    "where do I talk to this person" stays at the top of page one.

    A direct chat WhatsApp keeps under both the phone JID and the `@lid` of the
    same person is one row, reported under the phone spelling with both in
    `aliases` (issue #337).

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    _require_unambiguous_identifier(jid)
    # An empty (or bare "@lid") argument would otherwise reach the SQL as an
    # empty alias and match every row whose address form the bridge left empty.
    if not (jid or "").strip().split("@", 1)[0]:
        raise ToolError("invalid_argument", "a contact JID or phone number is required")
    limit = page_size(limit, CONTACT_CHATS_MAX_LIMIT)
    page = page_number(page)
    cursor_state = decode_cursor(cursor, "contact_chats")
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        # The candidate JIDs are mapped through the phone/LID pairs before they
        # are unioned, so a person who spoke under both spellings is one
        # conversation here too (issue #337).
        twins = _chat_twins(cur)
        twin_cte, twin_params = twins.cte()

        # rank 0 = a conversation (the contact has messages here, or it is
        # their own direct chat), 1 = membership only. `order_at` is the
        # timestamp each rank is sorted by. Both are spelled out again in the
        # keyset clause because SQLite does not accept result aliases in WHERE.
        talks = "(s.chat_jid IS NOT NULL OR d.jid IS NOT NULL)"
        rank = f"(NOT {talks})"
        order_at = f"CASE WHEN {talks} THEN c.last_message_time ELSE gm.member_seen_at END"

        offset = page * limit
        keyset_clause, keyset_params = "", []
        if cursor_state is not None:
            offset = 0
            # Cursors written before memberships existed carry no rank; those
            # pages were conversations only, which is rank 0.
            rank_at = cursor_state.get("rk") or 0
            if cursor_state.get("t") is not None:
                keyset_clause = (
                    f"AND ({rank} > ? OR ({rank} = ? AND ({order_at} < ? "
                    f"OR ({order_at} = ? AND k.jid > ?) OR {order_at} IS NULL)))"
                )
                keyset_params = [rank_at, rank_at, cursor_state["t"], cursor_state["t"], cursor_state["j"]]
            else:
                keyset_clause = f"AND ({rank} > ? OR ({rank} = ? AND {order_at} IS NULL AND k.jid > ?))"
                keyset_params = [rank_at, rank_at, cursor_state["j"]]
        policy_clause, policy_params = CHAT_POLICY.sql_clause("k.jid")

        aliases = _sender_aliases(jid)
        placeholders = ",".join("?" * len(aliases))
        # An older store has no group_members table; an empty CTE keeps one
        # query shape instead of branching the whole statement.
        #
        # The `!= ''` guards matter: the bridge writes an address form it does
        # not know as the empty string, never NULL, so an empty alias would
        # match every roster row in the store.
        #
        # is_admin and roster_seen_at are read from the roster rows only. A row
        # written by a join event or by someone's first message says nothing
        # about admin status and has not seen the whole group, so reporting
        # is_admin: false and a fresh roster_seen_at for one would be a lie an
        # agent then repeats. member_seen_at is the ordering key and does count
        # every feed — the membership is real either way.
        if _group_members_available(cur):
            member_of = f"""
                SELECT group_jid,
                       MAX(CASE WHEN source = 'roster' THEN is_admin END) AS is_admin,
                       MAX(CASE WHEN source = 'roster' THEN last_seen END) AS roster_seen_at,
                       MAX(last_seen) AS member_seen_at
                FROM group_members
                WHERE user IN ({placeholders})
                   OR (phone != '' AND phone IN ({placeholders}))
                   OR (lid != '' AND lid IN ({placeholders}))
                GROUP BY group_jid
            """
            member_params = [*aliases, *aliases, *aliases]
        else:
            member_of = (
                "SELECT NULL AS group_jid, NULL AS is_admin, NULL AS roster_seen_at, NULL AS member_seen_at WHERE 0"
            )
            member_params = []

        cur.execute(
            f"""
            WITH {twin_cte},
            spoke_in AS (
                SELECT DISTINCT COALESCE(tw.listed_jid, messages.chat_jid) AS chat_jid
                  FROM messages LEFT JOIN chat_twin tw ON tw.jid = messages.chat_jid
                 WHERE messages.sender IN ({placeholders})
            ),
            member_of AS ({member_of}),
            direct_chat AS (
                SELECT DISTINCT COALESCE(tw.listed_jid, chats.jid) AS jid
                  FROM chats LEFT JOIN chat_twin tw ON tw.jid = chats.jid
                 WHERE chats.jid IN ({placeholders})
            ),
            candidate AS (
                SELECT chat_jid AS jid FROM spoke_in
                UNION SELECT jid FROM direct_chat
                UNION SELECT group_jid AS jid FROM member_of
            )
            SELECT
                k.jid,
                c.name,
                c.last_message_time,
                last_msg.content as last_message,
                last_msg.sender as last_sender,
                last_msg.is_from_me as last_is_from_me,
                {_last_read_time_select(cur, "c")},
                last_msg.id IS NOT NULL as has_messages,
                s.chat_jid IS NOT NULL as spoke,
                gm.group_jid IS NOT NULL as member,
                gm.is_admin,
                gm.roster_seen_at,
                {rank} as sort_rank,
                {order_at} as sort_at
            FROM candidate k
            LEFT JOIN chats c ON c.jid = k.jid
            LEFT JOIN spoke_in s ON s.chat_jid = k.jid
            LEFT JOIN direct_chat d ON d.jid = k.jid
            LEFT JOIN member_of gm ON gm.group_jid = k.jid
            {_last_message_join("k", "last_msg")}
            WHERE {policy_clause} {keyset_clause}
            ORDER BY {rank} ASC, {order_at} DESC, k.jid ASC
            LIMIT ? OFFSET ?
        """,
            (*twin_params, *aliases, *member_params, *aliases, *policy_params, *keyset_params, limit + 1, offset),
        )

        chats = cur.fetchall()
        has_more = len(chats) > limit
        chats = chats[:limit]
        next_cursor = None
        if has_more and chats:
            last = chats[-1]
            next_cursor = encode_cursor({"k": "contact_chats", "rk": last[12], "t": last[13], "j": last[0]})

        page_chats = [
            Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
                last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
                has_messages=bool(chat_data[7]),
            )
            for chat_data in chats
        ]
        twins.merge(page_chats)
        _apply_name_fallback(page_chats)

        items = []
        for chat, chat_data in zip(page_chats, chats):
            items.append(
                {
                    **chat_to_dict(chat),
                    "membership": _membership_flag(bool(chat_data[8]), bool(chat_data[9])),
                    # Both stay null unless a roster fetch backed them: see the
                    # member_of CTE above.
                    "is_admin": bool(chat_data[10]) if chat_data[10] is not None else None,
                    "roster_seen_at": parse_db_time(chat_data[11]).isoformat() if chat_data[11] else None,
                }
            )
        return PageResult(items, next_cursor, has_more)

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
    _require_unambiguous_identifier(jid)
    try:
        policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()

        aliases = _sender_aliases(jid)
        placeholders = ",".join("?" * len(aliases))
        # The contact's own chat under every spelling a chat_jid filter would
        # bind, not only the one typed: what this account sent there has no
        # sender to match on. A group JID stays itself.
        chats = list(dict.fromkeys([jid, *_chat_jid_aliases(jid)]))
        cursor.execute(
            f"""
            SELECT {message_columns(cursor)}
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE (messages.sender IN ({placeholders}) OR {_chat_jid_clause("chats.jid", chats)})
              AND {policy_clause}
            ORDER BY messages.timestamp DESC
            LIMIT 1
        """,
            (*aliases, *chats, *policy_params),
        )

        msg_data = cursor.fetchone()

        if not msg_data:
            _require_readable(jid)
            return None

        return msg_to_dict(_row_to_message(msg_data))

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True, both_spellings: bool = True) -> dict[str, Any] | None:
    """Get chat metadata by JID.

    Either spelling of a merged phone/LID pair (issue #337) returns the same
    merged row, reported under the phone JID with both in `aliases`. A
    Brazilian mobile is also found under its other spelling, with or without
    the ninth digit (issue #475). Both stored phone spellings merge when allowed,
    using the shorter phone JID and up to one mapped LID (issue #479).
    `both_spellings=False` is the literal lookup and the literal allow-list
    check, for a caller on the way to a write.

    Returns:
        Chat dictionary or None if not found
    """
    try:
        if both_spellings:
            _require_readable(chat_jid)
        else:
            _require_allowed(chat_jid)
        policy_clause, policy_params = CHAT_POLICY.sql_clause("c.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()
        # A chat is asked for by JID here; a bare number is not one.
        alternate = other_phone_spelling(chat_jid) if both_spellings and "@" in chat_jid else None
        lookups = [chat_jid, alternate] if alternate else [chat_jid]
        twins = _chat_twins(cursor, only=lookups, phone_pairs=both_spellings)
        rows = [twins.listing_jid(jid) for jid in lookups]

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
                {_last_read_time_select(cursor, "c")},
                m.id IS NOT NULL as has_messages
            FROM chats c
            {_last_message_join("c", "m")}
            WHERE {_chat_jid_clause("c.jid", rows)} AND {policy_clause}
            ORDER BY CASE WHEN c.jid = ? THEN 0 ELSE 1 END
            LIMIT 1
        """

        cursor.execute(query, (*rows, *policy_params, rows[0]))
        chat_data = cursor.fetchone()

        if not chat_data:
            return None

        chat = Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
            has_messages=bool(chat_data[7]),
        )
        twins.merge([chat])
        _apply_name_fallback([chat], phone_aliases=both_spellings)
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
    user = raw.split("@", 1)[0]
    bare = phone_digits(user) or user.lstrip("+").replace(" ", "").replace("-", "")
    return raw, f"{bare}@s.whatsapp.net", f"{bare}@lid"


def get_direct_chat_by_contact(sender_phone_number: str) -> dict[str, Any] | None:
    """Get chat metadata by sender phone number (exact match on the number's JID forms).

    A merged phone/LID pair answers as one chat here too (issue #337): each
    candidate spelling is mapped to the row that lists for it, so the answer is
    the row carrying the conversation whichever of the two holds it.

    A Brazilian mobile is also looked up under its other spelling (issue #444):
    WhatsApp registered the account with or without the ninth digit and the
    chat is stored under that one. The number as given wins when both spellings
    have a chat the allow-list admits.

    A chat the store holds only under the contact's LID is found too (issue
    #465): the LID map gives the LID for the number, in either spelling. A
    phone row, under the spelling given or the other one, comes before any LID
    row.
    """
    _require_unambiguous_identifier(sender_phone_number)
    try:
        policy_clause, policy_params = CHAT_POLICY.sql_clause("c.jid")
        conn = _connect_messages_db()
        cursor = conn.cursor()
        spellings = _direct_chat_candidates(sender_phone_number)
        digits, alternate = _phone_spellings(sender_phone_number)
        # The LIDs the map pairs with the number, in either spelling: the row
        # may exist under one of those alone.
        mapped = [jid for jid in _lid_jids_of_number(digits, alternate) if jid not in spellings]
        lookups = [*spellings, *mapped, *([alternate] if alternate else [])]
        twins = _chat_twins(cursor, only=lookups)
        candidates = [twins.listing_jid(jid) for jid in lookups]
        alternate_row = twins.listing_jid(alternate) if alternate else ""

        cursor.execute(
            f"""
            SELECT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me,
                {_last_read_time_select(cursor, "c")},
                m.id IS NOT NULL as has_messages
            FROM chats c
            {_last_message_join("c", "m")}
            WHERE c.jid IN ({_placeholders(candidates)}) AND {policy_clause}
            ORDER BY CASE
                WHEN c.jid = ? THEN 0
                WHEN c.jid = ? THEN 2
                WHEN c.jid LIKE '%@s.whatsapp.net' THEN 1
                ELSE 3
            END
            LIMIT 1
        """,
            (*candidates, *policy_params, sender_phone_number, alternate_row),
        )

        chat_data = cursor.fetchone()

        if not chat_data:
            # "No chat" is an answer only about a number the list names. The
            # input can mean more than one JID (a bare number is tried as a
            # phone and as a LID), and it is refused only when the list names
            # none of them (issue #466): it used to be refused unless it named
            # them all, so an allow-listed number without a chat was "denied".
            # A number outside the list gets this same refusal whether or not
            # a chat exists for it: the query above never saw its row.
            typed, phone_jid, lid_jid = spellings
            server = typed.rpartition("@")[2].lower() if "@" in typed else ""
            if server:
                # A JID is read in the namespace it names.
                primary = next((jid for jid in (phone_jid, lid_jid) if jid.endswith(f"@{server}")), typed)
                readings = [primary]
            else:
                primary = lid_jid if _reads_as_lid(lid_jid.partition("@")[0]) else phone_jid
                readings = [phone_jid, lid_jid]
            if server in ("", DEFAULT_USER_SERVER):
                # The contact may be on the list as the LID the map pairs it with.
                readings += mapped
            if listed_reading(primary, readings) is None:
                raise ToolError("denied", CHAT_POLICY.denial_message(typed))
            return None

        chat = Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
            has_messages=bool(chat_data[7]),
        )
        twins.merge([chat])
        _apply_name_fallback([chat])
        return chat_to_dict(chat)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


# Codes the bridge may assert in its error body, because they say something no
# HTTP status can: media_unavailable means missing bytes or download fields;
# media_refused means an unsafe cache identity. Both download and forward name
# these permanent failures. Every other failure keeps the status map.
_BRIDGE_NAMED_CODES = frozenset({"media_unavailable", MEDIA_REFUSED_CODE})


def _bridge_error_code(status: int) -> str:
    """Map a bridge HTTP status to an error code."""
    if status in (400, 422):
        return "invalid_argument"
    if status == 403:
        return "denied"
    if status == 429:
        return "rate_limited"
    if status == 404:
        return "not_found"
    if status == 401:
        return "internal"  # our own token was rejected: configuration, not the caller's fault
    if status == 408 or status >= 500:
        return "bridge_unavailable"
    return "internal"


def _bridge_json(response) -> dict[str, Any]:
    """Decode a bridge response; raise ToolError for HTTP or application failures.

    The bridge answers 200 + {"success": true, ...} on success, 200 +
    {"success": false, "message"} or {"ok": false, "error"} for application
    failures, and 4xx/5xx (JSON or plain text) otherwise. A failure body may
    carry ``error: {"code", "message"}``; that code is honoured only for the
    handful of meanings finer than any status (``_BRIDGE_NAMED_CODES``, today
    ``media_unavailable`` / ``media_refused`` on download and forward). Everything else
    keeps the status map, so what the bridge already answers cannot change
    meaning because a handler picked a different word for it.
    """
    try:
        payload = response.json()
    except (json.JSONDecodeError, ValueError):
        payload = {}
    if not isinstance(payload, dict):
        payload = {}
    error_body = payload.get("error")
    if not isinstance(error_body, dict):
        error_body = {}
    message = (
        payload.get("message")
        or error_body.get("message")
        or (payload.get("error") if isinstance(payload.get("error"), str) else "")
        or (getattr(response, "text", "") or "").strip()[:300]
    )
    if response.status_code != 200:
        if response.status_code == 429:
            retry = payload.get("retry_after_s") or getattr(response, "headers", {}).get("Retry-After", 1)
            try:
                retry = max(1, int(retry))
            except (TypeError, ValueError):
                retry = 1
            raise ToolError(
                "rate_limited",
                f"Send limit reached; stop and report to the operator instead of retrying (retry after {retry}s)",
                retry_after_s=retry,
                limit=payload.get("limit"),
            )
        named = error_body.get("code")
        code = named if named in _BRIDGE_NAMED_CODES else _bridge_error_code(response.status_code)
        raise ToolError(code, message or f"bridge answered HTTP {response.status_code}")
    if "success" in payload and not payload.get("success"):
        raise ToolError("internal", message or "bridge reported failure")
    if "ok" in payload and not payload.get("ok"):
        raise ToolError("internal", message or "bridge reported failure")
    return payload


def _bridge_media_json(response, message_id: str, chat_jid: str) -> dict[str, Any]:
    """Remember per-row refusals for every manual or background media fetch."""
    import media_notes

    try:
        result = _bridge_json(response)
    except ToolError as exc:
        if exc.code == MEDIA_REFUSED_CODE:
            exc._media_refusal_recorded = False
            try:
                media_notes.record_media_refusal(message_id, chat_jid, exc.message)
                exc._media_refusal_recorded = True
            except (ToolError, sqlite3.Error) as note_error:
                logger.warning("could not record media refusal: %s", note_error)
        raise
    try:
        media_notes.forget_media_refusal(message_id, chat_jid)
    except (ToolError, sqlite3.Error) as note_error:
        logger.warning("could not clear media refusal after successful fetch: %s", note_error)
    return result


def _sent_info(result: dict[str, Any]) -> dict[str, Any]:
    """message_id / chat_jid / timestamp of a message the bridge just sent (when reported)."""
    return {k: result[k] for k in ("message_id", "chat_jid", "timestamp") if result.get(k)}


def _require_allowed(jid: str | None) -> None:
    if denied := _policy_denied(jid):
        raise ToolError("denied", denied)


def _require_unambiguous_identifier(identifier: str | None) -> None:
    validate_chat_target(identifier)


def _validate_chat_filter_entry(entry: str, argument: str) -> None:
    entry = entry.strip()
    if not entry:
        return
    if _JID_SEPARATORS.search(entry):
        raise ToolError(
            "invalid_argument",
            f"{argument} takes one JID or a list of JIDs, not several joined into one string: "
            f'pass {argument}=["a@s.whatsapp.net", "b@g.us"] (got {entry!r})',
        )
    _require_unambiguous_identifier(entry)


def _validate_filter_identifiers(
    chat_jid: str | Sequence[str] | None,
    exclude_chat_jid: str | Sequence[str] | None,
    sender: str | None = None,
) -> None:
    _require_unambiguous_identifier(sender)
    for argument, value in (("chat_jid", chat_jid), ("exclude_chat_jid", exclude_chat_jid)):
        if value is not None:
            for entry in [value] if isinstance(value, str) else value:
                _validate_chat_filter_entry(str(entry), argument)


def _require_readable(jid: str | None) -> None:
    """The allow-list check of every read that names a chat or a contact.

    A Brazilian mobile has two spellings (`phone.py`) and the list names one of
    them, so a read is refused only when it admits neither. That decides
    whether to answer at all: what comes back is still limited to the stored
    JIDs the list admits, by the SQL policy clause of each query. Writes keep
    `_require_allowed`: the bridge decides which number a send reaches.
    """
    validate_chat_target(jid)
    if not _readable(jid):
        raise ToolError("denied", CHAT_POLICY.denial_message(jid))


def _readable(jid: str | None) -> bool:
    """Does the allow-list name this chat, in either spelling of a Brazilian mobile?"""
    if CHAT_POLICY.allows(jid):
        return True
    alternate = other_phone_spelling(jid or "")
    return alternate is not None and CHAT_POLICY.allows(alternate)


def _named_exactly(jid: str) -> bool:
    """Does an entry of the allow-list name this JID itself, not a server wildcard?"""
    if jid.count("@") > 1:
        return False
    spellings = [jid, other_phone_spelling(jid)]
    return any(normalize_chat_entry(spelling) in CHAT_POLICY.exact for spelling in spellings if spelling)


def listed_reading(primary: str, readings: Sequence[str]) -> str | None:
    """Which JID to answer about for an identifier that has no chat the list admits, or None: refuse.

    The one rule get_direct_chat_by_contact and get_contact share for "allowed,
    no chat stored" against "not allowed" (issue #466). `primary` is what the
    identifier is taken to be; `readings` is everything it could be: the other
    namespace of a bare number, the LIDs the map pairs with a number.

    The list admits the primary reading the way it admits any read: by an entry
    or by a server wildcard, in either spelling of a Brazilian mobile. Another
    reading counts only when an entry names it exactly. A wildcard must not:
    `*@lid` would turn every phone number into "allowed, no chat", make a LID
    out of a phone number, and tell a number the LID map knows from one it does
    not. Without an allow-list the primary reading is always the answer.
    """
    if _readable(primary):
        return primary
    return next((jid for jid in readings if _named_exactly(jid)), None)


def _reads_as_lid(bare: str) -> bool:
    """Is a bare number a LID and not a phone number, as far as this store can tell?

    What get_contact asks, in the same order: the namespace the archive or the
    LID map gives it, then the 14-15 digit rule (#375). That last one presumes
    the chats table came up empty for the number, which is only known when the
    list lets its phone reading be looked up.
    """
    if not bare.isdigit():
        return False
    if sender_identity(bare, stored_sender_namespace(bare)).lid is not None:
        return True
    return _readable(f"{bare}@{DEFAULT_USER_SERVER}") and unknown_lid_digits(bare)


DRY_RUN_MESSAGE = "Dry run: nothing was sent. Show this to the user and call again with dry_run=false to send."


def _chat_name(jid: str) -> str | None:
    """Best-effort display name for a dry-run preview; never fails the preview.

    The chat stored under the recipient exactly as given: the bridge decides
    which number a send reaches, so the preview must not name another row.
    """
    try:
        chat = get_chat(jid, include_last_message=False, both_spellings=False)
    except ToolError:
        return None
    return (chat or {}).get("name")


def _dry_run(endpoint: str, payload: dict[str, Any], **extra: Any) -> dict[str, Any]:
    """The exact request a real call would post, without posting it.

    Everything a human reviewer needs to approve a send: the endpoint, the JSON
    body verbatim, and whatever the caller resolved on the way (recipient JID,
    chat name, media file check).
    """
    return {
        "success": True,
        "dry_run": True,
        "message": DRY_RUN_MESSAGE,
        "endpoint": endpoint,
        "payload": payload,
        **extra,
    }


def _recipient_preview(recipient: str) -> dict[str, Any]:
    """Resolved recipient for a preview: canonical JID plus the chat's name."""
    jid = normalize_chat_entry(recipient)
    return {"recipient_jid": jid, "recipient_name": _chat_name(jid)}


def send_message(
    recipient: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
    dry_run: bool = False,
) -> tuple[bool, str, dict[str, Any]]:
    """Send a text message. Returns (True, status, {message_id, chat_jid, timestamp}) or raises ToolError.

    ``dry_run=True`` validates and resolves everything, then returns the request
    that would have been posted instead of posting it.
    """
    recipient = normalize_recipient(recipient)
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
    if dry_run:
        return True, DRY_RUN_MESSAGE, _dry_run("POST /api/send", payload, **_recipient_preview(recipient))
    result = _bridge_json(_bridge_request("POST", "/send", json=payload))
    return True, result.get("message", "Message sent"), _sent_info(result)


def _media_source(media_path: str, media_base64: str, upload_id: str = "") -> str:
    """Exactly one server path, inline payload or HTTP upload."""
    if sum(bool(value) for value in (media_path, media_base64, upload_id)) != 1:
        raise ToolError("invalid_argument", "provide exactly one of media_path / media_base64 / upload_id")
    if upload_id:
        return "upload"
    return "inline" if media_base64 else "path"


def _inline_name(filename: str, default_name: str | None) -> str:
    if not filename and default_name is None:
        raise ToolError(
            "invalid_argument",
            "filename is required with media_base64: its extension decides how WhatsApp "
            "presents the file (report.pdf, photo.jpg, clip.mp4)",
        )
    return media_upload.safe_filename(filename, default_name or "file")


def send_file(
    recipient: str,
    media_path: str = "",
    caption: str = "",
    dry_run: bool = False,
    media_base64: str = "",
    filename: str = "",
    upload_id: str = "",
) -> tuple[bool, str, dict[str, Any]]:
    """Send a media file (image, video, document) with an optional caption.

    The bridge populates the WA media-message Caption field from `message`, so
    passing both in one /api/send call produces a single attachment-with-caption
    message instead of two separate messages.

    Exactly one of ``media_path``, ``media_base64`` or HTTP ``upload_id``.
    Inline bytes are written under the shared outbox and always removed;
    uploaded IDs are removed after success and retained on failure until expiry.

    ``dry_run=True`` runs the same validation (recipient, allow-list, the file
    exists or the payload decodes) and returns the request that would have
    been posted; an inline payload is not written to disk for a dry run.
    """
    recipient = normalize_recipient(recipient)
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    source = _media_source(media_path, media_base64, upload_id)
    if source == "upload" and filename:
        raise ToolError("invalid_argument", "filename is fixed at upload; omit filename when using upload_id")
    _require_allowed(recipient)
    if source == "upload":
        with media_upload.uploaded_path(upload_id, consume=not dry_run) as path:
            return send_file(recipient, path, caption, dry_run=dry_run)
    if source == "path":
        if not os.path.isfile(media_path):
            raise ToolError(
                "not_found",
                f"Media file not found on the server: {media_path}. "
                "For a file on another machine, POST /upload and use upload_id (http/sse), "
                "or provide media_base64 with filename.",
            )
        payload = {"recipient": recipient, "media_path": media_upload.bridge_media_path(media_path)}
        if caption:
            payload["message"] = caption
        if dry_run:
            # The bridge additionally confines media_path to WHATSAPP_MEDIA_ROOTS,
            # which only it knows; report what this side can check.
            media = {"path": os.path.abspath(media_path), "exists": True, "bytes": os.path.getsize(media_path)}
            return (
                True,
                DRY_RUN_MESSAGE,
                _dry_run("POST /api/send", payload, media=media, **_recipient_preview(recipient)),
            )
        result = _bridge_json(_bridge_request("POST", "/send", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
        return True, result.get("message", "File sent"), _sent_info(result)

    name = _inline_name(filename, default_name=None)
    data = media_upload.decode_inline(media_base64)
    if dry_run:
        upload_dir = media_upload.upload_dir()
        payload = {
            "recipient": recipient,
            "media_path": media_upload.bridge_media_path(os.path.join(upload_dir, "<upload>", name), preview=True),
        }
        if caption:
            payload["message"] = caption
        media = {
            "filename": name,
            "bytes": len(data),
            "mime": media_upload.guess_mime(name),
            "inline": True,
            "upload_dir": upload_dir,
        }
        return True, DRY_RUN_MESSAGE, _dry_run("POST /api/send", payload, media=media, **_recipient_preview(recipient))
    path = media_upload.write_inline(data, name)
    result: dict[str, Any] = {}
    try:
        payload = {"recipient": recipient, "media_path": media_upload.bridge_media_path(path)}
        if caption:
            payload["message"] = caption
        result = _bridge_json(_bridge_request("POST", "/send", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
    finally:
        media_upload.discard(path)
    return True, result.get("message", "File sent"), _sent_info(result)


def send_audio_message(
    recipient: str, media_path: str = "", media_base64: str = "", filename: str = "", upload_id: str = ""
) -> tuple[bool, str, dict[str, Any]]:
    """Send a voice note from exactly one of ``media_path``, ``media_base64``
    or HTTP ``upload_id``. Anything that is not an ``.ogg`` is
    converted with ffmpeg first. Both the inline upload and the converted
    file live under the outbox the bridge may read and are removed after the
    send; a caller's own ``media_path`` is never touched."""
    recipient = normalize_recipient(recipient)
    if not recipient:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    source = _media_source(media_path, media_base64, upload_id)
    if source == "upload" and filename:
        raise ToolError("invalid_argument", "filename is fixed at upload; omit filename when using upload_id")
    _require_allowed(recipient)
    if source == "upload":
        with media_upload.uploaded_path(upload_id) as path:
            return send_audio_message(recipient, path)
    cleanup: list[str] = []
    result: dict[str, Any] = {}
    try:
        if source == "inline":
            name = _inline_name(filename, default_name="voice.ogg")
            path = media_upload.write_inline(media_upload.decode_inline(media_base64), name)
            cleanup.append(path)
        else:
            path = media_path
            if not os.path.isfile(path):
                raise ToolError(
                    "not_found",
                    f"Media file not found on the server: {path}. "
                    "POST /upload and use upload_id (http/sse), or provide media_base64 with filename.",
                )
        if not path.lower().endswith(".ogg"):
            try:
                # Into the outbox, not the system temp directory: the bridge only
                # reads inside WHATSAPP_MEDIA_ROOTS.
                with media_upload.receiving_upload(limit=media_upload.MAX_OUTBOX_BYTES) as converted:
                    output = audio.convert_to_opus_ogg_temp(
                        path, directory=converted.folder, write_chunk=converted.write
                    )
                    converted.finish("voice.ogg", source=output)
                    path = os.path.join(converted.folder, "voice.ogg")
            except ToolError:
                raise
            except Exception as e:
                raise ToolError("internal", f"Error converting file to opus ogg (is ffmpeg installed?): {e}") from e
            cleanup.append(path)
        payload = {"recipient": recipient, "media_path": media_upload.bridge_media_path(path)}
        result = _bridge_json(_bridge_request("POST", "/send", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S))
    finally:
        for stale in cleanup:
            media_upload.discard(stale)
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


GROUP_MEMBERS_MAX_LIMIT = 500


def _group_member_rank(member: dict[str, Any]) -> tuple[int, str]:
    """Stable sort key: super admins, then admins, then everyone else, by JID."""
    if member.get("is_super_admin"):
        rank = 0
    elif member.get("is_admin"):
        rank = 1
    else:
        rank = 2
    return (rank, str(member.get("jid") or ""))


def get_group_members(group_jid: str, limit: int = 100, page: int = 0, cursor: str | None = None) -> dict[str, Any]:
    """One page of a group's participants via the bridge (live query to WhatsApp).

    The bridge answers with the whole membership, so paging happens here: the
    list is sorted (admins first, then JID) before slicing, which keeps pages
    stable across calls even though each call re-queries WhatsApp.

    Returns {"success": true, ...} with group metadata, "participant_count" and
    the PageResult keys (items, next_cursor, has_more); items are
    {jid, phone_number, lid, name, display, is_admin, is_super_admin}.
    Raises ToolError.
    """
    group_jid = _group_jid(group_jid)
    limit = page_size(limit, GROUP_MEMBERS_MAX_LIMIT)
    cursor_state = decode_cursor(cursor, "group_members")
    offset = page_number(page) * limit
    if cursor_state is not None:
        if cursor_state.get("g") != group_jid:
            raise ToolError(
                "invalid_argument", "cursor belongs to a different group; pass next_cursor from the same chat_jid"
            )
        offset = max(0, int(cursor_state.get("o", 0)))

    payload = _bridge_json(_bridge_request("GET", "/group/members", params={"jid": group_jid}))
    members = payload.pop("members", None) or []
    for member in members:
        member["display"] = member.get("name") or member.get("phone_number") or member.get("jid")
    members.sort(key=_group_member_rank)

    items = members[offset : offset + limit]
    has_more = offset + len(items) < len(members)
    next_cursor = encode_cursor({"k": "group_members", "g": group_jid, "o": offset + len(items)}) if has_more else None
    payload["participant_count"] = len(members)
    payload.update(PageResult(items, next_cursor, has_more).to_dict())
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


PURGE_MEDIA_TYPES = MEDIA_TYPES


def purge_media(
    items: list[dict[str, str]] | None = None,
    chat_jid: str = "",
    older_than_days: int = 0,
    min_bytes: int = 0,
    media_type: str = "",
    dry_run: bool = True,
    summary_only: bool = False,
    cursor: str | None = None,
) -> dict[str, Any]:
    """Ask the bridge to drop cached media bytes (rows untouched); dry run unless told otherwise.

    `summary_only` leaves the per-file `items` list out of the result: a criteria
    call over hundreds of files otherwise returns tens of kilobytes the caller
    does not read.
    """
    chat_jid = (chat_jid or "").strip()
    media_type = (media_type or "").strip()
    normalized: list[dict[str, str]] = []
    if len(items or []) > 1000:
        raise ToolError("invalid_argument", "items must contain at most 1000 entries; use criteria for bulk purges")
    if cursor and items:
        raise ToolError("invalid_argument", "cursor belongs to criteria calls, not items")
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
        if cursor:
            body["cursor"] = cursor
        if chat_jid:
            body["chat_jid"] = chat_jid
        if older_than_days:
            body["older_than_days"] = int(older_than_days)
        if min_bytes:
            body["min_bytes"] = int(min_bytes)
        if media_type:
            body["media_type"] = media_type
    payload = _bridge_json(_bridge_request("POST", "/media/purge", json=body))
    if not dry_run:
        _forget_media_listing(None)  # any chat may have lost files
    result: dict[str, Any] = {
        "success": True,
        "dry_run": bool(payload.get("dry_run", dry_run)),
        "message": payload.get("message") or "",
        "matched": int(payload.get("matched") or 0),
        "purged_files": int(payload.get("purged_files") or 0),
        "purged_bytes": int(payload.get("purged_bytes") or 0),
        "truncated": bool(payload.get("truncated", False)),
        # Criteria form: cached files left for the next identical call, whether
        # the bridge's scan stopped early, and rows it cannot reach. A bridge
        # from before these fields answers without them: 0 / false.
        "remaining": int(payload.get("remaining") or 0),
        "scan_truncated": bool(payload.get("scan_truncated", False)),
        "unreachable": int(payload.get("unreachable") or 0),
        "failed": int(payload.get("failed") or 0),
        "next_cursor": payload.get("next_cursor") or None,
        "examined": int(payload.get("examined") or 0),
    }
    if not summary_only:
        result["items"] = payload.get("items") or []
    return result


def _read_receipt_targets(chat_jid: str, message_ids: list[str] | None) -> list[tuple[str, list[str] | None]]:
    """The (chat row, message ids) one read receipt call has to become.

    One target for a chat WhatsApp knows one way, which is nearly all of them.
    A merged phone/LID row lists the unread messages of *both* spellings (issue
    #366) while the bridge validates every id against a single `chats` row, so
    the ids are routed back to the row that stores them and the whole-chat form
    is sent to both. No allow-list hole: a pair only merges when the policy
    admits both spellings, so every target here was reachable already.

    An id neither row holds stays with the chat the caller named, and the bridge
    answers about it as it always did; a database that cannot be read leaves the
    call exactly as it was before this routing existed.
    """
    conn = None
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        twins = _chat_twins(cur, only=[chat_jid])
        # The caller holds the merged row's JID, which is the phone spelling —
        # not always the row that lists for the pair.
        twin = twins.merged_row(twins.listing_jid(chat_jid))
        if twin is None:
            return [(chat_jid, message_ids)]
        spellings = list(twin.aliases)
        if message_ids is None:
            return [(jid, None) for jid in spellings]
        owner: dict[str, str] = {}
        for chunk in _in_chunks(message_ids):
            owner.update(
                cur.execute(
                    f"SELECT id, chat_jid FROM messages WHERE id IN ({_placeholders(chunk)})"
                    f" AND chat_jid IN ({_placeholders(spellings)})",
                    (*chunk, *spellings),
                ).fetchall()
            )
        by_chat: dict[str, list[str]] = {}
        for message_id in message_ids:
            by_chat.setdefault(owner.get(message_id, chat_jid), []).append(message_id)
        return list(by_chat.items())
    except sqlite3.Error as e:
        logger.warning("read receipt: could not resolve the chat rows of %s: %s", chat_jid, e)
        return [(chat_jid, message_ids)]
    finally:
        if conn is not None:
            conn.close()


def mark_messages_read(
    message_ids: list[str] | None,
    chat_jid: str,
    sender_jid: str = "",
    timestamp: str | None = None,
    up_to: str | None = None,
) -> dict[str, Any]:
    """Send read receipts through the WhatsApp bridge.

    message_ids=None marks the whole chat read up to `up_to` (default: now);
    the bridge groups the pending messages by sender itself. An empty list is
    rejected instead: a caller that computed zero IDs did not ask for the whole
    chat. Returns the bridge's counts (messages, senders, batches, truncated).

    A merged phone/LID chat is two `chats` rows behind one JID, so it becomes one
    call per row (`_read_receipt_targets`) and the counts are summed — otherwise
    the ids `list_unread` returned under the merged row would be refused by the
    bridge as "not in that chat".
    """
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(chat_jid)

    payload: dict[str, Any] = {}
    if message_ids is None:
        if sender_jid:
            raise ToolError(
                "invalid_argument",
                "sender_jid is only used together with message_ids; without them the bridge groups by sender itself",
            )
        if up_to:
            payload["up_to"] = up_to
    else:
        normalized_ids = [message_id.strip() for message_id in message_ids]
        if not normalized_ids or any(not message_id for message_id in normalized_ids):
            raise ToolError(
                "invalid_argument",
                "At least one non-empty message ID must be provided (omit message_ids to mark the whole chat read)",
            )
        if up_to:
            raise ToolError("invalid_argument", "up_to_timestamp is only used without message_ids")
        if chat_jid.endswith("@g.us") and not sender_jid:
            raise ToolError("invalid_argument", "sender_jid must be provided for group read receipts")
        message_ids = normalized_ids
        if sender_jid:
            payload["sender_jid"] = sender_jid
    if timestamp:
        payload["timestamp"] = timestamp

    counts = {"messages": 0, "senders": 0, "batches": 0}
    truncated = False
    messages: list[str] = []
    for target_jid, ids in _read_receipt_targets(chat_jid, message_ids):
        body = {**payload, "chat_jid": target_jid}
        if ids is not None:
            body["message_ids"] = ids
        result = _bridge_json(_bridge_request("POST", "/mark-read", json=body))
        for key in counts:
            counts[key] += int(result.get(key) or 0)
        truncated = truncated or bool(result.get("truncated", False))
        message = result.get("message") or ""
        if message and message not in messages:
            messages.append(message)
    return {"success": True, "message": "; ".join(messages) or "Marked as read", **counts, "truncated": truncated}


HISTORY_DEFAULT_COUNT = 50
HISTORY_MAX_COUNT = 500  # maxHistoryCount in whatsapp-bridge/history_ondemand.go


def request_history(chat_jid: str, count: int = HISTORY_DEFAULT_COUNT) -> dict[str, Any]:
    """Ask the phone for messages older than the oldest one stored for a chat.

    Wraps POST /api/history. The bridge anchors the request on the oldest stored
    message, so the chat must already have one (404 otherwise), and needs a live
    WhatsApp connection (503 otherwise); both surface as a ToolError.
    """
    chat_jid = (chat_jid or "").strip()
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(chat_jid)
    try:
        count = int(count)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"count must be an integer, got {count!r}") from exc
    if count < 1:
        raise ToolError("invalid_argument", "count must be at least 1")
    count = min(count, HISTORY_MAX_COUNT)
    payload = _bridge_json(_bridge_request("POST", "/history", json={"chat_jid": chat_jid, "count": count}))
    return {
        "success": True,
        "chat_jid": chat_jid,
        "requested_count": count,
        "message": payload.get("message") or f"Requested up to {count} older messages for {chat_jid}",
        "note": (
            "The phone answers asynchronously and decides how much it returns; nothing is stored yet when "
            "this call succeeds. Verify with list_messages(chat_jid=..., sort_by='oldest', limit=1) and "
            "compare the timestamp of that first message before and after."
        ),
    }


def _forget_media_listing(chat_jid: str | None) -> None:
    """Drop the memoised directory listing of a chat this process just changed.

    ``media_inventory`` keeps the names of a chat's cached files for a few
    seconds and lets the directory's mtime invalidate them, which covers files
    that arrive on their own. A file this process asked for is different: on a
    store whose timestamps cannot separate the write from the read, the next
    listing would keep saying the bytes are not here (issue #318). Imported
    inside the function because media_inventory reads this module.
    """
    import media_inventory

    media_inventory.forget_cached_names(chat_jid)


def download_media(message_id: str, chat_jid: str) -> str | None:
    """Download media from a message and return the local file path.

    Raises ToolError when the chat is denied, the bridge is unreachable or the
    bridge reports a failure; returns None only when the bridge answered
    success without a path.
    """
    _require_allowed(chat_jid)
    payload = {"message_id": message_id, "chat_jid": chat_jid}
    result = _bridge_media_json(
        _bridge_request("POST", "/download", json=payload, timeout=BRIDGE_MEDIA_TIMEOUT_S), message_id, chat_jid
    )
    path = result.get("path")
    if path:
        logger.info("Media downloaded successfully: %s", path)
    _forget_media_listing(chat_jid)
    return path


COVERAGE_DEFAULT_GAP_HOURS = 24.0
COVERAGE_DEFAULT_MAX_GAPS = 20
COVERAGE_BY_CHAT_DEFAULT_LIMIT = 50
COVERAGE_BY_CHAT_MAX_LIMIT = 200

# Gap detection subtracts one row's timestamp from the next, never compares
# against a bound, so it does not need the index and can afford substr(): the
# 19-character cut is what julianday() understands in every spelling, including
# Go's "… -0300 -03" in a store the bridge has not migrated yet. The offset is
# the same on every row of one archive, so it cancels out of the difference.
_COVERAGE_TS = "julianday(substr({col}, 1, 19))"


def _coverage_scope(
    after: str | None, before: str | None, chat_jid: str | Sequence[str] | None
) -> tuple[dict[str, Any], list[str], list[Any]]:
    """The narrowing arguments as (echo, window clauses, window params).

    The window predicates are kept apart from the chat filter because the
    per-chat query needs them inside its LEFT JOIN: moved into the WHERE they
    would drop the very rows that queue exists to list, the chats holding
    nothing in the window.
    """
    after_bound = _parse_filter_date(after, "after") if after else None
    before_bound = _parse_filter_date(before, "before") if before else None
    window: list[str] = []
    params: list[Any] = []
    if after_bound is not None:
        window.append("messages.timestamp > ?")
        params.append(after_bound)
    if before_bound is not None:
        window.append("messages.timestamp < ?")
        params.append(before_bound)
    scope = {"after": after_bound, "before": before_bound, "chat_jid": chat_jid_filter(chat_jid) or None}
    return scope, window, params


def _coverage_fingerprint(scope: dict[str, Any]) -> str:
    """Digest of the scope, carried in the by_chat cursor.

    Paging is by offset over one ORDER BY, so resuming against a different
    window or chat filter would silently skip or repeat chats; the cursor
    carries this instead and the mismatch is refused. The JIDs are sorted first:
    the same set named in another order is the same scope, not a new one.
    """
    jids = scope.get("chat_jid")
    canonical = {**scope, "chat_jid": sorted(jids) if jids else None}
    raw = json.dumps(canonical, separators=(",", ":"), sort_keys=True).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()[:16]


def _coverage_by_chat(
    cur: sqlite3.Cursor,
    scope: dict[str, Any],
    window: list[str],
    window_params: list[Any],
    chat_where: str,
    chat_params: list[Any],
    cursor: str | None,
    limit: int,
    twins: ChatTwins = NO_CHAT_TWINS,
) -> dict[str, Any]:
    """One page of the per-chat work queue for request_history.

    A phone/LID pair is one entry in the queue, and its numbers count both rows
    (issue #337): the twin the listing row absorbed is already out of
    `chat_where`, so the message predicates take it in explicitly.
    """
    fingerprint = _coverage_fingerprint(scope)
    state = decode_cursor(cursor, "coverage_by_chat")
    offset = 0
    if state is not None:
        if state.get("s") != fingerprint:
            raise ToolError(
                "invalid_argument",
                "cursor was created for a different window or chat_jid; start again without a cursor",
            )
        # A cursor is opaque but caller-supplied: anything that is not a
        # non-negative integer offset is a mangled cursor, not a server fault.
        raw_offset = state.get("o")
        if not isinstance(raw_offset, int) or isinstance(raw_offset, bool) or raw_offset < 0:
            raise ToolError("invalid_argument", "cursor is not valid; pass next_cursor from the previous page")
        offset = raw_offset

    cur.execute(f"SELECT COUNT(*) FROM chats WHERE {chat_where}", tuple(chat_params))
    chats_total = int(cur.fetchone()[0] or 0)

    # The queue ranks on what each chat is missing *overall*, never on the
    # window: request_history backfills whole histories, and inside a window a
    # fully synced chat with one quiet week is indistinguishable from one that
    # never synced. So `depth` (0 = nothing stored, 1 = a lone history-sync
    # stub, 2 = a real history) and the tie-break both read the chat's own rows,
    # while the reported counts and boundaries stay scoped to the window.
    #
    # The inner LIMIT 2 is what keeps `depth` cheap: it answers "none, one or
    # more" with at most two rows of the (chat_jid, timestamp) index per chat
    # instead of counting the chat's whole history.
    prefix, twin_join, of_this_chat, twin_params = twins.both_rows()
    depth = f"""(SELECT COUNT(*) FROM (
                    SELECT 1 FROM messages AS probe WHERE probe.chat_jid {of_this_chat} LIMIT 2))"""
    overall_first = f"(SELECT MIN(timestamp) FROM messages AS whole WHERE whole.chat_jid {of_this_chat})"
    join_on = " AND ".join([f"messages.chat_jid {of_this_chat}", *window])
    cur.execute(
        f"""{prefix}SELECT chats.jid, chats.name,
                   MIN(messages.timestamp) AS first_time,
                   MAX(messages.timestamp) AS last_time,
                   COUNT(messages.id) AS stored,
                   {depth} AS depth,
                   {overall_first} AS overall_first
              FROM chats {twin_join} LEFT JOIN messages ON {join_on}
             WHERE {chat_where}
             GROUP BY chats.jid, chats.name
             ORDER BY depth, overall_first DESC, chats.jid
             LIMIT ? OFFSET ?""",
        (*twin_params, *window_params, *chat_params, limit + 1, offset),
    )
    rows = cur.fetchall()
    has_more = len(rows) > limit
    rows = rows[:limit]

    merged = {jid: twins.merged_row(jid) for jid, *_ in rows}
    placeholders = [
        (twin.jid if (twin := merged[jid]) else jid) for jid, name, *_ in rows if _is_placeholder_name(name)
    ]
    names = _contact_names(placeholders) if placeholders else {}
    items = []
    for jid, name, first_time, last_time, stored, depth_of, _overall_first in rows:
        twin = merged[jid]
        listed_jid = twin.jid if twin else jid
        if twin is not None and _is_placeholder_name(name) and not _is_placeholder_name(twin.name):
            name = twin.name
        shown = names.get(listed_jid, name) if _is_placeholder_name(name) else name
        item = {
            "chat_jid": listed_jid,
            # The status feed is a chat to backfill like any other, under the
            # name the chat listings give it (#379).
            "name": chat_display_name(listed_jid, shown),
            "first_message_time": first_time,
            "last_message_time": last_time,
            "messages": int(stored or 0),
            "stub_only": depth_of == 1,
        }
        if twin is not None:
            item["aliases"] = list(twin.aliases)
        items.append(item)
    next_cursor = (
        encode_cursor({"k": "coverage_by_chat", "o": offset + len(rows), "s": fingerprint}) if has_more else None
    )
    return {
        "by_chat": True,
        **PageResult(items, next_cursor, has_more).to_dict(),
        "chats_total": chats_total,
        "scope": scope,
        "allow_list_applied": CHAT_POLICY.restricted,
        "hint": (
            "Ordered as a work queue: chats with nothing stored first, then stub_only chats (their whole stored "
            "history is one row, usually the history-sync stub the phone pushed at pair time), then the chats "
            "whose stored history starts latest. Backfill one with request_history(chat_jid); it needs a stored "
            "message to anchor on, so a chat with messages: 0 has to receive one first. With after/before set, "
            "first/last_message_time and messages describe the window; stub_only and the order still describe "
            "each chat's whole history, because a window cannot tell a quiet week from a chat that never synced."
        ),
    }


# Ceilings on the cached-file count of the audio block (issue #334). The counts
# themselves are SQL, but "is this one on disk" is the filesystem: one directory
# read per chat that has audio in scope, and no stat per row — a stat per file a
# chat ever received is exactly the cost issue #318 removed from the media
# tools. An archive-wide call therefore reads at most COVERAGE_AUDIO_MAX_CHATS
# directories, over the newest COVERAGE_AUDIO_MAX_ROWS audio rows. When a
# ceiling cuts the work, `cached_examined` is below `messages` and the two
# cached counts are floors over the rows that were examined — the untranscribed
# ones first, so `backlog_cached` is the last number to lose accuracy; every
# other number stays exact. Narrowing with chat_jid or after/before is how a
# caller on a huge archive gets an exact count — and pays for fewer directories.
COVERAGE_AUDIO_MAX_ROWS = 20_000
COVERAGE_AUDIO_MAX_CHATS = 200

# Inbound, undeleted voice notes that carry a content hash: exactly what a
# transcription batch looks at (transcribe_worker._pending_rows). A row without
# a hash cannot be keyed to a transcript note, so the worker can never drain it
# and counting it would leave a backlog that never reaches zero. The status feed
# is out for the same reason: the worker does not walk it (issue #447).
_COVERAGE_AUDIO_WHERE = (
    "messages.media_type = 'audio' AND messages.is_from_me = 0 "
    "AND messages.deleted_at IS NULL AND messages.file_sha256 IS NOT NULL "
    f"AND messages.chat_jid <> '{STATUS_BROADCAST_JID}'"
)


def _coverage_audio(cur: sqlite3.Cursor, msg_clause: str, msg_params: Sequence[Any]) -> dict[str, int]:
    """The voice-note transcription backlog inside the same scope as the aggregates.

    `messages` is every inbound voice note in scope; `transcribed`, `errors` and
    `unavailable` the ones whose content hash already carries a `transcript` /
    `transcript_error` / `media_unavailable` note — notes.db is attached for the
    query instead of pulling every transcribed hash into the statement as
    parameters, the way transcribe_worker reads it. No notes.db, or no table in
    it yet, means nothing was transcribed. `unavailable` is audio no download
    brought here and none is expected to: the sender's phone answered that it no
    longer has it (issue #378), or the row was stored without the CDN fields a
    download needs (issue #392). Either way it leaves the backlog instead of
    being asked for on every pass.

    `refused` counts unsafe row identities recorded in notes.db per message,
    using the same predicate as the ingest worker; other copies of that hash
    remain in the backlog until handled independently.

    Two cached counts, because they answer different questions: `cached` is how
    much of the audio in scope is on disk at all, and `backlog_cached` how much
    of the *backlog* is, so `backlog - backlog_cached` is what a batch would
    have to download first. Counting only the first would be misleading in the
    deployment this is for — with a retention sweep on, most cached files are
    recent and already transcribed, while the backlog is old and gone from disk.
    """
    import media_inventory  # local: media_inventory reads this module
    import media_notes

    clause = f"{msg_clause} AND {_COVERAGE_AUDIO_WHERE}"
    from runtime_settings import ingest_chat_clause

    clause += f" AND {ingest_chat_clause('messages.chat_jid')}"
    params = tuple(msg_params)

    note_params: tuple[Any, ...] = ()
    transcribed_expr = error_expr = unavailable_expr = refused_expr = "0"
    notes_path = media_notes.notes_db_path()
    if os.path.exists(notes_path):
        attach_notes_read_only(cur.connection, notes_path)
        refused_expr = f"NOT ({media_notes.media_refusal_clause(cur.connection, 'messages')})"
        if cur.execute("SELECT 1 FROM notesdb.sqlite_master WHERE type = 'table' AND name = 'media_notes'").fetchone():
            note_exists = (
                "EXISTS (SELECT 1 FROM notesdb.media_notes n "
                "WHERE n.sha256 = lower(hex(messages.file_sha256)) AND n.key = ?)"
            )
            transcribed_expr = error_expr = unavailable_expr = note_exists
            note_params = (
                media_notes.TRANSCRIPT_KEY,
                media_notes.TRANSCRIPT_ERROR_KEY,
                media_notes.MEDIA_UNAVAILABLE_KEY,
            )

    # `handled` is the predicate the ingest worker skips on: any of the three
    # notes, counted once. They overlap — a hash the worker failed on and an
    # agent then transcribed by hand carries two — so subtracting them
    # separately would take that row off the backlog twice.
    handled_expr = "0" if not note_params else f"({transcribed_expr} OR {error_expr} OR {unavailable_expr})"
    handled_expr = f"({handled_expr} OR {refused_expr})"
    cur.execute(
        f"""SELECT COUNT(*),
                   COALESCE(SUM({transcribed_expr}), 0),
                   COALESCE(SUM({error_expr}), 0),
                   COALESCE(SUM({unavailable_expr}), 0),
                   COALESCE(SUM({refused_expr}), 0),
                   COALESCE(SUM({handled_expr}), 0)
              FROM messages WHERE {clause}""",
        # Each EXISTS probe carries its own key placeholder, in the order the
        # expressions appear above (transcript, error, unavailable, then all
        # three again for `handled`), then the scope.
        (*note_params, *note_params, *params),
    )
    total, transcribed, errors, unavailable, refused, handled_total = (int(value or 0) for value in cur.fetchone())

    # Untranscribed rows first, then newest: when COVERAGE_AUDIO_MAX_ROWS bites,
    # the budget is spent on the rows the answer is about. The worker transcribes
    # newest-first, so ordering by time alone would spend a whole ceiling on
    # already-transcribed rows and report backlog_cached: 0 on an archive whose
    # entire backlog is on disk. `handled` is computed once in the subquery, so
    # the ordering costs no extra probe.
    cur.execute(
        f"""SELECT chat_jid, id, handled FROM (
                SELECT chat_jid, id, timestamp, {handled_expr} AS handled
                  FROM messages WHERE {clause}
            ) ORDER BY handled, timestamp DESC LIMIT ?""",
        (*note_params, *params, COVERAGE_AUDIO_MAX_ROWS),
    )
    by_chat: dict[str, list[tuple[str, bool]]] = {}
    for chat_jid, message_id, handled in cur.fetchall():
        by_chat.setdefault(str(chat_jid), []).append((str(message_id), bool(handled)))
    if len(by_chat) > COVERAGE_AUDIO_MAX_CHATS:
        # The busiest chats first: they are where a backlog actually lives.
        busiest = sorted(by_chat.items(), key=lambda item: len(item[1]), reverse=True)
        by_chat = dict(busiest[:COVERAGE_AUDIO_MAX_CHATS])

    # The memoised listing while it can hold every chat this call reads: a store
    # with a handful of voice-note chats then costs nothing when an agent polls
    # coverage(). Above that bound the memo would only evict what list_media is
    # paging through, so the plain read is used instead.
    list_names = (
        media_inventory.cached_names
        if len(by_chat) <= media_inventory.CACHE_MAX_CHATS
        else media_inventory.list_chat_names
    )
    cached = backlog_cached = 0
    for chat_jid, rows in by_chat.items():
        names = list_names(chat_jid)
        for message_id, handled in rows:
            if message_id not in names:
                continue
            cached += 1
            if not handled:
                backlog_cached += 1

    return {
        "messages": total,
        "cached": cached,
        "cached_examined": sum(len(rows) for rows in by_chat.values()),
        "transcribed": transcribed,
        "errors": errors,
        "unavailable": unavailable,
        "refused": refused,
        "backlog": total - handled_total,
        "backlog_cached": backlog_cached,
    }


def coverage(
    gap_hours: float = COVERAGE_DEFAULT_GAP_HOURS,
    max_gaps: int = COVERAGE_DEFAULT_MAX_GAPS,
    after: str | None = None,
    before: str | None = None,
    chat_jid: str | Sequence[str] | None = None,
    by_chat: bool = False,
    cursor: str | None = None,
    limit: int = COVERAGE_BY_CHAT_DEFAULT_LIMIT,
) -> dict[str, Any]:
    """What the local archive actually contains, and where it is missing.

    `after`/`before`/`chat_jid` narrow every number, the gap scan included, so
    the answer is about one period or one conversation instead of the whole
    archive. `by_chat` swaps the aggregates for the paginated per-chat queue.

    Reads messages.db only (works with the bridge down). Aggregates and the gap
    scan run in SQL, so nothing proportional to the archive is held in memory.
    Honours WHATSAPP_ALLOWED_CHATS: with an allow-list set, every number
    describes the allowed chats only. A contact stored under both a phone JID
    and a `@lid` counts as one chat, with the messages of both (issue #337).
    """
    try:
        gap_hours = float(gap_hours)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"gap_hours must be a number, got {gap_hours!r}") from exc
    if gap_hours <= 0:
        raise ToolError("invalid_argument", "gap_hours must be greater than 0")
    try:
        max_gaps = int(max_gaps)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"max_gaps must be an integer, got {max_gaps!r}") from exc
    max_gaps = max(1, min(max_gaps, 500))
    # Validated even when by_chat is off, so a typo is named rather than
    # silently dropped along with the argument it belongs to.
    limit = page_size(limit, COVERAGE_BY_CHAT_MAX_LIMIT)
    if cursor and not by_chat:
        raise ToolError("invalid_argument", "cursor only pages the by_chat=True result; pass by_chat=True with it")

    scope, window, window_params = _coverage_scope(after, before, chat_jid)
    msg_clauses, msg_params = list(window), list(window_params)
    chat_clauses: list[str] = []
    chat_params: list[Any] = []
    if jids := scope["chat_jid"]:
        msg_clauses.append(_chat_jid_clause("messages.chat_jid", jids))
        msg_params.extend(jids)
        chat_clauses.append(_chat_jid_clause("chats.jid", jids))
        chat_params.extend(jids)
    policy_clause, policy_params = CHAT_POLICY.sql_clause("messages.chat_jid")
    msg_clauses.append(policy_clause)
    msg_params.extend(policy_params)
    policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    chat_clauses.append(policy_clause)
    chat_params.extend(policy_params)
    msg_clause = " AND ".join(msg_clauses)
    chat_clause = " AND ".join(chat_clauses)
    # The window lives on the messages side, so it narrows the EXISTS probe
    # rather than the chats it runs over.
    window_where = "".join(f" AND {clause}" for clause in window)

    conn = None
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        # A phone/LID pair counts once here too: the absorbed row leaves the
        # chat side of every query, and its messages stay on the message side,
        # where they belong to the same conversation (issue #337).
        twins = _chat_twins(cur)
        if twins.active:
            hidden_clause, hidden_params = twins.hidden_clause("chats.jid")
            chat_clause = f"{chat_clause} AND {hidden_clause}"
            chat_params = [*chat_params, *hidden_params]

        if by_chat:
            return _coverage_by_chat(
                cur,
                scope,
                window,
                window_params,
                chat_clause,
                chat_params,
                cursor=cursor,
                limit=limit,
                twins=twins,
            )

        cur.execute(
            f"SELECT COUNT(*), MIN(timestamp), MAX(timestamp) FROM messages WHERE {msg_clause}",
            tuple(msg_params),
        )
        total_messages, first_time, last_time = cur.fetchone()

        cur.execute(f"SELECT COUNT(*) FROM chats WHERE {chat_clause}", tuple(chat_params))
        chats_total = int(cur.fetchone()[0] or 0)
        # The probe reads both rows of a merged pair: the listing row holds the
        # newest message, but a window can name a period only the other one has.
        prefix, twin_join, of_this_chat, twin_params = twins.both_rows()
        cur.execute(
            f"""{prefix}SELECT COUNT(*) FROM chats {twin_join}
                 WHERE {chat_clause}
                   AND NOT EXISTS (
                       SELECT 1 FROM messages WHERE messages.chat_jid {of_this_chat}{window_where})""",
            (*twin_params, *chat_params, *window_params),
        )
        chats_without_messages = int(cur.fetchone()[0] or 0)

        cur.execute(
            f"""SELECT substr(timestamp, 1, 7) AS month, COUNT(*)
                  FROM messages WHERE {msg_clause}
                 GROUP BY month ORDER BY month""",
            tuple(msg_params),
        )
        messages_by_month = {month: int(count) for month, count in cur.fetchall() if month}

        audio = _coverage_audio(cur, msg_clause, msg_params)

        # One ordered pass over the timestamp index: LAG gives every pair of
        # consecutive points, and only the pairs further apart than the
        # threshold ever reach Python.
        #
        # The bounds join the messages as points, or a window would only ever
        # see the holes *between* two stored messages: asking about a period
        # that begins three weeks before the first message it holds — or that
        # falls entirely inside an outage, with no message to pair up at all —
        # would answer "no gaps" about a period with nothing in it. Each bound
        # is written in the stored spelling, so it sorts and subtracts like a
        # row; msg_clause is strict on both ends, so neither can collide with a
        # real message.
        edges = []
        edge_params: list[Any] = []
        for bound in (scope["after"], scope["before"]):
            if bound is not None:
                edges.append("UNION ALL SELECT ?")
                edge_params.append(bound)
        delta_hours = f"({_COVERAGE_TS.format(col='ts')} - {_COVERAGE_TS.format(col='prev_ts')}) * 24.0"
        gaps: list[dict[str, Any]] = []
        try:
            cur.execute(
                f"""WITH points AS (
                        SELECT timestamp AS ts FROM messages WHERE {msg_clause}
                        {" ".join(edges)}
                    ),
                    ordered AS (
                        SELECT ts, LAG(ts) OVER (ORDER BY ts) AS prev_ts FROM points
                    )
                    SELECT prev_ts, ts, {delta_hours} AS hours
                      FROM ordered
                     WHERE prev_ts IS NOT NULL AND {delta_hours} > ?
                     ORDER BY hours DESC LIMIT ?""",
                (*msg_params, *edge_params, gap_hours, max_gaps),
            )
            gaps = [{"from": start, "to": end, "hours": round(float(hours), 1)} for start, end, hours in cur.fetchall()]
        except sqlite3.OperationalError as exc:  # SQLite without window functions
            logger.warning("coverage: gap scan unavailable (%s)", exc)
            gaps = []
    except sqlite3.Error as e:
        logger.error("Database error in coverage: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if conn is not None:
            conn.close()

    return {
        "first_message_time": first_time,
        "last_message_time": last_time,
        "total_messages": int(total_messages or 0),
        "chats_total": chats_total,
        "chats_with_messages": chats_total - chats_without_messages,
        "chats_without_messages": chats_without_messages,
        "messages_by_month": messages_by_month,
        "audio": audio,
        "gap_hours": gap_hours,
        "gaps": gaps,
        "gaps_truncated": len(gaps) >= max_gaps,
        "scope": scope,
        "allow_list_applied": CHAT_POLICY.restricted,
        "hint": _coverage_hint(scope, audio),
    }


def _coverage_hint(scope: dict[str, Any], audio: dict[str, int]) -> str:
    """How to read the aggregates — different once a window narrows them.

    Unbounded, first_message_time is where the archive itself begins and
    chats_without_messages means "never synced". Under after/before both
    describe the window and nothing else, so saying otherwise would have the
    agent report the period before the bound as never synced.

    The audio sentence is only added when there is a backlog: on an archive with
    every voice note transcribed (or none at all) it would be noise.
    """
    windowed = scope["after"] is not None or scope["before"] is not None
    middle = (
        "after/before are set, so first_message_time, chats_without_messages and the gaps describe that window "
        "alone — they say nothing about what the archive holds outside it. Drop the bounds to ask that."
        if windowed
        else "Chats in chats_without_messages, and anything before first_message_time, were never synced either. "
        "Narrow with after/before/chat_jid to keep old sync artefacts out of the list."
    )
    backlog = ""
    if audio["backlog"]:
        backlog = (
            f" {audio['backlog']} of the {audio['messages']} voice notes in scope are still untranscribed, "
            f"{audio['backlog_cached']} of them with their bytes already on disk: transcribe_audio one at a "
            "time, or set TRANSCRIBE_ON_INGEST=1 to walk them in the background."
        )
    return (
        "Gaps cover every chat in scope: no message at all between 'from' and 'to', which usually means the bridge "
        f"was down or never synced that period rather than everyone going quiet. {middle} Call "
        "coverage(by_chat=True) for the queue of chats to backfill with request_history(chat_jid); it cannot fill "
        f"a period the phone itself no longer has.{backlog}"
    )


def _whisper_status() -> dict[str, Any]:
    """Transcription capability for bridge_status, never raising.

    A misconfigured backend must still leave every other field of the status
    readable, so a failure here is logged and reported as "nothing usable"
    with the reason attached.
    """
    try:
        return transcribe.describe_status()
    except Exception as exc:  # a capability report cannot fail the status call
        logger.warning("bridge_status: whisper status unavailable: %s", exc)
        return {
            "configured": False,
            "backend": None,
            "reachable": None,
            "model": None,
            "on_ingest": False,
            "error": str(exc),
        }


def _endpoint_cert_status() -> dict[str, Any]:
    """Published-endpoint certificate fields for bridge_status, never raising.

    Empty when WHATSAPP_PUBLIC_URL is unset, so the status of a deployment
    that publishes nothing is unchanged.
    """
    try:
        return endpoint_cert.status()
    except Exception as exc:  # an outbound probe cannot fail the status call
        logger.warning("bridge_status: endpoint certificate check failed: %s", exc)
        return {"endpoint_cert_error": str(exc)}


# --- the account's own identity (whatsmeow_device / bridge me.go) --------------
#
# An agent cannot recognise itself in a group without this: WhatsApp writes a
# mention as the mentioned account's LID, which reads like a phone number.
#
# Read locally first. whatsmeow stores the paired account's JID and LID in
# whatsapp.db (whatsmeow_device), which this server already opens read-only for
# name and LID resolution, so "who am I" — and therefore mentions_me — keeps
# working when the bridge is down, as every other read does (AGENTS.md §8.7).
# GET /api/me is the fallback for a deployment whose whatsapp.db is not
# reachable from this container. Cached for a minute either way: the answer
# changes only when the account is re-paired.

_OWNER_CACHE_TTL_S = 60.0
_owner_lock = threading.Lock()
_owner_cache: tuple[float, dict[str, str | None]] | None = None


def _reset_owner_cache() -> None:
    """Drop the cached identity (tests, and after a re-pair)."""
    global _owner_cache
    with _owner_lock:
        _owner_cache = None


def _owner_from_device_table() -> dict[str, str | None] | None:
    """The paired account as whatsmeow recorded it, or None when unreadable."""
    if not os.path.isfile(WHATSMEOW_DB_PATH):
        return None
    try:
        conn = _connect_whatsmeow_db()
        try:
            row = conn.execute("SELECT jid, lid FROM whatsmeow_device LIMIT 1").fetchone()
        finally:
            conn.close()
    except sqlite3.Error as exc:  # not paired yet, or an older whatsmeow schema
        logger.debug("owner identity: whatsmeow_device unreadable: %s", exc)
        return None
    if not row:
        return None
    jid, phone = _own_jid_parts(row[0])
    if not phone:
        return None
    _, lid = _own_jid_parts(row[1])
    if lid is None:
        # A pairing older than whatsmeow's device.lid column. The mapping table
        # still knows it, and it is the answer the bridge would give (me.go
        # falls back to the same store): without it mentions_me would match the
        # phone spelling alone and quietly miss every group mention.
        lid = _own_lid_from_map(phone)
    return {"jid": jid, "phone": phone, "lid": lid}


def _own_lid_from_map(phone: str) -> str | None:
    """The LID whatsmeow mapped to our phone number, or None.

    whatsmeow_lid_map stores both sides as bare user parts.
    """
    try:
        conn = _connect_whatsmeow_db()
        try:
            row = conn.execute("SELECT lid FROM whatsmeow_lid_map WHERE pn = ? LIMIT 1", (phone,)).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        logger.debug("owner identity: whatsmeow_lid_map unreadable: %s", exc)
        return None
    if not row or not row[0]:
        return None
    _, lid = _own_jid_parts(f"{row[0]}@lid" if "@" not in str(row[0]) else row[0])
    return lid


def _own_jid_parts(raw: Any) -> tuple[str | None, str | None]:
    """('<user>@<server>', '<user>') from a stored device JID ('user.0:12@server')."""
    if not raw:
        return None, None
    user, _, server = str(raw).partition("@")
    user = user.split(":", 1)[0].split(".", 1)[0]
    if not user or not server:
        return None, None
    return f"{user}@{server}", user


def _owner_from_bridge() -> dict[str, str | None]:
    """GET /api/me. Raises ToolError("bridge_unavailable") when it cannot answer."""
    resp = _bridge_request("GET", "/me", timeout=10)
    if resp.status_code != 200:
        raise ToolError(
            "bridge_unavailable",
            f"the bridge could not say which account it is logged in as (/api/me answered HTTP {resp.status_code}); "
            "call bridge_status",
        )
    try:
        body = resp.json()
    except (json.JSONDecodeError, ValueError) as exc:
        raise ToolError("bridge_unavailable", f"/api/me returned an unreadable body: {exc}") from exc
    if not isinstance(body, dict) or not body.get("phone"):
        raise ToolError("bridge_unavailable", "/api/me returned no phone number for this account; call bridge_status")
    return {"jid": body.get("phone_jid"), "phone": body.get("phone"), "lid": body.get("lid")}


def _cached_owner() -> dict[str, str | None] | None:
    """The memoised answer while it is still fresh, else None."""
    with _owner_lock:
        if _owner_cache is not None and time.monotonic() - _owner_cache[0] < _OWNER_CACHE_TTL_S:
            return dict(_owner_cache[1])
    return None


def _remember_owner(owner: dict[str, str | None]) -> dict[str, str | None]:
    """Memoise an answer for the next minute and hand back a copy of it."""
    global _owner_cache
    with _owner_lock:
        _owner_cache = (time.monotonic(), dict(owner))
    return dict(owner)


def owner_identity() -> dict[str, str | None]:
    """{"jid", "phone", "lid"} of the account this deployment is logged in as.

    Raises ToolError("bridge_unavailable") when neither the local store nor the
    bridge can say: "who am I" has no useful empty answer, and a mentions_me
    filter that silently matched nothing would read as "nobody mentioned you".
    """
    cached = _cached_owner()
    if cached is not None:
        return cached
    return _remember_owner(_owner_from_device_table() or _owner_from_bridge())


def owner_identity_local() -> dict[str, str | None] | None:
    """The same answer from the local store alone, or None. Never calls the bridge.

    For the callers where "who am I" only sharpens a filter and is not the
    question being asked (`_own_mention_stripped`). Reads must not need the
    bridge, and asking it here would make a plain `list_unanswered` wait out the
    whole connection-retry ladder on a deployment where the bridge is down
    (issue #411) for the sake of a word list.
    """
    cached = _cached_owner()
    if cached is not None:
        return cached
    owner = _owner_from_device_table()
    return _remember_owner(owner) if owner else None


def bridge_status() -> dict[str, Any]:
    """Health, readiness and build identity of the bridge in one call.

    Never raises for a bridge that is down: returns ok=false with the reason,
    so the agent can tell "bridge unreachable" from "nothing matched".
    """
    status: dict[str, Any] = {"ok": False, "bridge_url": WHATSAPP_API_BASE_URL, "whisper": _whisper_status()}
    from runtime_settings import ingest_setting

    try:
        status["transcription_ingest_chats"] = ingest_setting()
    except (OSError, sqlite3.Error, ValueError):
        status["transcription_ingest_chats"] = {"error": "Runtime setting unavailable"}
    status.update(_endpoint_cert_status())
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
            "send_usage": body.get("send_usage"),
        }
    )
    status["ok"] = status["connected"] and status["paired"]
    if not status["ok"]:
        status["reason"] = (
            "bridge is up but not paired: scan the QR code in its log"
            if not status["paired"]
            else "bridge is paired but disconnected from WhatsApp; it reconnects automatically"
        )
    problem = body.get("connection_problem")
    status["connection_problem_persistence_failed"] = body.get("connection_problem_persistence_failed") is True
    pairing_state = body.get("pairing_state")
    if isinstance(problem, dict):
        status["connection_problem"] = problem
        kind = problem.get("kind")
        if kind in ("banned", "locked", "client_outdated", "temporarily_banned"):
            status["ok"] = False
            status["reason"] = f"WhatsApp connection problem: {kind}; see docs/TROUBLESHOOTING.md"
    if pairing_state in ("passkey_required", "passkey_confirm", "passkey_failed"):
        status["pairing_state"] = pairing_state
        status["ok"] = False
        status["reason"] = f"WhatsApp pairing step: {pairing_state}; operator intervention required; see docs/DOCKER.md"
    try:
        version = _bridge_request("GET", "/version", timeout=10)
        if version.status_code == 200:
            info = version.json()
            if isinstance(info, dict):
                status["version"] = {k: info.get(k) for k in ("version", "commit", "go", "whatsmeow", "fts5")}
    except (ToolError, json.JSONDecodeError, ValueError):
        pass
    # Who this deployment is logged in as: the LID is what a group mention is
    # written with, so an agent needs it to recognise itself. Absent rather than
    # null when nothing can say (not paired yet, no readable store, bridge
    # down). Broad except on purpose: this is the one tool an agent calls when
    # everything else is broken, and it must not raise (see the docstring).
    try:
        status["owner"] = owner_identity()
    except Exception as exc:
        logger.debug("bridge_status: owner identity unavailable: %s", exc)
    return status


def unread_filters(
    since: str | None = None,
    max_age_days: int | None = None,
    exclude_groups: bool = False,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
) -> tuple[str, list[Any]]:
    """The predicates shared by the unread queries: the caller's, plus the status feed.

    Returns an AND-prefixed SQL fragment and its parameters, for a query that
    has both `messages` and `chats` in scope. The caller's filters are all
    optional; dropping `status@broadcast` is not (issue #379).
    `since` and `max_age_days` are two spellings of the same lower bound, so
    passing both is an error rather than a silent winner.
    """
    if since and max_age_days is not None:
        raise ToolError("invalid_argument", "pass either since or max_age_days, not both")
    since_ts: str | None = None
    if since:
        try:
            since_ts = timestamp_bound(datetime.fromisoformat(since))
        except ValueError as exc:
            raise ToolError("invalid_argument", f"since must be ISO-8601, got {since!r}") from exc
    elif max_age_days is not None:
        days = int(max_age_days)
        if days < 1:
            raise ToolError("invalid_argument", f"max_age_days must be 1 or more, got {max_age_days!r}")
        since_ts = timestamp_bound(datetime.now(UTC) - timedelta(days=days))

    # The status feed is never waiting for a reply, so it leaves the triage
    # listings the way a reaction never counts as speaking (issue #379) —
    # unconditionally, even when the caller named it in chat_jid. Its posts stay
    # readable through list_messages / list_chats.
    clauses: list[str] = [f"AND chats.jid <> '{STATUS_BROADCAST_JID}'"]
    params: list[Any] = []
    if since_ts is not None:
        clauses.append("AND messages.timestamp > ?")
        params.append(since_ts)
    if chats := chat_jid_filter(chat_jid):
        clauses.append("AND " + _chat_jid_clause("chats.jid", chats))
        params.extend(chats)
    if excluded := chat_jid_filter(exclude_chat_jid, "exclude_chat_jid", require_allowed=False):
        clauses.append("AND " + _chat_jid_clause("chats.jid", excluded, negated=True))
        params.extend(excluded)
    if exclude_groups:
        clauses.append("AND " + _direct_only_clause("chats.jid"))
    return (" ".join(clauses), params)


def list_unread(
    limit_chats: int = 20,
    limit_per_chat: int = 5,
    since: str | None = None,
    exclude_groups: bool = False,
    max_age_days: int | None = None,
    count_only: bool = False,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    hide_handled: bool = False,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
) -> dict[str, Any]:
    """Chats with unread inbound messages, each with its newest unread rows.

    "Unread" means inbound (is_from_me = 0) and newer than the chat's read
    marker (chats.last_read_time, as reported by any linked device); chats
    with no marker count as entirely unread. Ordered by most recent unread
    message. Honours WHATSAPP_ALLOWED_CHATS.

    exclude_groups keeps direct conversations only (`@s.whatsapp.net` / `@lid`),
    dropping groups, broadcast lists, channels and bots; max_age_days is the relative
    form of since (both are rejected together). chat_jid / exclude_chat_jid narrow
    the set of conversations (one JID or a list). All of them bound the counted
    rows and the returned messages alike.

    The triage notes (`triage.py`) are honoured per chat, never per message: a
    chat is either in the list with all its unread rows or not in it at all.
    hide_handled defaults to *False* here, unlike list_unanswered — `handled_at`
    deliberately writes no read receipt, so a handled chat is still genuinely
    unread, and hiding it by default would hide from the one list an agent uses
    to decide what to mark_messages_read. mute and snooze are statements about
    surfacing rather than about reading, so they default on as they do there.

    count_only returns {"count", "chats_with_unread"} over *every* matching
    chat, ignoring limit_chats and limit_per_chat, and reads no message row.
    """
    # count_only ignores both list sizes (they describe rows it does not read),
    # so they are only validated on the path that uses them.
    if not count_only:
        limit_chats = page_size(limit_chats, UNREAD_MAX_CHATS, "limit_chats")
        limit_per_chat = page_size(limit_per_chat, UNREAD_MAX_PER_CHAT, "limit_per_chat")
    filter_clause, filter_params = unread_filters(since, max_age_days, exclude_groups, chat_jid, exclude_chat_jid)
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        # One row per person, not one per spelling (issue #366): the row another
        # one reports for leaves the chat side, its unread messages join the
        # listing row's, and the marker both are measured against is the newer of
        # the two — a conversation read under one spelling is read under both.
        twins = _chat_twins(cursor)
        prefix, twin_join, of_this_chat, twin_params = twins.both_rows()
        hidden_clause, hidden_params = twins.hidden_clause("chats.jid")
        read_marker = _last_read_time_select(cursor, "chats")
        message_table = "messages"
        if twins.active:
            twin_join += " LEFT JOIN chats twin ON twin.jid = tw.twin_jid"
            other_marker = _last_read_time_select(cursor, "twin")
            read_marker = f"NULLIF(MAX(COALESCE({read_marker}, ''), COALESCE({other_marker}, '')), '')"
            if any(len(twin.aliases) > 2 for twin in twins.by_listed.values()):
                twin_join += " LEFT JOIN chats third ON third.jid = tw.third_jid"
                third_marker = _last_read_time_select(cursor, "third")
                read_marker = f"NULLIF(MAX(COALESCE({read_marker}, ''), COALESCE({third_marker}, '')), '')"
            # An equality join keeps SQLite seeking by chat even when a
            # large VALUES table makes it estimate an IN-list poorly.
            # Singles have no member row and retain their own spelling.
            twin_join += " LEFT JOIN chat_twin member ON member.listed_jid = chats.jid"
            of_this_chat = "= COALESCE(member.jid, chats.jid)"
            # SQLite 3.53 can prefer an is_from_me-only automatic index,
            # scanning every inbound row for each chat. Pin an available
            # bridge-owned chat index; older schemas may have neither.
            for index in ("idx_messages_chat_timestamp", "idx_messages_chat_jid"):
                columns = cursor.execute(f"PRAGMA main.index_info('{index}')").fetchall()
                if columns and columns[0][2] == "chat_jid":
                    message_table += f" INDEXED BY {index}"
                    break
        policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
        spoken_filter = _spoken_filter("messages")
        unread_where = f"""
            FROM chats
            {twin_join}
            JOIN {message_table} ON messages.chat_jid {of_this_chat}
            WHERE messages.is_from_me = 0
              AND ({read_marker} IS NULL OR messages.timestamp > {read_marker})
              AND {spoken_filter}
              AND {policy_clause}
              AND {hidden_clause}
              {filter_clause}
            """
        scope_params: tuple[Any, ...] = (*twin_params, *policy_params, *hidden_params, *filter_params)
        # Per chat, not per row: the mark is compared with the chat's newest
        # unread message, which set_anchor computes under these same bounds. Only
        # the two timestamp notes need it — a mute filter alone reads no message.
        triage_clause, triage_params = _install_triage_filter(
            conn, hide_handled, exclude_muted, include_snoozed, anchor="t.anchor"
        )
        if triage_clause and (hide_handled or not include_snoozed):
            _set_triage_anchor(conn, unread_where, scope_params, prefix)
        where_params: tuple[Any, ...] = (*scope_params, *triage_params)
        if count_only:
            cursor.execute(
                f"{prefix}SELECT COUNT(*), COUNT(DISTINCT chats.jid) {unread_where} {triage_clause}", where_params
            )
            counted = cursor.fetchone() or (0, 0)
            return {"count": int(counted[0] or 0), "chats_with_unread": int(counted[1] or 0)}
        params: list[Any] = [*where_params, limit_chats]
        cursor.execute(
            f"""
            {prefix}SELECT chats.jid, chats.name, {read_marker} AS last_read_time,
                   COUNT(*) AS unread_count, MAX(messages.timestamp) AS latest_unread
            {unread_where} {triage_clause}
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
            twin = twins.merged_row(jid)
            # The pair's rows, and the marker the count was taken against, so the
            # messages listed are the ones counted.
            spellings = twin.aliases if twin is not None else [jid]
            cursor.execute(
                f"""
                SELECT {message_columns(cursor)}
                FROM messages JOIN chats ON messages.chat_jid = chats.jid
                WHERE {_chat_jid_clause("messages.chat_jid", spellings)} AND messages.is_from_me = 0
                  AND (? IS NULL OR messages.timestamp > ?)
                  AND {spoken_filter}
                  {filter_clause}
                ORDER BY messages.timestamp DESC, messages.id DESC
                LIMIT ?
                """,
                (*spellings, last_read, last_read, *filter_params, limit_per_chat),
            )
            rows = cursor.fetchall()
            total += int(count)
            if twin is not None and _is_placeholder_name(name) and not _is_placeholder_name(twin.name):
                name = twin.name
            chat_row: dict[str, Any] = {
                "chat_jid": jid if twin is None else twin.jid,
                "chat_name": name,
                "is_group": jid.endswith("@g.us"),
                "unread_count": int(count),
                "latest_unread": latest,
                "last_read_time": last_read,
                "messages": [_row_to_message(row) for row in reversed(rows)],
            }
            if twin is not None:
                chat_row["aliases"] = list(twin.aliases)
            chats.append(chat_row)
        # One notes query for the whole call, not one per chat or per message.
        every_message = [message for chat in chats for message in chat["messages"]]
        notes = fetch_media_notes(every_message)
        identities = fetch_sender_identities(every_message)
        prefetch_sender_names(every_message)
        for chat in chats:
            chat["messages"] = [
                msg_to_dict(message, notes=notes, identities=identities) for message in chat["messages"]
            ]
        return {"chats": chats, "total_unread": total, "chats_with_unread": len(chats)}
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def _hours_since(timestamp: str | None) -> float | None:
    """Hours between a stored timestamp and now, or None when unparseable.

    Both sides are instants: the stored value keeps its offset instead of being
    truncated to a wall clock, and now() is taken in UTC. `age_hours >=
    min_age_hours` therefore holds for every row `min_age_hours` kept, since
    that bound is rendered by the same convention (issues #257, #270).
    """
    if not timestamp:
        return None
    try:
        moment = parse_db_time(timestamp)
    except ValueError:
        return None
    return round((datetime.now(UTC) - moment).total_seconds() / 3600.0, 2)


def list_unanswered(
    since: str | None = None,
    limit: int = 20,
    exclude_groups: bool = False,
    min_age_hours: float = 0,
    include_last_message: bool = True,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    hide_handled: bool = True,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
    ignore_closing_messages: bool = False,
    include_group_mentions: bool = False,
    min_messages: int = 0,
) -> list[dict[str, Any]]:
    """Items of one page of list_unanswered_page."""
    return list_unanswered_page(
        since=since,
        limit=limit,
        exclude_groups=exclude_groups,
        min_age_hours=min_age_hours,
        include_last_message=include_last_message,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        hide_handled=hide_handled,
        exclude_muted=exclude_muted,
        include_snoozed=include_snoozed,
        ignore_closing_messages=ignore_closing_messages,
        include_group_mentions=include_group_mentions,
        min_messages=min_messages,
    ).items


def _min_messages_clause(min_messages: int, chat_match: str = "") -> tuple[str, list[Any]]:
    """Chats where at least N messages were spoken, `chats` in scope.

    Inbound and outbound alike — what this filter is for is the number that
    never became a conversation, and both sides make one. Reactions, poll votes
    and revoked rows do not count, the same rule the rest of the tool applies:
    a 👍 on a one-line delivery notice must not turn it into a conversation.
    0 and 1 are no-ops, since a chat with nothing said never reaches these lists.

    Counted through a bounded subquery so a 200k-message group costs the
    threshold, not the group: the question is "at least N", never "how many".

    `chat_match` counts a merged phone/LID pair as the one conversation it is
    (issue #366) — halves that each said one thing must not each be dropped.
    """
    threshold = int(min_messages or 0)
    if threshold < 0:
        raise ToolError("invalid_argument", f"min_messages must be 0 or more, got {min_messages!r}")
    if threshold < 2:
        return "", []
    match = chat_match or "= chats.jid"
    clause = (
        "AND (SELECT COUNT(*) FROM (SELECT 1 FROM messages stored"
        f" WHERE stored.chat_jid {match} AND {_spoken_filter('stored')} LIMIT ?)) >= ?"
    )
    return clause, [threshold, threshold]


def _unanswered_from_where(
    since: str | None,
    exclude_groups: bool,
    min_age_hours: float,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    ignore_closing_messages: bool = False,
    min_messages: int = 0,
    twins: ChatTwins = NO_CHAT_TWINS,
) -> tuple[str, str, list[Any]]:
    """(WITH prefix, FROM/WHERE, params) shared by the list_unanswered page and its count.

    A phone/LID pair is one waiting conversation, not two (issue #366): the row
    another one reports for leaves the chat side, and both rows' messages feed
    the "who spoke last" join, so the anchor is the newest of the pair. The
    prefix is returned apart because `WITH` has to lead the whole statement,
    ahead of the caller's own SELECT.
    """
    prefix, twin_join, of_this_chat, twin_params = twins.both_rows()
    # Empty on a store with no pair, so the queries keep the shape they had.
    of_this_chat = of_this_chat if twins.active else ""
    hidden_clause, hidden_params = twins.hidden_clause("chats.jid")
    filter_clause, filter_params = unread_filters(since, None, exclude_groups, chat_jid, exclude_chat_jid)
    age_clause, age_params = "", []
    hours = float(min_age_hours or 0)
    if hours < 0:
        raise ToolError("invalid_argument", f"min_age_hours must be 0 or more, got {min_age_hours!r}")
    if hours > 0:
        age_clause = "AND messages.timestamp <= ?"
        age_params = [timestamp_bound(datetime.now(UTC) - timedelta(hours=hours))]
    closing_clause, closing_params = "", []
    if ignore_closing_messages:
        closing, closing_params = _closing_message_clause("messages")
        closing_clause = f"AND NOT {closing}"
    stored_clause, stored_params = _min_messages_clause(min_messages, of_this_chat)
    policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    sql = f"""
            FROM chats
            {twin_join}
            {_last_message_join("chats", "messages", spoken_only=True, chat_match=of_this_chat)}
            WHERE messages.is_from_me = 0
              AND {policy_clause}
              AND {hidden_clause}
              {filter_clause} {age_clause} {closing_clause} {stored_clause}
            """
    return (
        prefix,
        sql,
        [
            *twin_params,
            *policy_params,
            *hidden_params,
            *filter_params,
            *age_params,
            *closing_params,
            *stored_params,
        ],
    )


def _install_triage_filter(
    conn: sqlite3.Connection,
    hide_handled: bool,
    exclude_muted: bool,
    include_snoozed: bool,
    anchor: str = "messages.timestamp",
) -> tuple[str, list[Any]]:
    """The handled/snoozed/muted predicate, applied before LIMIT so counts and pages agree.

    triage imports this module, so the import lives here rather than at the top.
    """
    from triage import install_filter

    return install_filter(conn, hide_handled, exclude_muted, include_snoozed, anchor)


def _set_triage_anchor(conn: sqlite3.Connection, from_where: str, params: Sequence[Any], prefix: str = "") -> None:
    """Fill `t.anchor` for a caller that groups several rows per chat (list_unread)."""
    from triage import set_anchor

    set_anchor(conn, from_where, params, prefix)


# --- group mentions in triage --------------------------------------------------
#
# "The last message is inbound" is the wrong question in a group: groups are
# always inbound. What waits for an answer there is a mention of the owner that
# nobody answered — newer than the owner's own last word in that group. The
# ordinary rule already returns most of those chats (a mention is itself an
# inbound message), so include_group_mentions does two things: it flags the
# rows that actually addressed you, and it adds the groups the age bound hid
# because the group kept talking after the mention.


def _pending_mention_where(
    cur: sqlite3.Cursor, alias: str, chat_jid_expr: str, own_match: str = ""
) -> tuple[str, list[Any]]:
    """`alias` is an inbound mention of the owner, newer than the owner's last word.

    "Word" on both sides means a spoken message (_spoken_filter): a thumbs-up
    from you does not answer a question, and a mention you revoked is not one
    any more — the same rule list_unanswered already applies to everything else.

    `own_match` widens "your last word" to both rows of a merged phone/LID pair
    (issue #366): a reply sent under one spelling answers a mention filed under
    the other, exactly as it does for the ordinary "who spoke last" rule.
    """
    mention_clause, mention_params = mentions_me_predicate(cur, alias)
    own_scope = own_match or f"= {chat_jid_expr}"
    sql = f"""{alias}.is_from_me = 0 AND {_spoken_filter(alias)} AND {mention_clause}
              AND {alias}.timestamp > COALESCE((SELECT MAX(own.timestamp) FROM messages own
                                                 WHERE own.chat_jid {own_scope}
                                                   AND own.is_from_me = 1 AND {_spoken_filter("own")}), '')"""
    return sql, mention_params


def _closing_mention_clause(ignore_closing_messages: bool, alias: str) -> tuple[str, list[str]]:
    """`AND NOT <closing>` on a mention row, or nothing when the flag is off.

    The mention stream anchors on the mention itself, so `ignore_closing_messages`
    has to be applied there as well as to the chat's newest inbound message: a
    group whose only pending mention is "ok @me" or a sticker is acknowledging,
    not waiting, and used to come back through this door (issue #395).

    The predicate is the ordinary rule's own, `@…` and all: since issue #411
    both sides read past this account's own mention (`_own_mention_stripped`).
    """
    if not ignore_closing_messages:
        return "", []
    clause, params = _closing_message_clause(alias)
    return f" AND NOT {clause}", params


def newest_pending_mentions(
    cur: sqlite3.Cursor,
    chat_jids: Sequence[str],
    bounds: Sequence[tuple[str, str]] = (),
    twins: ChatTwins = NO_CHAT_TWINS,
    ignore_closing_messages: bool = False,
) -> dict[str, tuple[str, str]]:
    """{chat_jid: (message_id, timestamp)} of the newest unanswered mention per chat.

    One row per chat (MAX picks the bare columns with it), for the chats given,
    so the annotation costs one bounded query per page. `bounds` carries the
    same time bounds the page was built with — a row anchored on a mention must
    name that mention, not a newer one the bound excluded.

    `ignore_closing_messages` is there for the same reason: with the flag on, a
    mention that only acknowledges is not a mention anyone waits on, so the row
    anchored on the older real one must name that one rather than the "ok @me"
    the page was built without (issue #395).

    `twins` makes "you answered" span both spellings of a merged pair: without
    it a reply sent under the phone JID would leave a mention filed under the
    `@lid` pending for ever (issue #366).
    """
    if not chat_jids:
        return {}
    prefix, twin_join, own_match, twin_params = "", "", "", []
    if twins.active:
        cte, twin_params = twins.cte()
        prefix = f"WITH {cte} "
        twin_join = " LEFT JOIN chat_twin tw ON tw.jid = messages.chat_jid"
        own_match = twins.message_members("messages.chat_jid")
    where, params = _pending_mention_where(cur, "messages", "messages.chat_jid", own_match)
    bound_sql = "".join(f" AND messages.timestamp {op} ?" for op, _ in bounds)
    closing_sql, closing_params = _closing_mention_clause(ignore_closing_messages, "messages")
    placeholders = ",".join("?" * len(chat_jids))
    cur.execute(
        f"""{prefix}SELECT messages.chat_jid, messages.id, MAX(messages.timestamp)
            FROM messages{twin_join}
            WHERE messages.chat_jid IN ({placeholders}) AND {where}{bound_sql}{closing_sql}
            GROUP BY messages.chat_jid""",
        (*twin_params, *chat_jids, *params, *(value for _, value in bounds), *closing_params),
    )
    return {chat: (message_id, timestamp) for chat, message_id, timestamp in cur.fetchall()}


def _pending_mentions_by_row(
    cur: sqlite3.Cursor,
    listed: Sequence[str],
    bounds: Sequence[tuple[str, str]],
    twins: ChatTwins,
    ignore_closing_messages: bool = False,
) -> dict[str, tuple[str, str]]:
    """`newest_pending_mentions` keyed by the row that lists, both spellings folded.

    A mention of this account can sit in the row of a merged pair that another
    one reports for (issue #366), and the row is about the conversation: both
    halves are probed, answered messages on either side of the pair count as
    answered, and the newer of what is left names the row.
    """
    if not twins.active:
        return newest_pending_mentions(cur, listed, bounds, ignore_closing_messages=ignore_closing_messages)
    lists_for = {jid: jid for jid in listed}
    for jid in listed:
        twin = twins.merged_row(jid)
        if twin is not None:
            lists_for.update({spelling: jid for spelling in twin.aliases})
    merged: dict[str, tuple[str, str]] = {}
    pending_by_chat = newest_pending_mentions(
        cur, list(lists_for), bounds, twins, ignore_closing_messages=ignore_closing_messages
    )
    for chat, pending in pending_by_chat.items():
        row = lists_for[chat]
        if row not in merged or merged[row][1] < pending[1]:
            merged[row] = pending
    return merged


def _mention_only_rows(
    cur: sqlite3.Cursor,
    *,
    since: str | None,
    min_age_hours: float,
    chat_jid: str | Sequence[str] | None,
    exclude_chat_jid: str | Sequence[str] | None,
    read_marker: str,
    include_last_message: bool,
    cursor_state: dict[str, Any] | None,
    limit: int,
    min_messages: int = 0,
    ignore_closing_messages: bool = False,
    triage_clause: str = "",
    triage_params: Sequence[Any] = (),
) -> tuple[list[tuple], list[tuple[str, str]]]:
    """Groups waiting on a mention that the ordinary unanswered rule does not return.

    Same row shape and order as the main query, anchored on the mention instead
    of on the chat's newest message — that anchor is the point: a group that
    kept talking after the mention is dropped by min_age_hours even though the
    mention itself has been waiting for days. Chats the ordinary rule already
    returns are left to it (the COALESCE(last_spoken…) clause), so the two
    streams never describe the same chat twice, on this page or the next.

    ``triage_clause`` is the caller's own filter (`install_filter`), applied here
    to the mention row: a chat marked handled, snoozed or muted must not come
    back through this door (issue #361), and the message that lifts a mark is the
    mention itself, since that is what this row is about. Both timestamp marks
    hide the rows *older* than themselves, so the surviving mentions are still
    the newest ones and MAX() below picks the same anchor
    ``newest_pending_mentions`` names.

    ``ignore_closing_messages`` applies the ordinary rule's own predicate to the
    mention row (issue #395): "ok @me" or a sticker acknowledges, it does not ask
    for anything, so it must not put a group on the list on its own. Unlike the
    triage marks this one hides a *newer* row, so the annotation query is told
    about it too and both stay anchored on the same mention. It also narrows what
    counts as covered, since a chat the ordinary rule dropped for a closing last
    message is not in that stream to be left to it: a group whose newest word is
    somebody else's "ok" still owes you the answer to the mention below it (issue
    #407).
    """
    filter_clause, filter_params = unread_filters(since, None, False, chat_jid, exclude_chat_jid)
    # unread_filters bounds `messages`, which is the mention row here.
    mention_where, mention_params = _pending_mention_where(cur, "messages", "chats.jid")
    stored_clause, stored_params = _min_messages_clause(min_messages)
    policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    age_clause, age_params = "", []
    if float(min_age_hours or 0) > 0:
        age_clause = "AND messages.timestamp <= ?"
        age_params = [timestamp_bound(datetime.now(UTC) - timedelta(hours=float(min_age_hours)))]
    closing_clause, closing_params = _closing_mention_clause(ignore_closing_messages, "messages")
    # The bounds the ordinary rule applies to the chat's newest spoken message:
    # when it passes them the chat is already in that stream.
    covered_bounds, covered_params = "", []
    if since:
        # Already validated by unread_filters above, same conversion.
        covered_bounds += " AND last_spoken.timestamp > ?"
        covered_params.append(timestamp_bound(datetime.fromisoformat(since)))
    if age_params:
        covered_bounds += " AND last_spoken.timestamp <= ?"
        covered_params.append(age_params[0])
    if ignore_closing_messages:
        # The ordinary rule's own predicate, on its own row. Mirroring it
        # exactly — the same words, read past the same `@…` — is what keeps the
        # two streams disjoint: a chat it lists is covered, a chat it dropped
        # for a closing word is not.
        covered_closing, covered_closing_params = _closing_message_clause("last_spoken")
        covered_bounds += f" AND NOT {covered_closing}"
        covered_params += covered_closing_params
    # The keyset belongs on the chat's newest mention, not on the individual
    # mention rows: a group with two unanswered mentions would otherwise come
    # back on the next page anchored on the older one.
    keyset_clause, keyset_params = "", []
    if cursor_state is not None:
        keyset_clause = "HAVING (MAX(messages.timestamp) < ? OR (MAX(messages.timestamp) = ? AND chats.jid > ?))"
        keyset_params = [cursor_state["t"], cursor_state["t"], cursor_state["j"]]

    # MAX() is the only aggregate, so SQLite takes the bare columns (the
    # mention's id, text and sender) from the row that produced it.
    # last_* describes the chat's newest spoken message here too, so a consumer
    # that prints last_message beside last_message_time sees one message; the
    # mention this row is about is named by mention_message_id / mention_time.
    last_message_select = "last_spoken.content, last_spoken.sender" if include_last_message else "NULL, NULL"
    cur.execute(
        f"""
        SELECT chats.jid, chats.name, chats.last_message_time,
               {last_message_select},
               last_spoken.is_from_me, {read_marker},
               1, MAX(messages.timestamp)
        FROM chats
        JOIN messages ON messages.chat_jid = chats.jid
        {_last_message_join("chats", "last_spoken", spoken_only=True)}
        WHERE chats.jid LIKE '%@g.us'
          AND {policy_clause}
          AND {mention_where}
          {filter_clause} {age_clause}{closing_clause} {stored_clause} {triage_clause}
          AND COALESCE(last_spoken.is_from_me = 0 {covered_bounds}, 0) = 0
        GROUP BY chats.jid
        {keyset_clause}
        ORDER BY MAX(messages.timestamp) DESC, chats.jid ASC
        LIMIT ?
        """,
        (
            *policy_params,
            *mention_params,
            *filter_params,
            *age_params,
            *closing_params,
            *stored_params,
            *triage_params,
            *covered_params,
            *keyset_params,
            limit,
        ),
    )
    # The bounds the anchor was chosen under, for the annotation query.
    bounds: list[tuple[str, str]] = []
    if filter_params and since:
        bounds.append((">", timestamp_bound(datetime.fromisoformat(since))))
    if age_params:
        bounds.append(("<=", age_params[0]))
    return cur.fetchall(), bounds


def _sorts_first(row: tuple, other: tuple) -> bool:
    """The page order: newest anchor first, chat JID ascending on a tie.

    Timestamps are the canonical UTC spelling, so comparing the strings
    compares the instants (issue #270).
    """
    if row[8] != other[8]:
        return row[8] > other[8]
    return row[0] <= other[0]


def _merge_by_anchor(primary: Sequence[tuple], extra: Sequence[tuple], cap: int) -> list[tuple]:
    """Merge two streams already ordered by (anchor DESC, jid ASC), keeping that order.

    Both queries answer the same page bounds, so merging their heads gives the
    same rows the union would in one query. A chat cannot be in both streams
    (see _mention_only_rows), so no de-duplication is needed here.
    """
    merged: list[tuple] = []
    i = j = 0
    while len(merged) < cap and (i < len(primary) or j < len(extra)):
        if j >= len(extra):
            merged.append(primary[i])
            i += 1
        elif i >= len(primary):
            merged.append(extra[j])
            j += 1
        elif _sorts_first(primary[i], extra[j]):
            merged.append(primary[i])
            i += 1
        else:
            merged.append(extra[j])
            j += 1
    return merged


def count_unanswered(
    since: str | None = None,
    exclude_groups: bool = False,
    min_age_hours: float = 0,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    hide_handled: bool = True,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
    ignore_closing_messages: bool = False,
    min_messages: int = 0,
) -> int:
    """How many chats are waiting for a reply, without returning any of them."""
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        # One row per waiting conversation, so a merged pair counts once and
        # this number keeps agreeing with the page (issue #366).
        prefix, from_where, params = _unanswered_from_where(
            since,
            exclude_groups,
            min_age_hours,
            chat_jid,
            exclude_chat_jid,
            ignore_closing_messages,
            min_messages,
            _chat_twins(cur),
        )
        triage_clause, triage_params = _install_triage_filter(conn, hide_handled, exclude_muted, include_snoozed)
        cur.execute(f"{prefix}SELECT COUNT(*) {from_where} {triage_clause}", (*params, *triage_params))
        row = cur.fetchone()
        return int(row[0]) if row else 0
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


def list_unanswered_page(
    since: str | None = None,
    limit: int = 20,
    exclude_groups: bool = False,
    min_age_hours: float = 0,
    include_last_message: bool = True,
    cursor: str | None = None,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    hide_handled: bool = True,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
    ignore_closing_messages: bool = False,
    include_group_mentions: bool = False,
    min_messages: int = 0,
) -> PageResult:
    """Chats whose newest stored message is inbound: the other side spoke last.

    Complements list_unread, which answers the same question through the read
    marker and therefore misses everything already read on the phone but never
    replied to. Reactions, poll votes and revoked messages do not count as
    speaking, so a thumbs-up from you does not hide a chat and a thumbs-up from
    them does not create one. Newest inbound message first. Honours
    WHATSAPP_ALLOWED_CHATS.

    The triage notes an agent writes back (`triage.py`) are honoured by default:
    a chat marked handled after its last inbound message, one snoozed into the
    future, and one muted are all left out — see `install_filter` for why that
    happens in SQL rather than on the page.

    include_group_mentions marks every row that carries an unanswered mention of
    this account (`mention`, `mention_message_id`, `mention_time`) and adds the
    groups whose mention is still waiting even though the group kept talking
    after it — the rows min_age_hours would otherwise hide.

    min_messages drops the chats where fewer than N messages were ever spoken,
    the one-line broadcasts that were never a conversation.

    A contact WhatsApp knows under both a phone JID and a `@lid` waits in one
    row, as everywhere else (`ChatTwins`, issue #366): the pair is collapsed
    before the page is cut, so it cannot straddle a page boundary either.
    """
    limit = page_size(limit, UNANSWERED_MAX_LIMIT)
    cursor_state = decode_cursor(cursor, "unanswered")
    # Resolve "who am I" before opening the database (see list_messages_page).
    if include_group_mentions:
        owner_identity()

    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        twins = _chat_twins(cur)
        prefix, from_where, where_params = _unanswered_from_where(
            since,
            exclude_groups,
            min_age_hours,
            chat_jid,
            exclude_chat_jid,
            ignore_closing_messages,
            min_messages,
            twins,
        )
        read_marker = _last_read_time_select(cur, "chats")
        triage_clause, triage_params = _install_triage_filter(conn, hide_handled, exclude_muted, include_snoozed)
        # Same contract as list_chats: the last message is always joined
        # because is_from_me is the filter, its content is optional.
        if include_last_message:
            last_message_select = "messages.content, messages.sender"
        else:
            last_message_select = "NULL, NULL"
        keyset_clause, keyset_params = "", []
        if cursor_state is not None:
            keyset_clause = "AND (messages.timestamp < ? OR (messages.timestamp = ? AND chats.jid > ?))"
            keyset_params = [cursor_state["t"], cursor_state["t"], cursor_state["j"]]

        cur.execute(
            f"""
            {prefix}SELECT chats.jid, chats.name, chats.last_message_time,
                   {last_message_select},
                   messages.is_from_me, {read_marker},
                   messages.id IS NOT NULL, messages.timestamp
            {from_where} {triage_clause} {keyset_clause}
            ORDER BY messages.timestamp DESC, chats.jid ASC
            LIMIT ?
            """,
            (*where_params, *triage_params, *keyset_params, limit + 1),
        )
        rows = cur.fetchall()
        mentions_by_chat: dict[str, tuple[str, str]] = {}
        if include_group_mentions:
            # exclude_groups wins: it says "no groups", so no group is added back
            # here either. The flag on the remaining rows still means what it says.
            extra: list[tuple] = []
            bounds: list[tuple[str, str]] = []
            if not exclude_groups:
                extra, bounds = _mention_only_rows(
                    cur,
                    since=since,
                    min_age_hours=min_age_hours,
                    chat_jid=chat_jid,
                    exclude_chat_jid=exclude_chat_jid,
                    read_marker=read_marker,
                    include_last_message=include_last_message,
                    cursor_state=cursor_state,
                    limit=limit + 1,
                    min_messages=min_messages,
                    ignore_closing_messages=ignore_closing_messages,
                    # The same clause, and deliberately so: its anchor is
                    # `messages.timestamp`, which is the last inbound message
                    # above and the mention row there — the message each stream
                    # compares a mark with. Switching the query above to
                    # `t.anchor` (as list_unread does) would have to build the
                    # mention stream its own clause, since that column is not
                    # filled for it.
                    triage_clause=triage_clause,
                    triage_params=triage_params,
                )
            rows = _merge_by_anchor(rows, extra, limit + 1)
            # Same bounds as the rows, and the same closing-message rule: the
            # mention named here is the one the page was built around, never a
            # newer one a bound or "ok @me" left out. The triage notes are not
            # among them — the flag says what the chat holds, and a row they did
            # not hide keeps naming its mention.
            mentions_by_chat = _pending_mentions_by_row(
                cur,
                [row[0] for row in rows],
                bounds,
                twins,
                ignore_closing_messages=ignore_closing_messages,
            )
        has_more = len(rows) > limit
        rows = rows[:limit]
        next_cursor = None
        if has_more and rows:
            next_cursor = encode_cursor({"k": "unanswered", "t": rows[-1][8], "j": rows[-1][0]})

        page_chats = [
            Chat(
                jid=row[0],
                name=row[1],
                last_message_time=parse_db_time(row[2]) if row[2] else None,
                last_message=row[3],
                last_sender=row[4],
                last_is_from_me=row[5],
                last_read_time=parse_db_time(row[6]) if row[6] else None,
                has_messages=bool(row[7]),
            )
            for row in rows
        ]
        twins.merge(page_chats)
        _apply_name_fallback(page_chats)
        items = []
        for chat, row in zip(page_chats, rows, strict=True):
            item = chat_to_dict(chat)
            item["last_inbound_time"] = row[8]
            item["age_hours"] = _hours_since(row[8])
            if include_group_mentions:
                # Keyed by the row, not by chat.jid: merge() has just relabelled
                # a pair to its phone spelling, which need not be the row's.
                pending = mentions_by_chat.get(row[0])
                item["mention"] = pending is not None
                if pending is not None:
                    item["mention_message_id"], item["mention_time"] = pending
            items.append(item)
        return PageResult(items, next_cursor, has_more)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise ToolError("internal", f"database error: {e}") from e
    finally:
        if "conn" in locals():
            conn.close()


# --- group management (bridge group_manage.go) ---------------------------------


def _group_jid(value: str) -> str:
    jid = (value or "").strip()
    if jid.count("@") > 1:
        _require_allowed(jid)
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
    # Access reductions must remain possible without allowing a member's DM.
    if CHAT_POLICY.restricted and action in ("add", "promote"):
        for participant in cleaned:
            CHAT_POLICY.require_participant(participant, _participant_twins(participant))
    return _bridge_json(
        _bridge_request(
            "POST", "/group/participants", json={"group_jid": jid, "action": action, "participants": cleaned}
        )
    )


def _participant_twins(raw: str) -> list[str]:
    """Local identity aliases only; group updates do not resolve via WhatsApp."""
    target = normalize_recipient(raw).strip()
    validate_chat_target(target)
    jid = normalize_chat_entry(target)
    user, _, server = jid.rpartition("@")
    if server not in (DEFAULT_USER_SERVER, "lid"):
        return []
    conn = None
    try:
        conn = _connect_whatsmeow_db()
        if server == "lid":
            row = conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ?", (user,)).fetchone()
            if not row or not row[0]:
                return []
            user = row[0]
        users = [user]
        if alternate := br_mobile_alternate(user):
            users.append(alternate)
        rows = conn.execute(f"SELECT lid, pn FROM whatsmeow_lid_map WHERE pn IN ({_placeholders(users)})", users)
        return [value for lid, pn in rows for value in (f"{lid}@lid", f"{pn}@{DEFAULT_USER_SERVER}")]
    except sqlite3.Error:
        return []
    finally:
        if conn is not None:
            conn.close()


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


def archive_chat(chat_jid: str, archived: bool = True) -> dict[str, Any]:
    """Archive/unarchive through the bridge's app-state endpoint."""
    target = (chat_jid or "").strip()
    if "@" not in target or not all(target.rsplit("@", 1)):
        raise ToolError("invalid_argument", "chat_jid must be a full JID")
    _require_allowed(target)
    return _bridge_json(_bridge_request("POST", "/chat/archive", json={"chat_jid": target, "archived": archived}))


def send_typing(chat_jid: str, is_typing: bool = True) -> dict[str, Any]:
    """Show (or clear) the 'typing…' presence in a chat."""
    target = normalize_recipient((chat_jid or "").strip())
    if not target:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(target)
    return _bridge_json(_bridge_request("POST", "/typing", json={"recipient": target, "is_typing": bool(is_typing)}))


def edit_message(chat_jid: str, message_id: str, text: str, dry_run: bool = False) -> dict[str, Any]:
    """Edit an own message (WhatsApp accepts edits for ~15 minutes after sending).

    ``dry_run=True`` returns the request that would have been posted instead.
    """
    chat_jid, message_id = (chat_jid or "").strip(), (message_id or "").strip()
    if not chat_jid or not message_id:
        raise ToolError("invalid_argument", "chat_jid and message_id are required")
    if not (text or "").strip():
        raise ToolError("invalid_argument", "text must not be empty")
    _require_allowed(chat_jid)
    payload = {"chat_jid": chat_jid, "message_id": message_id, "text": text}
    if dry_run:
        return _dry_run("POST /api/edit", payload, **_recipient_preview(chat_jid))
    return _bridge_json(_bridge_request("POST", "/edit", json=payload))


def forward_message(chat_jid: str, message_id: str, to_chat_jid: str) -> dict[str, Any]:
    """Re-send a stored message (text or cached media with caption) to another chat."""
    chat_jid, message_id = (chat_jid or "").strip(), (message_id or "").strip()
    to = normalize_recipient((to_chat_jid or "").strip())
    if not chat_jid or not message_id or not to:
        raise ToolError("invalid_argument", "chat_jid, message_id and to_chat_jid are required")
    _require_allowed(chat_jid)
    _require_allowed(to)
    return _bridge_media_json(
        _bridge_request(
            "POST",
            "/forward",
            json={"chat_jid": chat_jid, "message_id": message_id, "to_chat_jid": to},
            timeout=BRIDGE_MEDIA_TIMEOUT_S,
        ),
        message_id,
        chat_jid,
    )


def _label_chat_jid(chat_jid: str) -> str:
    target = (chat_jid or "").strip()
    if (
        target.count("@") != 1
        or not all(target.rsplit("@", 1))
        or any(c in target.split("@", 1)[0] for c in ":. ")
        or target.rsplit("@", 1)[1].lower() not in {"s.whatsapp.net", "lid", "g.us"}
    ):
        raise ToolError("invalid_argument", "chat_jid must be a full JID")
    target = normalize_chat_entry(target)
    _require_allowed(target)
    return target


def _label_chat_aliases(target: str) -> list[str]:
    aliases = [target]
    user, server = target.rsplit("@", 1)
    lookup = {
        "s.whatsapp.net": ("lid", "pn", "lid"),
        "lid": ("pn", "lid", "s.whatsapp.net"),
    }.get(server)
    if lookup is None or not os.path.isfile(WHATSMEOW_DB_PATH):
        return aliases
    column, key, twin_server = lookup
    conn = None
    try:
        conn = _connect_whatsmeow_db()
        row = conn.execute(f"SELECT {column} FROM whatsmeow_lid_map WHERE {key} = ?", (user,)).fetchone()
        if row and row[0]:
            twin = f"{row[0]}@{twin_server}"
            if CHAT_POLICY.allows(twin):
                aliases.append(twin)
    except sqlite3.Error:
        pass
    finally:
        if conn is not None:
            conn.close()
    return aliases


def _labels_available(conn: sqlite3.Connection) -> bool:
    # Probe every time: an upgraded bridge can create the tables in its WAL
    # without changing the main database file's mtime or size.
    return bool(conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='labels'").fetchone())


def list_labels(chat_jid: str | None = None, include_deleted: bool = False) -> dict[str, Any]:
    target = _label_chat_jid(chat_jid) if chat_jid is not None else None
    query = "SELECT id, name, color, deleted, type, immutable, predefined_id FROM labels l WHERE (NOT deleted OR ?)"
    args: list[Any] = [include_deleted]
    if target is not None:
        aliases = _label_chat_aliases(target)
        # An older positive twin must not override a newer removal.
        query += (
            " AND (SELECT c.labeled FROM chat_labels c WHERE c.label_id=l.id"
            f" AND c.chat_jid IN ({_placeholders(aliases)})"
            " ORDER BY c.action_ms DESC, c.labeled ASC LIMIT 1)=1"
        )
        args.extend(aliases)
    conn = _connect_messages_db()
    try:
        if not _labels_available(conn):
            return {"labels": []}
        rows = conn.execute(query + " ORDER BY id", args).fetchall()
        return {
            "labels": [
                {
                    "id": id_,
                    "name": name,
                    "color": color,
                    "deleted": bool(deleted),
                    "type": type_,
                    "immutable": bool(immutable),
                    "predefined_id": predefined_id,
                }
                for id_, name, color, deleted, type_, immutable, predefined_id in rows
            ]
        }
    finally:
        conn.close()


def label_chat(chat_jid: str, label: str, labeled: bool = True) -> dict[str, Any]:
    target = _label_chat_jid(chat_jid)
    needle = label or ""
    if not needle.strip():
        raise ToolError("invalid_argument", "label must be an id or an exact name")
    conn = _connect_messages_db()
    try:
        if not _labels_available(conn):
            raise ToolError("not_found", "Unknown label; use list_labels")
        row = conn.execute("SELECT id, deleted, immutable FROM labels WHERE id=?", (needle,)).fetchone()
        if row is not None:
            if row[1]:
                raise ToolError("not_found", "Label is deleted")
            label_id = row[0]
            immutable = row[2]
        else:
            clean_needle = sanitize_name(needle)
            rows = [
                (id_, immutable)
                for id_, name, immutable in conn.execute("SELECT id, name, immutable FROM labels WHERE NOT deleted")
                if name == needle or sanitize_name(name) == clean_needle
            ]
            if not rows:
                raise ToolError("not_found", "Unknown label; use list_labels")
            if len(rows) != 1:
                raise ToolError("invalid_argument", "Ambiguous label name; use its id from list_labels")
            label_id = rows[0][0]
            immutable = rows[0][1]
        if immutable:
            raise ToolError("invalid_argument", "Label is immutable and cannot be applied or removed")
    finally:
        conn.close()
    return _bridge_json(
        _bridge_request("POST", "/chat/label", json={"chat_jid": target, "label_id": label_id, "labeled": labeled})
    )
