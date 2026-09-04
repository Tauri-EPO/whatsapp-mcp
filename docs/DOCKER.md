# Running WhatsApp MCP with Docker Compose

`docker-compose.yml` at the repo root runs the two components as containers on
an always-on machine (a home server, a NAS, a small VPS) and exposes the MCP
server over **streamable HTTP** so remote MCP clients can use it.

| Service  | Image                       | Role                                                                 |
| -------- | --------------------------- | -------------------------------------------------------------------- |
| `bridge` | `whatsapp-mcp-bridge:local` | Go bridge: WhatsApp Web session, `messages.db`, media, REST on `:8080` |
| `mcp`    | `whatsapp-mcp-server:local` | Python MCP server, `WHATSAPP_MCP_TRANSPORT=http` on `:8000`, path `/mcp` |

The `mcp` container joins the bridge's network namespace
(`network_mode: service:bridge`). The bridge therefore stays exactly as it runs
on a laptop: bound to `127.0.0.1`, loopback-only `Host` allow-list, token file
on disk. The MCP server reaches it at `http://127.0.0.1:8080` and reads
`messages.db` from the shared `whatsapp-store` volume. Because the two share a
namespace, the MCP port is **published on the `bridge` service**.

## Quick start

```bash
git clone https://github.com/Tauri-EPO/whatsapp-mcp.git
cd whatsapp-mcp
cp .env.example .env          # see "Configuration" and the checklist below
GIT_SHA=$(git rev-parse --short HEAD) docker compose up -d --build
docker compose logs -f bridge # first run: token banner, then the QR code to scan
```

Before the first `up`, decide these in `.env` (all optional, all safe to add
later, but the first three save a re-pair or a token rotation):

- `WHATSAPP_BRIDGE_TOKEN`: set your own (32+ random chars) so both containers
  and your MCP client share one known secret from the start; otherwise copy
  the generated one from the bridge log banner.
- `WHATSAPP_DEVICE_NAME`: the label shown under Linked Devices; only applied
  at pair time.
- `WHATSAPP_ALLOWED_CHATS`: the groups/contacts the bot may touch. Start
  narrow; widen later.
- `WHATSAPP_MCP_ALLOWED_HOSTS`: your MagicDNS name (`box.tailnet.ts.net`) so
  Host checking stays on. `WHISPER_URL` + `COMPOSE_PROFILES=whisper` for
  transcription; `WHATSAPP_MEDIA_RETENTION_DAYS` on a small disk.

`mcp` (and `whisper`) start only after the bridge healthcheck passes, i.e.
once the REST API is up and `.bridge-token` exists, and are recreated whenever
the bridge container is (they share its network namespace, which does not
survive a recreate). This needs Docker Compose v2.17 or newer. Once the phone confirms
the link, `docker compose ps` shows `bridge` as `healthy`, `GET /api/ready`
returns 200 and the MCP endpoint answers at `http://127.0.0.1:8000/mcp`.

Verify from the host:

```bash
curl -i -H 'Accept: application/json, text/event-stream' \
     -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}' \
     http://127.0.0.1:8000/mcp
```

A `200` with a `mcp-session-id` header means the server is up. A `421
Misdirected Request` means the `Host` header you used is not allow-listed (see
below).

## Pairing (QR code)

The bridge prints the QR code to its stdout, which `docker compose logs`
captures. On first start:

```bash
docker compose up -d bridge
docker compose logs -f bridge
```

Scan with WhatsApp > Linked Devices > Link a Device. The linked device is
labelled with `WHATSAPP_DEVICE_NAME` (default in compose: `WhatsApp MCP`).

To request the full message history at pair time, run the bridge once with the
flag and let compose take over afterwards:

```bash
docker compose run --rm --service-ports bridge --full-history-pair
docker compose up -d
```

The session lives in the `whatsapp-store` volume, so restarts and image
rebuilds do **not** require re-pairing. Deleting the volume does.

## Configuration

Compose reads `.env` from the repo root (copy `.env.example`). The keys that matter for the container deployment are below; every variable is described in [CONFIGURATION.md](CONFIGURATION.md).

| Variable | Default | Purpose |
| --- | --- | --- |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | *(empty = accept any Host)* | Hostnames clients use to reach `/mcp`, e.g. your Tailscale MagicDNS name. Strongly recommended; see [Tailscale](#tailscale). |
| `WHATSAPP_MCP_TOKEN` | *(empty = reuse the bridge token)* | Bearer token every MCP request must carry. Empty → the bridge token from the shared volume is used; `off` disables auth. See [Funnel](#funnel-public-internet). |
| `WHATSAPP_ALLOWED_CHATS` | *(empty = all chats)* | Least-privilege allow-list of conversations for the agent (JIDs, numbers, `*@g.us`). Passed to both containers. Strongly recommended for a bot that can send messages. |
| `WHATSAPP_MCP_BIND` | `127.0.0.1` | Host interface the MCP port is published on. Keep loopback and front it with `tailscale serve` or a reverse proxy. |
| `WHATSAPP_MCP_PORT` | `8000` | Host port for `/mcp`. |
| `WHATSAPP_BRIDGE_BIND`, `WHATSAPP_BRIDGE_ALLOWED_HOSTS` | `127.0.0.1`, *(loopback only)* | Only needed for the [split topology](#split-topology-mcp-server-on-another-container-or-host). Leave unset with the default compose file. |
| `WHATSAPP_BRIDGE_TOKEN` | *(generated)* | Inject the bridge REST token from outside (e.g. a secret store). Empty lets the bridge generate `/app/store/.bridge-token`, which the MCP server reads from the shared volume. |
| `WHATSAPP_DEVICE_NAME` | `WhatsApp MCP` | Linked-device label; applied at pair time only. |
| `WEBHOOK_ENABLED` | `false` | Outbound webhooks are off in the container by default because the upstream default URL points at `localhost:8769`. Set to `true` together with `WEBHOOK_URL`. |
| `WEBHOOK_URL`, `FORWARD_SELF` | | Passed through to the bridge. |
| `WHATSAPP_MEDIA_AUTODOWNLOAD`, `WHATSAPP_MEDIA_RETENTION_DAYS` | `true`, *(unset)* | Keep the media cache bounded: stop caching on arrival and/or expire files older than N days. `download_media` still fetches on demand. |
| `WHATSAPP_OUTBOX` | `./outbox` | Host directory mounted at `/app/outbox` in both containers. `send_file` / `send_audio_message` may only read from here (`WHATSAPP_MEDIA_ROOTS`). Give the MCP client paths like `/app/outbox/report.pdf`. |

Paths returned by `download_media` are container paths under `/app/store/...`.
To read those files from the host, inspect the volume
(`docker volume inspect whatsapp-mcp_whatsapp-store`) or bind-mount a host
directory instead of the named volume.

## Tailscale

The intended deployment is a machine on your tailnet. Publish the MCP port over
Tailscale rather than on a LAN or public interface:

```bash
# tailnet-only HTTPS at https://<host>.<tailnet>.ts.net/mcp
sudo tailscale serve --bg --https=443 http://127.0.0.1:8000
```

and allow-list that name so the SDK's DNS-rebinding protection stays on:

```dotenv
WHATSAPP_MCP_ALLOWED_HOSTS=<host>.<tailnet>.ts.net
```

Tailscale forwards the original `Host` header, so requests arrive as
`Host: <host>.<tailnet>.ts.net`; a bare hostname in the allow-list matches with
or without a port.

### Funnel (public internet)

Funnel is not enabled by default: it publishes the endpoint to the whole
internet, and the tools can read and send messages on your WhatsApp account.
If a client outside your tailnet (a hosted bot, for example) genuinely needs
access, the minimum bar is:

1. Make sure a bearer token is enforced: by default the MCP container reuses
   the bridge token (`docker compose exec bridge cat /app/store/.bridge-token`
   shows it), or set your own `WHATSAPP_MCP_TOKEN` (e.g. `openssl rand -hex 32`).
   Every request then needs `Authorization: Bearer <token>`; anything else gets `401`.
2. Keep `WHATSAPP_MCP_ALLOWED_HOSTS` set to the public hostname.
3. `sudo tailscale funnel --bg --https=443 http://127.0.0.1:8000`

Then configure the remote client with the URL and the bearer header. Rotate the
token by changing `.env` and `docker compose up -d mcp`. An authenticating
reverse proxy in front is still a reasonable extra layer if the client supports
it.

## Voice-note transcription (optional `whisper` profile)

The MCP server ships a `transcribe_audio` tool backed by
[whisper.cpp](https://github.com/ggml-org/whisper.cpp), fully local. The
`whisper` profile runs the official `ghcr.io/ggml-org/whisper.cpp` image as a
`whisper-server` next to the bridge and MCP containers (same network
namespace), downloading the chosen ggml model into the `whisper-models` volume
on first start:

```dotenv
# .env
WHISPER_URL=http://127.0.0.1:8178/inference
WHISPER_MODEL_NAME=small     # tiny | base | small | medium | large-v3-turbo ...
WHISPER_LANGUAGE=pt          # default language for transcripts; "auto" to detect
```

```bash
docker compose --profile whisper up -d
docker compose logs -f whisper      # first start: model download progress
```

`small` (~470 MB) is a good CPU default for Portuguese voice notes;
`medium`/`large-v3-turbo` are more accurate and several times slower. The
server is CPU-only in this image; set `WHISPER_THREADS` to the cores you can
spare. Without the profile (or without `WHISPER_URL`), `transcribe_audio`
returns a clear "no whisper backend configured" error and everything else
works as before.

## Health and operations

- `bridge` is healthy as soon as its REST API answers (`GET /api/health` →
  `200` with `status` = `ok` | `awaiting_pairing` | `disconnected`,
  `connected`, `paired`, `uptime_seconds`, `store_bytes`, `media_bytes`,
  `media_files`). The API starts before pairing, so
  a first run shows `healthy` while you scan the QR. To wait for WhatsApp
  itself, poll `GET /api/ready` (`200` only while connected, `503` otherwise).
- `mcp` is healthy while the ASGI server answers on `/mcp`.
- Logs: `docker compose logs -f bridge` / `docker compose logs -f mcp`. Both
  services log one line per event with a level; the bridge also logs one
  line per REST request (`POST /api/send → 200 (12ms) from=127.0.0.1 ua="…"`,
  never bodies; health probes at DEBUG); raise verbosity with
  `WHATSAPP_LOG_LEVEL=DEBUG` (bridge, also echoes stored messages) or
  `WHATSAPP_MCP_LOG_LEVEL=DEBUG` (MCP server) in `.env`.
- Update: `git pull && GIT_SHA=$(git rev-parse --short HEAD) docker compose up -d --build`.
  Check what is running with
  `docker compose exec bridge wget -qO- http://127.0.0.1:8080/api/version`
  (version, commit, Go and whatsmeow versions, FTS5 state); the MCP server
  reports its version in the `initialize` response and its startup log.
- Backup: the `whatsapp-store` volume holds the session, `messages.db`,
  `whatsapp.db`, downloaded media and `.bridge-token`. Stop the stack before
  copying it (`docker compose stop`), or snapshot the volume with your usual
  tooling.
- If the phone unlinks the device (WhatsApp > Linked Devices > Log out) or
  WhatsApp rejects the client version as outdated, the bridge exits (code 3
  or 4) instead of idling; `restart: unless-stopped` brings it back into the
  pairing flow and `docker compose logs -f bridge` shows a fresh QR code.
- Only one bridge may use a session at a time. Do not run the compose stack
  and a laptop bridge against the same store, and do not pair the same phone
  twice with two different stores: WhatsApp will keep replacing the stream.

## Split topology (MCP server on another container or host)

The default compose file keeps the bridge loopback-only and puts the MCP
server in the same network namespace. If you would rather run the MCP
server elsewhere (another compose project, another machine on your
tailnet), open the bridge up explicitly:

```yaml
services:
  bridge:
    environment:
      WHATSAPP_BRIDGE_BIND: 0.0.0.0          # listen on every interface
      WHATSAPP_BRIDGE_ALLOWED_HOSTS: bridge  # Host names clients will use
  mcp:
    # network_mode: "service:bridge"        # remove; use a normal network
    environment:
      WHATSAPP_API_URL: http://bridge:8080/api
```

Rules of thumb:

- The bearer token is still required; share `.bridge-token` (or set
  `WHATSAPP_BRIDGE_TOKEN` on both sides).
- `WHATSAPP_BRIDGE_ALLOWED_HOSTS` takes `host` (any port), `host:port`
  (exact) or `*` (any Host, DNS-rebinding protection off). Loopback
  spellings are always accepted. A non-loopback bind **without** the
  allow-list keeps refusing non-loopback Hosts with 403 and logs a warning,
  so forgetting it fails closed.
- Never publish the bridge port to the internet; keep it on a private
  network or the tailnet. Only `/mcp` is meant to be exposed.

## Running the images without compose

```bash
docker build -t whatsapp-mcp-bridge ./whatsapp-bridge
docker build -t whatsapp-mcp-server ./whatsapp-mcp-server

docker network create wamcp
docker run -d --name bridge --network wamcp -v whatsapp-store:/app/store \
  -p 127.0.0.1:8000:8000 whatsapp-mcp-bridge
docker run -d --name mcp --network container:bridge -v whatsapp-store:/app/store \
  whatsapp-mcp-server
```

The MCP image defaults to the HTTP transport bound to `0.0.0.0:8000` **inside
the container**; publish that port thoughtfully.
