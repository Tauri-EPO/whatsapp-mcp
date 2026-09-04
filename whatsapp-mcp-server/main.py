import logging
import os
import signal
import sys
from typing import Any

from mcp.server.mcpserver import MCPServer

from errors import ToolError, tool_errors
from http_auth import (
    BearerTokenMiddleware,
    RateLimitMiddleware,
    resolve_http_token,
    resolve_max_body_bytes,
    resolve_rate_limit,
)
from mcp_config import build_transport_security, resolve_host, resolve_port, resolve_transport
from observability import JSON_FORMAT_ENV, MetricsMiddleware, log_formatter, metrics_enabled
from parent_watchdog import install_stdio_parent_watchdog
from transcribe import TranscriptionError, transcribe_file
from transcribe import load_config as load_whisper_config
from whatsapp import (
    _read_bridge_token as whatsapp_read_bridge_token,
)
from whatsapp import (
    bridge_status as whatsapp_bridge_status,
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
    list_unread as whatsapp_list_unread,
)
from whatsapp import (
    manage_group_participants as whatsapp_manage_group_participants,
)
from whatsapp import (
    mark_messages_read as whatsapp_mark_messages_read,
)
from whatsapp import (
    msg_to_dict,
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
mcp = MCPServer("whatsapp", version=MCP_VERSION)


@mcp.tool()
@tool_errors
def bridge_status() -> dict[str, Any]:
    """Health of the WhatsApp bridge: is it reachable, paired and connected, and which build runs.

    Call this first when a tool returns nothing you expected or reports
    bridge_unavailable. Returns ok=true when the bridge is paired and connected;
    otherwise ok=false with a human-readable reason (unreachable, awaiting QR
    pairing, disconnected). Also reports uptime_seconds, store_bytes / media_bytes /
    media_files (cache size) and the build (version, commit, go, whatsmeow, fts5).
    Read-only; never fails, so it is safe to call before anything else.
    """
    return whatsapp_bridge_status()


@mcp.tool()
@tool_errors
def search_contacts(query: str) -> list[dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.

    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts


@mcp.tool()
@tool_errors
def get_contact(identifier: str) -> dict[str, Any]:
    """Look up a WhatsApp contact by phone number, LID, or full JID.

    Automatically detects the identifier type and queries appropriately.

    Args:
        identifier: Phone number, LID, or full JID. Examples:
                    - "12025551234" (phone number)
                    - "35047067385985" (LID - numeric)
                    - "12025551234@s.whatsapp.net" (phone JID)
                    - "184125298348272@lid" (LID JID)

    Returns:
        Dictionary with jid, name, display_name, is_lid, and resolved status
    """
    identifier = (identifier or "").strip()
    if not identifier:
        raise ValueError("identifier must be non-empty")

    # Detect identifier type and normalize to JID.
    bare_numeric_digits: str | None = None
    if "@" in identifier:
        # Already a JID - use as-is
        jid = identifier
        is_lid = jid.endswith("@lid") or jid.split("@", 1)[-1] == "lid"
    else:
        digits = "".join(c for c in identifier if c.isdigit())
        if digits:
            # LIDs can overlap phone-number lengths, so bare numeric inputs try phone first.
            jid = f"{digits}@s.whatsapp.net"
            is_lid = False
            if identifier.isdigit():
                bare_numeric_digits = digits
        else:
            # Non-numeric and not a JID; try as-is.
            jid = identifier
            is_lid = False

    jid_user = jid.split("@", 1)[0]

    display_name: str | None = None
    resolved = False

    # Prefer chats table lookup via get_chat (works for both phone and LID contacts).
    candidates: list[tuple[str, bool]] = [(jid, is_lid)]
    if bare_numeric_digits:
        candidates.append((f"{bare_numeric_digits}@lid", True))

    chat = None
    for candidate_jid, candidate_is_lid in candidates:
        chat = whatsapp_get_chat(candidate_jid, include_last_message=False)
        if chat:
            jid = candidate_jid
            is_lid = candidate_is_lid
            jid_user = jid.split("@", 1)[0]
            break

    if chat and chat.get("name"):
        display_name = chat["name"]
        resolved = display_name not in (jid, jid_user)
    else:
        # Fallback: best-effort sender-name resolution (may use fuzzy LIKE lookup).
        display_name = whatsapp_get_sender_name(jid)
        resolved = display_name not in (jid, jid_user, identifier)

    return {
        "identifier": identifier,
        "jid": jid,
        "phone_number": jid_user if not is_lid else None,
        "lid": jid_user if is_lid else None,
        "name": display_name if resolved else jid_user,
        "display_name": display_name,
        "is_lid": is_lid,
        "resolved": resolved,
    }


MAX_LIST_LIMIT = 500
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


@mcp.tool()
@tool_errors
def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_jid: str | None = None,
    chat_jid: str | None = None,
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
) -> dict[str, Any]:
    """Get WhatsApp messages matching specified criteria with optional context.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}. To page,
    pass next_cursor back as `cursor` (same filters and sort_by); stop when
    has_more is false. `page` still works but cursors are cheaper and stable
    while new messages arrive.

    Each message includes sender_display showing "Name (phone)" for easy identification.
    Media messages carry media_type and filename (the sender's original document name,
    or the bridge's generated name for images/audio/video); pass id + chat_jid to
    download_media to fetch the file.

    Args:
        after: ISO-8601 date string (e.g., "2026-01-01" or "2026-01-01T09:00:00")
        before: ISO-8601 date string (e.g., "2026-01-09" or "2026-01-09T18:00:00")
        sender_jid: Only messages from this sender: phone number with country code
               ("12025551234") or JID ("12025551234@s.whatsapp.net")
        chat_jid: Chat JID to filter by (e.g., "12025551234@s.whatsapp.net" or group JID)
        query: Search term to filter messages by content. Accent-insensitive and
               word-based (e.g. "orcamento" finds "orçamento", "ana" does not match
               "semana"); supports AND / OR / NOT, "exact phrase" and prefix*
               (e.g. 'boleto OR fatura', '"nota fiscal"', 'orcament*')
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
    """
    # Cap limit at 500 to prevent excessive queries
    limit = max(0, min(limit, MAX_LIST_LIMIT))
    context_before, context_after = _cap_context(limit, include_context, context_before, context_after)
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_jid,
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
        cursor=cursor,
    )
    return messages.to_dict()


@mcp.tool()
@tool_errors
def list_unread(limit_chats: int = 20, limit_per_chat: int = 5, since: str | None = None) -> dict[str, Any]:
    """What is waiting for me: chats with unread inbound messages and their newest unread rows.

    One call instead of list_chats followed by list_messages(unread_only=True) per
    chat. A message is unread when it is inbound and newer than the chat's read
    marker (as read on any of your devices); chats never read count as entirely
    unread. Reactions, poll votes and deleted messages are not counted.

    Args:
        limit_chats: Max chats to return, most recently active first (default 20, max 100)
        limit_per_chat: Newest unread messages to include per chat, oldest first (default 5, max 50)
        since: Only count messages after this ISO-8601 timestamp (e.g. "2026-09-04T08:00:00")

    Returns:
        {"chats": [{chat_jid, chat_name, is_group, unread_count, latest_unread,
        last_read_time, messages: [...]}], "total_unread": N, "chats_with_unread": N}.
        Use mark_messages_read(chat_jid, message_ids) once handled.
    """
    return whatsapp_list_unread(limit_chats=limit_chats, limit_per_chat=limit_per_chat, since=since)


@mcp.tool()
@tool_errors
def list_chats(
    query: str | None = None,
    limit: int = 50,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
    cursor: str | None = None,
) -> dict[str, Any]:
    """Get WhatsApp chats matching specified criteria.

    Returns {"items": [...], "next_cursor": str|null, "has_more": bool}; pass
    next_cursor back as `cursor` to fetch the following page.

    Args:
        query: Search term to filter chats by name or JID
        limit: Max chats to return (default 50, max 200)
        page: Page number for pagination (default 0); ignored when cursor is set
        cursor: next_cursor from the previous page
        include_last_message: Include the last message in each chat (default True)
        sort_by: "last_active" (default, most recent first) or "name" (alphabetical)

    Returns:
        Chat dictionaries with jid, name, is_group, last_message_time, last_message,
        last_sender, last_is_from_me, last_read_time and unread. `last_read_time` is
        how far the chat has been read on any device (null if never reported); `unread`
        is true when the last message is inbound and newer than that marker, so chats
        already read on the phone are not reported as unread.
    """
    # Cap limit at 200 to prevent excessive queries
    limit = min(limit, 200)
    chats = whatsapp_list_chats(
        query=query, limit=limit, page=page, include_last_message=include_last_message, sort_by=sort_by, cursor=cursor
    )
    return chats.to_dict()


@mcp.tool()
@tool_errors
def get_chat(chat_jid: str, include_last_message: bool = True) -> dict[str, Any]:
    """Get WhatsApp chat metadata by JID.

    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)

    Returns:
        Chat dictionary — same shape as list_chats, including last_read_time and unread.
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    if chat is None:
        raise ToolError("not_found", f"No chat {chat_jid} in the archive")
    return chat


@mcp.tool()
@tool_errors
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
def get_contact_chats(contact_jid: str, limit: int = 20, page: int = 0, cursor: str | None = None) -> dict[str, Any]:
    """Get all WhatsApp chats involving the contact.

    Args:
        contact_jid: The contact's JID or phone number
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(contact_jid, limit, page, cursor=cursor)
    return chats.to_dict()


@mcp.tool()
@tool_errors
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
def get_message_context(chat_jid: str, message_id: str, before: int = 5, after: int = 5) -> dict[str, Any]:
    """Get context around a specific WhatsApp message.

    Messages use the same shape as list_messages (including media_type and filename
    for media messages).

    Args:
        chat_jid: JID of the chat containing the message (message IDs are only
                  unique per chat; both come from list_messages rows)
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after, chat_jid or None)
    return {
        "message": msg_to_dict(context.message),
        "before": [msg_to_dict(message) for message in context.before],
        "after": [msg_to_dict(message) for message in context.after],
    }


@mcp.tool()
@tool_errors
def send_message(
    chat_jid: str,
    message: str,
    quoted_message_id: str = "",
    quoted_sender_jid: str = "",
    quoted_content: str = "",
    mentions: list[str] | None = None,
) -> dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

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

    Returns:
        {"success": true, "message": ..., "message_id": ..., "chat_jid": ..., "timestamp": ...}.
        Keep message_id + chat_jid to react to, quote, edit or delete this message later.
    """
    # Validate input
    if not chat_jid:
        raise ToolError("invalid_argument", "chat_jid must be provided")

    success, status_message, sent = whatsapp_send_message(
        chat_jid, message, quoted_message_id, quoted_sender_jid, quoted_content, mentions
    )
    return {"success": success, "message": status_message, **sent}


@mcp.tool()
@tool_errors
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
def list_group_members(chat_jid: str) -> dict[str, Any]:
    """List the participants of a WhatsApp group with names and admin flags.

    Queries WhatsApp live through the bridge, so the bridge must be connected. Each
    member has jid (address to use when mentioning or messaging them), phone_number
    (when known), lid, name (from your contacts, when known), display, is_admin and
    is_super_admin. Also returns the group's name, topic and owner.

    Args:
        chat_jid: The group JID (e.g. "120363000000000001@g.us")
    """
    return whatsapp_get_group_members(chat_jid)


@mcp.tool()
@tool_errors
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
def update_group(chat_jid: str, name: str | None = None, description: str | None = None) -> dict[str, Any]:
    """Rename a WhatsApp group and/or change its description (admin only).

    Args:
        chat_jid: The group JID
        name: New subject (omit to keep)
        description: New description; empty string clears it (omit to keep)
    """
    return whatsapp_update_group(chat_jid, name=name, description=description)


@mcp.tool()
@tool_errors
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
def leave_group(chat_jid: str) -> dict[str, Any]:
    """Leave a WhatsApp group. Irreversible without a new invite; the archive keeps the history.

    Args:
        chat_jid: The group JID
    """
    return whatsapp_leave_group(chat_jid)


@mcp.tool()
@tool_errors
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
def edit_message(chat_jid: str, message_id: str, text: str) -> dict[str, Any]:
    """Edit the text of a message this account sent (WhatsApp allows it for about 15 minutes).

    Only own messages can be edited; recipients see the new text with an
    "edited" marker. The local archive is updated too. Use the message_id
    returned by send_message.

    Args:
        chat_jid: Chat containing the message
        message_id: ID of the message to edit
        text: The new text
    """
    return whatsapp_edit_message(chat_jid, message_id, text)


@mcp.tool()
@tool_errors
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
def mark_messages_read(
    chat_jid: str,
    message_ids: list[str],
    sender_jid: str = "",
    timestamp: str | None = None,
) -> dict[str, Any]:
    """Mark selected WhatsApp messages as read and send read receipts.

    This is an explicit external side effect. All message IDs must belong to the
    same chat and sender.
    Read receipts are the ordinary "blue ticks"; they do not consume view-once media
    (that needs a separate view receipt the bridge never sends).

    Args:
        chat_jid: JID of the chat containing the messages
        message_ids: IDs of the messages to mark as read
        sender_jid: JID or bare phone number of the original sender; required for groups
        timestamp: Optional RFC 3339 read timestamp; defaults to the current time

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_mark_messages_read(message_ids, chat_jid, sender_jid, timestamp)
    return {"success": success, "message": status_message}


@mcp.tool()
@tool_errors
def send_file(chat_jid: str, media_path: str, caption: str = "") -> dict[str, Any]:
    """Send a file (image, video, document) via WhatsApp, optionally with a caption.

    When `caption` is provided, the file and text arrive as a single
    attachment-with-caption message (one bubble in the WA UI), instead of
    needing a separate follow-up send_message call. For group chats use the JID.

    Args:
        chat_jid: Phone number with country code (no symbols), direct-chat JID or
                  group JID
        media_path: Absolute path to the media file (image, video, document)
        caption: Optional text rendered with the file as a caption. Omit for a
                 bare attachment.

    Returns:
        A dictionary containing success status and a status message
    """

    # Call the whatsapp_send_file function
    success, status_message, sent = whatsapp_send_file(chat_jid, media_path, caption)
    return {"success": success, "message": status_message, **sent}


@mcp.tool()
@tool_errors
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
def download_media(chat_jid: str, message_id: str) -> dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.

    Args:
        chat_jid: The JID of the chat containing the message
        message_id: The ID of the message containing the media

    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path = whatsapp_download_media(message_id, chat_jid)

    if file_path:
        return {"success": True, "message": "Media downloaded successfully", "file_path": file_path}
    raise ToolError("internal", "Bridge reported success without a file path")


@mcp.tool()
@tool_errors
def transcribe_audio(
    chat_jid: str = "",
    message_id: str = "",
    file_path: str = "",
    language: str = "",
) -> dict[str, Any]:
    """Transcribe a WhatsApp voice note (or any audio file) to text with local whisper.cpp.

    Pass either message_id + chat_jid (the audio is downloaded via the bridge first)
    or an absolute file_path that is already on disk. Requires a whisper backend
    configured through WHISPER_URL (whisper.cpp server) or WHISPER_BIN + WHISPER_MODEL;
    nothing is sent to a cloud API.

    Args:
        chat_jid: JID of the chat containing the message
        message_id: ID of the audio/voice message to transcribe
        file_path: Alternative to message_id/chat_jid: path of an audio file on disk
        language: ISO-639-1 language code (default WHISPER_LANGUAGE, "pt"); "auto" to detect

    Returns:
        A dictionary with success, text, language, backend and file_path (or an error message)
    """
    if not file_path:
        if not message_id or not chat_jid:
            raise ToolError("invalid_argument", "Provide chat_jid and message_id, or file_path")
        file_path = whatsapp_download_media(message_id, chat_jid)
        if not file_path:
            raise ToolError("internal", "Failed to download media for transcription")
    try:
        result = transcribe_file(file_path, language=language or None, config=load_whisper_config())
    except FileNotFoundError as exc:
        raise ToolError("not_found", str(exc), file_path=file_path) from exc
    except TranscriptionError as exc:
        raise ToolError("internal", str(exc), file_path=file_path) from exc
    return {"success": True, "file_path": file_path, **result}


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
        # Outermost so /metrics answers without a token and counts every
        # response, including 401/429 from the layers below.
        app = MetricsMiddleware(app)
    return app


if __name__ == "__main__":
    # Diagnostics go to stderr only: on the stdio transport stdout is the MCP
    # protocol channel. WHATSAPP_MCP_LOG_LEVEL controls verbosity (default INFO).
    _handler = logging.StreamHandler(sys.stderr)
    _handler.setFormatter(log_formatter(os.getenv(JSON_FORMAT_ENV)))
    logging.basicConfig(level=(os.getenv("WHATSAPP_MCP_LOG_LEVEL") or "INFO").upper(), handlers=[_handler])
    logging.getLogger("whatsapp_mcp").info("whatsapp-mcp-server %s", MCP_VERSION)
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
