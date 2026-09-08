import logging
import os
import signal
import sys
from collections.abc import Sequence
from typing import Any

from mcp.server.mcpserver import MCPServer
from mcp_types import ContentBlock

from errors import ToolError, tool_errors
from export import export_messages as export_messages_to_disk
from http_auth import (
    BearerTokenMiddleware,
    RateLimitMiddleware,
    resolve_http_token,
    resolve_max_body_bytes,
    resolve_rate_limit,
)
from mcp_config import build_transport_security, resolve_host, resolve_port, resolve_transport
from media_image import DEFAULT_MAX_EDGE, DEFAULT_QUALITY
from media_inventory import list_media_page, media_stats
from media_notes import TRANSCRIPT_BACKEND_KEY, TRANSCRIPT_KEY, TRANSCRIPT_LANG_KEY
from media_notes import annotate_media as notes_annotate_media
from media_notes import get_media_notes as notes_get_media_notes
from media_notes import search_media_notes as notes_search_media_notes
from media_notes import store_transcript as notes_store_transcript
from media_read import cached_only_path as media_cached_only_path
from media_read import content_tool
from media_read import read_media as media_read_bytes
from media_resource import MediaResourceServer, attach_resource_links
from notes import annotate as notes_annotate
from notes import attach_notes
from notes import compact as notes_compact
from notes import get_notes as notes_get_notes
from notes import search_notes as notes_search_notes
from observability import JSON_FORMAT_ENV, METRICS_TOKEN_ENV, MetricsMiddleware, log_formatter, metrics_enabled
from parent_watchdog import install_stdio_parent_watchdog
from tool_policy import (
    apply_tool_policy,
    load_tool_policy,
    mutating_tool,
    offers_download,
    registered_tool_names,
    set_active_policy,
)
from transcribe import TranscriptionError, transcribe_file
from transcribe import load_config as load_whisper_config
from transcribe_worker import install_ingest_worker
from triage import mark_handled as triage_mark_handled
from triage import snooze as triage_snooze
from untrusted import WRAP_ENV, parse_wrap_env, untrusted_content
from whatsapp import (
    CHAT_FIELDS,
    MESSAGES_MAX_LIMIT,
    UNANSWERED_FIELDS,
    UNANSWERED_MENTION_FIELDS,
    attach_message_notes,
    fetch_media_notes,
    fetch_sender_identities,
    msg_to_dict,
    page_size,
    prefetch_sender_names,
    sender_identity,
    shape_rows,
    stored_sender_namespace,
    unknown_lid_digits,
)
from whatsapp import (
    _read_bridge_token as whatsapp_read_bridge_token,
)
from whatsapp import (
    bridge_status as whatsapp_bridge_status,
)
from whatsapp import (
    contact_profile as whatsapp_contact_profile,
)
from whatsapp import (
    count_chats as whatsapp_count_chats,
)
from whatsapp import (
    count_messages as whatsapp_count_messages,
)
from whatsapp import (
    count_unanswered as whatsapp_count_unanswered,
)
from whatsapp import (
    coverage as whatsapp_coverage,
)
from whatsapp import (
    delete_message as whatsapp_delete_message,
)
from whatsapp import (
    download_media as whatsapp_download_media,
)
from whatsapp import (
    edit_message as whatsapp_edit_message,
)
from whatsapp import (
    forward_message as whatsapp_forward_message,
)
from whatsapp import (
    get_chat as whatsapp_get_chat,
)
from whatsapp import (
    get_contact_chats_page as whatsapp_get_contact_chats,
)
from whatsapp import (
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
)
from whatsapp import (
    get_group_invite_link as whatsapp_get_group_invite_link,
)
from whatsapp import (
    get_group_members as whatsapp_get_group_members,
)
from whatsapp import (
    get_last_interaction as whatsapp_get_last_interaction,
)
from whatsapp import (
    get_message_context as whatsapp_get_message_context,
)
from whatsapp import (
    get_poll_results as whatsapp_get_poll_results,
)
from whatsapp import (
    get_sender_name as whatsapp_get_sender_name,
)
from whatsapp import (
    leave_group as whatsapp_leave_group,
)
from whatsapp import (
    list_chats_page as whatsapp_list_chats,
)
from whatsapp import (
    list_messages_page as whatsapp_list_messages,
)
from whatsapp import (
    list_unanswered_page as whatsapp_list_unanswered,
)
from whatsapp import (
    list_unread as whatsapp_list_unread,
)
from whatsapp import (
    manage_group_participants as whatsapp_manage_group_participants,
)
from whatsapp import (
    mark_messages_read as whatsapp_mark_messages_read,
)
from whatsapp import (
    media_notes_for_message as whatsapp_media_notes_for_message,
)
from whatsapp import (
    message_stats as whatsapp_message_stats,
)
from whatsapp import (
    purge_media as whatsapp_purge_media,
)
from whatsapp import (
    request_history as whatsapp_request_history,
)
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
)
from whatsapp import (
    send_audio_message as whatsapp_audio_voice_message,
)
from whatsapp import (
    send_file as whatsapp_send_file,
)
from whatsapp import (
    send_message as whatsapp_send_message,
)
from whatsapp import (
    send_reaction as whatsapp_send_reaction,
)
from whatsapp import (
    send_typing as whatsapp_send_typing,
)
from whatsapp import (
    update_group as whatsapp_update_group,
)

# Initialize the MCP server. Env-var handling is deferred to the __main__ block
# so importing this module never parses env vars or exits the process. With SDK
# v2, host/port/transport security are passed to run() rather than stored on the
# server, so nothing network-related is decided at import time either.
MCP_VERSION = (os.getenv("WHATSAPP_MCP_VERSION") or "dev").strip()
# MediaResourceServer, not MCPServer: the StrictArgumentServer (the SDK drops
# undeclared arguments silently, so a misspelled or invented parameter would be
# ignored instead of reported — strict_args.py) plus the
# whatsapp://media/<chat_jid>/<message_id> resource scheme (media_resource.py).
mcp = MediaResourceServer("whatsapp", version=MCP_VERSION)


@mcp.tool()
@tool_errors
def bridge_status() -> dict[str, Any]:
    """Health of the WhatsApp bridge: is it reachable, paired and connected, and which build runs.

    Call this first when a tool returns nothing you expected or reports
    bridge_unavailable. Returns ok=true when the bridge is paired and connected;
    otherwise ok=false with a human-readable reason (unreachable, awaiting QR
    pairing, disconnected). Also reports uptime_seconds, store_bytes / media_bytes /
    media_files (cache size), the build (version, commit, go, whatsmeow, fts5) and
    whisper: {configured, backend ("url" | "bin"), reachable, model, on_ingest} —
    check it before planning transcription work, because with configured=false every
    transcribe_audio call fails and voice notes stay unreadable on this deployment.
    With WHATSAPP_PUBLIC_URL set it also reports endpoint_cert_expires_at and
    endpoint_cert_days_left for the published HTTPS endpoint (endpoint_cert_error
    when the handshake fails): a low or negative days_left is why other clients
    cannot connect while this session still works.
    Read-only; never fails, so it is safe to call before anything else.
    """
    return whatsapp_bridge_status()


@mcp.tool()
@tool_errors
@untrusted_content
def coverage(
    gap_hours: float = 24.0,
    max_gaps: int = 20,
    after: str | None = None,
    before: str | None = None,
    chat_jid: str | list[str] | None = None,
    by_chat: bool = False,
    cursor: str | None = None,
    limit: int = 50,
) -> dict[str, Any]:
    """What the local archive actually contains, and the periods it is missing.

    Call this before concluding "this chat has been quiet" or "nothing happened
    that week". The archive only holds what the phone pushed at pair time plus
    what arrived since, so it can start late, skip a period the bridge was down,
    and hold chats with metadata but no messages at all. Those holes are
    invisible in list_messages, which simply returns nothing.

    Returns first_message_time and last_message_time (the archive's real
    boundaries), total_messages, chats_total / chats_with_messages /
    chats_without_messages, messages_by_month ({"2026-06": 1234}) to see where
    coverage thins out, gaps (periods longer than gap_hours with no message in
    *any* chat in scope, biggest first, as {"from", "to", "hours"}) and scope,
    the window and chat filter that produced them. A gap means the bridge stored
    nothing at all then, not that your contacts were silent.

    audio is the voice-note side of the same scope: {messages, cached,
    transcribed, errors, backlog, backlog_cached, cached_examined} — inbound
    voice notes stored, how many have their bytes on disk, how many already have
    a transcript (or a recorded failure) in the notes, and backlog = the rest,
    what transcribe_audio or TRANSCRIBE_ON_INGEST would still work through. Ask
    it before starting a batch: backlog - backlog_cached is how many of those
    would have to be downloaded first. cached_examined equals messages unless an
    archive-wide call hit the scan ceiling, in which case the two cached counts
    are floors over the newest voice notes.

    after/before/chat_jid narrow every number, the gap scan included: scope the
    question to the period you care about instead of reading past sync artefacts
    out of a whole-archive answer. Under a window those numbers describe the
    window and nothing else — first_message_time is then the first message after
    the bound, not the moment the archive begins, and chats_without_messages
    means "nothing in this period" rather than "never synced". The returned hint
    says which of the two readings applies. The bounds also act as gap edges, so
    an empty stretch between `after` and the first stored message shows up, and
    a window falling entirely inside an outage comes back as one gap covering
    it rather than as no gaps at all.

    by_chat=True answers a different question — *which* chats to backfill. It
    returns {"items", "next_cursor", "has_more"} where each item is
    {chat_jid, name, first_message_time, last_message_time, messages,
    stub_only}, ordered as a work queue: chats with nothing stored, then
    stub_only ones (their whole stored history is a single row, usually the
    pair-time history-sync stub), then the chats whose history starts latest.
    That order and stub_only always describe each chat's whole history, even
    with after/before set — a window scopes the counts and boundaries, but it
    cannot tell a quiet week from a chat that never synced. Feed the JIDs to
    request_history, which needs one stored message to anchor on — so a chat
    with messages: 0 cannot be backfilled until something arrives there.

    Read-only, computed from messages.db, so it works while the bridge is down.
    With WHATSAPP_ALLOWED_CHATS set, every number covers the allowed chats only
    (allow_list_applied says so).

    Args:
        gap_hours: Report periods with no message longer than this (default 24)
        max_gaps: Largest N gaps to return (default 20, capped at 500)
        after: Only count messages after this ISO-8601 moment
        before: Only count messages before this ISO-8601 moment
        chat_jid: One chat JID, or a list of them, to report on instead of every chat
        by_chat: Return the paginated per-chat queue instead of the aggregates
        cursor: next_cursor from the previous by_chat page
        limit: Chats per by_chat page (default 50, max 200)
    """
    return whatsapp_coverage(
        gap_hours=gap_hours,
        max_gaps=max_gaps,
        after=after,
        before=before,
        chat_jid=chat_jid,
        by_chat=by_chat,
        cursor=cursor,
        limit=limit,
    )


@mcp.tool()
@tool_errors
@mutating_tool
def request_history(chat_jid: str, count: int = 50) -> dict[str, Any]:
    """Ask the phone to send older messages for one chat, to fill a gap in the archive.

    The archive only holds what the phone pushed at pair time plus everything
    that arrived since, so a chat can start abruptly or miss a period. This asks
    WhatsApp for messages *older* than the oldest one already stored for that
    chat; call it again to page further back, since the anchor moves back as
    older messages land.

    The result is asynchronous. A successful call means "the request was sent",
    not "the messages are here": they arrive later (usually seconds, sometimes
    not at all) through history sync, and the phone decides how much it actually
    returns — count is a request, not a guarantee. Messages the phone itself has
    deleted are gone for good. To verify, note
    list_messages(chat_jid=..., sort_by="oldest", limit=1) before the call and
    compare that first message's timestamp a few seconds after: older means the
    backfill landed, unchanged means the phone sent nothing.

    Errors: not_found means the chat has no stored message to anchor on (send or
    receive one message there first); bridge_unavailable means the bridge is not
    connected to WhatsApp right now.

    Args:
        chat_jid: Chat to backfill (direct-chat JID ...@s.whatsapp.net or group JID ...@g.us)
        count: Messages to request, 1-500 (default 50; higher values are capped at 500)

    Returns:
        {"success": true, "chat_jid", "requested_count", "message", "note"}
    """
    return whatsapp_request_history(chat_jid, count)


@mcp.tool()
@tool_errors
@untrusted_content
def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search WhatsApp contacts by name, push name or phone number.

    Each hit carries jid, name, push_name, phone_number, lid and `matched`. The
    two identifier namespaces stay apart: a contact WhatsApp only knows
    anonymously has phone_number null and its LID in `lid`.

    `name` is what this account knows them by, `push_name` the name they gave
    themselves, so someone saved as "Z Aa" is found by searching "Alena".
    `matched` names the field the query hit: "name" (the chat name this account
    stored), "full_name", "push_name", "first_name" or "business_name" (the
    phone-book fields behind `name`), or "jid". The three that are not returned
    as keys are still reported by name, so "found by their business name" is
    not mistaken for "found by the name you saved". It is null when nothing in
    those fields contains the query literally — a wildcard search that only the
    JID pattern matched. A push name is a cached snapshot with no date
    attached: present it as "recorded as", never as "is".

    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts


@mcp.tool()
@tool_errors
@untrusted_content
def get_contact(identifier: str) -> dict[str, Any]:
    """Look up a WhatsApp contact by phone number, LID, or full JID.

    Detects which namespace the identifier belongs to. A bare number is
    ambiguous — phone numbers and LIDs overlap in length — so the archive
    answers first (every message the bridge stores records its sender's
    namespace), then whatsmeow's LID map; an identifier too long to be a phone
    number (over 15 digits) is a LID on sight, and one of 14 or 15 digits that
    no chat, message or contact has ever carried is reported as a LID too.
    `phone_number` therefore never holds a LID: for one it is the mapped
    number, or null when the map has never seen that LID.
    (An identifier that is neither a number nor a LID — a name, another
    server's JID — is echoed back in `phone_number` as it always was.) A LID
    nobody can name returns `name: null` rather than its own digits: nobody
    knows who it is.

    `name` is what this account knows them by; `push_name` is the name the
    contact gave themselves, a cached snapshot with no date attached — present
    it as "recorded as", never as "is".

    Args:
        identifier: Phone number, LID, or full JID. Examples:
                    - "12025551234" (phone number)
                    - "35047067385985" (LID - numeric)
                    - "12025551234@s.whatsapp.net" (phone JID)
                    - "184125298348272@lid" (LID JID)

    Returns:
        Dictionary with jid, phone_number, lid, name, push_name, display_name,
        is_lid and resolved status
    """
    identifier = (identifier or "").strip()
    if not identifier:
        raise ValueError("identifier must be non-empty")

    # Detect identifier type and normalize to JID.
    bare_numeric_digits: str | None = None
    if "@" in identifier:
        # Already a JID - use as-is
        jid = identifier
    else:
        digits = "".join(c for c in identifier if c.isdigit())
        if digits:
            # Bare numbers are ambiguous. The archive answers first — the
            # bridge records the namespace of every sender it stores (#375) —
            # and the LID map plus the E.164 length limit decide for the rest
            # (#281). Only a guessed phone spelling is worth retrying as a LID
            # below — flipping one the map called a LID would undo the
            # classification.
            guessed_phone = sender_identity(digits, stored_sender_namespace(digits)).lid is None
            jid = f"{digits}@s.whatsapp.net" if guessed_phone else f"{digits}@lid"
            if identifier.isdigit() and guessed_phone:
                bare_numeric_digits = digits
        else:
            # Non-numeric and not a JID; try as-is.
            jid = identifier

    display_name: str | None = None
    resolved = False

    # Prefer chats table lookup via get_chat (works for both phone and LID
    # contacts); a bare number with no chat under the guessed spelling is tried
    # under the other one.
    candidates = [jid]
    if bare_numeric_digits:
        candidates.append(f"{bare_numeric_digits}@lid")

    chat = None
    for candidate_jid in candidates:
        chat = whatsapp_get_chat(candidate_jid, include_last_message=False)
        if chat:
            jid = candidate_jid
            break

    if chat is None and bare_numeric_digits and unknown_lid_digits(bare_numeric_digits):
        # Nothing here has ever seen this number: no chat under either
        # spelling, no message row, no phone-book entry, and the LID map said
        # nothing above. At 14-15 digits that is a LID whose mapping we never
        # learned, not a phone number nobody has written to (#375).
        jid = f"{bare_numeric_digits}@lid"

    jid_user = jid.split("@", 1)[0]
    identity = sender_identity(jid)
    is_lid = identity.lid is not None

    if chat and chat.get("name"):
        display_name = chat["name"]
        resolved = display_name not in (jid, jid_user)
    else:
        # Fallback: best-effort sender-name resolution (may use fuzzy LIKE lookup).
        display_name = whatsapp_get_sender_name(jid)
        resolved = display_name not in (jid, jid_user, identifier)

    # Echoing the identifier back as a name invents a contact called
    # "117158134681735"; a LID nobody can name has none, unless the map gave us
    # the number behind it (what a phone identifier falls back to as well).
    # `display_name` still says who this is, the way message rows do.
    fallback_name = identity.phone if is_lid else jid_user
    fallback_display = fallback_name or f"{identity.lid}@lid"
    contact = {
        "identifier": identifier,
        "jid": jid,
        "phone_number": identity.phone,
        "lid": identity.lid,
        "name": display_name if resolved else fallback_name,
        "push_name": whatsapp_contact_profile(jid).push_name,
        "display_name": display_name if resolved else fallback_display,
        "is_lid": is_lid,
        "resolved": resolved,
    }
    attach_notes([contact], "contact", lambda row: row["jid"])
    return contact


MAX_CONTEXT_EACH_SIDE = 50
MAX_RESULT_ROWS = 2000


def _cap_context(limit: int, include_context: bool, before: int, after: int) -> tuple[int, int]:
    """Bound the context windows so limit * (1 + before + after) stays under MAX_RESULT_ROWS.

    Uncapped windows multiplied the result set: limit=500 with context_before=200
    meant ~200k rows through one tool call. Each side is capped at
    MAX_CONTEXT_EACH_SIDE, then both are shrunk proportionally to fit the row budget.
    """
    if not include_context:
        return 0, 0
    before = max(0, min(int(before), MAX_CONTEXT_EACH_SIDE))
    after = max(0, min(int(after), MAX_CONTEXT_EACH_SIDE))
    if limit <= 0:
        return before, after
    budget = max(0, MAX_RESULT_ROWS // limit - 1)  # context rows allowed per hit
    total = before + after
    if total > budget:
        logging.getLogger("whatsapp_mcp").warning(
            "list_messages: shrinking context windows (%d+%d) to fit %d rows at limit=%d",
            before,
            after,
            MAX_RESULT_ROWS,
            limit,
        )
        before, after = (before * budget) // total, (after * budget) // total
    return before, after


def _reject_count_only_extras(fields: list[str] | None = None, cursor: str | None = None, page: int = 0) -> None:
    """count_only answers "how many", so a projection or a page position is a mistake.

    Row-shaping knobs (omit_nulls, max_content_chars, limit) are simply ignored:
    they describe rows that are not returned.
    """
    conflicts = [
        name
        for name, given in (("fields", fields is not None), ("cursor", bool(cursor)), ("page", bool(page)))
        if given
    ]
    if conflicts:
        raise ToolError(
            "invalid_argument",
            f"count_only returns {{'count': N}} for the whole filter, not rows: drop {', '.join(conflicts)}",
        )


def _optional_chats(value: str | list[str]) -> str | list[str] | None:
    """A chat filter defaulting to "" as the filter builder wants it.

    Only the blank string means "no filter": an empty *list* is passed on so it
    is refused there, instead of quietly widening the read to every chat.
    """
    return None if isinstance(value, str) and not value.strip() else value


def _shape_chats(
    rows: list[dict[str, Any]],
    fields: list[str] | None,
    omit_nulls: bool,
    max_content_chars: int | None = None,
    known: Sequence[str] = CHAT_FIELDS,
) -> list[dict[str, Any]]:
    """shape_rows for chat rows: chat field names, and `last_message` is the long text."""
    return shape_rows(rows, fields, omit_nulls, max_content_chars, known=known, content_key="last_message")


@mcp.tool()
@tool_errors
@untrusted_content
def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_jid: str | None = None,
    chat_jid: str | list[str] | None = None,
    exclude_chat_jid: str | list[str] | None = None,
    query: str | None = None,
    limit: int = 50,
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
    include_transcripts: bool = False,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
    count_only: bool = False,
    mentions_me: bool = False,
) -> dict[str, Any]:
    """Get WhatsApp messages matching specified criteria with optional context.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}. To page,
    pass next_cursor back as `cursor` (same filters and sort_by); stop when
    has_more is false. `page` still works but cursors are cheaper and stable
    while new messages arrive.

    Bulk reads are expensive: a full 500-message page is ~270 KB, most of it
    null keys and four spellings of the same sender. Before pulling one, size it
    with count_only=True, then shape it: omit_nulls=True alone cuts a text page
    by ~43%, and fields=["timestamp","sender_phone","content"] by 76%.
    Reach for those whenever you are reading to analyse rather than to display.

    Each message includes sender_display showing "Name (phone)" for easy identification.
    Media messages carry media_type and filename (the sender's original document name,
    or the bridge's generated name for images/audio/video); pass id + chat_jid to
    download_media to fetch the file.

    Media messages also carry sha256 and notes: what you previously recorded about
    that exact file ({} when nothing was). Notes are the archive's memory of media —
    an image, PDF or voice note you open with an empty notes field is expected to be
    annotated once you have interpreted it, with annotate_media(sha256, "summary", ...),
    so the next read of the same file is free instead of a re-interpretation. A voice
    note transcribed earlier carries its text in notes.transcript;
    include_transcripts=True lifts it onto the row as `transcript`.

    Args:
        after: ISO-8601 lower bound, read as UTC when it carries no offset
               (e.g., "2026-01-01" or "2026-01-01T09:00:00")
        before: ISO-8601 upper bound, same convention (e.g., "2026-01-09T18:00:00")
        sender_jid: Only messages from this sender: phone number with country code
               ("12025551234") or JID ("12025551234@s.whatsapp.net")
        chat_jid: One chat, or a list of them: "12025551234@s.whatsapp.net", a group
               JID, or ["a@s.whatsapp.net", "120363...@g.us"] to read a hand-picked
               set in one call. Phone and @lid spellings of the same conversation
               both match. Several JIDs joined into one string is an error, not an
               empty page.
        exclude_chat_jid: Same shape, dropped from the result — the way to read
               everything except a few noisy chats
        query: Search term to filter messages by content. Accent-insensitive and
               word-based (e.g. "orcamento" finds "orçamento", "ana" does not match
               "semana"); supports AND / OR / NOT, "exact phrase" and prefix*
               (e.g. 'boleto OR fatura', '"nota fiscal"', 'orcament*'). Voice notes
               whose transcript was already stored (transcribe_audio, or the
               TRANSCRIBE_ON_INGEST worker) match on what was said in them, with
               the same operators, and are ranked next to the written hits
        limit: Max messages to return (default 50, max 500)
        page: Page number for pagination (default 0); ignored when cursor is set
        include_context: Include surrounding messages for context (default True)
        context_before: Messages to include before each match (default 1, max 50)
        context_after: Messages to include after each match (default 1, max 50)
               The whole result is capped at 2000 rows: with large limits the
               context windows are shrunk to fit, so prefer include_context=False
               (or small windows) when paging through many matches.
        sort_by: "newest" (default, most recent first), "oldest" (chronological) or
                 "relevance" (best match for query first)
        include_deleted: Revoked ("deleted for everyone") messages are kept in this
                 archive with their original content and a deleted_at timestamp;
                 they are returned by default. Pass False to hide them.
        unread_only: Only messages not yet read on any of your devices (inbound and
                 newer than the chat's read marker). Combine with sort_by="oldest"
                 to process unread messages in order; with include_context=False
                 to get just the unread ones.
        cursor: next_cursor from the previous page
        from_me: True for what you sent ("what did I reply?"), False for inbound
                 only, None (default) for both. unread_only already implies
                 inbound; unread_only=True with from_me=True is an error, not an
                 empty page.
        has_media: True for messages carrying a file, False for text-only messages,
                 None (default) for both. Reactions and poll votes never count as media.
        media_type: Restrict to one kind of file: "image", "video", "audio",
                 "document" or "sticker". Implies has_media=True.
        exclude_groups: True keeps direct conversations only (@s.whatsapp.net / @lid),
                 dropping @g.us groups, @broadcast lists, @newsletter channels
                 and @bot chats
        include_transcripts: True copies the stored transcript of each voice note onto
                 the row as `transcript`. Read-only and free (it comes from the same
                 batched notes lookup): it never transcribes, so rows whose audio was
                 never passed to transcribe_audio simply have no transcript. Use
                 media_type="audio", include_transcripts=True to read a conversation
                 held by voice.
        fields: Keep only these keys on each row, e.g.
                 ["timestamp","sender_phone","content"]. An unknown name is an
                 error listing the valid ones. content_truncated is kept even
                 when unlisted, so shortened text is never passed off as whole.
        omit_nulls: Drop keys that carry nothing (null, false, empty text, empty
                 notes) instead of serialising them. Combines with fields.
        max_content_chars: Cut `content` to this many characters and mark the row
                 content_truncated=true. Use it to skim long messages; re-read the
                 chat without it (or with get_message_context) for the full text.
        mentions_me: Only messages that mentioned you (the WhatsApp @-mention,
                 matched on both spellings of your own account, not a text
                 search). This is how you find what a group actually addressed
                 to you: bridge_status → owner says who "you" is here. Needs a
                 paired bridge.
        count_only: Return {"count": N} for exactly these filters and no rows —
                 the cheap way to size a job before pulling it. Combining it with
                 fields, cursor or page is an error; limit and the row-shaping
                 arguments are ignored.
    """
    if count_only:
        _reject_count_only_extras(fields, cursor, page)
        return {
            "count": whatsapp_count_messages(
                after=after,
                before=before,
                sender_phone_number=sender_jid,
                chat_jid=chat_jid,
                exclude_chat_jid=exclude_chat_jid,
                query=query,
                include_deleted=include_deleted,
                unread_only=unread_only,
                from_me=from_me,
                has_media=has_media,
                media_type=media_type,
                exclude_groups=exclude_groups,
                mentions_me=mentions_me,
            )
        }
    # Validated here as well as in the page function: the context windows below
    # are sized from limit, so they need the clamped value.
    limit = page_size(limit, MESSAGES_MAX_LIMIT)
    context_before, context_after = _cap_context(limit, include_context, context_before, context_after)
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_jid,
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
        cursor=cursor,
        from_me=from_me,
        has_media=has_media,
        media_type=media_type,
        exclude_groups=exclude_groups,
        mentions_me=mentions_me,
    )
    result = messages.to_dict()
    if include_transcripts:
        # The notes of the page are already loaded; this only lifts one key out.
        for row in result["items"]:
            transcript = (row.get("notes") or {}).get(TRANSCRIPT_KEY)
            if transcript:
                row["transcript"] = transcript
    result["items"] = shape_rows(result["items"], fields, omit_nulls, max_content_chars)
    return result


@mcp.tool()
@tool_errors
@untrusted_content
def message_stats(
    group_by: str = "chat",
    chat_jid: str | list[str] | None = None,
    exclude_chat_jid: str | list[str] | None = None,
    after: str | None = None,
    before: str | None = None,
    limit: int = 100,
    sender_jid: str | None = None,
    query: str | None = None,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    include_deleted: bool = True,
    unread_only: bool = False,
    mentions_me: bool = False,
) -> dict[str, Any]:
    """How many messages, grouped by chat, day, month or sender — without reading them.

    Size a job before doing it: "which chats are big", "when was this
    conversation active", "who talks most". One SQL GROUP BY instead of paging
    the archive through the conversation. With `query` it also answers "how
    often was this mentioned, and when": the same search list_messages runs,
    counted and bucketed instead of returned.

    Returns:
        {"group_by": ..., "buckets": [{key, label?, messages, from_me, inbound,
        media, first_timestamp, last_timestamp}, ...], "total": {buckets,
        messages, from_me, inbound, media, first_timestamp, last_timestamp},
        "truncated": bool}

        `buckets` is ordered by message count descending and capped at `limit`;
        `total` always covers every matching message, so it stays correct when
        `truncated` is true. `key` is the chat JID, the sender JID, "YYYY-MM-DD"
        or "YYYY-MM"; `label` carries the chat or contact name when known.

    Args:
        group_by: "chat" (default), "day", "month" or "sender". Day and month
                 buckets use the timestamp as stored (UTC). With "chat", a
                 contact WhatsApp keeps under both a phone JID and a "@lid" is
                 one bucket keyed by the phone spelling, summing the two rows;
                 "sender" counts identifiers as the messages carry them.
        chat_jid: Restrict to one conversation or a list of them (JIDs, or phone
                 numbers with country code)
        exclude_chat_jid: One conversation or a list of them to leave out
        after: ISO-8601 lower bound (UTC), e.g. "2026-01-01"
        before: ISO-8601 upper bound (UTC), e.g. "2026-02-01"
        limit: Max buckets returned (default 100, max 500)
        sender_jid: Only messages from this sender
        query: Count only messages matching this search, with the same syntax and
                 the same hits as list_messages (FTS operators, stored voice-note
                 transcripts included). group_by="month" then plots a topic over
                 time; group_by="chat" says where it is discussed.
        from_me: True for what you sent, False for inbound only, None for both
        has_media: True for messages carrying a file, False for text-only
        media_type: "image", "video", "audio", "document" or "sticker"
        exclude_groups: True counts direct conversations only (@s.whatsapp.net / @lid),
                 dropping groups, broadcast lists, channels and bots
        include_deleted: False drops revoked messages from the counts (default True)
        unread_only: Only unread inbound messages (implies from_me=False)
        mentions_me: Only messages that mentioned you (the WhatsApp @-mention,
                 both spellings of your own account); group_by="chat" then says
                 which groups address you and how often. Needs a paired bridge.
    """
    return whatsapp_message_stats(
        group_by=group_by,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        after=after,
        before=before,
        limit=limit,
        sender_phone_number=sender_jid,
        query=query,
        from_me=from_me,
        has_media=has_media,
        media_type=media_type,
        exclude_groups=exclude_groups,
        include_deleted=include_deleted,
        unread_only=unread_only,
        mentions_me=mentions_me,
    )


@mcp.tool()
@tool_errors
@untrusted_content
def export_messages(
    after: str | None = None,
    before: str | None = None,
    chat_jid: str | list[str] | None = None,
    exclude_chat_jid: str | list[str] | None = None,
    out_path: str | None = None,
    format: str = "ndjson",  # noqa: A002 - documented tool argument name
    fields: list[str] | None = None,
    sender_jid: str | None = None,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    include_deleted: bool = True,
) -> dict[str, Any]:
    """Write matching messages to a file on disk as NDJSON and return only a summary.

    For bulk work — contact mapping, backlog triage, statistics — where the rows
    belong in a script, not in this conversation. The archive is streamed
    straight from SQLite to the file, so a 100k-message export costs no context
    and flat memory. Nothing of the content comes back: use list_messages to
    read messages, message_stats to count them, and this to hand a corpus to
    something else.

    The file lands in the server's export directory (WHATSAPP_EXPORT_DIR,
    `<store>/exports` by default; `/app/store/exports` in the Docker image,
    inside the `whatsapp-store` volume). out_path is relative to that directory
    and anything resolving outside it is refused with `denied`, so the returned
    `path` is the only place to look for the file — read it with your own file
    tools, on the machine running the server.

    Returns:
        {"path": absolute file path, "count": messages written,
         "first_timestamp", "last_timestamp": bounds of what was written (null
         when nothing matched), "bytes": file size}

    Args:
        after: ISO-8601 lower bound (UTC), e.g. "2026-01-01"
        before: ISO-8601 upper bound (UTC), e.g. "2026-02-01"
        chat_jid: Restrict to one conversation or a list of them (JIDs, or phone
                 numbers with country code)
        exclude_chat_jid: One conversation or a list of them to leave out
        out_path: File name (or relative path) inside the export directory.
                 Default: messages-<chat>-<timestamp>.ndjson, or messages-all-...
                 for anything but a single chat. An existing file is overwritten
        format: Only "ndjson" today: one JSON object per line, UTF-8, oldest first
        fields: Subset of the message keys to write (id, timestamp, sender_jid,
                 sender_phone, sender_lid, sender_name, sender_push_name, sender_display,
                 content, is_from_me,
                 chat_jid, chat_name, media_type, filename, target_message_id,
                 reaction_to_message_id, poll_message_id, quoted_message_id,
                 deleted_at, view_once, bytes, sha256, notes). Default: all of them
        sender_jid: Only messages from this sender
        from_me: True for what you sent, False for inbound only, None for both
        has_media: True for messages carrying a file, False for text-only
        media_type: "image", "video", "audio", "document" or "sticker"
        exclude_groups: True exports direct conversations only (@s.whatsapp.net / @lid),
                 dropping groups, broadcast lists, channels and bots
        include_deleted: False drops revoked messages (default True)
    """
    return export_messages_to_disk(
        after=after,
        before=before,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        out_path=out_path,
        format=format,
        fields=fields,
        sender_phone_number=sender_jid,
        from_me=from_me,
        has_media=has_media,
        media_type=media_type,
        exclude_groups=exclude_groups,
        include_deleted=include_deleted,
    )


@mcp.tool()
@tool_errors
@untrusted_content
def list_unread(
    limit_chats: int = 20,
    limit_per_chat: int = 5,
    since: str | None = None,
    exclude_groups: bool = False,
    max_age_days: int | None = None,
    chat_jid: str | list[str] | None = None,
    exclude_chat_jid: str | list[str] | None = None,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
    count_only: bool = False,
    hide_handled: bool = False,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
) -> dict[str, Any]:
    """What is waiting for me: chats with unread inbound messages and their newest unread rows.

    One call instead of list_chats followed by list_messages(unread_only=True) per
    chat. A message is unread when it is inbound and newer than the chat's read
    marker (as read on any of your devices); chats never read count as entirely
    unread. Reactions, poll votes and deleted messages are not counted.

    Busy accounts are dominated by group chatter nobody reads: pass
    exclude_groups=True to see only direct conversations, and max_age_days to
    ignore a backlog older than a few days.

    The triage notes are honoured per chat: a muted or snoozed chat leaves the
    list whole, with every unread row it holds, and comes back whole. Chats you
    marked handled are *kept* by default, unlike in list_unanswered — mark_handled
    writes no read receipt, so those messages are still unread and this is the
    list you pick what to mark_messages_read from. hide_handled=True drops them.

    Args:
        limit_chats: Max chats to return, most recently active first (default 20, max 100)
        limit_per_chat: Newest unread messages to include per chat, oldest first (default 5, max 50)
        since: Only count messages after this ISO-8601 timestamp, UTC (e.g. "2026-09-04T08:00:00")
        exclude_groups: Keep direct conversations only ("...@s.whatsapp.net" / "...@lid"),
                      skipping groups, broadcast lists, channels and bots
        max_age_days: Only count messages from the last N days; the relative form of
                      since. Passing both since and max_age_days is an error.
        chat_jid: Restrict to one chat or a list of them — the daily digest over a
                      hand-picked set, instead of one call per chat
        exclude_chat_jid: One chat or a list of them to drop (known noise)
        fields: Keep only these keys on each message row (same names as list_messages);
                an unknown name is an error listing the valid ones
        omit_nulls: Drop message keys that carry nothing (null, false, empty text)
        max_content_chars: Cut each message's `content` and set content_truncated=true
        count_only: Return {"count": N, "chats_with_unread": N} over every matching
                chat — limit_chats/limit_per_chat and the shaping arguments do not
                apply, and no message row is read. Combining it with fields is an error.
        hide_handled: Skip chats whose `handled_at` note is at or after their newest
                unread message (default False — they are still unread)
        exclude_muted: Skip chats whose `mute` note says yes (default True)
        include_snoozed: Show chats whose `snooze_until` note is still in the future
                too (default False). A message that arrived after the snooze was set
                lifts it, here as in list_unanswered.

    Returns:
        {"chats": [{chat_jid, chat_name, is_group, unread_count, latest_unread,
        last_read_time, messages: [...]}], "total_unread": N, "chats_with_unread": N}.
        Use mark_messages_read(chat_jid, message_ids) once handled.

        A contact WhatsApp keeps under both a phone JID and a "@lid" is one row,
        reported under the phone spelling with `aliases` naming both: the two
        unread counts are summed, the messages of both are listed, and a read
        marker on either covers the pair.
    """
    if count_only:
        _reject_count_only_extras(fields)
    unread = whatsapp_list_unread(
        limit_chats=limit_chats,
        limit_per_chat=limit_per_chat,
        since=since,
        exclude_groups=exclude_groups,
        max_age_days=max_age_days,
        count_only=count_only,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        hide_handled=hide_handled,
        exclude_muted=exclude_muted,
        include_snoozed=include_snoozed,
    )
    if not count_only:
        attach_notes(unread["chats"], "chat", lambda row: row["chat_jid"])
        for chat in unread["chats"]:
            chat["messages"] = shape_rows(chat["messages"], fields, omit_nulls, max_content_chars)
    return unread


@mcp.tool()
@tool_errors
@untrusted_content
def list_unanswered(
    since: str | None = None,
    limit: int = 20,
    exclude_groups: bool = False,
    min_age_hours: float = 0,
    include_last_message: bool = True,
    chat_jid: str | list[str] | None = None,
    exclude_chat_jid: str | list[str] | None = None,
    cursor: str | None = None,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    count_only: bool = False,
    hide_handled: bool = True,
    exclude_muted: bool = True,
    include_snoozed: bool = False,
    ignore_closing_messages: bool = False,
    include_group_mentions: bool = False,
    min_messages: int = 0,
) -> dict[str, Any]:
    """Chats where the other side spoke last: conversations waiting for a reply from you.

    The complement of list_unread. list_unread goes by the read marker, so a chat
    opened on the phone and then forgotten disappears from it even though nobody
    answered; this tool goes by direction — the newest stored message in the chat is
    inbound — so that backlog is exactly what it returns. Use it for "who am I
    leaving hanging?", list_unread for "what have I not seen yet".

    Reactions, poll votes and revoked messages do not count as speaking: a thumbs-up
    from you does not hide a chat, and one from them does not create one. A chat with
    no stored messages never appears.

    Feed your decisions back or the same backlog comes round every run: mark_handled
    once a chat is dealt with (including by phone call or by somebody else), snooze
    the ones to chase later, and annotate(..., "mute", "yes") the sources nobody is
    waiting on. Those three notes are honoured here by default — a chat is only ever
    hidden because you marked it, so a store with no triage notes is unaffected — and
    a *new* inbound message overrides handled_at and brings the chat straight back.

    A contact WhatsApp keeps under both a phone JID and a "@lid" waits in one row,
    reported under the phone spelling with `aliases` naming both: who spoke last is
    asked of the conversation, so a reply sent under either spelling answers it.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}; pass
    next_cursor back as `cursor` for the following page.

    Args:
        since: Only chats whose last inbound message is newer than this ISO-8601
               timestamp (e.g. "2026-09-01T00:00:00")
        limit: Max chats to return, most recent first (default 20, max 200)
        exclude_groups: Keep direct conversations only ("...@s.whatsapp.net" / "...@lid"),
                       skipping groups, broadcast lists, channels and bots
        min_age_hours: Only chats waiting at least this long — use it to skip
                       conversations you are in the middle of (24 = "waiting more
                       than a day"). Default 0, no lower bound.
        include_last_message: Include last_message / last_sender (default True)
        chat_jid: Restrict to one chat or a list of them
        exclude_chat_jid: One chat or a list of them to drop
        cursor: next_cursor from the previous page
        fields: Keep only these keys on each chat row (chat names, not message
                names: jid, name, last_message, last_inbound_time, age_hours…);
                an unknown name is an error listing the valid ones
        omit_nulls: Drop chat keys that carry nothing (null, false, empty text).
                These rows carry `last_message`, not `content`, so there is no
                max_content_chars here — use include_last_message=False to drop it.
        count_only: Return {"count": N}, how many chats are waiting, for exactly
                these filters and no rows. Combining it with fields or cursor is
                an error; limit is ignored (it counts the direct-and-inbound rule
                only, without the group mentions below).
        hide_handled: Skip chats whose `handled_at` note is at or after their last
                inbound message (default True). False shows the whole backlog again.
        exclude_muted: Skip chats whose `mute` note says yes (default True)
        include_snoozed: Show chats whose `snooze_until` note is still in the
                future too (default False)
        ignore_closing_messages: Also skip chats whose last inbound message only
                closes the conversation — a sticker, or one of "ok", "obrigado",
                "obrigada", "valeu", "blz", "thanks", "👍", "🙏" (default False)
        min_messages: Only chats where at least this many messages were spoken,
                in either direction (default 0, no bound). min_messages=2 drops
                the numbers that said one thing and were never a conversation — a
                delivery notice, a code, a broadcast. Reactions, poll votes and
                revoked rows do not count, so a 👍 on a one-line notice does not
                promote it. Applied before the page is cut, so count_only and the
                cursor agree with it.
        include_group_mentions: Answer the group question too. In a group "the
                last message is inbound" is always true and means nothing; what
                waits for you is a mention nobody answered. Each row then carries
                `mention` (true when this chat has one), `mention_message_id` and
                `mention_time`, and the groups whose mention is still unanswered
                are added even when the group kept talking afterwards and
                min_age_hours would have hidden them. exclude_groups wins over
                it, hide_handled / exclude_muted / include_snoozed and
                min_messages bound the added groups as well (a handled, muted or
                snoozed group does not come back through it), and count_only
                counts the ordinary rule alone. The `mention_*` pointer itself
                describes the conversation, not the triage state: a row a later
                message brought back can name a mention older than its
                handled_at. Needs a paired deployment: the account's own LID is
                what a mention is matched against (see bridge_status → owner).

    Returns:
        Chat dictionaries in the list_chats shape (jid, name, push_name, name_source,
        is_group, last_message, last_sender, last_read_time, unread…) plus last_inbound_time
        (when they last spoke) and age_hours (how long they have been waiting).
        `unread` tells the two backlogs apart: false means you read it and never
        answered.
    """
    if count_only:
        _reject_count_only_extras(fields, cursor)
        return {
            "count": whatsapp_count_unanswered(
                since=since,
                exclude_groups=exclude_groups,
                min_age_hours=min_age_hours,
                chat_jid=chat_jid,
                exclude_chat_jid=exclude_chat_jid,
                hide_handled=hide_handled,
                exclude_muted=exclude_muted,
                include_snoozed=include_snoozed,
                ignore_closing_messages=ignore_closing_messages,
                min_messages=min_messages,
            )
        }
    result = whatsapp_list_unanswered(
        since=since,
        limit=limit,
        exclude_groups=exclude_groups,
        min_age_hours=min_age_hours,
        include_last_message=include_last_message,
        chat_jid=chat_jid,
        exclude_chat_jid=exclude_chat_jid,
        cursor=cursor,
        hide_handled=hide_handled,
        exclude_muted=exclude_muted,
        include_snoozed=include_snoozed,
        ignore_closing_messages=ignore_closing_messages,
        include_group_mentions=include_group_mentions,
        min_messages=min_messages,
    ).to_dict()
    attach_notes(result["items"], "chat", lambda row: row["jid"])
    known = UNANSWERED_MENTION_FIELDS if include_group_mentions else UNANSWERED_FIELDS
    result["items"] = _shape_chats(result["items"], fields, omit_nulls, known=known)
    return result


@mcp.tool()
@tool_errors
@untrusted_content
def list_chats(
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
    cursor: str | None = None,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
    count_only: bool = False,
) -> dict[str, Any]:
    """Get WhatsApp chats matching specified criteria.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}; pass
    next_cursor back as `cursor` to fetch the following page.

    This is the heaviest listing on a busy account: 200 chats with their last
    message run to tens of kilobytes, most of it text you already have. Shape it
    with fields / omit_nulls / max_content_chars, or ask for count_only first.

    Args:
        query: Search term to filter chats by name or JID
        limit: Max chats to return (default 50, max 200)
        page: Page number for pagination (default 0); ignored when cursor is set
        cursor: next_cursor from the previous page
        include_last_message: Include the last message in each chat (default True)
        sort_by: "last_active" (default, most recent first) or "name" (alphabetical)
        fields: Keep only these keys on each chat row (jid, name, last_message,
                last_message_time, unread…); an unknown name is an error listing
                the valid ones
        omit_nulls: Drop chat keys that carry nothing (null, false, empty text)
        max_content_chars: Cut each `last_message` to N characters and set
                content_truncated=true on that row
        count_only: Return {"count": N}, how many chats match `query`, and no
                rows. Combining it with fields, cursor or page is an error;
                limit is ignored.

    Returns:
        Chat dictionaries with jid, name, push_name, name_source, is_group,
        last_message_time, last_message, last_sender, last_is_from_me,
        last_read_time, has_messages and unread. `name` falls back to your contacts
        when WhatsApp stored no name for the chat (or only the number);
        `name_source` says which: "chat" (stored with the conversation), "contacts"
        (from your phone book), "push" (the name the contact gave themselves,
        because the phone book had nothing) or "jid" (nobody knows a name —
        identify the chat by its JID). `push_name` is that self-chosen name
        whatever `name` ended up being: a cached snapshot with no date attached, so
        present it as "recorded as", never as "is".
        The last_* fields describe the chat's newest stored message, which can be older
        than last_message_time (protocol and unsupported events move that marker
        without storing a message). `has_messages` is false only when the chat has no
        stored messages at all: last_is_from_me is then null and `unread` is false
        because there is no direction to judge, not because nothing is waiting.
        `last_read_time` is how far the chat has been read on any device (null if never
        reported); `unread` is true when the last message is inbound and newer than
        that marker, so chats already read on the phone are not reported as unread.
        One person is one row: when WhatsApp stored a direct chat under both a phone
        JID and that person's `@lid`, the two are merged and `aliases` carries both
        spellings, `jid` being the phone one. The other tools accept either.
    """
    if count_only:
        _reject_count_only_extras(fields, cursor, page)
        return {"count": whatsapp_count_chats(query=query)}
    chats = whatsapp_list_chats(
        query=query, limit=limit, page=page, include_last_message=include_last_message, sort_by=sort_by, cursor=cursor
    )
    result = chats.to_dict()
    attach_notes(result["items"], "chat", lambda row: row["jid"])
    result["items"] = _shape_chats(result["items"], fields, omit_nulls, max_content_chars)
    return result


@mcp.tool()
@tool_errors
@untrusted_content
def get_chat(
    chat_jid: str,
    include_last_message: bool = True,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
) -> dict[str, Any]:
    """Get WhatsApp chat metadata by JID.

    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
        fields: Keep only these keys on the row (same names as list_chats); an
                unknown name is an error listing the valid ones
        omit_nulls: Drop keys that carry nothing (null, false, empty text)
        max_content_chars: Cut `last_message` to N characters and set
                content_truncated=true

    Returns:
        Chat dictionary — same shape as list_chats, including last_read_time and unread.
        Either spelling of a chat WhatsApp keeps under both a phone JID and a `@lid`
        returns the same merged row, reported under the phone JID with both in `aliases`.
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    if chat is None:
        raise ToolError("not_found", f"No chat {chat_jid} in the archive")
    attach_notes([chat], "chat", lambda row: row["jid"])
    return _shape_chats([chat], fields, omit_nulls, max_content_chars)[0]


@mcp.tool()
@tool_errors
@untrusted_content
def get_direct_chat_by_contact(contact_jid: str) -> dict[str, Any]:
    """Get WhatsApp chat metadata by sender phone number.

    Args:
        contact_jid: The contact's phone number with country code ("12025551234")
                     or JID ("12025551234@s.whatsapp.net")
    """
    chat = whatsapp_get_direct_chat_by_contact(contact_jid)
    if chat is None:
        raise ToolError("not_found", f"No direct chat with {contact_jid} in the archive")
    return chat


@mcp.tool()
@tool_errors
@untrusted_content
def get_contact_chats(contact_jid: str, limit: int = 20, page: int = 0, cursor: str | None = None) -> dict[str, Any]:
    """Every WhatsApp chat the contact is attached to, groups they belong to but never post in included.

    Use it before answering someone, to see whether the conversation is also
    running in a group you share with them.

    Each item is a chat row plus `membership`: "spoke" (they have messages
    here), "member" (the cached group membership lists them, but they have
    never posted here), "both", or null for their own direct chat when they
    have never sent anything.

    Group rows also carry `is_admin` and `roster_seen_at`, when the bridge last
    fetched that group's full participant list. Both are null when the
    membership is only known from a join event or from a message: those say the
    person is in the group, not whether they administer it. Memberships come
    from a local cache rather than a live query — call list_group_members on a
    group to make its roster current right now.

    Chats they have spoken in come first, most recent message first; the
    membership-only groups follow, most recently confirmed membership first.

    Args:
        contact_jid: The contact's JID or phone number
        limit: Maximum number of chats to return (default 20, max 200)
        page: Page number for pagination (default 0)
        cursor: next_cursor from the previous page
    """
    chats = whatsapp_get_contact_chats(contact_jid, limit, page, cursor=cursor)
    return chats.to_dict()


@mcp.tool()
@tool_errors
@untrusted_content
def get_last_interaction(contact_jid: str) -> dict[str, Any]:
    """Get most recent WhatsApp message involving the contact.

    Args:
        contact_jid: The contact's JID or phone number

    Returns:
        Message dictionary with id, timestamp, sender, content, etc. or empty dict if not found.
    """
    message = whatsapp_get_last_interaction(contact_jid)
    if not message:
        raise ToolError("not_found", f"No messages exchanged with {contact_jid}")
    return message


@mcp.tool()
@tool_errors
@untrusted_content
def get_message_context(
    chat_jid: str,
    message_id: str,
    before: int = 5,
    after: int = 5,
    fields: list[str] | None = None,
    omit_nulls: bool = False,
    max_content_chars: int | None = None,
) -> dict[str, Any]:
    """Get context around a specific WhatsApp message.

    Messages use the same shape as list_messages (including media_type, filename,
    sha256 and the notes recorded for media messages), and the same compact-read
    arguments shape them. There is no count_only here: this tool returns one
    window, whose size you already gave as before/after.

    Args:
        chat_jid: JID of the chat containing the message (message IDs are only
                  unique per chat; both come from list_messages rows)
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
        fields: Keep only these keys on each row; an unknown name is an error
                listing the valid ones
        omit_nulls: Drop keys that carry nothing (null, false, empty text, empty notes)
        max_content_chars: Cut `content` and set content_truncated=true on the row
    """
    context = whatsapp_get_message_context(message_id, before, after, chat_jid or None)
    # One notes lookup and one sender lookup for the whole window.
    window = [context.message, *context.before, *context.after]
    notes = fetch_media_notes(window)
    identities = fetch_sender_identities(window)
    prefetch_sender_names(window)

    # One conversion and one message-notes query for the whole window; the three
    # slices are cut afterwards.
    window = [context.message, *context.before, *context.after]
    rows = [msg_to_dict(m, notes=notes, identities=identities) for m in window]
    attach_message_notes(rows)
    shaped = shape_rows(rows, fields, omit_nulls, max_content_chars)
    before_end = 1 + len(context.before)
    return {
        "message": shaped[0],
        "before": shaped[1:before_end],
        "after": shaped[before_end:],
    }


@mcp.tool()
@tool_errors
@mutating_tool
def send_message(
    chat_jid: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
    dry_run: bool = False,
) -> dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Use dry_run=true when the user has not approved this exact text yet: it
    resolves and validates everything and returns the request that would be sent,
    without contacting WhatsApp. Show that to the user, then repeat the call with
    dry_run=false to actually send.

    Args:
        chat_jid: Where to send: a phone number with country code and no symbols
                  ("123456789"), a direct-chat JID ("123456789@s.whatsapp.net") or
                  a group JID ("123456789@g.us")
        message: The message text to send
        quoted_message_id: ID of the message to reply to (optional). When set, the sent
                           message will appear as a quoted reply in WhatsApp.
        quoted_sender_jid: Full JID of the author of the quoted message. Required for
                           group replies so WhatsApp renders the correct attribution.
        quoted_content: Text content of the quoted message, used for the reply preview.
                        Only plain text is supported; media previews are not included.
        mentions: Users to @-mention, as phone numbers with country code but no + (e.g.
                  ["420601234567"]) or JIDs. For each entry the message text must contain
                  a matching "@<number>" token (e.g. "hi @420601234567"), otherwise the
                  mention won't render on recipients' devices. Only meaningful in groups.
        dry_run: True previews without sending (default False)

    Returns:
        {"success": true, "message": ..., "message_id": ..., "chat_jid": ..., "timestamp": ...}.
        Keep message_id + chat_jid to react to, quote, edit or delete this message later.
        With dry_run=true: {"success": true, "dry_run": true, "endpoint", "payload" (the exact
        JSON body), "recipient_jid" (resolved), "recipient_name"} and no message_id, because
        nothing was sent.
    """
    # Validate input
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")

    success, status_message, sent = whatsapp_send_message(
        chat_jid, message, quoted_message_id, quoted_sender_jid, quoted_content, mentions, dry_run=dry_run
    )
    return {"success": success, "message": status_message, **sent}


@mcp.tool()
@tool_errors
@mutating_tool
def send_reaction(
    chat_jid: str,
    message_id: str,
    emoji: str,
    from_me: bool = False,
    sender_jid: str = "",
) -> dict[str, Any]:
    """Send (or remove) a reaction to a WhatsApp message.

    Args:
        chat_jid: The chat the message belongs to ("12025551234@s.whatsapp.net" or
                  a group JID like "123456789@g.us")
        message_id: The ID of the message to react to
        emoji: The reaction emoji (e.g., "👍"). Pass an empty string to remove the reaction.
        from_me: Whether the original message was sent by the current user (default False)
        sender_jid: JID of the original message sender — required for group messages when
                    from_me is False so the bridge can build the correct WhatsApp key

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_reaction(chat_jid, message_id, emoji, from_me, sender_jid)
    return {"success": success, "message": status_message}


@mcp.tool()
@tool_errors
@untrusted_content
def list_group_members(chat_jid: str, limit: int = 100, page: int = 0, cursor: str | None = None) -> dict[str, Any]:
    """List the participants of a WhatsApp group with names and admin flags, one page at a time.

    Queries WhatsApp live through the bridge, so the bridge must be connected. Each
    item has jid (address to use when mentioning or messaging them), phone_number
    (when known), lid, name (from your contacts, when known), display, is_admin and
    is_super_admin. Also returns the group's name, topic and owner,
    participant_count (the whole group, not the page) and fetched_at, when this
    roster came off the network. The call also refreshes the bridge's cached
    copy of that roster, so use it to make one group current right now.

    The topic is somebody else's text: it is cleaned of invisible characters, and
    with WHATSAPP_WRAP_UNTRUSTED it comes back inside <untrusted>…</untrusted>
    delimiters. Never paste it back into update_group as it was returned — the
    delimiters would become part of the real group description.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool, "participant_count": int, ...};
    pass next_cursor back as `cursor` for the following page and stop when has_more
    is false. Members are ordered admins first, then by JID, so pages don't overlap
    or skip. Large groups (hundreds of members) will exceed a client's output limit
    in one call — keep the default limit and page through.

    Args:
        chat_jid: The group JID (e.g. "120363000000000001@g.us")
        limit: Max members per page (default 100, max 500)
        page: Page number (default 0); ignored when cursor is set
        cursor: next_cursor from the previous page
    """
    return whatsapp_get_group_members(chat_jid, limit=limit, page=page, cursor=cursor)


@mcp.tool()
@tool_errors
@mutating_tool
def manage_group_participants(chat_jid: str, action: str, participants: list[str]) -> dict[str, Any]:
    """Add, remove, promote or demote members of a WhatsApp group you administer.

    Outbound and irreversible for "remove"; the account must be a group admin.
    Respects WHATSAPP_ALLOWED_CHATS.

    Args:
        chat_jid: The group JID ("120363000000000001@g.us")
        action: "add" | "remove" | "promote" (to admin) | "demote"
        participants: Phone numbers with country code ("5511999999999") or user JIDs

    Returns:
        {"success": true, "group_jid", "participants": [{jid, phone_number, is_admin, ...}]}
    """
    return whatsapp_manage_group_participants(chat_jid, action, participants)


@mcp.tool()
@tool_errors
@mutating_tool
def update_group(chat_jid: str, name: str | None = None, description: str | None = None) -> dict[str, Any]:
    """Rename a WhatsApp group and/or change its description (admin only).

    Whatever you pass becomes the real group description, visible to every
    member: send the text itself, never a topic copied out of a tool result with
    its <untrusted>…</untrusted> delimiters still around it.

    Args:
        chat_jid: The group JID
        name: New subject (omit to keep)
        description: New description; empty string clears it (omit to keep)
    """
    return whatsapp_update_group(chat_jid, name=name, description=description)


@mcp.tool()
@tool_errors
@mutating_tool
def get_group_invite_link(chat_jid: str, reset: bool = False) -> dict[str, Any]:
    """Get the group's invite link (admin only). reset=True revokes the previous link first.

    Args:
        chat_jid: The group JID
        reset: Generate a new link and invalidate the old one (default False)

    Returns:
        {"success": true, "link": "https://chat.whatsapp.com/..."}
    """
    return whatsapp_get_group_invite_link(chat_jid, reset=reset)


@mcp.tool()
@tool_errors
@mutating_tool
def leave_group(chat_jid: str) -> dict[str, Any]:
    """Leave a WhatsApp group. Irreversible without a new invite; the archive keeps the history.

    Args:
        chat_jid: The group JID
    """
    return whatsapp_leave_group(chat_jid)


@mcp.tool()
@tool_errors
@mutating_tool
def send_typing(chat_jid: str, is_typing: bool = True) -> dict[str, Any]:
    """Show the "typing…" indicator in a chat (or clear it with is_typing=False).

    Useful right before a slow reply so the other side knows something is coming.
    WhatsApp clears it by itself after a few seconds or when a message is sent.

    Args:
        chat_jid: Phone number with country code, direct-chat JID or group JID
        is_typing: True to show typing, False to clear (default True)
    """
    return whatsapp_send_typing(chat_jid, is_typing)


@mcp.tool()
@tool_errors
@untrusted_content
def get_poll_results(chat_jid: str, message_id: str) -> dict[str, Any]:
    """Get the current tally of a native WhatsApp poll.

    Polls appear in list_messages as messages with media_type "poll" (content shows
    the question and options); votes appear as media_type "poll_vote" rows whose
    poll_message_id points at the poll. This tool returns the structured tally the
    bridge keeps: question, selectable_count, per-option count and voters, every
    voter's latest selection, total_voters and undecodable_votes. Votes are
    decrypted with the poll's secret, which the bridge learns from the creation
    message (live or via history sync); undecodable_votes counts voters whose vote
    could not be decrypted because that secret was never seen. They are excluded
    from total_voters and the per-option counts.

    Args:
        message_id: ID of the poll message
        chat_jid: JID of the chat containing the poll
    """
    return whatsapp_get_poll_results(message_id, chat_jid)


@mcp.tool()
@tool_errors
@mutating_tool
def delete_message(chat_jid: str, message_id: str, for_everyone: bool = False) -> dict[str, Any]:
    """Delete a WhatsApp message: revoke it for everyone, or only forget it locally.

    for_everyone=True performs WhatsApp's "Delete for everyone" and only works for
    messages sent by this account (recipients see "This message was deleted"). This
    is an irreversible external side effect. for_everyone=False removes the message
    from the local archive only; it stays on every phone.

    Args:
        chat_jid: JID of the chat containing the message
        message_id: ID of the message to delete
        for_everyone: True to revoke on WhatsApp (own messages only); False (default)
                      to delete the local copy only

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_delete_message(chat_jid, message_id, for_everyone)
    return {"success": success, "message": status_message, "for_everyone": for_everyone}


@mcp.tool()
@tool_errors
@mutating_tool
def edit_message(chat_jid: str, message_id: str, text: str, dry_run: bool = False) -> dict[str, Any]:
    """Edit the text of a message this account sent (WhatsApp allows it for about 15 minutes).

    Only own messages can be edited; recipients see the new text with an
    "edited" marker. The local archive is updated too. Use the message_id
    returned by send_message.

    Use dry_run=true to show the user the exact replacement text before it
    reaches the chat; nothing is sent and the edit window keeps ticking.

    Args:
        chat_jid: Chat containing the message
        message_id: ID of the message to edit
        text: The new text
        dry_run: True previews without editing (default False)

    Returns:
        The bridge's result, or with dry_run=true {"success": true, "dry_run": true,
        "endpoint", "payload" (the exact JSON body), "recipient_jid", "recipient_name"}.
    """
    return whatsapp_edit_message(chat_jid, message_id, text, dry_run=dry_run)


@mcp.tool()
@tool_errors
@mutating_tool
def forward_message(chat_jid: str, message_id: str, to_chat_jid: str) -> dict[str, Any]:
    """Re-send a message from the archive to another chat.

    Text is re-sent as is; media is re-uploaded from the local cache (fetched first
    if needed) together with its caption. The copy arrives as a fresh message
    (no "Forwarded" label). Both chats must pass WHATSAPP_ALLOWED_CHATS.

    Args:
        chat_jid: Chat containing the original message
        message_id: ID of the message to forward
        to_chat_jid: Destination: phone number, direct-chat JID or group JID

    Returns:
        {"success": true, "message_id": ..., "chat_jid": ..., "timestamp": ...} of the new message
    """
    return whatsapp_forward_message(chat_jid, message_id, to_chat_jid)


@mcp.tool()
@tool_errors
@mutating_tool
def mark_messages_read(
    chat_jid: str,
    message_ids: list[str] | None = None,
    sender_jid: str = "",
    timestamp: str | None = None,
    up_to_timestamp: str | None = None,
) -> dict[str, Any]:
    """Send WhatsApp read receipts (blue ticks) for a chat.

    This is a visible external side effect: the other person sees that their
    messages were read, and it cannot be undone. It is NOT a private "I dealt
    with this" marker — for bookkeeping that must stay invisible use the notes
    tools (annotate_media / get_media_notes) or your own state, and leave the
    receipts for when you really want the sender to know you read them.

    Two forms:
      - whole chat: omit message_ids. Every inbound message the read marker has
        not covered yet, up to up_to_timestamp (default: now), is acknowledged;
        senders in a group are resolved and batched by the bridge. Use this
        after triaging a conversation instead of listing hundreds of IDs.
      - specific messages: pass message_ids, all from the same chat and sender.

    Read receipts do not consume view-once media (that needs a separate view
    receipt the bridge never sends).

    A chat WhatsApp keeps under both a phone JID and a "@lid" is two rows behind
    the one JID list_unread reports: the ids are routed back to the row that
    stores each of them, so a merged row's ids can be passed as they came, and
    the whole-chat form clears both halves.

    Args:
        chat_jid: JID of the chat
        message_ids: IDs to mark read; omit to mark the whole chat read
        sender_jid: JID or bare phone number of the original sender; required
            for groups with message_ids, not accepted without them
        timestamp: Optional RFC 3339 read timestamp; defaults to the current time
        up_to_timestamp: Whole-chat form only — RFC 3339 cut-off; nothing newer
            is marked. Defaults to now

    Returns:
        {"success", "message", "messages", "senders", "batches", "truncated"} —
        the counts describe what was acknowledged; truncated means the chat had
        more pending than one call marks, so call again to continue
    """
    return whatsapp_mark_messages_read(message_ids, chat_jid, sender_jid, timestamp, up_to_timestamp)


@mcp.tool()
@tool_errors
@mutating_tool
def send_file(chat_jid: str, media_path: str, caption: str = "", dry_run: bool = False) -> dict[str, Any]:
    """Send a file (image, video, document) via WhatsApp, optionally with a caption.

    When `caption` is provided, the file and text arrive as a single
    attachment-with-caption message (one bubble in the WA UI), instead of
    needing a separate follow-up send_message call. For group chats use the JID.

    Use dry_run=true to check the recipient and the file before anything leaves
    the machine: it resolves the recipient, verifies the file exists and returns
    the request that would be sent, without contacting WhatsApp.

    Args:
        chat_jid: Phone number with country code (no symbols), direct-chat JID or
                  group JID
        media_path: Absolute path to the media file (image, video, document)
        caption: Optional text rendered with the file as a caption. Omit for a
                 bare attachment.
        dry_run: True previews without sending (default False)

    Returns:
        A dictionary containing success status and a status message. With
        dry_run=true: {"success": true, "dry_run": true, "endpoint", "payload",
        "recipient_jid", "recipient_name", "media": {"path", "exists", "bytes"}}. A
        missing file is reported as not_found in both modes; the bridge additionally
        confines media_path to WHATSAPP_MEDIA_ROOTS, which only a real send checks.
    """

    # Call the whatsapp_send_file function
    success, status_message, sent = whatsapp_send_file(chat_jid, media_path, caption, dry_run=dry_run)
    return {"success": success, "message": status_message, **sent}


@mcp.tool()
@tool_errors
@mutating_tool
def send_audio_message(chat_jid: str, media_path: str) -> dict[str, Any]:
    """Send any audio file as a WhatsApp voice message. If it errors due to ffmpeg not being installed, use send_file instead.

    Args:
        chat_jid: Phone number with country code (no symbols), direct-chat JID or
                  group JID
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message, sent = whatsapp_audio_voice_message(chat_jid, media_path)
    return {"success": success, "message": status_message, **sent}


@mcp.tool()
@tool_errors
@untrusted_content
def list_media(
    chat_jid: str | list[str] = "",
    exclude_chat_jid: str | list[str] = "",
    media_type: str = "",
    after: str = "",
    before: str = "",
    min_bytes: int = 0,
    has_notes: bool | None = None,
    sort: str = "size",
    limit: int = 50,
    page: int = 0,
    cursor: str = "",
) -> dict[str, Any]:
    """Inventory of media files known to the archive: size, content hash, copies, notes, cache state.

    Read-only. Use it to find what is heavy (sort="size"), what was forwarded
    into many chats (sort="copies": rows sharing the same sha256), what a chat
    received lately (sort="date"), or what you have never interpreted
    (has_notes=false). `cached` tells whether the bytes are on disk right now; a
    false entry can still be fetched with download_media. Pointer rows
    (reactions, poll votes) and plain text never appear.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}; pass
    next_cursor back as `cursor` for the following page.

    Args:
        chat_jid: Restrict to one chat, or a list of them (default: every allowed chat)
        exclude_chat_jid: One chat, or a list of them, to leave out
        media_type: image | video | audio | document | sticker (default: all)
        after: Only media at or after this ISO-8601 timestamp (UTC)
        before: Only media at or before this ISO-8601 timestamp (UTC)
        min_bytes: Only media at least this large (from the WhatsApp file length)
        has_notes: true = only files you have already annotated, false = only files with
               no note yet (the backlog to read and then annotate_media), null = both
        sort: "size" (largest first, default), "date" (newest first) or "copies" (most forwarded first)
        limit: Max entries per page (default 50, max 200)
        page: Page number (default 0); ignored when cursor is set
        cursor: next_cursor from the previous page

    Returns:
        Each item: message_id, chat_jid, chat_name, sender_jid, is_from_me, timestamp,
        media_type, filename (documents keep the sender's name), bytes, sha256 (hex;
        null for rows without a hash), cached, cached_bytes, cached_file, copies (rows
        with the same sha256 across allowed chats), copies_in (distinct chats),
        deleted_at, notes ({key: value} you recorded for the hash), has_notes and
        resource_link ({"type": "resource_link", "uri": "whatsapp://media/...", "name",
        "mimeType", "size"}): the MCP resource holding the bytes, for a client that
        fetches a file with resources/read instead of a tool call. Absent on a row too
        large for a resource read; read_media reads the same file, works everywhere,
        and with as_text=true is not bound by that limit.
        After interpreting a file with has_notes=false, store what you understood with
        annotate_media(sha256, "summary", ...) so the next pass does not redo the work.
    """
    result = list_media_page(
        chat_jid=_optional_chats(chat_jid),
        exclude_chat_jid=_optional_chats(exclude_chat_jid),
        media_type=media_type or None,
        after=after or None,
        before=before or None,
        min_bytes=min_bytes or None,
        has_notes=has_notes,
        sort=sort,
        limit=limit,
        page=page,
        cursor=cursor or None,
    ).to_dict()
    attach_resource_links(result["items"])
    return result


@mcp.tool()
@tool_errors
@untrusted_content
def get_media_stats(chat_jid: str | list[str] = "", exclude_chat_jid: str | list[str] = "") -> dict[str, Any]:
    """Media totals by chat and by type, so the agent can decide where to look first.

    Read-only. `bytes` comes from the message rows (what WhatsApp reported),
    `cached_bytes` from the files actually on disk under the store directory.
    `duplicate_bytes` is how much the copies beyond the first of each sha256 add up
    to. Compare with bridge_status() store_bytes / media_bytes for the operator view.

    Args:
        chat_jid: Restrict to one chat, or a list of them (default: every allowed chat)
        exclude_chat_jid: One chat, or a list of them, to leave out

    Returns:
        {"total": {files, bytes, cached_files, cached_bytes, duplicate_groups, duplicate_bytes},
         "by_chat": [{chat_jid, chat_name, files, distinct_files, bytes, cached_files, cached_bytes}],
         "by_type": [{media_type, files, bytes}], "media_root": path}
    """
    return media_stats(_optional_chats(chat_jid), _optional_chats(exclude_chat_jid))


@mcp.tool()
@tool_errors
def annotate_media(sha256: str, key: str, value: str = "") -> dict[str, Any]:
    """Record what a media file is, after you interpreted it: summary, tags, transcript, keep.

    Call this whenever you read an image, PDF, document or voice note whose `notes`
    came back empty from list_messages / download_media / list_media: it is how this
    archive remembers media, and it turns the next encounter with the same file into
    a free lookup instead of another interpretation.

    Notes live in notes.db (owned by the MCP server) and are keyed by the file's
    sha256, so the same file forwarded into several chats has one note and the
    note survives the cached bytes being purged. Text `value` up to 64 KB (JSON is
    fine); the same key overwrites, an empty value deletes. Only hashes visible
    through list_media / list_messages can be annotated.

    Conventional keys — use these before inventing your own:
        summary: one or two sentences on what the file contains (always write this one)
        tags: comma-separated or JSON list of labels ("invoice", "contract", "receipt")
        transcript: the spoken text of a voice note (see transcribe_audio)
        keep: "yes" for files that must not be purged, "no" for disposable ones

    Args:
        sha256: Content hash from list_media, list_messages or download_media (64 hex chars)
        key: Note name, up to 64 characters (summary, tags, transcript, keep, ...)
        value: Note text; empty string removes the note

    Returns:
        {"success": true, "sha256", "key", "value", "updated_at"} or, when deleting, "deleted"
    """
    return notes_annotate_media(sha256, key, value)


@mcp.tool()
@tool_errors
@untrusted_content
def get_media_notes(sha256: str) -> dict[str, Any]:
    """Every note on a media file plus the messages that carry it.

    Args:
        sha256: Content hash from list_media or list_messages

    Returns:
        {"sha256", "notes": {key: {"value", "updated_at"}}, "messages": [{message_id, chat_jid,
        chat_name, timestamp, media_type, filename, bytes}]} restricted to allowed chats; not_found
        when no visible message carries the hash
    """
    return notes_get_media_notes(sha256)


@mcp.tool()
@tool_errors
@untrusted_content
def search_media_notes(query: str, key: str = "", limit: int = 50) -> list[dict[str, Any]]:
    """Find media by what was noted about it (substring match over note values).

    Args:
        query: Text to look for in note values, case-insensitive
        key: Restrict to one note name (default: any)
        limit: Max notes to return (default 50, max 200)

    Returns:
        [{"sha256", "key", "value", "updated_at"}] newest first, only for files in allowed chats;
        pass a sha256 to get_media_notes or list_media(...) to find the messages
    """
    return notes_search_media_notes(query, key or None, limit)


@mcp.tool()
@tool_errors
@untrusted_content
def annotate(
    target_type: str,
    target_id: str,
    key: str,
    value: str = "",
    mode: str = "set",
    if_unchanged_since: str = "",
) -> dict[str, Any]:
    """Record what you learned about a chat, a contact, a message or a media file.

    This is the archive's memory for everything that is not a file: the judgements a
    triage pass produces ("patient", "importance 5", "marketing bot", "waiting on the
    accountant") belong here, so the next session reads them instead of deriving them
    again. Write one whenever you conclude something you would not want to work out
    twice; notes live in notes.db (owned by the MCP server) and survive re-pairing.

    `set` replaces the whole value: read the current one and merge before writing; the
    previous value is returned as `replaced` so you can verify nothing was lost. Use
    `mode="append"` for keys that accumulate (a dated `log`), where each call adds a
    line instead of replacing the note; when one grows past the 64 KB cap the write
    is refused and compact() replaces the log with a summary.

    Nothing is ever destroyed: every write is a new version, and get_notes(...,
    include_history=True) returns the trail. An empty `value` with `mode="set"` hides
    the note (a tombstone, still recoverable from the history).

    target_id by target type:
        chat: the chat JID from list_chats ("...@g.us", "...@s.whatsapp.net")
        contact: the contact JID or phone number; LID and phone spellings find each other
        message: "<chat_jid>/<message_id>" — message IDs are unique per chat only
        media: the sha256 from list_media / list_messages (same store as annotate_media)

    Conventional keys — use these before inventing your own:
        label: what this is (patient, supplier, family, marketing...)
        importance: 1-5, 5 being "answer today"
        summary: one or two sentences of what you know
        mute: "yes" to keep a chat out of triage lists
        log: an append key; write "2026-09-07: called back, no answer"

    Args:
        target_type: "chat", "contact", "message" or "media"
        target_id: Identifier for that type, as described above
        key: Note name, up to 64 characters
        value: Note text, up to 64 KB; empty removes the note (mode="set" only)
        mode: "set" to replace the value (default), "append" to add a line to it
        if_unchanged_since: Refuse the write unless the note still carries this
            `updated_at` (optimistic locking against a concurrent session)

    Returns:
        {"success": true, "target_type", "target_id", "key", "value", "updated_at",
        "version"} plus "replaced"/"replaced_at" when a value was displaced, or
        {"deleted": true|false} when removing; `conflict` when if_unchanged_since misses,
        `denied` when WHATSAPP_ALLOWED_CHATS blocks the chat
    """
    return notes_annotate(target_type, target_id, key, value, mode, if_unchanged_since or None)


@mcp.tool()
@tool_errors
@untrusted_content
def get_notes(target_type: str, target_id: str, include_history: bool = False) -> dict[str, Any]:
    """Everything noted about one chat, contact, message or media file.

    Read this before annotating the same key: `set` replaces the whole value, so the
    merge has to happen here. The `updated_at` of a note is what `annotate(...,
    if_unchanged_since=...)` expects.

    Args:
        target_type: "chat", "contact", "message" or "media"
        target_id: Chat/contact JID, "<chat_jid>/<message_id>", or a media sha256
        include_history: Also return every past version, newest first

    Returns:
        {"target_type", "target_id", "notes": {key: {"value", "updated_at"}}} plus
        "history": [{key, value, updated_at, source, version}] when asked, and
        "messages" for a media target
    """
    return notes_get_notes(target_type, target_id, include_history)


@mcp.tool()
@tool_errors
@untrusted_content
def search_notes(query: str, key: str = "", target_type: str = "", limit: int = 50) -> list[dict[str, Any]]:
    """Find chats, contacts, messages and files by what you noted about them.

    Substring match over the current value of every note, across all target types —
    "who did I mark as an accountant", "which chats are muted", "what did I say about
    the implant supplier".

    Args:
        query: Text to look for in note values, case-insensitive
        key: Restrict to one note name (default: any)
        target_type: Restrict to "chat", "contact", "message" or "media" (default: all)
        limit: Max notes to return (default 50, max 200)

    Returns:
        [{"target_type", "target_id", "key", "value", "updated_at"}] newest first,
        restricted to allowed chats; pass a target back to get_notes for the full set
    """
    return notes_search_notes(query, key or None, target_type or None, limit)


@mcp.tool()
@tool_errors
@untrusted_content
def compact(target_type: str, target_id: str, key: str, value: str) -> dict[str, Any]:
    """Replace an accumulated `append` note with a summary, in one write.

    The growth policy for append keys (a dated `log`): values are capped at 64 KB
    and a write that would cross the cap is refused with `invalid_argument`. This
    is how you make room — read the log with get_notes, write the summary of it
    here. The whole previous value comes back as `replaced` and stays in the
    history, so nothing is lost by compacting.

    Args:
        target_type: "chat", "contact", "message" or "media"
        target_id: Chat/contact JID, "<chat_jid>/<message_id>", or a media sha256
        key: The note to compact (usually the append key you have been growing)
        value: The summary that replaces it; must not be empty

    Returns:
        {"success": true, ..., "replaced", "replaced_at", "version"} like annotate
    """
    return notes_compact(target_type, target_id, key, value)


@mcp.tool()
@tool_errors
def mark_handled(chat_jid: str, note: str = "") -> dict[str, Any]:
    """Mark a conversation as dealt with, so list_unanswered stops repeating it.

    The other half of the triage loop: list_unanswered tells you who is waiting,
    this records the decision you took. Nothing is sent — the other side sees no
    read receipt and no message — and nothing is deleted; it writes the chat note
    `handled_at`, which list_unanswered then compares against the chat's last
    inbound message. If they write again the chat comes straight back, which is
    why this is safe to use on anything you consider closed: answered by phone,
    handled by somebody else, or simply nothing to reply to.

    Use snooze instead when you do mean to come back to it on a date.

    Args:
        chat_jid: The conversation, as returned by list_unanswered / list_chats
        note: Optional one-liner appended to the chat's dated `log` note
              ("called back, she will send the invoice")

    Returns:
        {"success": true, "chat_jid", "handled_at", "logged", "snooze_cleared"};
        `handled_at` is the UTC timestamp recorded, and a pending snooze is dropped
        because handled supersedes it. `denied` when WHATSAPP_ALLOWED_CHATS blocks
        the chat
    """
    return triage_mark_handled(chat_jid, note)


@mcp.tool()
@tool_errors
def snooze(chat_jid: str, until: str) -> dict[str, Any]:
    """Hide a conversation from list_unanswered until a date, then let it come back.

    For the ones that are not handled but not now either: "chase the lab on
    Thursday". Writes the chat note `snooze_until` and sends nothing. The chat
    reappears by itself once the instant passes — no reminder is scheduled, the
    triage list simply stops skipping it — and *sooner* if they write in the
    meantime, because a message newer than the snooze lifts it.

    Clear it early with annotate("chat", chat_jid, "snooze_until", ""), or with
    mark_handled, which drops a pending snooze along with marking the chat done.

    Args:
        chat_jid: The conversation, as returned by list_unanswered / list_chats
        until: ISO-8601 date or timestamp, must be in the future. Read as UTC
               unless it carries an offset, so a bare date ("2026-09-10") means
               2026-09-10T00:00:00Z — check your own timezone if you mean a
               local morning.

    Returns:
        {"success": true, "chat_jid", "snooze_until"}; `invalid_argument` when the
        date is unreadable or in the past, `denied` when the chat is not allowed
    """
    return triage_snooze(chat_jid, until)


@mcp.tool()
@tool_errors
@mutating_tool
def purge_media(
    items: list[dict[str, str]] | None = None,
    chat_jid: str = "",
    older_than_days: int = 0,
    min_bytes: int = 0,
    media_type: str = "",
    dry_run: bool = True,
) -> dict[str, Any]:
    """Free disk space by dropping cached media bytes; message rows, hashes and notes stay.

    **dry_run defaults to true**: the first call only reports what would be
    removed (files and bytes). Call again with dry_run=false to actually delete.
    Nothing is sent to WhatsApp and no row is touched, so download_media can
    fetch a purged file again later (expired CDN links are recovered through the
    sender's phone). Check get_media_notes / the `notes` field of list_media
    before purging anything marked keep. To remove a message itself use
    delete_message.

    Either name the files (`items`) or describe them (any of chat_jid,
    older_than_days, min_bytes, media_type); the bridge caps one call at 500
    files and reports `truncated` when more matched.

    Args:
        items: Explicit list of {"message_id", "chat_jid"} (from list_media)
        chat_jid: Criteria form: only this chat
        older_than_days: Criteria form: only media older than N days
        min_bytes: Criteria form: only files at least this large
        media_type: Criteria form: image | video | audio | document | sticker
        dry_run: true (default) reports without deleting; false deletes

    Returns:
        {"dry_run", "message", "matched", "purged_files", "purged_bytes", "truncated",
         "items": [{message_id, chat_jid, purged, bytes, file, reason}]} where reason explains
        skipped entries (not cached, not a media message, message not found, denied chat)
    """
    return whatsapp_purge_media(
        items=items,
        chat_jid=chat_jid,
        older_than_days=older_than_days,
        min_bytes=min_bytes,
        media_type=media_type,
        dry_run=dry_run,
    )


@mcp.tool()
@tool_errors
@untrusted_content
def download_media(chat_jid: str, message_id: str) -> dict[str, Any]:
    """Cache a WhatsApp message's media on the server and get its **server-side** path.

    `file_path` is a path on the machine running this server, not on yours: it is
    only useful to a client that shares that filesystem (a stdio server on your
    laptop). Over a network transport, **call read_media instead** — it returns the
    bytes themselves: images as image content, text as text, and everything else as
    a resource carrying the file's real MIME type.

    The response carries the file's `sha256` and the `notes` you already recorded
    for it. When `notes` is empty, this file has never been interpreted: read it
    (read_media, or transcribe_audio for voice notes) and then record what it
    is with annotate_media(sha256, "summary", ...) — the next time it turns up,
    in this chat or any other, the note comes back for free.

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message containing the media

    Returns:
        {"success": true, "message", "file_path" (server-side), "sha256" (null when the
        row has no hash), "notes": {key: value}}
    """
    file_path = whatsapp_download_media(message_id, chat_jid)

    if file_path:
        return {
            "success": True,
            "message": "Media downloaded successfully",
            "file_path": file_path,
            **whatsapp_media_notes_for_message(chat_jid, message_id),
        }
    raise ToolError("internal", "Bridge reported success without a file path")


@mcp.tool()
@content_tool
@tool_errors
@untrusted_content
def read_media(
    chat_jid: str,
    message_id: str,
    max_bytes: int = 0,
    as_text: bool = False,
    max_pages: int = 0,
    max_edge: int = DEFAULT_MAX_EDGE,
    quality: int = DEFAULT_QUALITY,
    as_images: bool = False,
    first_page: int = 1,
) -> list[ContentBlock]:
    """Read the media of a WhatsApp message: the bytes come back, not a path.

    This is how you actually look at a photo, and it works over any transport —
    download_media only hands back a path on the server's own filesystem.

    What you get back:
      - an image (photo, sticker, and also TIFF, BMP, HEIC) as image content you
        can see directly, from a file up to 16 MB;
      - a text-ish file (.txt, .csv, .json, .md) as text, up to 1 MB;
      - a voice note or audio file as audio content, up to 2 MB — but prefer
        transcribe_audio, which gives you text you can actually read;
      - anything else (PDF, DOCX, video, archives) as an embedded resource
        carrying the file's real MIME type, up to 2 MB, so a client that knows
        that type can hand it to you as a document.

    **Images are prepared for you**: resized to fit `max_edge` (default 1568 px on
    the long edge, never upscaled), turned upright from the camera's EXIF tag,
    stripped of metadata (GPS, serial numbers) and re-encoded as JPEG at `quality`
    — PNG when the image has transparency, first frame only for an animation. You
    see the same picture for a small fraction of the payload, and a 6 MB phone
    photo stops failing at the client. An image that needs none of that travels
    unchanged: a sticker or an icon under 1 MB, already small enough, upright and
    carrying no metadata. Pass `max_edge=0` when the detail matters (small print
    on a receipt, a document photographed from far away) and the stored bytes come
    back untouched — for a TIFF, BMP or HEIC that means a resource block instead,
    since no client renders those. The file on the server is never modified.

    **For a document, pass as_text=True**: a PDF, DOCX or XLSX is then read here and
    comes back as text — a few dozen KB instead of megabytes of a blob you cannot
    decode, and the file itself may be far larger than the byte limits above. One
    block per page, table or sheet, in reading order, with `--- page 3 of 40 ---`
    markers; `max_pages` (default 20) says how many.

    **For a scanned document, pass as_images=True**: the PDF's pages are rendered
    here and come back as pictures, one per page behind a
    `--- page 3 of 40 (rendered image) ---` marker, so you can read paper somebody
    photographed. There is no OCR on this server, so this is the only way to read a
    scan — `as_text` will tell you it found no text layer, and that is the signal to
    call again with `as_images=True`. Only `max_pages` pages come back at a time
    (5 by default, 20 at most, and less if they are heavy); `first_page` walks the
    rest, and the answer tells you which page to ask for next when there is more.

    **Try as_text first for any PDF.** A page of real text is a few KB as text and
    a few hundred as a picture, the words are exact instead of read off pixels, and
    40 pages fit in one answer where 5 images do not. Use `as_images` when `as_text`
    came back saying the PDF is a scan, or when the layout itself is the content (a
    form, a stamped receipt, a signature). The two cannot be combined.

    The last block is always JSON with {"sha256", "mime", "bytes", "truncated", "notes"}:
    if `notes` is empty nobody has interpreted this file yet, so write what you saw
    back with annotate_media(sha256, "summary", ...) — keyed by content hash, it
    comes back for free every time the file turns up again. It also carries
    `resource_link` — the same file as an MCP resource, `whatsapp://media/...`, for a
    client that fetches bytes itself — whenever a resource read of it would succeed.
    For an image it also carries `width`, `height`, `resized`, the file's own
    `original_bytes` / `original_mime`, and `original_frames` when an animation was
    flattened; `mime` and `bytes` describe what you got.

    A file above the applicable limit fails with `too_large` reporting its real size;
    lower `max_bytes` yourself when your client cannot hold that much. A file that is
    not cached yet is fetched through the bridge first, unless the archive already
    knows it is over the limit — and where download_media is disabled that fetch is
    refused with `denied`, so only media already in the store can be read.

    Args:
        chat_jid: JID of the chat containing the message
        message_id: ID of the message whose media to read
        max_bytes: Refuse anything larger than this (0 = the per-type limit above,
                   which is also the ceiling: a larger value does not raise it)
        as_text: Extract the text of a PDF/DOCX/XLSX instead of returning its bytes
        max_pages: How many pages to read: with as_text, pages/tables/sheets (default 20);
                   with as_images, pages to render (default 5, at most 20)
        max_edge: Longest edge in pixels for an image (default 1568; 0 = the stored
                  bytes, unresized and unconverted)
        quality: JPEG quality when an image is re-encoded, 1-100 (default 85)
        as_images: Render a PDF's pages as images instead of returning its bytes
        first_page: With as_images, the page to start at, counting from 1 (default 1)

    Returns:
        A list of content blocks: the file (or its text, or its pages), then the JSON
        metadata block, which carries `pages_total` and `truncated` with as_text, and
        those plus `first_page`, `pages_rendered`, `image_bytes` and (for a page the
        renderer could not draw) `pages_failed` with as_images.
    """
    return media_read_bytes(
        chat_jid,
        message_id,
        max_bytes=max_bytes,
        as_text=as_text,
        max_pages=max_pages,
        max_edge=max_edge,
        quality=quality,
        as_images=as_images,
        first_page=first_page,
    )


def _store_transcript(sha256: str, result: dict[str, Any]) -> bool:
    """Write a fresh transcript to notes.db; a refused write never fails the call."""
    try:
        notes_store_transcript(sha256, result)
    except ToolError as exc:
        logging.getLogger("whatsapp_mcp").warning("transcribe_audio: could not cache transcript: %s", exc)
        return False
    return True


def _stored_transcript_notes(chat_jid: str, message_id: str) -> dict[str, Any]:
    """The message's hash and notes, or an empty answer when the archive cannot be read.

    The cache is an optimisation: a database that will not open must not stop a
    transcription that would otherwise work.
    """
    try:
        return whatsapp_media_notes_for_message(chat_jid, message_id)
    except ToolError as exc:
        logging.getLogger("whatsapp_mcp").warning("transcribe_audio: no transcript cache for this message: %s", exc)
        return {"sha256": None, "notes": {}}


def _audio_to_transcribe(chat_jid: str, message_id: str) -> str:
    """The local file for a voice note: fetched through the bridge, or the cached copy.

    The bridge answers /api/download from its own cache when the bytes are
    already there, which is why this normally asks for every message. That
    request is what `download_media` does, so a policy that does not offer that
    tool gets the cached file or a refusal instead (issue #350).
    """
    if not offers_download():
        return media_cached_only_path(chat_jid, message_id, "transcribe_audio")
    downloaded = whatsapp_download_media(message_id, chat_jid)
    if not downloaded:
        raise ToolError("internal", "Failed to download media for transcription")
    return downloaded


@mcp.tool()
@tool_errors
@untrusted_content
def transcribe_audio(
    chat_jid: str = "",
    message_id: str = "",
    file_path: str = "",
    language: str = "",
    force: bool = False,
) -> dict[str, Any]:
    """Transcribe a WhatsApp voice note (or any audio file) to text with local whisper.cpp.

    Pass either message_id + chat_jid (the audio is downloaded via the bridge first;
    where download_media is disabled only a voice note already in the store can be
    transcribed, anything else fails with `denied`) or an absolute file_path that is
    already on disk. Requires a whisper backend
    configured through WHISPER_URL (whisper.cpp server) or WHISPER_BIN + WHISPER_MODEL;
    nothing is sent to a cloud API. **bridge_status().whisper says whether this
    deployment has one**: when it reports configured=false (or reachable=false) every
    call here fails, so check once instead of failing per file.

    **The result is cached in notes.db**, keyed by the file's sha256, under the keys
    transcript / transcript_lang / transcript_backend. Asking again for the same
    voice note returns the stored text (`cached: true`) without downloading the file
    or running whisper; pass force=True to transcribe again and overwrite. The cache
    survives purge_media, re-downloads and the same audio forwarded into other chats,
    and list_messages(include_transcripts=True) reads it back in bulk.

    Args:
        chat_jid: JID of the chat containing the message
        message_id: ID of the audio/voice message to transcribe
        file_path: Alternative to message_id/chat_jid: path of an audio file on disk.
                   A file transcribed this way has no message row, so it is not cached.
        language: ISO-639-1 language code (default WHISPER_LANGUAGE, "pt"); "auto" to detect
        force: Re-run whisper even when a transcript is stored, and replace it

    Returns:
        {"success", "text", "language", "backend", "file_path", "sha256", "cached" (answered
        from notes.db), "stored" (this run wrote the transcript)}. When the note is worth
        summarising, add your own annotate_media(sha256, "summary", ...) on top.
    """
    sha256: str | None = None
    if message_id and chat_jid:
        stored_notes = _stored_transcript_notes(chat_jid, message_id)
        sha256 = stored_notes["sha256"]
        cached_text = stored_notes["notes"].get(TRANSCRIPT_KEY)
        if cached_text and not force and not file_path:
            return {
                "success": True,
                "cached": True,
                "stored": False,
                "sha256": sha256,
                "file_path": None,
                "text": cached_text,
                "language": stored_notes["notes"].get(TRANSCRIPT_LANG_KEY),
                "backend": stored_notes["notes"].get(TRANSCRIPT_BACKEND_KEY),
            }
    if not file_path:
        if not message_id or not chat_jid:
            raise ToolError("invalid_argument", "Provide chat_jid and message_id, or file_path")
        file_path = _audio_to_transcribe(chat_jid, message_id)
    try:
        result = transcribe_file(file_path, language=language or None, config=load_whisper_config())
    except FileNotFoundError as exc:
        raise ToolError("not_found", str(exc), file_path=file_path) from exc
    except TranscriptionError as exc:
        raise ToolError("internal", str(exc), file_path=file_path) from exc
    stored = bool(sha256 and (result.get("text") or "").strip()) and _store_transcript(sha256 or "", result)
    return {"success": True, "cached": False, "stored": stored, "sha256": sha256, "file_path": file_path, **result}


def shutdown_handler(signum, frame):
    """Handle shutdown signals gracefully to prevent zombie processes."""
    sys.exit(0)


def build_http_app(
    server: MCPServer, transport: str, token: str | None, rate_limit_per_minute: int = 0, **app_kwargs: Any
):
    """Build the ASGI app for the http/sse transports.

    Mirrors what MCPServer.run(transport=...) does internally, but returns the app so
    our middleware can sit in front of the SDK's own DNS-rebinding middleware:
    rate limit (outermost, throttles credential guessing too) → bearer auth → SDK.
    """
    if transport == "sse":
        app = server.sse_app(**app_kwargs)
    else:
        app = server.streamable_http_app(**app_kwargs)
    if token:
        app = BearerTokenMiddleware(app, token)
    if rate_limit_per_minute > 0:
        app = RateLimitMiddleware(app, rate_limit_per_minute)
    if metrics_enabled(os.getenv("WHATSAPP_MCP_METRICS")):
        # Outermost so /metrics answers without the MCP token and counts every
        # response, including 401/429 from the layers below. Its own optional
        # token (WHATSAPP_MCP_METRICS_TOKEN) is for endpoints exposed past the
        # tailnet (Funnel).
        app = MetricsMiddleware(app, token=os.getenv(METRICS_TOKEN_ENV))
    return app


if __name__ == "__main__":
    # Diagnostics go to stderr only: on the stdio transport stdout is the MCP
    # protocol channel. WHATSAPP_MCP_LOG_LEVEL controls verbosity (default INFO).
    _handler = logging.StreamHandler(sys.stderr)
    _handler.setFormatter(log_formatter(os.getenv(JSON_FORMAT_ENV)))
    logging.basicConfig(level=(os.getenv("WHATSAPP_MCP_LOG_LEVEL") or "INFO").upper(), handlers=[_handler])
    logging.getLogger("whatsapp_mcp").info("whatsapp-mcp-server %s", MCP_VERSION)

    # Operation-level access control (WHATSAPP_READ_ONLY, WHATSAPP_ALLOW_TOOLS,
    # WHATSAPP_DENY_TOOLS; tool_policy.py). Blocked tools are unregistered here,
    # before any transport starts, so they never appear in tools/list; mutating
    # tools refuse at call time as well. A value we cannot parse — an unreadable
    # boolean or a tool name that does not exist — stops the process rather than
    # running with a policy the operator did not mean.
    try:
        _tool_policy = load_tool_policy()
        _tool_policy.validate(registered_tool_names(mcp))
        # Read here only to reject a value we cannot parse before serving; the
        # tools themselves consult the environment per call (untrusted.py).
        _wrap_untrusted = parse_wrap_env(os.getenv(WRAP_ENV))
    except ValueError as exc:
        raise SystemExit(str(exc)) from None
    set_active_policy(_tool_policy)
    _removed_tools = apply_tool_policy(mcp, _tool_policy)
    logging.getLogger("whatsapp_mcp").info("%s", _tool_policy.summary(_removed_tools))
    logging.getLogger("whatsapp_mcp").info(
        "%s=%s: message content, names and notes %s",
        WRAP_ENV,
        "1" if _wrap_untrusted else "0",
        "wrapped in <untrusted> delimiters" if _wrap_untrusted else "returned as-is (tool descriptions warn)",
    )

    # Opt-in background transcription (TRANSCRIBE_ON_INGEST). Started before any
    # transport so it runs on stdio and http alike; it is a daemon thread that
    # only reads messages.db and writes notes.db, so it never delays a tool call
    # or a shutdown. Off unless asked for: whisper costs CPU on this machine.
    try:
        install_ingest_worker()
    except ValueError as exc:
        raise SystemExit(str(exc)) from None

    # Capture before any await — os.getppid() is dynamic.
    parent_pid = os.getppid()
    # Register signal handlers for clean shutdown
    signal.signal(signal.SIGINT, shutdown_handler)
    signal.signal(signal.SIGTERM, shutdown_handler)

    # Resolve the transport first: host/port are only used (and validated) for the
    # network transports, so a bad WHATSAPP_MCP_PORT can't break a stdio launch.
    # The localhost default keeps a remote server unreachable until explicitly opened up.
    try:
        transport = resolve_transport(os.getenv("WHATSAPP_MCP_TRANSPORT"))
        if transport == "stdio":
            install_stdio_parent_watchdog("WHATSAPP_PARENT_WATCHDOG_S", parent_pid=parent_pid)
            mcp.run(transport="stdio")
            raise SystemExit(0)

        host = resolve_host(os.getenv("WHATSAPP_MCP_HOST"))
        port = resolve_port(os.getenv("WHATSAPP_MCP_PORT"))
        # Explicit WHATSAPP_MCP_TOKEN wins; a non-loopback bind without one reuses
        # the bridge token so the deployment has a single secret to manage.
        token, token_source = resolve_http_token(os.getenv("WHATSAPP_MCP_TOKEN"), host, whatsapp_read_bridge_token)
        rate_limit = resolve_rate_limit(os.getenv("WHATSAPP_MCP_RATE_LIMIT"), token is not None)
        app_kwargs: dict[str, Any] = {
            "host": host,
            "max_request_body_size": resolve_max_body_bytes(os.getenv("WHATSAPP_MCP_MAX_BODY_BYTES")),
        }
        # The SDK enables a loopback-only Host allow-list when bound to loopback
        # and none otherwise; WHATSAPP_MCP_ALLOWED_HOSTS lets an operator keep
        # DNS-rebinding protection on for a non-loopback bind.
        security = build_transport_security(
            host,
            os.getenv("WHATSAPP_MCP_ALLOWED_HOSTS"),
            os.getenv("WHATSAPP_MCP_ALLOWED_ORIGINS"),
        )
        if security is not None:
            app_kwargs["transport_security"] = security
            if not security.enable_dns_rebinding_protection:
                print(
                    "WARNING: accepting any Host header (no WHATSAPP_MCP_ALLOWED_HOSTS set); "
                    "set it to the hostname(s) clients use to keep DNS-rebinding protection on",
                    file=sys.stderr,
                )
        if token is None and token_source == "none":
            print(
                "WARNING: no WHATSAPP_MCP_TOKEN set and no bridge token found; anyone who can reach "
                "this port can read and send WhatsApp messages. Set a token or keep the listener "
                "tailnet/loopback-only.",
                file=sys.stderr,
            )
    except ValueError as exc:
        raise SystemExit(str(exc)) from None

    # stdout is reserved for the protocol on stdio; log startup to stderr.
    auth_state = f"bearer token required, from {token_source}" if token else f"no auth ({token_source})"
    limit_state = f"{rate_limit} req/min per client" if rate_limit else "no rate limit"
    print(
        f"WhatsApp MCP server listening on {host}:{port} via {transport} ({auth_state}; {limit_state})",
        file=sys.stderr,
    )

    import uvicorn

    uvicorn.run(
        build_http_app(mcp, transport, token, rate_limit_per_minute=rate_limit, **app_kwargs),
        host=host,
        port=port,
        log_level="info",
    )
