import base64
import hashlib
import json
import logging
import os
import os.path
import re
import sqlite3
import threading
import time
from collections.abc import Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from itertools import islice
from typing import Any, NamedTuple

import httpx

import audio
import endpoint_cert
import transcribe
from chat_policy import load_chat_policy, normalize_chat_entry
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
        timestamp=parse_db_time(timestamp),
        sender=sender,
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
    # Whether a stored message row backs last_message / last_sender /
    # last_is_from_me. False means the chat has no rows in `messages` at all
    # (history never synced, or every row was pruned), which is why the three
    # last_* fields are null — not "the last message could not be matched".
    has_messages: bool = False
    # Where `name` comes from: "chat" = stored in messages.db (the WhatsApp
    # chat title or group subject), "contacts" = your phone book
    # (whatsmeow_contacts) because the stored name was missing or just the
    # number, "push" = the name the contact gave themselves, because the phone
    # book had nothing either, "jid" = nothing better than the number exists.
    name_source: str = "chat"
    # The name the contact gave themselves, whatever `name` ended up being.
    # A cached snapshot of unknown age (whatsmeow_contacts has no timestamp).
    push_name: str | None = None

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

        A chat with no stored messages (`has_messages` false, so
        `last_is_from_me is None`) has no direction to go on — protocol and
        unsupported events advance last_message_time without storing a row —
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
    """

    phone: str | None
    lid: str | None


# E.164 allows 15 digits at most, so a longer bare identifier cannot be a phone
# number and is a LID on sight. At 15 and below the two overlap, and only the
# LID map tells them apart: a bare number it does not know stays a phone number,
# which is the deliberate limit of this classification — an unmapped 15-digit
# LID is indistinguishable from a (rare, but legal) 15-digit number, so it is
# reported as one rather than guessed away.
MAX_PHONE_DIGITS = 15


def _sender_identities(values: Sequence[str]) -> dict[str, SenderIdentity]:
    """Classify a batch of stored senders / identifiers in one LID-map query.

    Results are cached per value for NAME_CACHE_TTL_S like sender names, so a
    page with repeated senders — or the next page of the same chat — is free.
    """
    result: dict[str, SenderIdentity] = {}
    pending: dict[str, str] = {}  # value -> bare digits still to look up
    for value in values:
        if value in result or value in pending:
            continue
        hit, cached = _cache_get("identity", value)
        if hit:
            result[value] = cached
            continue
        bare, _, server = value.partition("@")
        if server == "lid" or (bare.isdigit() and server == ""):
            # An @lid JID says which namespace it is even when the user part
            # carries a device suffix ("1841...:3"); a bare number does not.
            pending[value] = bare
        else:
            # A phone JID, or something we cannot classify at all (an empty
            # sender, a group JID): it stays where it has always been.
            result[value] = _cache_put("identity", value, SenderIdentity(bare or value, None))
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
        is_lid = value.endswith("@lid") or bare in known_lids or len(bare) > MAX_PHONE_DIGITS
        identity = SenderIdentity(pn_by_lid.get(bare), bare) if is_lid else SenderIdentity(bare, None)
        result[value] = identity
        # Only an answer the map actually gave is cached. A classification made
        # while whatsapp.db was missing, locked or broken is a guess, and
        # caching it would report a LID as a phone number for the next five
        # minutes — the bug this exists to prevent (#281). Guessing costs no
        # I/O, so recomputing it on the next call is cheap.
        if map_read:
            _cache_put("identity", value, identity)
    return result


def sender_identity(value: str) -> SenderIdentity:
    """Phone/LID namespace of one sender or identifier (see :class:`SenderIdentity`)."""
    return _sender_identities([value])[value]


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

    hashes = [m.sha256 for m in messages if m.sha256 and m.media_type and not _is_pointer_row(m)]
    return fetch_notes(hashes) if hashes else {}


def fetch_sender_identities(messages: Sequence[Message]) -> dict[str, SenderIdentity]:
    """Sender namespaces for a batch of messages: {stored sender: SenderIdentity}.

    One LID-map query for a whole page instead of one per row; pass the result
    to msg_to_dict alongside the notes.
    """
    return _sender_identities([message.sender for message in messages if message.sender])


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
    # or nothing, `sender_lid` the anonymous link-ID (#281). `sender_jid` keeps
    # the value the bridge stored, whichever form that was.
    bare = message.sender.split("@", 1)[0]
    identity = (identities or {}).get(message.sender) or sender_identity(message.sender)
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

    is_media = bool(message.media_type) and not _is_pointer_row(message)
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
        "chat_name": message.chat_name,
        "media_type": message.media_type,
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
    """Convert a Chat dataclass to a dictionary for JSON serialization."""
    return {
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
CHAT_FIELDS: tuple[str, ...] = (*_CHAT_ROW_FIELDS, "content_truncated")
UNANSWERED_FIELDS: tuple[str, ...] = (*_CHAT_ROW_FIELDS, "last_inbound_time", "age_hours")

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
    )
    return f"{table_alias}.last_read_time" if has_column else "NULL"


def _has_mentions_column(cursor: sqlite3.Cursor) -> bool:
    """Whether the bridge that wrote this store records messages.mentions."""
    return _schema_memo(
        "messages.mentions",
        MESSAGES_DB_PATH,
        lambda: "mentions" in {row[1] for row in cursor.execute("PRAGMA table_info(messages)").fetchall()},
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


def _closing_message_clause(alias: str) -> tuple[str, list[str]]:
    """Rows that acknowledge rather than ask: a sticker, or one of CLOSING_MESSAGES.

    Reactions and poll votes are already excluded by `_spoken_filter`. Trailing
    "." and "!" are ignored so "ok!" reads the same as "ok"; SQLite's lower() is
    ASCII-only, which is all these words need and leaves the emoji untouched.

    Both columns are nullable and the caller negates this clause, so `IS` and
    COALESCE keep it two-valued: `NOT NULL` is NULL, which would drop every plain
    text message instead of keeping it.
    """
    placeholders = ",".join("?" * len(CLOSING_MESSAGES))
    clause = (
        f"({alias}.media_type IS 'sticker'"
        f" OR rtrim(lower(trim(COALESCE({alias}.content, ''))), '.!') IN ({placeholders}))"
    )
    return clause, list(CLOSING_MESSAGES)


def _last_message_join(chat_alias: str, msg_alias: str, spoken_only: bool = False) -> str:
    """Deterministic single-row join to the chat's newest stored message.

    The row is picked by ordering the chat's messages, never by matching
    `chats.last_message_time`: that marker is advanced by protocol and
    unsupported events that store no message row, and history sync writes
    second-resolution timestamps, so an equality join left the last_* fields
    NULL for a large share of chats (issue #218).

    Multiple messages can share a timestamp, so `id DESC` is the tie-break —
    joining on the timestamp alone would duplicate chat rows and make
    last_is_from_me / unread non-deterministic. The correlated subquery is
    driven by idx_messages_chat_timestamp and only runs for the rows the outer
    LIMIT actually returns.

    `spoken_only` skips reactions, poll votes and revoked messages, for callers
    that ask "who spoke last" rather than "what is the last row".
    """
    spoken = f"AND {_spoken_filter('m')}" if spoken_only else ""
    return f"""
            LEFT JOIN messages {msg_alias} ON {chat_alias}.jid = {msg_alias}.chat_jid
                AND {msg_alias}.id = (
                    SELECT m.id FROM messages m
                    WHERE m.chat_jid = {chat_alias}.jid
                    {spoken}
                    ORDER BY m.timestamp DESC, m.id DESC
                    LIMIT 1
                )
    """


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


def _in_chunks(values: list[str]) -> list[list[str]]:
    return [values[i : i + _SQL_IN_CHUNK] for i in range(0, len(values), _SQL_IN_CHUNK)]


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


def _apply_name_fallback(chats: list[Chat]) -> None:
    """Fill name/push_name/name_source from the phone book for a page of chats.

    `chats.name` is what WhatsApp pushed for the conversation; for a large share
    of direct chats that is empty or the bare number even when the contact is in
    the phone book (issue #230). `push_name` — the name the contact gave
    themselves — is attached whatever `name` ended up being, because it answers
    a different question (#280). Resolution is batched over the whole page.
    """
    direct = [chat for chat in chats if not chat.is_group]
    profiles = _contact_profiles([chat.jid for chat in direct]) if direct else {}
    for chat in chats:
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
            # The anchor is compared against the raw column, so it has to be
            # spelled the way the column is (timestamp_bound), not re-rendered
            # by isoformat().
            hit_params.extend([hit.id, hit.chat_jid, timestamp_bound(hit.timestamp)])
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


# Media rows the archive actually stores a file for. `reaction` and `poll_vote`
# also live in messages.media_type but are pointer rows (gotcha 3), never media.
MEDIA_TYPES = ("image", "video", "audio", "document", "sticker")
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
    # never holds; keep the JIDs.
    return [alias for alias in _sender_aliases(user) if "@" in alias]


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
        if _JID_SEPARATORS.search(entry):
            raise ToolError(
                "invalid_argument",
                f"{argument} takes one JID or a list of JIDs, not several joined into one string: "
                f'pass {argument}=["a@s.whatsapp.net", "b@g.us"] (got {entry!r})',
            )
        if require_allowed:
            _require_allowed(entry)
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

        if CHAT_POLICY.restricted:
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
        if media_type is not None and media_type not in MEDIA_TYPES:
            raise ToolError("invalid_argument", f"media_type must be one of {', '.join(MEDIA_TYPES)}")
        if media_type is not None:
            if self.has_media is False:
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
        media_type: One of image/video/audio/document/sticker (implies has_media=True)
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
        query_parts = [f"SELECT {MESSAGE_COLUMNS} FROM messages"]
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
    if group_by not in MESSAGE_STATS_GROUPINGS:
        raise ToolError("invalid_argument", f"group_by must be one of {', '.join(MESSAGE_STATS_GROUPINGS)}")
    limit = max(1, min(int(limit), MAX_STATS_BUCKETS))
    bucket_sql = _STATS_GROUP_SQL[group_by]
    # Resolve "who am I" before opening the database: it is a local read in the
    # normal case but can fall back to the bridge over HTTP, and a cursor must
    # never be held open across that.
    if mentions_me:
        owner_identity()

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
        base = f"FROM messages JOIN chats ON messages.chat_jid = chats.jid{join} {where}"
        # SUM(media) placeholders come first: they sit in the SELECT list, ahead
        # of the search join and the WHERE.
        params = [*MEDIA_TYPES, *bound]
        if match_index is not None:
            match_index += len(MEDIA_TYPES)

        label = "MIN(chats.name)" if group_by == "chat" else "NULL"
        _execute_message_sql(
            cur,
            f"SELECT {bucket_sql}, {label}, {_STATS_AGGREGATES} {base} "
            f"GROUP BY {bucket_sql} ORDER BY COUNT(*) DESC, {bucket_sql} DESC LIMIT ?",
            [*params, limit],
            match_index,
            query,
        )
        rows = cur.fetchall()
        _execute_message_sql(
            cur, f"SELECT COUNT(DISTINCT {bucket_sql}), {_STATS_AGGREGATES} {base}", list(params), match_index, query
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
            bucket["label"] = chat_name or key
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


def _chat_filter(query: str | None) -> tuple[list[str], list[Any]]:
    """WHERE clauses selecting the chats a caller may see: name/JID search plus the allow-list.

    Shared by list_chats_page and count_chats so the count is taken over exactly
    the rows the page would return.
    """
    clauses: list[str] = []
    params: list[Any] = []
    if query:
        # instr() on the raw column matches Unicode; LOWER()+LIKE only covers ASCII.
        clauses.append("(instr(LOWER(chats.name), LOWER(?)) > 0 OR instr(chats.name, ?) > 0 OR chats.jid LIKE ?)")
        params.extend([query, query, f"%{query}%"])
    if CHAT_POLICY.restricted:
        clause, clause_params = CHAT_POLICY.sql_clause("chats.jid")
        clauses.append(clause)
        params.extend(clause_params)
    return clauses, params


def count_chats(query: str | None = None) -> int:
    """How many chats match the list_chats filter, without returning any of them."""
    clauses, params = _chat_filter(query)
    where = f" WHERE {' AND '.join(clauses)}" if clauses else ""
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
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

    Returns:
        List of chat dictionaries with jid, name, is_group, last_message, etc.
    """
    cursor_state = decode_cursor(cursor, "chats")
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()

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
                {_last_read_time_select(cur, "chats")},
                messages.id IS NOT NULL as has_messages
            FROM chats
            {_last_message_join("chats", "messages")}
        """
        ]

        where_clauses, params = _chat_filter(query)

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
                state["n"] = last[1]
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


def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search contacts by name or phone number.

    Searches both the messages.db chats table and whatsmeow's contact store
    (whatsapp.db) to find contacts. Results are deduplicated by JID, and each
    one says which field the query matched (#280), because a contact saved as
    "Z Aa" is found by the name they gave themselves.
    """
    seen_jids: set[str] = set()
    # (jid, name, push name if this hit carried one, fields the query could
    # have matched in reporting order)
    found: list[tuple[str, str | None, str | None, list[tuple[str, str | None]]]] = []
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
                # No push name on a chats-table row; the phone book below has it.
                found.append((jid, name, None, [("name", name)]))
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
        rows.append({**contact_to_dict(contact), "matched": _matched_field(query, candidates)})
    return rows


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

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    # An empty (or bare "@lid") argument would otherwise reach the SQL as an
    # empty alias and match every row whose address form the bridge left empty.
    if not (jid or "").strip().split("@", 1)[0]:
        raise ToolError("invalid_argument", "a contact JID or phone number is required")
    cursor_state = decode_cursor(cursor, "contact_chats")
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()

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
            WITH spoke_in AS (
                SELECT DISTINCT chat_jid FROM messages WHERE sender IN ({placeholders})
            ),
            member_of AS ({member_of}),
            direct_chat AS (
                SELECT jid FROM chats WHERE jid IN ({placeholders})
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
            (*aliases, *member_params, *aliases, *policy_params, *keyset_params, limit + 1, offset),
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
                {_last_read_time_select(cursor, "c")},
                m.id IS NOT NULL as has_messages
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
            last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
            has_messages=bool(chat_data[7]),
        )
        _apply_name_fallback([chat])
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
                {_last_read_time_select(cursor, "c")},
                m.id IS NOT NULL as has_messages
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
            last_message_time=parse_db_time(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5],
            last_read_time=parse_db_time(chat_data[6]) if chat_data[6] else None,
            has_messages=bool(chat_data[7]),
        )
        _apply_name_fallback([chat])
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


DRY_RUN_MESSAGE = "Dry run: nothing was sent. Show this to the user and call again with dry_run=false to send."


def _chat_name(jid: str) -> str | None:
    """Best-effort display name for a dry-run preview; never fails the preview."""
    try:
        chat = get_chat(jid, include_last_message=False)
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


def send_file(
    recipient: str, media_path: str, caption: str = "", dry_run: bool = False
) -> tuple[bool, str, dict[str, Any]]:
    """Send a media file (image, video, document) with an optional caption.

    The bridge populates the WA media-message Caption field from `message`, so
    passing both in one /api/send call produces a single attachment-with-caption
    message instead of two separate messages.

    ``dry_run=True`` runs the same validation (recipient, allow-list, the file
    exists) and returns the request that would have been posted.
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
    if dry_run:
        # The bridge additionally confines media_path to WHATSAPP_MEDIA_ROOTS,
        # which only it knows; report what this side can check.
        media = {"path": os.path.abspath(media_path), "exists": True, "bytes": os.path.getsize(media_path)}
        return True, DRY_RUN_MESSAGE, _dry_run("POST /api/send", payload, media=media, **_recipient_preview(recipient))
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
    group_jid = (group_jid or "").strip()
    if not group_jid.endswith("@g.us"):
        raise ToolError("invalid_argument", f"Not a group JID: {group_jid!r} (expected ...@g.us)")
    _require_allowed(group_jid)
    limit = max(1, min(int(limit), GROUP_MEMBERS_MAX_LIMIT))
    cursor_state = decode_cursor(cursor, "group_members")
    offset = max(0, int(page)) * limit
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
    """
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")
    _require_allowed(chat_jid)

    payload: dict[str, Any] = {"chat_jid": chat_jid}
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
        payload["message_ids"] = normalized_ids
        if sender_jid:
            payload["sender_jid"] = sender_jid
    if timestamp:
        payload["timestamp"] = timestamp

    result = _bridge_json(_bridge_request("POST", "/mark-read", json=payload))
    return {
        "success": True,
        "message": result.get("message") or "Marked as read",
        "messages": int(result.get("messages") or 0),
        "senders": int(result.get("senders") or 0),
        "batches": int(result.get("batches") or 0),
        "truncated": bool(result.get("truncated", False)),
    }


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
) -> dict[str, Any]:
    """One page of the per-chat work queue for request_history."""
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
    depth = """(SELECT COUNT(*) FROM (
                    SELECT 1 FROM messages AS probe WHERE probe.chat_jid = chats.jid LIMIT 2))"""
    overall_first = "(SELECT MIN(timestamp) FROM messages AS whole WHERE whole.chat_jid = chats.jid)"
    join_on = " AND ".join(["messages.chat_jid = chats.jid", *window])
    cur.execute(
        f"""SELECT chats.jid, chats.name,
                   MIN(messages.timestamp) AS first_time,
                   MAX(messages.timestamp) AS last_time,
                   COUNT(messages.id) AS stored,
                   {depth} AS depth,
                   {overall_first} AS overall_first
              FROM chats LEFT JOIN messages ON {join_on}
             WHERE {chat_where}
             GROUP BY chats.jid, chats.name
             ORDER BY depth, overall_first DESC, chats.jid
             LIMIT ? OFFSET ?""",
        (*window_params, *chat_params, limit + 1, offset),
    )
    rows = cur.fetchall()
    has_more = len(rows) > limit
    rows = rows[:limit]

    placeholders = [jid for jid, name, *_ in rows if _is_placeholder_name(name)]
    names = _contact_names(placeholders) if placeholders else {}
    items = [
        {
            "chat_jid": jid,
            "name": names.get(jid, name),
            "first_message_time": first_time,
            "last_message_time": last_time,
            "messages": int(stored or 0),
            "stub_only": depth == 1,
        }
        for jid, name, first_time, last_time, stored, depth, _overall_first in rows
    ]
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
    describes the allowed chats only.
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
    try:
        limit = int(limit)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"limit must be an integer, got {limit!r}") from exc
    limit = max(1, min(limit, COVERAGE_BY_CHAT_MAX_LIMIT))
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

        if by_chat:
            return _coverage_by_chat(
                cur, scope, window, window_params, chat_clause, chat_params, cursor=cursor, limit=limit
            )

        cur.execute(
            f"SELECT COUNT(*), MIN(timestamp), MAX(timestamp) FROM messages WHERE {msg_clause}",
            tuple(msg_params),
        )
        total_messages, first_time, last_time = cur.fetchone()

        cur.execute(f"SELECT COUNT(*) FROM chats WHERE {chat_clause}", tuple(chat_params))
        chats_total = int(cur.fetchone()[0] or 0)
        cur.execute(
            f"""SELECT COUNT(*) FROM chats
                 WHERE {chat_clause}
                   AND NOT EXISTS (
                       SELECT 1 FROM messages WHERE messages.chat_jid = chats.jid{window_where})""",
            (*chat_params, *window_params),
        )
        chats_without_messages = int(cur.fetchone()[0] or 0)

        cur.execute(
            f"""SELECT substr(timestamp, 1, 7) AS month, COUNT(*)
                  FROM messages WHERE {msg_clause}
                 GROUP BY month ORDER BY month""",
            tuple(msg_params),
        )
        messages_by_month = {month: int(count) for month, count in cur.fetchall() if month}

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
        "gap_hours": gap_hours,
        "gaps": gaps,
        "gaps_truncated": len(gaps) >= max_gaps,
        "scope": scope,
        "allow_list_applied": CHAT_POLICY.restricted,
        "hint": _coverage_hint(scope),
    }


def _coverage_hint(scope: dict[str, Any]) -> str:
    """How to read the aggregates — different once a window narrows them.

    Unbounded, first_message_time is where the archive itself begins and
    chats_without_messages means "never synced". Under after/before both
    describe the window and nothing else, so saying otherwise would have the
    agent report the period before the bound as never synced.
    """
    windowed = scope["after"] is not None or scope["before"] is not None
    middle = (
        "after/before are set, so first_message_time, chats_without_messages and the gaps describe that window "
        "alone — they say nothing about what the archive holds outside it. Drop the bounds to ask that."
        if windowed
        else "Chats in chats_without_messages, and anything before first_message_time, were never synced either. "
        "Narrow with after/before/chat_jid to keep old sync artefacts out of the list."
    )
    return (
        "Gaps cover every chat in scope: no message at all between 'from' and 'to', which usually means the bridge "
        f"was down or never synced that period rather than everyone going quiet. {middle} Call "
        "coverage(by_chat=True) for the queue of chats to backfill with request_history(chat_jid); it cannot fill "
        "a period the phone itself no longer has."
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


def owner_identity() -> dict[str, str | None]:
    """{"jid", "phone", "lid"} of the account this deployment is logged in as.

    Raises ToolError("bridge_unavailable") when neither the local store nor the
    bridge can say: "who am I" has no useful empty answer, and a mentions_me
    filter that silently matched nothing would read as "nobody mentioned you".
    """
    global _owner_cache
    now = time.monotonic()
    with _owner_lock:
        if _owner_cache is not None and now - _owner_cache[0] < _OWNER_CACHE_TTL_S:
            return dict(_owner_cache[1])

    owner = _owner_from_device_table() or _owner_from_bridge()
    with _owner_lock:
        _owner_cache = (now, dict(owner))
    return dict(owner)


def bridge_status() -> dict[str, Any]:
    """Health, readiness and build identity of the bridge in one call.

    Never raises for a bridge that is down: returns ok=false with the reason,
    so the agent can tell "bridge unreachable" from "nothing matched".
    """
    status: dict[str, Any] = {"ok": False, "bridge_url": WHATSAPP_API_BASE_URL, "whisper": _whisper_status()}
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
    """Optional predicates shared by the unread queries.

    Returns an AND-prefixed SQL fragment (empty when nothing is filtered) and
    its parameters, for a query that has both `messages` and `chats` in scope.
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

    clauses: list[str] = []
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

    count_only returns {"count", "chats_with_unread"} over *every* matching
    chat, ignoring limit_chats and limit_per_chat, and reads no message row.
    """
    limit_chats = max(1, min(int(limit_chats), 100))
    limit_per_chat = max(1, min(int(limit_per_chat), 50))
    filter_clause, filter_params = unread_filters(since, max_age_days, exclude_groups, chat_jid, exclude_chat_jid)
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        read_marker = _last_read_time_select(cursor, "chats")
        policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
        spoken_filter = _spoken_filter("messages")
        unread_where = f"""
            FROM messages
            JOIN chats ON chats.jid = messages.chat_jid
            WHERE messages.is_from_me = 0
              AND ({read_marker} IS NULL OR messages.timestamp > {read_marker})
              AND {spoken_filter}
              AND {policy_clause}
              {filter_clause}
            """
        if count_only:
            cursor.execute(
                f"SELECT COUNT(*), COUNT(DISTINCT chats.jid) {unread_where}", (*policy_params, *filter_params)
            )
            counted = cursor.fetchone() or (0, 0)
            return {"count": int(counted[0] or 0), "chats_with_unread": int(counted[1] or 0)}
        params: list[Any] = [*policy_params, *filter_params, limit_chats]
        cursor.execute(
            f"""
            SELECT chats.jid, chats.name, {read_marker} AS last_read_time,
                   COUNT(*) AS unread_count, MAX(messages.timestamp) AS latest_unread
            {unread_where}
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
                  AND {spoken_filter}
                  {filter_clause}
                ORDER BY messages.timestamp DESC, messages.id DESC
                LIMIT ?
                """,
                (jid, *filter_params, limit_per_chat),
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
                    "messages": [_row_to_message(row) for row in reversed(rows)],
                }
            )
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
    ).items


def _unanswered_from_where(
    since: str | None,
    exclude_groups: bool,
    min_age_hours: float,
    chat_jid: str | Sequence[str] | None = None,
    exclude_chat_jid: str | Sequence[str] | None = None,
    ignore_closing_messages: bool = False,
) -> tuple[str, list[Any]]:
    """FROM/WHERE shared by the list_unanswered page and its count."""
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
    policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    sql = f"""
            FROM chats
            {_last_message_join("chats", "messages", spoken_only=True)}
            WHERE messages.is_from_me = 0
              AND {policy_clause}
              {filter_clause} {age_clause} {closing_clause}
            """
    return sql, [*policy_params, *filter_params, *age_params, *closing_params]


def _install_triage_filter(
    conn: sqlite3.Connection, hide_handled: bool, exclude_muted: bool, include_snoozed: bool
) -> tuple[str, list[Any]]:
    """The handled/snoozed/muted predicate, applied before LIMIT so counts and pages agree.

    triage imports this module, so the import lives here rather than at the top.
    """
    from triage import install_filter

    return install_filter(conn, hide_handled, exclude_muted, include_snoozed)


# --- group mentions in triage --------------------------------------------------
#
# "The last message is inbound" is the wrong question in a group: groups are
# always inbound. What waits for an answer there is a mention of the owner that
# nobody answered — newer than the owner's own last word in that group. The
# ordinary rule already returns most of those chats (a mention is itself an
# inbound message), so include_group_mentions does two things: it flags the
# rows that actually addressed you, and it adds the groups the age bound hid
# because the group kept talking after the mention.


def _pending_mention_where(cur: sqlite3.Cursor, alias: str, chat_jid_expr: str) -> tuple[str, list[Any]]:
    """`alias` is an inbound mention of the owner, newer than the owner's last word.

    "Word" on both sides means a spoken message (_spoken_filter): a thumbs-up
    from you does not answer a question, and a mention you revoked is not one
    any more — the same rule list_unanswered already applies to everything else.
    """
    mention_clause, mention_params = mentions_me_predicate(cur, alias)
    sql = f"""{alias}.is_from_me = 0 AND {_spoken_filter(alias)} AND {mention_clause}
              AND {alias}.timestamp > COALESCE((SELECT MAX(own.timestamp) FROM messages own
                                                 WHERE own.chat_jid = {chat_jid_expr}
                                                   AND own.is_from_me = 1 AND {_spoken_filter("own")}), '')"""
    return sql, mention_params


def newest_pending_mentions(
    cur: sqlite3.Cursor,
    chat_jids: Sequence[str],
    bounds: Sequence[tuple[str, str]] = (),
) -> dict[str, tuple[str, str]]:
    """{chat_jid: (message_id, timestamp)} of the newest unanswered mention per chat.

    One row per chat (MAX picks the bare columns with it), for the chats given,
    so the annotation costs one bounded query per page. `bounds` carries the
    same time bounds the page was built with — a row anchored on a mention must
    name that mention, not a newer one the bound excluded.
    """
    if not chat_jids:
        return {}
    where, params = _pending_mention_where(cur, "messages", "messages.chat_jid")
    bound_sql = "".join(f" AND messages.timestamp {op} ?" for op, _ in bounds)
    placeholders = ",".join("?" * len(chat_jids))
    cur.execute(
        f"""SELECT messages.chat_jid, messages.id, MAX(messages.timestamp)
            FROM messages
            WHERE messages.chat_jid IN ({placeholders}) AND {where}{bound_sql}
            GROUP BY messages.chat_jid""",
        (*chat_jids, *params, *(value for _, value in bounds)),
    )
    return {chat: (message_id, timestamp) for chat, message_id, timestamp in cur.fetchall()}


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
) -> tuple[list[tuple], list[tuple[str, str]]]:
    """Groups waiting on a mention that the ordinary unanswered rule does not return.

    Same row shape and order as the main query, anchored on the mention instead
    of on the chat's newest message — that anchor is the point: a group that
    kept talking after the mention is dropped by min_age_hours even though the
    mention itself has been waiting for days. Chats the ordinary rule already
    returns are left to it (the COALESCE(last_spoken…) clause), so the two
    streams never describe the same chat twice, on this page or the next.
    """
    filter_clause, filter_params = unread_filters(since, None, False, chat_jid, exclude_chat_jid)
    # unread_filters bounds `messages`, which is the mention row here.
    mention_where, mention_params = _pending_mention_where(cur, "messages", "chats.jid")
    policy_clause, policy_params = CHAT_POLICY.sql_clause("chats.jid")
    age_clause, age_params = "", []
    if float(min_age_hours or 0) > 0:
        age_clause = "AND messages.timestamp <= ?"
        age_params = [timestamp_bound(datetime.now(UTC) - timedelta(hours=float(min_age_hours)))]
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
          {filter_clause} {age_clause}
          AND COALESCE(last_spoken.is_from_me = 0 {covered_bounds}, 0) = 0
        GROUP BY chats.jid
        {keyset_clause}
        ORDER BY MAX(messages.timestamp) DESC, chats.jid ASC
        LIMIT ?
        """,
        (*policy_params, *mention_params, *filter_params, *age_params, *covered_params, *keyset_params, limit),
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
) -> int:
    """How many chats are waiting for a reply, without returning any of them."""
    from_where, params = _unanswered_from_where(
        since, exclude_groups, min_age_hours, chat_jid, exclude_chat_jid, ignore_closing_messages
    )
    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
        triage_clause, triage_params = _install_triage_filter(conn, hide_handled, exclude_muted, include_snoozed)
        cur.execute(f"SELECT COUNT(*) {from_where} {triage_clause}", (*params, *triage_params))
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
    """
    limit = max(1, min(int(limit), 200))
    cursor_state = decode_cursor(cursor, "unanswered")
    from_where, where_params = _unanswered_from_where(
        since, exclude_groups, min_age_hours, chat_jid, exclude_chat_jid, ignore_closing_messages
    )
    # Resolve "who am I" before opening the database (see list_messages_page).
    if include_group_mentions:
        owner_identity()

    try:
        conn = _connect_messages_db()
        cur = conn.cursor()
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
            SELECT chats.jid, chats.name, chats.last_message_time,
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
                )
            rows = _merge_by_anchor(rows, extra, limit + 1)
            # Same bounds as the rows: the mention named here is the one the
            # page was built around, never a newer one the bound left out.
            mentions_by_chat = newest_pending_mentions(cur, [row[0] for row in rows], bounds)
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
        _apply_name_fallback(page_chats)
        items = []
        for chat, row in zip(page_chats, rows, strict=True):
            item = chat_to_dict(chat)
            item["last_inbound_time"] = row[8]
            item["age_hours"] = _hours_since(row[8])
            if include_group_mentions:
                pending = mentions_by_chat.get(chat.jid)
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
