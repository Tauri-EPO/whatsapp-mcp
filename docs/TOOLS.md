# Tool reference

Every MCP tool the server exposes, with parameters and behaviour notes. The tool docstrings in `whatsapp-mcp-server/main.py` are what the model reads; this page is the human copy. Chat allow-listing (`WHATSAPP_ALLOWED_CHATS`) applies to all of them, see [CONFIGURATION.md](CONFIGURATION.md).

Messages include `sender_display` showing "Name (phone)" format for easy identification by agents.

## Contact Operations

### `search_contacts`

Search contacts by name or phone number.

**Parameters:**

- `query` (required): Name or phone number to search

**Natural Language Examples:**

- "Find contacts named John"
- "Search for phone number 555-1234"
- "Who has the phone number starting with +1?"

### `get_contact`

Resolve a WhatsApp contact name from a phone number, LID, or full JID.

**Parameters:**

- `identifier` (required): Phone number, LID, or full JID (aliases: `phone_number`, `phone`)
  - Examples: `12025551234`, `184125298348272`, `12025551234@s.whatsapp.net`, `184125298348272@lid`

**Natural Language Examples:**

- "What's the name for phone number 5551234567?"
- "Look up who owns this number"
- "Who is 184125298348272@lid?"

## Message Operations

### `list_messages`

Get messages with filters, date ranges, and sorting.

**Parameters:**

- `chat_jid` (optional): Filter by specific chat JID
- `limit` (optional): Number of messages (default 50, max 500)
- `page` (optional): Page number (default 0)
- `before` / `after` (optional): ISO-8601 bounds (`2026-01-09` or `2026-01-09T18:00:00`)
- `sender_phone_number` (optional): Only messages from this number
- `include_context` (optional, default `true`), `context_before` / `context_after` (default 1, max 50 each): surrounding messages for every match. The whole result is capped at 2000 rows; with large `limit` values the windows shrink to fit, so prefer `include_context=false` when paging through many matches
- `query` (optional): Search term. With the bridge's FTS5 index (default in the Docker image and in builds with `-tags sqlite_fts5`) it is accent-insensitive and word-based: `orcamento` finds `orçamento`, `ana` no longer matches `semana`, and `AND` / `OR` / `NOT`, `"exact phrase"` and `prefix*` work. Queries in scripts without word spacing (CJK, Thai) and bridges built without FTS5 use a plain substring match
- `sort_by` (optional): "newest" (default), "oldest", or "relevance" (best match for `query` first)
- `include_deleted` (optional, default `true`): keep messages that were "deleted for everyone". They are returned with their original text/media and a `deleted_at` timestamp; `false` hides them
- `unread_only` (optional, default `false`): only inbound messages newer than their chat's read marker (`last_read_time`, as read on any linked device). `unread_only=true, sort_by="oldest", include_context=false` lists what still needs attention, oldest first

Revoked messages ("delete for everyone", by the sender or by you) stay in this
archive with their content, media and `filename`; only `deleted_at` is set.
This is deliberate: the archive is the account owner's copy. To really forget a
message locally use `delete_message` with `for_everyone=false`.

View-once photos, videos and voice notes are archived like any other media,
with `view_once: true` and a `🔒` prefix on the content; the phone's single
viewing is unaffected because the bridge never sends the view receipt. This is
your own account's archive; treat it accordingly.

Each returned message includes `media_type` and, for media messages, `filename`
(the sender's original document name, or the bridge's generated
`<type>_<timestamp>_<id>.<ext>` for images, audio, video and stickers). Pass the
message `id` and `chat_jid` to `download_media` to fetch the file.

**Natural Language Examples:**

- "Show me the last 100 messages from today"
- "Get messages from the family group chat"
- "Find messages from last week"

### `send_message`

Send a text message to a contact or group, optionally as a quoted reply.

**Parameters:**

- `recipient` (required): Phone number or group JID
- `message` (required): Text content to send
- `quoted_message_id` (optional): ID of the message to reply to. When provided, the sent message appears as a quoted reply in WhatsApp.
- `quoted_sender_jid` (optional): Full JID of the author of the quoted message. Required for group replies so WhatsApp renders the correct attribution header.
- `quoted_content` (optional): Text content of the quoted message, used for the reply preview. Only plain text is supported.
- `mentions` (optional): List of users to @-mention, as phone numbers with country code (e.g. `["12025551234"]`) or JIDs. For each entry the message text must contain a matching `@<number>` token (e.g. `"thanks @12025551234!"`), which recipients' devices render as a highlighted, tappable mention that also notifies the user. Only meaningful in group chats.

Inbound quoted replies are stored automatically. The `quoted_message_id` field in each message returned by `list_messages` indicates which message it is replying to (or `null` for non-replies).

**Natural Language Examples:**

- "Send 'Hello!' to +1234567890"
- "Message the team group saying 'Meeting at 3pm'"
- "Reply to that message saying 'Sounds good'"

### `get_poll_results`

Tally of a native WhatsApp poll.

**Parameters:**

- `message_id` (required): ID of the poll message
- `chat_jid` (required): JID of the chat

The bridge stores poll creations as messages with `media_type = "poll"` (content
`📊 <question> — options: a | b | c`) and each vote as `media_type = "poll_vote"`
with `poll_message_id` pointing at the poll, so both show up in `list_messages`
and search. Votes are end-to-end encrypted with the poll's key. The bridge
learns that key from the creation message, live or through history sync (so
polls created before the bridge ran, and votes cast while it was down, are
decoded as long as the phone included them in the sync). Votes whose key was
never seen are kept and reported in `undecodable_votes` rather than silently
dropped. Returns `question`, `selectable_count`, per-option `count` and
`voters`, each voter's latest `selected` options, `total_voters` and
`undecodable_votes`. Respects `WHATSAPP_ALLOWED_CHATS`.

### `delete_message`

Revoke a message for everyone (WhatsApp's "Delete for everyone", own messages
only) or drop it from the local archive without touching WhatsApp.

**Parameters:**

- `chat_jid` (required): JID of the chat
- `message_id` (required): ID of the message
- `for_everyone` (optional, default `false`): `true` revokes on WhatsApp; `false` only removes the local copy

Revoked messages keep their content locally with `deleted_at` set, the same as
revokes received from another device. Local-only deletion removes the row (the
FTS index follows); downloaded media is left in `store/`. Respects
`WHATSAPP_ALLOWED_CHATS`.

### `mark_messages_read`

Mark one or more messages from the same chat and sender as read. This explicitly
sends WhatsApp read receipts; reading or searching messages never does so
automatically.

**Parameters:**

- `message_ids` (required): IDs of messages from the same chat and sender
- `chat_jid` (required): JID of the chat containing the messages
- `sender_jid` (required for groups): Full JID or bare phone number of the original message sender
- `timestamp` (optional): RFC 3339 read timestamp; defaults to the current time

**Natural Language Examples:**

- "Mark those messages as read"
- "Mark the last three messages from Alice in the team group as read"

### `send_reaction`

Send (or remove) an emoji reaction to a message.

**Parameters:**

- `recipient` (required): Chat JID the message belongs to (phone JID or group JID)
- `message_id` (required): ID of the message to react to
- `emoji` (required): Reaction emoji (e.g. `"👍"`). Pass an empty string `""` to remove an existing reaction.
- `from_me` (optional, default `false`): Whether the original message was sent by the current user
- `sender_jid` (optional): Full JID of the original message sender — required for group messages when `from_me` is `false` so the correct WhatsApp key is built

Inbound reactions received from others are stored automatically as messages with `media_type = "reaction"`. The `reaction_to_message_id` field in each reaction message indicates which message was reacted to.

When webhook forwarding is enabled, inbound reactions are also posted to `WEBHOOK_URL` as typed events. Reaction removals use an empty `content`/`reactionEmoji` and `reactionRemoved: true`.

```json
{
  "eventType": "reaction",
  "sender": "15551234567",
  "chatJID": "15551234567@s.whatsapp.net",
  "isFromMe": true,
  "content": "👍",
  "messageId": "reaction-stanza-id",
  "mediaType": "reaction",
  "reactionToMessageId": "target-message-id",
  "reactionEmoji": "👍",
  "reactionRemoved": false
}
```

**Natural Language Examples:**

- "React to that message with a thumbs up"
- "Remove my reaction from the last message in the group chat"

### `send_file`

Send a media file (image, video, document).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to the file
- `caption` (optional): Caption for the media

The bridge only reads files inside configured media roots. By default this is
`~/.local/share/whatsapp-mcp/outbox`; set `WHATSAPP_MEDIA_ROOTS` to allow
additional absolute directories.

### `send_audio_message`

Send a voice message (automatically converts to Opus .ogg format).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to audio file

Converted audio is sent through the same media-path confinement as
`send_file`.

### `transcribe_audio`

Transcribe a voice note (or any audio file) to text with local
[whisper.cpp](https://github.com/ggml-org/whisper.cpp). Nothing leaves the
machine; there is no cloud fallback.

**Parameters:**

- `message_id` + `chat_jid`: the audio message (downloaded via the bridge first), **or**
- `file_path`: absolute path of an audio file already on disk
- `language` (optional): ISO-639-1 code, default `WHISPER_LANGUAGE` (`pt`); `auto` to detect

Requires a whisper backend, configured with either `WHISPER_URL` (a running
whisper.cpp `whisper-server`, see the `whisper` profile in
[`docs/DOCKER.md`](DOCKER.md)) or `WHISPER_BIN` + `WHISPER_MODEL` (a local
`whisper-cli` binary and a `ggml-*.bin` model). Audio is normalised to 16 kHz WAV
with ffmpeg before transcription. Returns `text`, `language`, `backend` and the
local `file_path`.

### `download_media`

Download media from a received message.

**Parameters:**

- `message_id` (required): ID of the message with media
- `chat_jid` (required): JID of the chat containing the message

By default the bridge caches every inbound file as it arrives, so this tool
usually returns immediately. On a server you can turn that off
(`WHATSAPP_MEDIA_AUTODOWNLOAD=false`) and/or expire old files
(`WHATSAPP_MEDIA_RETENTION_DAYS=N`); either way `download_media` fetches
what is missing. `/api/health` reports `store_bytes`, `media_bytes` and
`media_files` so you can watch the cache grow.

WhatsApp CDN URLs expire after a few days. When a stored URL answers 403/404/410
(typical for history-synced or forwarded media), the bridge automatically asks
the **sender's phone** to re-upload the file via WhatsApp's media-retry protocol,
downloads it from the refreshed path, and persists that path for next time. The
sender's phone must be online; the bridge waits up to 30 seconds before giving
up with a clear error. Media the phone no longer has cannot be recovered.

## Chat Operations

All chat tools (`list_chats`, `get_chat`, `get_direct_chat_by_contact`,
`get_contact_chats`) return the same chat shape:

```jsonc
{
  "jid": "1234567890@s.whatsapp.net",
  "name": "Alice",
  "is_group": false,
  "last_message_time": "2024-01-15T10:30:00+00:00",
  "last_message": "hello world",       // null when include_last_message=false
  "last_sender": "1234567890",         // null when include_last_message=false
  "last_is_from_me": false,
  "last_read_time": "2024-01-15T09:00:00+00:00", // how far the chat is read
  "unread": true                       // last message is inbound and unread
}
```

### Read state (`last_read_time` / `unread`)

`last_read_time` is the bridge's read marker for the chat, fed by read
receipts from your own devices and backfilled from history sync. `unread` is
derived from it: true when the chat's last message is inbound and newer than
the marker. This distinguishes a genuinely unread chat from one whose last
message merely happens to be inbound but was already read on the phone.

Caveats:

- **The marker only moves forward.** Marking an already-read chat as *unread*
  again on the phone is not reflected.
- **No marker means no read was ever reported** — for a chat with an inbound
  last message, `unread` then falls back to the old heuristic and reports
  true. Stores written by a bridge older than the `chats.last_read_time`
  column report `last_read_time: null` and behave the same way.
- **`unread` is a chat-level flag, not an unread count.** WhatsApp's unread
  counter is not persisted.

### `list_chats`

List all chats with metadata.

**Parameters:**

- `limit` (optional): Number of chats (default 50, max 200)

### `get_chat`

Get specific chat metadata by JID.

**Parameters:**

- `jid` (required): Chat JID

### `get_direct_chat_by_contact`

Find a direct message chat with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

### `get_contact_chats`

List all chats involving a specific contact.

**Parameters:**

- `phone` (required): Phone number of the contact

### `get_last_interaction`

Get the last message exchanged with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

### `list_group_members`

List the participants of a group (live query through the bridge).

**Parameters:**

- `group_jid` (required): The group JID (`...@g.us`)

Returns the group's `name`, `topic`, `owner_jid` and a `members` list with
`jid`, `phone_number`, `lid`, `name` (from your contacts when known),
`display`, `is_admin` and `is_super_admin`. Respects `WHATSAPP_ALLOWED_CHATS`.

### `get_message_context`

Get messages around a specific message for context.

**Parameters:**

- `message_id` (required): ID of the target message
- `chat_jid` (recommended): JID of the chat. Message IDs are only unique per chat; with it the lookup is an indexed primary-key hit, without it the most recent match is used
- `before` (optional): Number of messages before (default 5)
- `after` (optional): Number of messages after (default 5)

## Call history (data reference)

The bridge captures incoming WhatsApp voice and video calls live into a
dedicated `calls` table in `messages.db`. When a 1:1 call arrives
(`CallOffer`) or a group call is announced (`CallOfferNotice`), a row is
inserted with `result='in_progress'`. Subsequent `CallAccept` /
`CallReject` / `CallTerminate` events update the row — final result becomes
`answered`, `rejected`, `missed`, or `ended` depending on the event
sequence. See the state-machine comment above `StoreCallOffer` in `store.go`
for the exact transitions.

### Schema

```sql
CREATE TABLE calls (
    call_id TEXT,
    chat_jid TEXT,          -- group JID for group calls, call creator JID for 1:1
    from_jid TEXT,          -- JID of whoever started the call
    timestamp TIMESTAMP,    -- call start time
    is_from_me BOOLEAN,
    call_type TEXT,         -- 'voice' or 'video'
    is_group BOOLEAN,
    result TEXT,            -- 'in_progress' | 'answered' | 'ended' |
                            --   'missed' | 'rejected'
    duration_sec INTEGER,   -- computed when the call terminates
    ended_at TIMESTAMP,
    reason TEXT,            -- terminate reason string from whatsmeow
    PRIMARY KEY (call_id, chat_jid)
);
```

### Caveats

- **Outbound calls are not captured.** WhatsApp's primary device handles
  calls it initiates without notifying linked devices, so the bridge never
  sees an event for them.
- **Call results only reflect what the bridge saw.** If the bridge is
  offline when a call happens, the events are lost.
- **1:1 calls default to `call_type='voice'`.** `CallOffer` events don't
  expose media type directly (it's buried in the binary call data). Group
  calls via `CallOfferNotice` include a `Media` field and are recorded
  accurately as voice or video.
