# Configuration reference

Every environment variable and CLI flag, plus the transport, authentication and allow-list semantics behind them. Compose users set these in `.env` (see [DOCKER.md](DOCKER.md)); laptop users export them before launching (see [LAPTOP.md](LAPTOP.md)). `AGENTS.md` section 7 is the agent-facing copy of the same table; keep both in sync when adding a variable.

## Environment variables

Copy `.env.example` to `.env` and configure as needed:

| Variable               | Default                                  | Description                                  |
| ---------------------- | ---------------------------------------- | -------------------------------------------- |
| `WHATSAPP_BRIDGE_BIND`  | `127.0.0.1`                              | Address the bridge REST API listens on. `0.0.0.0` / `::` to expose it to other containers or hosts (pair with `WHATSAPP_BRIDGE_ALLOWED_HOSTS`) |
| `WHATSAPP_BRIDGE_ALLOWED_HOSTS` | *(loopback only)*                 | Comma-separated `Host` values accepted besides loopback (`host` = any port, `host:port` exact, `*` any). Off-loopback binds refuse non-loopback Hosts until this names them |
| `WHATSAPP_BRIDGE_PORT` | `8080`                                   | Port for Go bridge REST API                  |
| `WEBHOOK_URL`          | `http://localhost:8769/whatsapp/webhook` | Webhook for incoming messages                |
| `WEBHOOK_ENABLED`      | `true` (compose: `false`)                | Set to `false` to disable outbound webhooks  |
| `FORWARD_SELF`         | `true` (compose: `false`)                | Forward messages sent by self                |
| `WHATSAPP_STORE_DIR`   | `./store` (bridge), `../whatsapp-bridge/store` (MCP) | Directory holding `whatsapp.db`, `messages.db`, media, `.bridge-token`, `.bridge.lock`. Set the same value for both processes; absolute paths recommended for services |
| `WHATSAPP_DB_PATH`     | `$WHATSAPP_STORE_DIR/messages.db`        | Path to SQLite database (overrides the store dir)                      |
| `WHATSMEOW_DB_PATH`    | `$WHATSAPP_STORE_DIR/whatsapp.db`        | whatsmeow DB used for LID ↔ phone resolution (overrides the store dir) |
| `WHATSAPP_API_URL`     | `http://localhost:8080/api`              | Go bridge REST API URL                       |
| `WHATSAPP_BRIDGE_TIMEOUT_S` | `30`                                | Timeout for each MCP → bridge call; media upload/download use 120 s. Connection errors are retried twice, read timeouts are not |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token for bridge REST calls; also signed onto outbound webhook POSTs |
| `WHATSAPP_MEDIA_AUTODOWNLOAD` | `true`                            | Cache inbound media as it arrives. `false` = only `download_media` fetches files (media-retry makes late fetches reliable) |
| `WHATSAPP_MEDIA_RETENTION_DAYS` | *(unset = keep forever)*        | Daily sweep deletes cached media older than N days; message rows stay and `download_media` re-fetches on demand |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox`     | Path-list of directories allowed for outbound media files |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default)          | Label shown for this connection under WhatsApp > Linked Devices. Set to a recognisable name. Applies at pair time only (re-pair to change) |
| `WHATSAPP_LOG_LEVEL`   | `INFO`                                   | Bridge log level (`DEBUG`, `INFO`, `WARN`, `ERROR`) for bridge and whatsmeow client lines. `DEBUG` also echoes every stored message |
| `WHATSAPP_MCP_LOG_LEVEL` | `INFO`                                 | MCP server log level (stderr) |
| `WHATSAPP_MCP_TRANSPORT` | `stdio`                                | MCP transport to serve clients: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST`    | `127.0.0.1`                              | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT`    | `8000`                                   | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | loopback only                      | Comma-separated extra `Host` header values accepted by the `http`/`sse` transports (e.g. a Tailscale or container hostname); `*` disables the check |
| `WHATSAPP_MCP_ALLOWED_ORIGINS` | derived from allowed hosts       | Comma-separated extra `Origin` header values accepted by the `http`/`sse` transports (browser-based clients only) |
| `WHATSAPP_MCP_RATE_LIMIT` | `120` when a token is enforced, else `0`     | Requests per minute per client on the `http`/`sse` transports (token bucket, 429 + `Retry-After`); `0`/`off` disables |
| `WHATSAPP_MCP_MAX_BODY_BYTES` | `4194304`                              | Maximum request body accepted by the `http`/`sse` transports |
| `WHATSAPP_MCP_TOKEN`   | bridge token on non-loopback binds, none on loopback | Static bearer token required on every `http`/`sse` request (`Authorization: Bearer …`, min 16 chars). Unset on a non-loopback bind → the bridge token is reused; `off` disables auth explicitly |
| `WHATSAPP_ALLOWED_CHATS` | *(unset = all chats)*                  | Comma-separated allow-list of chats the MCP may read or act on (JIDs, bare phone numbers, `*@g.us` / `*@s.whatsapp.net` wildcards). Enforced by the MCP server and again by the bridge on send/react/mark-read/typing |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30`                              | Stdio parent-liveness poll interval (seconds); exits on parent reparent only |
| `WHISPER_URL`          | *(unset)*                                | whisper.cpp `whisper-server` inference endpoint for `transcribe_audio` (e.g. `http://127.0.0.1:8178/inference`) |
| `WHISPER_BIN` / `WHISPER_MODEL` | *(unset)*                       | Alternative to `WHISPER_URL`: local `whisper-cli` binary and `ggml-*.bin` model path |
| `WHISPER_LANGUAGE`     | `pt`                                     | Default transcription language (`auto` to detect) |
| `WHISPER_TIMEOUT_S`    | `300`                                    | Per-transcription timeout |
| `FFMPEG_TIMEOUT_S`     | `120`                                    | Timeout for each ffmpeg conversion (`send_audio_message` encode, whisper WAV prep) |

## MCP transport (stdio vs http/sse)

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

### Bearer-token authentication

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

If `WHATSAPP_MCP_TOKEN` is unset and the server is bound to a non-loopback
address, it **reuses the bridge token** (`WHATSAPP_BRIDGE_TOKEN` or the
`.bridge-token` file next to `WHATSMEOW_DB_PATH`), so a deployment has one
secret to manage; the startup line says which one is in use. Set
`WHATSAPP_MCP_TOKEN=off` to run without auth deliberately. On loopback no token
is required. The stdio transport is not affected by any of this.

Whenever a token is enforced the server also rate-limits each client (first
`X-Forwarded-For` hop, else the socket peer) to `WHATSAPP_MCP_RATE_LIMIT`
requests per minute (default 120; token bucket with the same burst), answering
`429` with `Retry-After`, and caps request bodies at
`WHATSAPP_MCP_MAX_BODY_BYTES` (default 4 MiB). The limiter runs before the
bearer check, so token guessing is throttled as well.

### Reaching the server by a non-loopback hostname

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

## Restricting which chats the agent can touch

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

## Bridge authentication and media paths

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

## CLI flags (Go bridge)

| Flag                  | Default | Description                                                                                                                                                                                                                                                       |
| --------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--full-history-pair` | `false` | Request full history at pair time. Only takes effect on a fresh pair (no existing `whatsapp.db`); no-op for already-paired sessions. The phone ultimately decides the actual history window sent — see [Requesting full history](#requesting-full-history) below. |

## Requesting full history

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

## Requesting history for a single chat (on-demand)

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
