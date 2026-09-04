# WhatsApp MCP Server

[![CI](https://github.com/Tauri-EPO/whatsapp-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/Tauri-EPO/whatsapp-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)](https://www.python.org/downloads/)
[![Go 1.25+](https://img.shields.io/badge/go-1.25+-00ADD8.svg)](https://go.dev/)

A Model Context Protocol (MCP) server for WhatsApp, enabling AI clients to read and send WhatsApp messages.

> **This is the Tauri-EPO fork**, tuned for an always-on home server reached over Tailscale by remote MCP clients. It tracks [verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp) (maintained by [Very Good Plugins](https://verygoodplugins.com/?utm_source=github)), which in turn descends from [Luke Harries](https://github.com/lharries/whatsapp-mcp)' original. See [About this fork](#about-this-fork) for what is different and why.

<p align="center">
  <a href="https://github.com/user-attachments/assets/9475af1d-2369-4315-9ccc-823dba2c5c32"><strong>Watch the WhatsApp MCP demo video</strong></a>
</p>

<p align="center">
  <sub>Product demo generated with Remotion using simulated data.</sub>
</p>

## About this fork

**Goal.** Run WhatsApp MCP 24/7 on a small home server, expose it over
**streamable HTTP** through Tailscale, and let remote MCP clients (an AI bot on
another machine, an IDE on a laptop) use one WhatsApp account. Upstream is
tuned for a developer laptop talking to Claude Desktop over stdio; this fork is
tuned for that server.

**What is different here**

| Area | Upstream (VGP) | This fork |
| --- | --- | --- |
| Deployment | `go run` + `uv run` on a laptop; no container story | `docker-compose.yml` with bridge + MCP images, shared store volume, healthchecks. [`docs/DOCKER.md`](docs/DOCKER.md) |
| Remote access | `WHATSAPP_MCP_HOST=0.0.0.0` answered 421 to every non-loopback `Host` | `WHATSAPP_MCP_ALLOWED_HOSTS` allow-list for Tailscale / Docker / proxy hostnames, loopback always kept |
| Expired media | 403/404/410 from the CDN was a hard failure | Bridge asks the sender's phone to re-upload (WhatsApp media-retry) and persists the fresh path |
| Voice notes | Out of scope upstream | `transcribe_audio` tool with local whisper.cpp; optional `whisper` compose profile. No cloud API |
| Concurrency safety | Two bridges on one store flap forever (`StreamReplaced`) | Single-instance lock on `store/.bridge.lock`; the second bridge refuses to start |
| Message metadata | `filename` stored but not exposed | `filename` returned on `list_messages` / `get_message_context` |
| Python SDK | `mcp<2` (FastMCP) | MCP SDK v2 (`MCPServer`), current `cryptography`, `pytest`, `ruff` |
| Releases | release-please cuts tags and `CHANGELOG.md` | No releases here; `main` is the deployable state |

**How we think about it**

- **Hard fork (since 2026-09-04).** The fork is ahead of upstream and no
  longer merges `upstream/main`. Dependencies move here first (Dependabot
  stays on, PRs merged once CI is green); refactors are welcome; there are no
  releases, `main` is the deployable state.
- **Upstream is an idea source, not a merge target.** Fixes in
  [verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp)
  and [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) are
  harvested by reading their PRs and reimplementing the delta here, credited
  in the commit. whatsmeow is bumped on a schedule because WhatsApp's protocol
  drifts. The routine is written down in [`AGENTS.md`](AGENTS.md) §2.
- **whatsmeow only.** No Baileys, no alternative WhatsApp Web stacks.
- **Fail-safe network defaults.** The MCP port is published on `127.0.0.1`
  and fronted by `tailscale serve`; Funnel requires `WHATSAPP_MCP_TOKEN` and,
  ideally, `WHATSAPP_ALLOWED_CHATS`.
- **Small PRs, always green.** One concern per PR, tests and docs in the same
  PR, squash-merged only with every CI check passing. Open work is tracked in
  the [fork hardening epic](https://github.com/Tauri-EPO/whatsapp-mcp/issues/64).

Issues and PRs for this fork live at
[Tauri-EPO/whatsapp-mcp](https://github.com/Tauri-EPO/whatsapp-mcp).

## Features

- **Message Management**: Search and read personal WhatsApp messages (text, images, videos, documents, audio)
- **Contact Search**: Search contacts by name or phone number with `sender_display` format ("Name (phone)")
- **Send Messages**: Send text messages to individuals or groups
- **Read Receipts**: Explicitly mark selected messages as read across linked devices
- **Media Support**: Send and download images, videos, documents, and voice messages
- **Call History**: Capture incoming voice/video calls into a local SQLite table (live, 1:1 and group)
- **Webhook Integration**: Forward incoming messages to external services
- **Local Storage**: All messages stored locally in SQLite - only sent to the AI client when you allow it
- **Remote MCP over HTTP** *(fork)*: streamable-HTTP transport with a `Host` allow-list for Tailscale / Docker hostnames
- **Docker Compose** *(fork)*: bridge + MCP containers for an always-on server, optional whisper.cpp sidecar
- **Voice-note transcription** *(fork)*: `transcribe_audio` with local whisper.cpp, Portuguese by default
- **Expired-media recovery** *(fork)*: automatic WhatsApp media-retry when CDN links have expired

## Installation

### Prerequisites

- Go 1.25+
- Python 3.11+
- [uv](https://docs.astral.sh/uv/) package manager
- Claude Desktop or Cursor
- FFmpeg (optional, for voice message conversion)

### Quick Start

1. **Clone the repository**

   ```bash
   git clone https://github.com/verygoodplugins/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Start the WhatsApp bridge**

   ```bash
   cd whatsapp-bridge
   go run -tags sqlite_fts5 .   # the tag enables full-text message search
   ```

   On first start, the bridge prints and stores a local REST API token at
   `whatsapp-bridge/store/.bridge-token`. Scan the QR code with WhatsApp on
   your phone to authenticate.

3. **Configure Claude Desktop**

   Add to `~/Library/Application Support/Claude/claude_desktop_config.json`:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "uv",
         "args": [
           "--directory",
           "/path/to/whatsapp-mcp/whatsapp-mcp-server",
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   Replace `/path/to/whatsapp-mcp` with your actual path.

4. **Restart Claude Desktop**

### Docker Compose (always-on server)

To run both components as containers on a home server or VPS and expose the MCP
server over streamable HTTP:

```bash
cp .env.example .env            # optional: WHATSAPP_MCP_ALLOWED_HOSTS, token, webhook
docker compose up -d --build
docker compose logs -f bridge   # scan the QR code on first run
```

The MCP endpoint is published on `127.0.0.1:8000/mcp`; front it with
`tailscale serve` or an authenticating reverse proxy. Session, `messages.db`,
media and the bridge token live in the `whatsapp-store` volume. Pairing,
Tailscale setup, health semantics and backups are covered in
[`docs/DOCKER.md`](docs/DOCKER.md).

### Updating

Pull the latest changes, then refresh whichever components moved:

```bash
git pull
```

| You changed                                                              | What to do                                                                                                                                            |
| ------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Bridge code** (`whatsapp-bridge/*.go`) and you run `go run -tags sqlite_fts5 .` | Nothing — `go run` recompiles each launch. Just restart the bridge.                                                                          |
| **Bridge code** and you run a built binary                               | `cd whatsapp-bridge && go build -tags sqlite_fts5 -o whatsapp-bridge && ./whatsapp-bridge`                                                            |
| **MCP server** (`whatsapp-mcp-server/*.py`, `pyproject.toml`, `uv.lock`) | Restart Claude Desktop / Cursor — `uv` re-resolves from the lockfile on next launch. Force a sync with `cd whatsapp-mcp-server && uv sync` if needed. |

Updates do **not** require re-pairing or deleting `whatsapp.db` — your session and message history are preserved. Re-pairing is only needed when explicitly requesting full history (see [Requesting full history](#requesting-full-history)).

For `v0.2.1` and later, restart both the bridge and MCP server after updating
so the MCP server can read the bridge token. If the two components do not share
the same checkout, set the same `WHATSAPP_BRIDGE_TOKEN` value in both
environments.

### Cursor IDE Configuration

Add to your Cursor MCP settings (`~/.cursor/mcp.json`):

```json
{
  "mcp": {
    "servers": {
      "whatsapp": {
        "command": "uv",
        "args": [
          "--directory",
          "/path/to/whatsapp-mcp/whatsapp-mcp-server",
          "run",
          "main.py"
        ]
      }
    }
  }
}
```

## Tools

Messages include `sender_display` showing "Name (phone)" format for easy identification by agents.

### Contact Operations

#### `search_contacts`

Search contacts by name or phone number.

**Parameters:**

- `query` (required): Name or phone number to search

**Natural Language Examples:**

- "Find contacts named John"
- "Search for phone number 555-1234"
- "Who has the phone number starting with +1?"

#### `get_contact`

Resolve a WhatsApp contact name from a phone number, LID, or full JID.

**Parameters:**

- `identifier` (required): Phone number, LID, or full JID (aliases: `phone_number`, `phone`)
  - Examples: `12025551234`, `184125298348272`, `12025551234@s.whatsapp.net`, `184125298348272@lid`

**Natural Language Examples:**

- "What's the name for phone number 5551234567?"
- "Look up who owns this number"
- "Who is 184125298348272@lid?"

### Message Operations

#### `list_messages`

Get messages with filters, date ranges, and sorting.

**Parameters:**

- `chat_jid` (optional): Filter by specific chat JID
- `limit` (optional): Number of messages (default 50, max 500)
- `before_date` (optional): Messages before this date (YYYY-MM-DD)
- `after_date` (optional): Messages after this date (YYYY-MM-DD)
- `query` (optional): Search term. With the bridge's FTS5 index (default in the Docker image and in builds with `-tags sqlite_fts5`) it is accent-insensitive and word-based: `orcamento` finds `orçamento`, `ana` no longer matches `semana`, and `AND` / `OR` / `NOT`, `"exact phrase"` and `prefix*` work. Queries in scripts without word spacing (CJK, Thai) and bridges built without FTS5 use a plain substring match
- `sort_by` (optional): "newest" (default), "oldest", or "relevance" (best match for `query` first)
- `include_deleted` (optional, default `true`): keep messages that were "deleted for everyone". They are returned with their original text/media and a `deleted_at` timestamp; `false` hides them

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

#### `send_message`

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

#### `get_poll_results`

Tally of a native WhatsApp poll.

**Parameters:**

- `message_id` (required): ID of the poll message
- `chat_jid` (required): JID of the chat

The bridge stores poll creations as messages with `media_type = "poll"` (content
`📊 <question> — options: a | b | c`) and each vote as `media_type = "poll_vote"`
with `poll_message_id` pointing at the poll, so both show up in `list_messages`
and search. Votes are end-to-end encrypted with the poll's key; whatsmeow keeps
that key when it sees the creation, so only votes received while the bridge was
running (for polls it saw) are decoded and counted. Returns `question`,
`selectable_count`, per-option `count` and `voters`, each voter's latest
`selected` options and `total_voters`. Respects `WHATSAPP_ALLOWED_CHATS`.

#### `delete_message`

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

#### `mark_messages_read`

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

#### `send_reaction`

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

#### `send_file`

Send a media file (image, video, document).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to the file
- `caption` (optional): Caption for the media

The bridge only reads files inside configured media roots. By default this is
`~/.local/share/whatsapp-mcp/outbox`; set `WHATSAPP_MEDIA_ROOTS` to allow
additional absolute directories.

#### `send_audio_message`

Send a voice message (automatically converts to Opus .ogg format).

**Parameters:**

- `recipient` (required): Phone number or group JID
- `file_path` (required): Path to audio file

Converted audio is sent through the same media-path confinement as
`send_file`.

#### `transcribe_audio`

Transcribe a voice note (or any audio file) to text with local
[whisper.cpp](https://github.com/ggml-org/whisper.cpp). Nothing leaves the
machine; there is no cloud fallback.

**Parameters:**

- `message_id` + `chat_jid`: the audio message (downloaded via the bridge first), **or**
- `file_path`: absolute path of an audio file already on disk
- `language` (optional): ISO-639-1 code, default `WHISPER_LANGUAGE` (`pt`); `auto` to detect

Requires a whisper backend, configured with either `WHISPER_URL` (a running
whisper.cpp `whisper-server`, see the `whisper` profile in
[`docs/DOCKER.md`](docs/DOCKER.md)) or `WHISPER_BIN` + `WHISPER_MODEL` (a local
`whisper-cli` binary and a `ggml-*.bin` model). Audio is normalised to 16 kHz WAV
with ffmpeg before transcription. Returns `text`, `language`, `backend` and the
local `file_path`.

#### `download_media`

Download media from a received message.

**Parameters:**

- `message_id` (required): ID of the message with media
- `chat_jid` (required): JID of the chat containing the message

WhatsApp CDN URLs expire after a few days. When a stored URL answers 403/404/410
(typical for history-synced or forwarded media), the bridge automatically asks
the **sender's phone** to re-upload the file via WhatsApp's media-retry protocol,
downloads it from the refreshed path, and persists that path for next time. The
sender's phone must be online; the bridge waits up to 30 seconds before giving
up with a clear error. Media the phone no longer has cannot be recovered.

### Chat Operations

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

#### Read state (`last_read_time` / `unread`)

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

#### `list_chats`

List all chats with metadata.

**Parameters:**

- `limit` (optional): Number of chats (default 50, max 200)

#### `get_chat`

Get specific chat metadata by JID.

**Parameters:**

- `jid` (required): Chat JID

#### `get_direct_chat_by_contact`

Find a direct message chat with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `get_contact_chats`

List all chats involving a specific contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `get_last_interaction`

Get the last message exchanged with a contact.

**Parameters:**

- `phone` (required): Phone number of the contact

#### `list_group_members`

List the participants of a group (live query through the bridge).

**Parameters:**

- `group_jid` (required): The group JID (`...@g.us`)

Returns the group's `name`, `topic`, `owner_jid` and a `members` list with
`jid`, `phone_number`, `lid`, `name` (from your contacts when known),
`display`, `is_admin` and `is_super_admin`. Respects `WHATSAPP_ALLOWED_CHATS`.

#### `get_message_context`

Get messages around a specific message for context.

**Parameters:**

- `message_id` (required): ID of the target message
- `chat_jid` (recommended): JID of the chat. Message IDs are only unique per chat; with it the lookup is an indexed primary-key hit, without it the most recent match is used
- `before` (optional): Number of messages before (default 5)
- `after` (optional): Number of messages after (default 5)

## Configuration

Copy `.env.example` to `.env` and configure as needed:

| Variable               | Default                                  | Description                                  |
| ---------------------- | ---------------------------------------- | -------------------------------------------- |
| `WHATSAPP_BRIDGE_PORT` | `8080`                                   | Port for Go bridge REST API                  |
| `WEBHOOK_URL`          | `http://localhost:8769/whatsapp/webhook` | Webhook for incoming messages                |
| `WEBHOOK_ENABLED`      | `true`                                   | Set to `false` to disable outbound webhooks  |
| `FORWARD_SELF`         | `true`                                   | Forward messages sent by self                |
| `WHATSAPP_DB_PATH`     | `../whatsapp-bridge/store/messages.db`   | Path to SQLite database                      |
| `WHATSMEOW_DB_PATH`    | `../whatsapp-bridge/store/whatsapp.db`   | whatsmeow DB used for LID ↔ phone resolution |
| `WHATSAPP_API_URL`     | `http://localhost:8080/api`              | Go bridge REST API URL                       |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token for bridge REST calls; also signed onto outbound webhook POSTs |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox`     | Path-list of directories allowed for outbound media files |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default)          | Label shown for this connection under WhatsApp > Linked Devices. Set to a recognisable name. Applies at pair time only (re-pair to change) |
| `WHATSAPP_MCP_TRANSPORT` | `stdio`                                | MCP transport to serve clients: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST`    | `127.0.0.1`                              | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT`    | `8000`                                   | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | loopback only                      | Comma-separated extra `Host` header values accepted by the `http`/`sse` transports (e.g. a Tailscale or container hostname); `*` disables the check |
| `WHATSAPP_MCP_ALLOWED_ORIGINS` | derived from allowed hosts       | Comma-separated extra `Origin` header values accepted by the `http`/`sse` transports (browser-based clients only) |
| `WHATSAPP_MCP_TOKEN`   | *(unset = no auth)*                      | Static bearer token required on every `http`/`sse` request (`Authorization: Bearer …`, min 16 chars). Set it before exposing the port beyond loopback/tailnet |
| `WHATSAPP_ALLOWED_CHATS` | *(unset = all chats)*                  | Comma-separated allow-list of chats the MCP may read or act on (JIDs, bare phone numbers, `*@g.us` / `*@s.whatsapp.net` wildcards). Enforced by the MCP server and again by the bridge on send/react/mark-read/typing |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30`                              | Stdio parent-liveness poll interval (seconds); exits on parent reparent only |
| `WHISPER_URL`          | *(unset)*                                | whisper.cpp `whisper-server` inference endpoint for `transcribe_audio` (e.g. `http://127.0.0.1:8178/inference`) |
| `WHISPER_BIN` / `WHISPER_MODEL` | *(unset)*                       | Alternative to `WHISPER_URL`: local `whisper-cli` binary and `ggml-*.bin` model path |
| `WHISPER_LANGUAGE`     | `pt`                                     | Default transcription language (`auto` to detect) |
| `WHISPER_TIMEOUT_S`    | `300`                                    | Per-transcription timeout |

### MCP transport (stdio vs http/sse)

By default the server speaks MCP over **stdio**, which is what local clients
like Claude Desktop and Cursor launch. To serve the server over the network
instead, set `WHATSAPP_MCP_TRANSPORT`:

```bash
# Streamable HTTP (current spec transport for remote MCP), endpoint at /mcp
WHATSAPP_MCP_TRANSPORT=http WHATSAPP_MCP_PORT=8000 uv run main.py

# Legacy Server-Sent Events transport (deprecated in the MCP spec), endpoint at /sse
WHATSAPP_MCP_TRANSPORT=sse uv run main.py
```

`http` is an alias for the spec's `streamable-http` transport and is the
recommended choice for remote connections; `sse` is kept for older clients.

> **Security:** `WHATSAPP_MCP_HOST` defaults to `127.0.0.1`, so the HTTP/SSE
> server is reachable only from the local machine. The underlying bridge can read
> and send WhatsApp messages on your account, so before binding to a non-loopback
> address (e.g. `0.0.0.0`) set `WHATSAPP_MCP_TOKEN` or put an authenticating
> reverse proxy / tunnel in front. Without a token the server logs a warning.

#### Bearer-token authentication

Set `WHATSAPP_MCP_TOKEN` (at least 16 characters; `openssl rand -hex 32` is a
good source) and every request to `/mcp` (or `/sse` + `/messages/`) must carry
`Authorization: Bearer <token>`. Anything else gets `401` with a
`WWW-Authenticate: Bearer` challenge and a JSON body. The check runs in constant
time and sits in front of the SDK's own DNS-rebinding middleware. Configure the
client the same way you would for any bearer-protected remote MCP server:

```json
{
  "url": "https://myserver.tail1234.ts.net/mcp",
  "headers": { "Authorization": "Bearer <token>" }
}
```

Leaving `WHATSAPP_MCP_TOKEN` unset keeps the transport open, which is only
sensible on loopback or a tailnet-only listener. The stdio transport is not
affected by any of this.

#### Reaching the server by a non-loopback hostname

The MCP SDK ships DNS-rebinding protection: it checks the HTTP `Host` header
against an allow-list and answers `421 Misdirected Request` for anything else.
Out of the box that allow-list is loopback only, so a client that reaches the
server through a Tailscale hostname, a Docker service name, or a reverse proxy
gets a 421 even when `WHATSAPP_MCP_HOST=0.0.0.0`. The server logs it as
`Invalid Host header: ...`.

Add the hostnames clients will use to `WHATSAPP_MCP_ALLOWED_HOSTS`:

```bash
WHATSAPP_MCP_TRANSPORT=http WHATSAPP_MCP_HOST=0.0.0.0 WHATSAPP_MCP_ALLOWED_HOSTS=myserver.tail1234.ts.net,whatsapp-mcp uv run main.py
```

- A bare hostname matches with or without a port (`myserver.tail1234.ts.net`
  and `myserver.tail1234.ts.net:8000`). Use `host:8000` to pin a port or the
  SDK's `host:*` form explicitly.
- Loopback spellings (`127.0.0.1`, `localhost`, `[::1]`) always stay allowed.
- `WHATSAPP_MCP_ALLOWED_ORIGINS` adds `Origin` values for browser-based
  clients; `http(s)://<host>` is derived automatically for each allowed host.
- Setting `WHATSAPP_MCP_ALLOWED_HOSTS=*` disables the check. Binding to a
  non-loopback address **without** an allow-list also disables it (with a
  warning on stderr) so the server stays reachable; prefer listing the hosts.

### Restricting which chats the agent can touch

`WHATSAPP_ALLOWED_CHATS` turns the whole system into least-privilege mode for
an agent: read tools only return the listed conversations and write tools
refuse any other target. Entries are comma-separated:

```dotenv
# one contact, one group, plus every group
WHATSAPP_ALLOWED_CHATS=5511999999999,120363000000000001@g.us,*@g.us
```

- Bare numbers mean the direct chat with that number (`@s.whatsapp.net`).
- `*@g.us` allows every group, `*@s.whatsapp.net` every direct chat.
- The MCP server filters `list_chats`, `list_messages`, `get_chat`,
  `get_message_context`, `get_direct_chat_by_contact`, `get_contact_chats` and
  `get_last_interaction`, and refuses `send_*`, `send_reaction`,
  `mark_messages_read`, `download_media` and `transcribe_audio` for other chats
  with a message naming the variable.
- The bridge enforces the same list on `/api/send`, `/api/react`,
  `/api/mark-read` and `/api/typing` (HTTP 403), so an MCP-side bug cannot
  reach a chat you did not enable. Set the variable for **both** processes
  (the compose file passes it to both containers).
- Contact search (`search_contacts`) is not filtered: it reads the address
  book, not conversations.

Unset keeps today's behaviour (everything allowed).

### Bridge authentication and media paths

The bridge requires bearer-token authentication for every `/api/*` request and
accepts only exact loopback Host headers for its configured port. This protects
the local REST API from other local processes and browser DNS-rebinding attacks.

On first start, the bridge generates a 256-bit token, writes it to
`.bridge-token` in the active bridge store directory with owner-only
permissions, and prints a setup banner. The MCP server reads
`WHATSAPP_BRIDGE_TOKEN` first, then falls back to `.bridge-token` in the same
directory as `WHATSMEOW_DB_PATH`. For split deployments, containers, or process
managers that do not share the store directory, set the same
`WHATSAPP_BRIDGE_TOKEN` value for both the bridge and MCP server.

The bridge also signs its **outbound** webhook POSTs (to `WEBHOOK_URL`) with this
same token, sent as an `X-Bridge-Token: <token>` header — a dedicated header
rather than `Authorization`, so it never collides with a receiver's own
Authorization-based auth (e.g. HTTP Basic auth embedded in `WEBHOOK_URL` as
`http://user:pass@host/...`, which `net/http` applies automatically as long as
the bridge doesn't set its own `Authorization` header). The header is attached only when a token is configured **and** `WEBHOOK_URL` was
explicitly set — never to the built-in local default. The bridge token also
authorizes `/api/*` calls like sending messages, and nothing has vetted the
implicit default address, so it must never be handed to whatever process
happens to be listening there. Upgrades that predate the token rollout, or
that never set `WEBHOOK_URL`, keep working unchanged. The webhook client also
never follows redirects, so a misconfigured or malicious endpoint can't
redirect the bridge into leaking the token to a different host. If your
webhook receiver enforces the token, set its copy to this exact value: e.g.
the AutoHub hub's `WHATSAPP_BRIDGE_TOKEN` must equal this bridge's token (from
`.bridge-token` or its own env) — the hub accepts it via `X-Bridge-Token` or
`Authorization: Bearer`. The bridge always sends the token it has; the hub
rejects unauthenticated forwards only once its `WHATSAPP_BRIDGE_TOKEN` is set
to the matching value.

Outbound `media_path` values are confined to `WHATSAPP_MEDIA_ROOTS`. The default
outbox is `~/.local/share/whatsapp-mcp/outbox`, created on bridge startup. Move
files there before calling `send_file` or `send_audio_message`, or set
`WHATSAPP_MEDIA_ROOTS` to a colon-separated list of absolute directories.

### Run automatically on macOS

macOS users can install optional per-user `launchd` jobs that start the Go
bridge at login and monitor it every 60 seconds for API health, disconnects, and
QR relink signals. The installer does not require `sudo` and does not install or
start the MCP server.

```bash
scripts/install-launchd-macos.sh
```

The installer builds `whatsapp-bridge/whatsapp-bridge` with `go build` when Go is
available, writes generated support files to
`~/Library/Application Support/whatsapp-mcp/`, writes LaunchAgents to
`~/Library/LaunchAgents/`, and writes logs to `~/Library/Logs/whatsapp-mcp/`.
It safely reloads only these labels:

- `com.whatsapp-mcp.bridge`
- `com.whatsapp-mcp.bridge-monitor`

To customize the launchd environment, export values before running the installer.
Re-run the installer after changing them.

```bash
export WHATSAPP_BRIDGE_PORT=8080
export WEBHOOK_URL=http://localhost:8769/whatsapp/webhook
export FORWARD_SELF=false
export WHATSAPP_MEDIA_ROOTS="$HOME/.local/share/whatsapp-mcp/outbox"
scripts/install-launchd-macos.sh
```

Verify the jobs and inspect logs:

```bash
launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge
launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge-monitor
tail -n 100 ~/Library/Logs/whatsapp-mcp/bridge.err.log
tail -n 100 ~/Library/Logs/whatsapp-mcp/monitor.err.log
```

The monitor sends a macOS notification once per failure type until recovery. It
alerts when the bridge LaunchAgent is unloaded, the token is missing, the health
endpoint is unreachable, WhatsApp is disconnected, or recent logs indicate that
QR relinking is needed.

Uninstall the generated LaunchAgents and support files with:

```bash
scripts/uninstall-launchd-macos.sh
```

Uninstall preserves `whatsapp-bridge/store/`, including WhatsApp session DBs,
message DBs, media, and `.bridge-token`. Logs are left in
`~/Library/Logs/whatsapp-mcp/` for manual cleanup.

### CLI flags (Go bridge)

| Flag                  | Default | Description                                                                                                                                                                                                                                                       |
| --------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--full-history-pair` | `false` | Request full history at pair time. Only takes effect on a fresh pair (no existing `whatsapp.db`); no-op for already-paired sessions. The phone ultimately decides the actual history window sent — see [Requesting full history](#requesting-full-history) below. |

### Requesting full history

whatsmeow's default pairing asks for "recent sync" — roughly the last 3 months, with the exact window decided by the phone. If you want to pull more history at pair time:

```bash
# Stop the bridge
launchctl bootout gui/$UID/com.whatsapp-mcp.bridge    # or however you manage it

# Back up, then remove the auth session (keeps messages.db intact)
cp whatsapp-bridge/store/whatsapp.db{,.bak}
rm whatsapp-bridge/store/whatsapp.db

# Re-pair with the flag
cd whatsapp-bridge
./whatsapp-bridge --full-history-pair
# Scan the QR with WhatsApp → Settings → Linked Devices → Link a Device
# Wait for "History sync complete" in the logs (can take 10-30 minutes)
# Ctrl+C when sync has quiesced, then restart under your normal process manager
```

Caveats:

- **The phone decides the actual cap.** The flag requests up to 10 years / 100 GB, but WhatsApp's iOS primary device enforces its own retention policy. iPad companion is documented at ~1 year max; other linked devices appear to follow similar logic.
- **Only effective on a fresh pair.** With `whatsapp.db` already present, no new pair handshake fires and the flag is a no-op.
- **Messages the phone has deleted are not recoverable** — auto-expire, low-storage cleanup, and manual delete all leave no trace for the phone to share.

### Requesting history for a single chat (on-demand)

`--full-history-pair` only applies to a fresh pair, so recovering a gap in one
chat otherwise means deleting `whatsapp.db` and re-syncing everything. To ask
the phone for older messages in a single chat *without* re-pairing:

```bash
curl -X POST http://127.0.0.1:8080/api/history \
  -H "Authorization: Bearer $(cat whatsapp-bridge/store/.bridge-token)" \
  -H "Content-Type: application/json" \
  -d '{"chat_jid": "1234567890@s.whatsapp.net", "count": 50}'
```

The request is anchored on the **oldest message already stored** for that chat,
so the phone returns messages from before it. Call it repeatedly to page
further back. Results arrive asynchronously through the normal history-sync
handler and land in `messages.db` — typically within a few seconds.

| Field | Required | Description |
| --------- | -------- | ------------------------------------------------------ |
| `chat_jid` | yes | Chat to backfill (`...@s.whatsapp.net` or `...@g.us`) |
| `count` | no | Messages to request; default `50`, capped at `500` |

Caveats:

- **The phone decides how much it returns**, exactly as with pair-time sync, so
  `count` is a request rather than a guarantee.
- **At least one message for the chat must already be stored**, since it is used
  as the anchor. Chats with no local messages return `404`; send or receive one
  message first.
- Messages the phone has deleted are not recoverable, as above.

## Call History

The bridge captures incoming WhatsApp voice and video calls live into a
dedicated `calls` table in `messages.db`. When a 1:1 call arrives
(`CallOffer`) or a group call is announced (`CallOfferNotice`), a row is
inserted with `result='in_progress'`. Subsequent `CallAccept` /
`CallReject` / `CallTerminate` events update the row — final result becomes
`answered`, `rejected`, `missed`, or `ended` depending on the event
sequence. See the state-machine comment above `StoreCallOffer` in `main.go`
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

## Architecture

```mermaid
flowchart TB
    subgraph Clients["AI Clients"]
        CD[Claude Desktop]
        CU[Cursor IDE]
        CC[Claude Code]
    end

    subgraph MCP["MCP Layer"]
        PY[Python MCP Server<br/>MCP SDK v2 MCPServer]
    end

    subgraph Bridge["WhatsApp Bridge"]
        GO[Go Bridge<br/>whatsmeow]
        DB[(SQLite<br/>messages.db)]
        WH[Webhook Handler]
    end

    subgraph External["External Services"]
        WA[WhatsApp Web API]
        EXT[External Webhook<br/>Receiver]
    end

    CD & CU & CC -->|MCP Protocol| PY
    PY -->|REST API| GO
    PY -->|Read| DB
    GO -->|Store| DB
    GO <-->|WebSocket| WA
    GO -->|Forward Messages| WH
    WH -->|POST| EXT
```

### Component Details

```mermaid
flowchart LR
    subgraph GoAPI["Go Bridge REST API"]
        direction TB
        SEND["/api/send"]
        READ["/api/mark-read"]
        DOWN["/api/download"]
        REACT["/api/react"]
        TYPE["/api/typing"]
        HIST["/api/history"]
        HEALTH["/api/health"]
    end

    subgraph MCPTools["MCP Tools (15 total)"]
        direction TB
        CONT["Contact Tools<br/>search_contacts, get_contact"]
        MSG["Message Tools<br/>list_messages, send_message, etc."]
        CHAT["Chat Tools<br/>list_chats, get_chat, etc."]
        MEDIA["Media Tools<br/>send_file, download_media, etc."]
    end

    MCPTools -->|HTTP Requests| GoAPI
```

### Data Flow

```mermaid
sequenceDiagram
    participant User as User
    participant Claude as Claude Desktop
    participant MCP as Python MCP Server
    participant Bridge as Go Bridge
    participant WA as WhatsApp

    User->>Claude: "Send 'Hello' to Mom"
    Claude->>MCP: send_message(recipient, message)
    MCP->>Bridge: POST /api/send
    Bridge->>WA: Send via WebSocket
    WA-->>Bridge: Delivery confirmation
    Bridge-->>MCP: Success response
    MCP-->>Claude: Message sent
    Claude-->>User: "Message sent to Mom"
```

### Incoming Message Flow

```mermaid
sequenceDiagram
    participant WA as WhatsApp
    participant Bridge as Go Bridge
    participant DB as SQLite
    participant WH as Webhook
    participant EXT as External Service

    WA->>Bridge: New message
    Bridge->>DB: Store message
    Bridge->>Bridge: Auto-download media
    Bridge->>WH: Forward to webhook
    WH->>EXT: POST with message data
    Note over EXT: Process incoming message
```

## Development

### Running Tests

```bash
cd whatsapp-mcp-server
uv pip install -e ".[dev]"
uv run pytest -v
```

### Linting

```bash
# Python
cd whatsapp-mcp-server
uv run ruff check .
uv run ruff format .

# Go
cd whatsapp-bridge
golangci-lint run
```

### Building

```bash
# Go bridge. -tags sqlite_fts5 compiles SQLite's FTS5 module in, which the
# bridge uses for the full-text message index; without the tag the bridge
# still runs and search falls back to a substring scan.
cd whatsapp-bridge
go build -tags sqlite_fts5 -o whatsapp-bridge

# Run the binary
./whatsapp-bridge

# During development (avoids stale binaries)
go run -tags sqlite_fts5 .

# Container images (see docs/DOCKER.md)
docker compose build
```

### Releasing (Maintainers)

Releases use Release Please automation; maintainer steps and fallback procedures
are documented in [docs/RELEASING.md](docs/RELEASING.md).

## Troubleshooting

### Authentication Issues

- **Pairing fails with `Client outdated` or HTTP 405**: Update to the latest
  release and rebuild the bridge. WhatsApp periodically raises the minimum
  supported linked-device client version, which can make older whatsmeow builds
  fail before pairing completes.
- **QR Code Not Displaying**: Restart the bridge. Check terminal QR code support.
- **Device Limit Reached**: Remove a linked device from WhatsApp Settings > Linked Devices.
- **No Messages Loading**: Initial sync can take several minutes for large chat histories.
- **`Refusing to start: another whatsapp-bridge already holds this store (pid N)`**:
  a second bridge is running against the same `store/` directory (typically a
  service-managed instance plus a manual `./whatsapp-bridge`). Two bridges on one
  session evict each other in a loop and silently stop saving messages, so the
  newcomer exits instead. Stop the other process, or use a different working
  directory. The lock (`store/.bridge.lock`) is released automatically when the
  holder exits or crashes; no cleanup is needed.
- **Out of Sync**: Back up `whatsapp-bridge/store`, then move
  `whatsapp-bridge/store/whatsapp.db` aside and re-authenticate. Keep
  `messages.db` unless you intentionally want to discard local message history.
- **Bridge returns 401 Unauthorized**: Restart the bridge so it creates
  `.bridge-token` next to `WHATSMEOW_DB_PATH`, then restart the MCP server. If
  the MCP server cannot read that file, set `WHATSAPP_BRIDGE_TOKEN` to the same
  value in both environments.
- **Bridge returns 403 Forbidden for Host**: Use `WHATSAPP_API_URL` with
  `http://127.0.0.1:<port>/api`, `http://localhost:<port>/api`, or
  `http://[::1]:<port>/api`; custom hostnames and missing ports are rejected.
- **Bridge returns 403 Forbidden for media_path**: Move the file into
  `~/.local/share/whatsapp-mcp/outbox` or add its absolute parent directory to
  `WHATSAPP_MEDIA_ROOTS`.

### App State / LTHash Conflicts

Some WhatsApp account state is managed by whatsmeow in
`whatsapp-bridge/store/whatsapp.db`. If the bridge reports errors like:

```text
SendAppState failed: server returned error updating app state (regular_low):
<error code="409" text="conflict"/>
failed to verify patch v12345: mismatching LTHash
```

then WhatsApp's app-state patch chain for the linked device is out of sync.
This usually affects operations that write chat settings such as archive,
mute, or pin state. Incoming and outgoing messages may still work because
message storage lives separately in `messages.db`.

Known manual resync attempts such as `FetchAppState(..., fullSync=true)` may
still fail on this upstream app-state error class. The practical recovery path
is to reset the whatsmeow session and re-pair:

```bash
# Stop the bridge first.
launchctl bootout gui/$UID/com.whatsapp-mcp.bridge    # or however you manage it

# Back up the whole runtime store.
cp -a whatsapp-bridge/store whatsapp-bridge/store.bak.$(date +%Y%m%d%H%M%S)

# Reset only the whatsmeow session/app-state DB.
mv whatsapp-bridge/store/whatsapp.db whatsapp-bridge/store/whatsapp.db.lthash.bak

# Restart the bridge and scan the new QR code.
cd whatsapp-bridge
./whatsapp-bridge    # or `go run -tags sqlite_fts5 .` during development
```

Do not remove `whatsapp-bridge/store/messages.db` for this recovery unless you
also want to delete the local message archive.

### Windows

Windows requires CGO for go-sqlite3. Install [MSYS2](https://www.msys2.org/) and enable CGO:

```bash
go env -w CGO_ENABLED=1
go run -tags sqlite_fts5 .
```

## Security Notice

> **Caution**: As with many MCP servers, this is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could lead to private data exfiltration. Use with awareness.

## License

MIT License - see [LICENSE](LICENSE) for details.

## Credits & History

This repository is the **Tauri-EPO fork** of [verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp) (see [About this fork](#about-this-fork)). The text below is upstream's own history and is kept as-is.

Very Good Plugins' project is a maintained fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp), originally created by [Luke Harries](https://github.com/lharries).

**Why we forked:** The original repository hasn't been updated since April 2025. We needed continued maintenance, bug fixes, and new features for production use.

**Highlights since the fork:**

- `/api/typing`, `/api/health`, and webhook forwarding (with reply context + image media)
- Auto-download of incoming media with collision-safe filenames
- `get_contact` tool, `sender_display` field, and LID ↔ phone resolution via the whatsmeow store
- Live capture of incoming voice/video calls into a `calls` table
- `--full-history-pair` flag to request extended history at pair time
- Resilience: recovers from `StreamReplaced` session conflicts; pinned `anyio` to dodge a cancel-scope regression
- CI/CD with GitHub Actions, Release Please for automated versioning, and Dependabot

The full release-by-release list lives in [CHANGELOG.md](CHANGELOG.md).

**Recent contributors** (huge thanks):

- [@edmenendez](https://github.com/edmenendez) — call capture (#39), full-history flag (#37), caption surfacing (#42), media filename collisions (#40), download race fix (#41), LID matching (#43), contact resolution via whatsmeow store (#30)
- [@davidsimoes](https://github.com/davidsimoes) — `StreamReplaced` recovery (#27)
- [@davidggphy](https://github.com/davidggphy) — LID → phone JID consistency (#12)
- [@maikol-solis](https://github.com/maikol-solis) — bridge run command fix (#23)
- [@DeetBot](https://github.com/DeetBot) — `anyio` cancel-scope pin (#44)

And to Luke for creating the original project. See [CONTRIBUTING.md](CONTRIBUTING.md) if you'd like to join in.

## Links

- [Very Good Plugins](https://verygoodplugins.com/?utm_source=github)
- [MCP Specification](https://modelcontextprotocol.io/)
- [whatsmeow](https://github.com/tulir/whatsmeow) - WhatsApp Web API library for Go
- [FastMCP](https://github.com/jlowin/fastmcp) - Fast Model Context Protocol implementation
