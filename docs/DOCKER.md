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
  Host checking stays on. `WHISPER_URL` for transcription (a whisper.cpp server
  you run, [below](#voice-note-transcription)); `WHATSAPP_MEDIA_RETENTION_DAYS`
  on a small disk.

`mcp` starts only after the bridge healthcheck passes, i.e. once the REST API
is up and `.bridge-token` exists, and is recreated whenever the bridge
container is (it shares the bridge's network namespace, which does not
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

## Rate limiting behind a proxy

With the shipped compose topology, a same-host proxy reaches the MCP through
Docker's default-route gateway, rather than a loopback socket inside the
container. To keep one rate-limit bucket per proxied caller, explicitly set
`WHATSAPP_MCP_TRUSTED_PROXIES=gateway`. This resolves `/proc/net/route` at
startup and trusts that one gateway address; resolution failure stops startup.
Keep `WHATSAPP_MCP_BIND=127.0.0.1`, the compose default: gateway trust is safe
only when the published port is loopback-bound and reachable by same-host
processes, such as Tailscale Serve. Do not combine it with a LAN/public bind.

For a server running directly on the host, `loopback` trusts a local proxy;
for other proxy topologies use the proxy's narrow CIDR. The default is empty,
so proxied callers share a socket-peer bucket until trust is configured.
Proxies must append the actual caller or replace incoming forwarding headers.
[Tailscale Serve replaces `X-Forwarded-For` with the caller's tailnet address](https://github.com/tailscale/tailscale/blob/main/ipn/ipnlocal/serve.go).

## Pairing (QR code)

If WhatsApp asks for a passkey after scanning, `bridge_status` and
`/api/health` report `pairing_state=passkey_required`, `passkey_confirm`, or
`passkey_failed`. The challenge, assertion and confirmation code are never
published in health or metrics. The bridge logs the step and stops automatic
QR retries after it; it stays alive for diagnosis. Resolve the phone's
passkey prompt or account access in the official WhatsApp
app. A headless bridge has no WebAuthn authenticator and cannot complete that
challenge. A page on a generic server origin cannot assert a passkey for
`whatsapp.com`; it requires a matching relying-party origin. The optional
operator endpoints below forward an assertion obtained by an external native
helper with that relying-party support; they do not create one or provide a
browser authenticator. Without such a helper, use the official phone flow and
restart the attempt after resolving the account's requirements.

The pinned whatsmeow `c386243a72ba` includes passkey support from `b572e5b`;
its QR channel automatically confirms `SkipHandoffUX` events. A manual
confirmation remains pending, bounded by the attempt timeout. A passkey request
uses its advertised timeout (milliseconds), capped at five minutes. Pairing by
phone-number code, account/device eligibility and native helper workarounds
have not been verified against a paired phone; no bypass is promised.

By default the bridge prints the QR code to stdout, which `docker compose logs`
captures. These QR codes are credentials: set `WHATSAPP_PAIRING_STDOUT=false`
when logs leave the host, and use operator HTTP. Setting an operator bind
defaults stdout pairing to false, including outside Compose; without an
operator the existing log flow remains the default.
On first start with log pairing:

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

### Pairing over private operator HTTP

The operator listener is off unless `WHATSAPP_OPERATOR_BIND` is set. It requires
a separate random secret (generate with `openssl rand -hex 32`), refuses weak
or placeholder tokens and a token equal to the effective bridge token (including
`store/.bridge-token`), and never serves data-plane routes or MCP tools.
For local use, bind `127.0.0.1`, port `8090`, set
`WHATSAPP_OPERATOR_TOKEN` and `WHATSAPP_PAIRING_STDOUT=false`. Alternatively set
`WHATSAPP_OPERATOR_TOKEN_FILE` to a regular file mounted read-only, owned by the
container uid 1000 and mode 0600; no symlinks or default world-readable secret
files. Choose token or file, never both. The data-plane token cannot pair and
the operator token cannot send messages. Read-only/tool policies do not narrow
the operator routes; possession of this separate credential grants pairing.

```bash
# Run from the host for a loopback listener, or an operator container below.
# OPERATOR_TOKEN is the separate secret; do not use a real value in shell history.
curl -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8090/operator/v1/pairing
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' -d '{"phone":"5511999999999"}' \
  http://127.0.0.1:8090/operator/v1/pairing/code
curl -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  http://127.0.0.1:8090/operator/v1/pairing/restart
```

`GET pairing` returns state, attempt/attempts, generation and the current
`qr: {payload, sequence, expires_at}`. Render only that current payload; it
rotates and becomes null when expired, passkey steps start, or pairing succeeds.
`POST pairing/code` reuses the connected QR attempt and returns an eight-character
WhatsApp code for Linked Devices > Link with phone number. At most three calls
per attempt; the fourth is 429. A paired client, inactive QR or concurrent action
is 409. The returned `expires_at` is a conservative local deadline tied to the
current QR, **not a promise about WhatsApp's undocumented pairing-code TTL**.
After three exhausted attempts the process remains alive with `expired`;
`restart` returns 202, invalidates credentials and creates a fresh client.
It refuses paired devices, an unexpired temporary ban and an outdated build.
An active attempt, including the SDK's final device-save step (`completing`),
must finish first; restart does not replace a device that finishes linking.
Locked/banned recovery requires this explicit operator action after resolving
the account restriction; restarting does not bypass WhatsApp enforcement.

`GET health`/`GET ready` are credential-free and authenticated here. All operator
responses use `Cache-Control: no-store`. Health and metrics never expose a QR,
phone number, pairing code, assertion or confirmation code. INFO audit lines
contain only the fixed mutating route, response status and socket peer;
authentication, origin and rate-limit refusals log at DEBUG. Limits are 120
requests/minute per socket peer before authentication and 60/minute for the
authenticated token; forwarded headers do not change the peer. Browser Origin
must exactly match the listener's scheme and Host, and Host must be explicitly
allowed; native clients may omit Origin. No CORS or forwarded-origin trust is
enabled. These examples use native/backend clients on the private network.

For a passkey step, `GET pairing` exposes `passkey` request options and
`step_expires_at` only on this authenticated private listener. An external
authenticator must generate the assertion for `whatsapp.com`. Submit
`{"generation":N,"assertion":{...WebAuthnResponse...}}` to
`POST pairing/passkey/response`; mismatched challenge, relying-party hash,
origin, expired generation and replay are refused. WhatsApp verifies the
signature. If state becomes `passkey_confirm`, compare `confirmation_code`
with the phone and send `{"generation":N,"code":"the-compared-code"}` to
`POST pairing/passkey/confirm`. The SDK handles `SkipHandoffUX` itself and this
route never duplicates it. These HTTP paths are exercised with fake events;
native authenticator availability and actual phone eligibility remain unverified.
Neither phone-number linking nor these endpoints promise avoidance of a
passkey challenge. `scripts/smoke.sh` keeps exit 2 while unpaired and, when the
operator listener is enabled, prints only its pairing state, never credentials.

### Private operator network

For an operator in a container, create one private external Docker network
per instance and attach only that operator client and instance. Use a unique
alias for each instance:

```bash
docker network create operator-private
# In .env: WHATSAPP_OPERATOR_NETWORK=operator-private
# WHATSAPP_OPERATOR_ALIAS=whatsapp-operator-example
# WHATSAPP_OPERATOR_TOKEN=<output of openssl rand -hex 32>
docker compose -f docker-compose.yml -f docker-compose.operator.yml up -d
# From the operator container joined to operator-private:
# http://whatsapp-operator-example:8090/operator/v1/health
```

`docker-compose.operator.yml` binds the operator listener to the single IP
resolved for that alias **on the operator network** and includes the alias in
its Host allow-list. It publishes no operator host port. Port 8080 stays on
loopback; enabling the operator refuses a widened `WHATSAPP_BRIDGE_BIND`.
The MCP container shares the bridge namespace, so joining a private network
does not isolate the agent plane. MCP port 8000 remains bound to `0.0.0.0`,
reachable on every joined network and authenticated with the MCP bearer token.
The override disables MCP `/metrics` to avoid exposing unauthenticated counts.
Re-enable metrics only with a separate `WHATSAPP_MCP_METRICS_TOKEN` in a custom
override. The MCP host-port publication stays unchanged; a split namespace and
proxy-only agent listener remain separate work in #634.

Every member of a **shared** operator network can reach every operator listener
on that network; separate tokens authenticate each instance. A per-instance
operator network limits that reachability too. Shared-network peers can also
reach each MCP listener; its token is the authentication boundary. If a proxy
network is added, use another alias there: 8090 accepts only on the operator
interface and 8080 remains loopback, while MCP 8000 remains token-gated.

For a file-backed operator secret, mount the file **only on the bridge**,
outside `/app/store` and `/app/outbox`. The MCP process uses the same uid and
can read files in those shared volumes, so they must never contain this secret.
Create the file with `openssl rand -hex 32`, mode `0600`, owner uid 1000, and
add an override such as:

```yaml
services:
  bridge:
    environment:
      WHATSAPP_OPERATOR_TOKEN: ""
      WHATSAPP_OPERATOR_TOKEN_FILE: /run/operator-secret/token
    volumes:
      - ./operator-secret:/run/operator-secret/token:ro
```

Do not add this mount to `mcp`. `scripts/smoke.sh` invokes the bridge binary's
`--operator-status` probe: the credential is read from env/file inside that
container, never put in argv or printed, and only the state name is returned.
Operator enablement defaults `WHATSAPP_PAIRING_STDOUT` to false even without
this override. SDK DEBUG is suppressed while the operator is enabled; bridge
and database DEBUG remain available. Explicit stdout opt-in is for trusted
console operators only.

Operator `GET/PATCH /operator/v1/settings` changes `tools.allow`, `tools.deny`
and `transcription.ingest_chats` without a restart. See the precedence and
atomic PATCH contract in [Runtime overrides](CONFIGURATION.md#runtime-overrides).
Transcription caps and send limits remain tracked in #637/#635. A private authenticated pairing
response reports a bounded `failure_reason` after a passkey failure; health,
metrics and INFO never expose that text. Real passkey eligibility and a native
WhatsApp authenticator still need the live verification tracked in #487/#647.

## Removing an instance

Unlink the linked device before deleting its store volume. From a client on
the private operator network, with the separate secret in `OPERATOR_TOKEN`:

```bash
scripts/operator-logout.sh http://127.0.0.1:8090 idle
```

The helper calls `POST /operator/v1/logout` with `{"after":"idle"}`. A native
client may also send that JSON directly with the operator bearer token.
`after` defaults to `exit`: the bridge flushes the response and exits 3 for the
supervisor to restart into pairing. `idle` leaves it serving health/operator
routes with `logged_out_by_operator`, no QR or automatic pairing; this state
survives a process restart and is cleared only by `POST pairing/restart`.
If the operator listener is disabled, startup logs one WARN naming this idle
state: re-enable `WHATSAPP_OPERATOR_BIND` and POST
`/operator/v1/pairing/restart` to resume pairing. Restart is refused while
local cleanup is incomplete; retry logout before restarting pairing.
The operator token works under `WHATSAPP_READ_ONLY`; data-plane tokens do not.
Unpaired devices return 409 without contacting WhatsApp.

Response: `{"server_unlinked":true,"local_session_wiped":true}`. Existing
client requests have one second to drain; otherwise logout returns 503
`client_busy`, stays parked and may be retried. Unlink is
bounded to five seconds, followed by an independent five-second local wipe,
even if the server is offline or the caller disconnects. A server failure
does not cancel local cleanup; the action has a twelve-second overall cleanup
deadline, leaving time for its terminal webhook and response.
A server failure reports `server_unlinked:false`; retry removal on the phone when needed.
A local wipe failure returns 500 with `local_session_wiped:false` and keeps
the instance parked: resolve that failure before removing storage. A
successful wipe emits connection `logged_out` with `reason:"operator"`.
Session deletion uses SQLite secure deletion, compacts the database and
truncates its WAL, removing old key bytes from both current database files.

After a successful local wipe, stop the stack and remove its instance volume
(or let your stack manager remove it). Existing backups still contain old
session keys; unlink does not erase those backups. No logout/settings route
is exposed on bridge REST or MCP, and neither is an MCP tool.

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
| `WEBHOOK_URL`, `FORWARD_SELF`, `WEBHOOK_FORWARD_STATUS`, `WEBHOOK_FORWARD_CHANNELS`, `WEBHOOK_FORWARD_BROADCASTS` | | Passed through to the bridge. Status, channels and broadcast lists stay off the webhook unless their independent forwarding switch is true. |
| `WHATSAPP_MEDIA_AUTODOWNLOAD`, `WHATSAPP_MEDIA_RETENTION_DAYS` | `true`, *(unset)* | Keep the media cache bounded: stop caching on arrival and/or expire files older than N days. `download_media` still fetches on demand; the agent can also free space on request with `purge_media` (`POST /api/media/purge`, dry run by default). |
| `TZ` | `UTC` | Timezone for log lines and the media retention sweep in all containers. |
| `WHATSAPP_OUTBOX` | `./outbox` | Host directory mounted at `/app/outbox` in both containers. `send_file` / `send_audio_message` may only read from here (`WHATSAPP_MEDIA_ROOTS`). Give the MCP client paths like `/app/outbox/report.pdf`. |

Paths returned by `download_media` are container paths under `/app/store/...`.
To read those files from the host, inspect the volume
(`docker volume inspect whatsapp-mcp_whatsapp-store`) or bind-mount a host
directory instead of the named volume.

The bridge creates the store directory and each chat's media directory `0700`,
and every file it writes there `0600`, owned by the container user (uid 1000 in
both images): the media files, the token, and the two databases `messages.db`
and `whatsapp.db` with their `-wal` / `-shm` files. On a bind-mounted store,
read the files as that user or as root.

A store an earlier release created is tightened as far as its files go: at
startup the bridge sets `messages.db`, `whatsapp.db` and whatever `-wal` /
`-shm` / `-journal` sits next to them to `0600` when group or others could read
them (they were created `0644`), and logs one `Tightened ... to 0600` line
naming the files. Directories are not re-moded: one an earlier release created
keeps `0750`, one you created for a bind mount keeps what `mkdir` gave it, and
`chmod -R go-rwx <store>` tightens those by hand. Both processes therefore have
to run as the same user, which the images and the compose file do (uid 1000, no
`user:` override); an MCP server started under another account that relied on
group access to `messages.db` loses it at the next bridge start.

The MCP server creates its own `notes.db`, `-wal` / `-shm`, export archives and
uploaded bytes `0600`, and directories it creates for exports and uploads
`0700`, independent of the host umask. At startup it tightens existing notes
files and recognizable owned export/upload artifacts to these modes, with one aggregate log
line. It skips links inside those trees and leaves the bridge's store directory
and databases alone. Both processes must keep the same uid so the bridge can
read uploaded bytes; the bridge never opens `notes.db`.

Migration recognizes generated `messages-...-<timestamp>.ndjson` archives and
custom paths recorded in `.mcp-export-artifacts`. Older custom archives with no
ownership record retain their modes; tighten those explicitly if needed.
Existing export directories always keep their modes. Migration scans generated
archives directly under the export root and custom paths recorded in the manifest,
without walking the rest of the tree. If the filesystem refuses permission changes
or the manifest cannot be read, the server logs a warning without private paths and
continues; files may retain their previous modes.
An unsafe upload tree is skipped at startup, while upload operations still refuse it.

A chat directory replaced by a symlink is not written to: see
[the store root](./ARCHITECTURE.md#the-store-root).

## OAuth HTTP clients

The MCP listener can validate tokens from an external OIDC/OAuth 2.1 provider.
It is a resource server: the provider owns login, consent, PKCE, registration,
refresh tokens and revocation. Static bearer clients keep working alongside it;
stdio is unaffected. With no issuer configured, HTTP behavior stays unchanged.

A generic provider deployment might use this configuration (all hosts below
are examples):

```dotenv
WHATSAPP_PUBLIC_URL=https://example.ts.net/mcp
WHATSAPP_MCP_ALLOWED_HOSTS=example.ts.net
WHATSAPP_MCP_OAUTH_ISSUER=https://identity.example.com/tenant/example
WHATSAPP_MCP_OAUTH_AUDIENCE=https://example.ts.net/mcp
WHATSAPP_MCP_OAUTH_SCOPES=whatsapp.connect
WHATSAPP_MCP_OAUTH_READ_SCOPE=whatsapp.read
WHATSAPP_MCP_OAUTH_SEND_SCOPE=whatsapp.send
WHATSAPP_MCP_OAUTH_SUBJECTS=example-user
```

Configure the external provider's resource/audience as exactly
`https://example.ts.net/mcp`, expose its RFC 8414 or OIDC metadata and JWKS,
and register a public client with PKCE S256 and that client's callback URL.
Grant `whatsapp.connect whatsapp.read` for read access and additionally
`whatsapp.send` for mutating tools. Set the issued token's `sub` to the allowed
subject, or leave the subject allow-list empty when every provider user is
intended to have access. Reverse-proxy TLS terminates in front of the listener;
OAuth configuration requires HTTPS except for loopback development.

A client receives HTTP 401 with a `resource_metadata` challenge, fetches
`/.well-known/oauth-protected-resource/mcp` (the root well-known URL returns
the same document), discovers the issuer and completes authorization with that
provider. JWTs require RS256 or ES256, a trusted `kid`, signature, exact issuer,
audience, nonempty subject, expiry, not-before when present and base scopes.
Clock claims have 60 seconds of skew leeway. A configured static bearer with
exactly two dots is refused at startup in OAuth mode; use an opaque static secret.
Discovery/JWKS fetches have an eight-second total budget, a 256 KiB response
ceiling, no redirects or ambient credentials, and a five-minute key cache.
Authorization and transcription JSON requests use `Accept-Encoding: identity`;
compressed responses are refused before decoding to prevent expansion beyond
their size budgets. Configure provider proxies to honor identity encoding.
An unknown `kid` can refresh keys at most once per five-second cooldown window.
Set `WHATSAPP_MCP_OAUTH_JWKS_URL` to override discovery when necessary.

For opaque tokens and prompt revocation, use RFC 7662 introspection instead:

```dotenv
WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=https://identity.example.com/tenant/example/introspect
WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID=example-resource-server
WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET_FILE=/run/secrets/oauth-introspection
```

Mount that owner-only regular UTF-8 secret file into the MCP container; compose
passes the path but does not mount an operator's file automatically. The client
ID also accepts `_FILE`. Set a value or its file, never both; do not combine
introspection with an explicit JWKS URL. Requests use Basic auth. Only valid
`active=true` responses with the resource audience are cached, keyed by a token
hash, for at most 60 seconds (and at most 1024 entries). Cache hits never wait
behind other tokens; same-token fetches share one flight, with at most four
remote introspections in flight and HTTP 503 when that bound is reached.
When present, introspection `token_type` must be `access_token` (case-insensitive).
Revocation becomes
visible within that window. An unreachable or invalid authorization service
fails closed with HTTP 503. Claims, raw access tokens and secrets are not logged
or forwarded to the bridge; downstream bridge authentication remains separate.

Insufficient per-tool scope returns HTTP 403 with `insufficient_scope`, all
required scopes and the metadata URL so the client can request a stronger
token. `tools/list` shows only tools permitted by the current token.
`WHATSAPP_READ_ONLY`, allow-lists and deny-lists still remove capabilities,
including for the static bearer. OAuth rate limits use subject identity instead
of the socket IP, so refreshes keep the same budget and distinct subjects have
separate budgets. Invalid credentials consume a separate peer budget; once
that budget is exhausted, remote verification is refused before contacting
the provider. Successful cached credentials retain their subject budget.

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

The certificate for that name is issued and renewed on the host, never in the
containers. Once it expires, clients fail at the TLS handshake with
`certificate verify failed: certificate has expired` while `bridge_status` still
reports `ok: true`; see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#published-https-endpoint).

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
3. Decide about `/metrics`: it is served on the same port without the MCP
   token (counts only, never content, see Health and operations). On Funnel that
   means the internet can read tool-call counts. Either set
   `WHATSAPP_MCP_METRICS_TOKEN` (Prometheus: `bearer_token_file`) or
   `WHATSAPP_MCP_METRICS=false`.
4. `sudo tailscale funnel --bg --https=443 http://127.0.0.1:8000`

Then configure the remote client with the URL and the bearer header. Rotate the
token by changing `.env` and `docker compose up -d mcp`. An authenticating
reverse proxy in front is still a reasonable extra layer if the client supports
it.

## Voice-note transcription

The MCP server ships a `transcribe_audio` tool backed by
[whisper.cpp](https://github.com/ggml-org/whisper.cpp), fully local, and
`TRANSCRIBE_ON_INGEST=1` transcribes voice notes as they arrive. The whisper
server is yours to run: nothing in this compose file starts one, so it can live
next to the stack, on a GPU box on the tailnet, or as a binary on the host.
Two backends:

- `WHISPER_URL`: a whisper.cpp `whisper-server` inference endpoint reachable
  from the **mcp container**. That container shares the bridge's network
  namespace, so the URL is resolved from there: `127.0.0.1` is the bridge's own
  loopback, and a service name resolves on the networks the bridge is on
  (`<project>_default`).
- `WHISPER_BIN` + `WHISPER_MODEL`: a `whisper-cli` binary and a ggml model on
  the machine the MCP server runs on (the stdio setup; the image ships neither).

### HTTP provider

Set `WHATSAPP_TRANSCRIPTION_PROVIDER=openai_compatible` to use an explicit
speech-to-text endpoint instead of whisper.cpp. These variable names match
upstream VGP #247:

```dotenv
WHATSAPP_TRANSCRIPTION_PROVIDER=openai_compatible
WHATSAPP_TRANSCRIPTION_URL=https://speech.example.com/v1/audio/transcriptions
WHATSAPP_TRANSCRIPTION_MODEL=example-speech-model
WHATSAPP_TRANSCRIPTION_LANGUAGE=auto
WHATSAPP_TRANSCRIPTION_API_KEY=
WHISPER_TIMEOUT_S=300
```

Supply the API key in the deployment environment. A remote endpoint receives
the audio as a third-party processor; this provider stays off until selected.
The tool and `TRANSCRIBE_ON_INGEST` share the provider and cache provider/model
in `notes.db`. There is no fallback, redirect, environment proxy or netrc auth.
Only transcoded, metadata-free mono Opus/OGG audio is uploaded, never original
file bytes. All inputs are converted and split into ordered ten-minute parts,
each capped at 25 MB; sources are capped at 256 MiB and 24 hours. Non-audio
inputs fail before any upload. Explicit `file_path` values must resolve inside
the store or `WHATSAPP_MEDIA_ROOTS`, for both providers; escaping symlinks are denied.
`WHISPER_TIMEOUT_S` is one whole-file HTTP budget (default 300 seconds), covering
the probe, conversion and all uploads. The split also has a duration-scaled
limit of `max(FFMPEG_TIMEOUT_S, 10 + duration_seconds / 10)`, capped by that
remaining whole-file budget. The packaged ffmpeg supplies the duration probe.
`scripts/smoke.sh` sends only an authenticated HEAD probe, without audio; its
two-second total deadline also covers response headers arriving slowly.
401/403, 408, 429, 5xx and transport failures are backend outages and leave no
`transcript_error`; a 400 file rejection parks that file for review.

Without either, `transcribe_audio` returns a clear "no whisper backend
configured" error and everything else works. `bridge_status().whisper.reachable`
says whether the URL answers, and `scripts/smoke.sh` probes it as step 5.

### Example: whisper.cpp next to the stack

A compose project of its own, attached to the stack's default network so
`whisper` resolves inside it. No published port: whisper-server has no
authentication, and nothing outside the network needs it.

```yaml
# whisper/docker-compose.yml  -  docker compose up -d   (from that directory)
services:
  whisper:
    image: ghcr.io/ggml-org/whisper.cpp:main    # pin a digest for reproducible pulls; amd64 only
    restart: unless-stopped
    mem_limit: 3g          # a model plus 4 threads can starve a small box
    cpus: 4
    networks: [whatsapp]
    volumes:
      - models:/models
    entrypoint: ["sh", "-c"]
    command:
      - |
        set -e
        [ -f /models/ggml-small.bin ] || /app/models/download-ggml-model.sh small /models
        exec /app/build/bin/whisper-server -m /models/ggml-small.bin -l pt -t 4 --host 0.0.0.0 --port 8178 --inference-path /inference
volumes:
  models:
networks:
  whatsapp:
    external: true
    name: whatsapp-mcp_default    # the stack's default network: <project>_default
```

Then, in the stack's `.env`:

```dotenv
WHISPER_URL=http://whisper:8178/inference
WHISPER_LANGUAGE=pt          # default language for transcripts; "auto" to detect
```

and `docker compose up -d` in the stack. `small` (~470 MB) is a good CPU
default for Portuguese voice notes; `medium` and `large-v3-turbo` are more
accurate and several times slower. Start the stack before the whisper project,
because the network must exist for the attachment; and `docker compose down`
on the stack cannot remove that network while whisper is on it (stop the
whisper project first, or ignore the message and restart whisper after the
`up`, so it joins the new network).

**Several stacks on one host.** One whisper container, attached to every
stack's network (`networks: [a, b]` with one external entry per stack), and the
same `WHISPER_URL` in each stack. whisper-server handles one request at a
time, so the stacks queue on it; with `TRANSCRIBE_ON_INGEST` in more than one
stack, give each a different `TRANSCRIBE_ON_INGEST_INTERVAL_S`.

**Managed stacks** (Komodo, Portainer): the whisper project is a stack of its
own with the compose above; the WhatsApp stack only carries `WHISPER_URL` in
its Environment.

### Example: a server elsewhere

Any reachable `whisper-server` works, such as a GPU box on the tailnet:
`WHISPER_URL=http://gpu.tailnet.ts.net:8178/inference`. There is no
authentication on that endpoint, so keep it inside a network you trust.

## Health and operations

- `bridge` is healthy as soon as its REST API answers (`GET /api/health` →
  `200` with `status` = `ok` | `awaiting_pairing` | `disconnected`,
  `connected`, `paired`, `uptime_seconds`, `store_bytes`, `media_bytes`,
  `media_files`). The API starts before pairing, so
  a first run shows `healthy` while you scan the QR. To wait for WhatsApp
  itself, poll `GET /api/ready` (`200` only while connected, `503` otherwise).
- `mcp` is healthy while the ASGI server answers on `/mcp`.
- `scripts/smoke.sh` runs the whole checklist from the host after a deploy:
  bridge `/api/health` and `/api/ready` (through the container, with the
  bridge token from `.env` or `store/.bridge-token`), MCP `/metrics`, an
  MCP `initialize` with the bearer token, and whisper at `WHISPER_URL` from
  inside the mcp container, when one is set.
  Exit 0 = paired and answering,
  2 = up but waiting for the QR scan, 1 = something to fix (the failing step
  names the variable to look at: token, `WHATSAPP_MCP_ALLOWED_HOSTS`, port).
  `--wait 90` polls while the containers start; `--url https://box.tailnet.ts.net`
  tests the endpoint the way a remote client reaches it. CI runs it against
  the compose stack on every PR (unpaired path).
  On a host where the stack belongs to a manager (Komodo, Portainer) the compose
  directory and its `.env` are root-owned, so `docker compose` refuses to read
  them: `scripts/smoke.sh --project whatsapp-mcp --url https://box.tailnet.ts.net`
  finds the containers by compose label instead
  (`docker ps --filter label=com.docker.compose.project=…`) and takes the bridge
  and MCP tokens from the container environment. Without `--project` the script
  falls back to the single running compose project whose name or config path
  mentions `whatsapp-mcp`, and names the ones it found when there are several.
  Without `--url` it asks docker for the published port. The token for the
  `initialize` step comes from `WHATSAPP_MCP_TOKEN` in the environment (or
  `--mcp-token <token>`, which anyone can read in `ps`). A `404` on `/metrics`
  is reported as a warning, not a failure: a Tailscale Serve mapping that only
  routes `/mcp` gives exactly that, and the metrics are still on the MCP port.
- Logs: `docker compose logs -f bridge` / `docker compose logs -f mcp`. Files
  rotate at 10 MB × 5 per container (json-file driver), so `DEBUG` cannot fill
  the disk. Both
  services log one line per event with a level; the bridge also logs one
  line per REST request (`POST /api/send → 200 (12ms) from=127.0.0.1 ua="…"`,
  never bodies; health probes at DEBUG); raise verbosity with
  `WHATSAPP_LOG_LEVEL=DEBUG` (bridge, also echoes stored messages) or
  `WHATSAPP_MCP_LOG_LEVEL=DEBUG` (MCP server) in `.env`. For a log shipper
  (Loki, Elastic, journald) set `WHATSAPP_LOG_FORMAT=json` and
  `WHATSAPP_MCP_LOG_FORMAT=json`: one JSON object per line with `ts`,
  `level`, `module`/`logger` and `msg`.
- Metrics: both processes expose Prometheus text on `GET /metrics`
  (bridge on `8080`, MCP server on the published MCP port), unauthenticated
  like `/api/version` and limited to counts and state (never content).
  The bridge reports `whatsapp_bridge_connected`, `whatsapp_bridge_paired`,
  store and media sizes, messages stored/sent, download and webhook
  failures, reconnects and HTTP requests by status class; the MCP server
  reports `whatsapp_mcp_tool_calls_total{tool}`, errors by tool and code,
  seconds per tool and HTTP requests by class. Scrape from the tailnet:

  ```yaml
  scrape_configs:
    - job_name: whatsapp-mcp
      static_configs:
        - targets: ["home-server:8000"]   # MCP server
    - job_name: whatsapp-bridge
      static_configs:
        - targets: ["home-server:8080"]   # only if the bridge port is published
  ```

  Bridge database pools expose `whatsapp_bridge_db_in_use` (gauge),
  `whatsapp_bridge_db_wait_total` and `whatsapp_bridge_db_wait_seconds_total`
  (counters), labelled only by `pool="messages"`, `"session"` or `"contacts"`.
  A rising `rate(whatsapp_bridge_db_wait_seconds_total[5m])` shows time spent
  waiting for a bounded pool. Collecting these statistics does not acquire a
  database connection, so saturation remains observable. Media purge closes its
  256-row page cursor before probing or removing files.

  The bridge applies a POSIX `077` creation mask once at process startup;
  store files share one `0600` mode and existing database permissions are still
  tightened. Windows uses its existing ACL-based behavior.

  The MCP server also exposes `whatsapp_mcp_tool_duration_seconds`, a
  histogram of tool wall-clock time per tool (buckets 5 ms → 300 s, plus
  `+Inf`), so a slow tail shows up even when the average does not. The 95th
  percentile per tool over the last hour:

  ```promql
  histogram_quantile(
    0.95,
    sum by (tool, le) (rate(whatsapp_mcp_tool_duration_seconds_bucket[1h]))
  )
  ```

  The share of `list_messages` calls slower than a second, which is the number
  to compare before and after a performance change:

  ```promql
  1 - (
    rate(whatsapp_mcp_tool_duration_seconds_bucket{tool="list_messages",le="1"}[1h])
    / ignoring(le) rate(whatsapp_mcp_tool_duration_seconds_count{tool="list_messages"}[1h])
  )
  ```

  The counters restart at zero when the container restarts, which `rate()`
  handles; comparing a deploy therefore means comparing two time ranges of the
  same query, not two absolute values.

  `WHATSAPP_METRICS=false` / `WHATSAPP_MCP_METRICS=false` remove the endpoints;
  `WHATSAPP_MCP_METRICS_TOKEN` puts a bearer token in front of the MCP one
  (needed when the port is reachable beyond the tailnet, see Funnel).
- Update, build mode: `git pull && GIT_SHA=$(git rev-parse --short HEAD) docker compose up -d --build`.
  Update, pull mode: `git pull && docker compose pull && docker compose up -d`
  (see [Published images](#published-images)). Check what is running with
  `docker compose exec bridge wget -qO- http://127.0.0.1:8080/api/version`
  (version, commit, Go and whatsmeow versions, FTS5 state); the MCP server
  reports its version in the `initialize` response and its startup log.
- Backup: see [Backup and restore](#backup-and-restore) below.
- If the phone unlinks the device (WhatsApp > Linked Devices > Log out) or
  WhatsApp rejects the client version as outdated, the bridge exits (code 3
  or 4) instead of idling; `restart: unless-stopped` brings it back into the
  pairing flow and `docker compose logs -f bridge` shows a fresh QR code.
- Only one bridge may use a session at a time. Do not run the compose stack
  and a laptop bridge against the same store, and do not pair the same phone
  twice with two different stores: WhatsApp will keep replacing the stream.

## Published images

Both images (`ghcr.io/tauri-epo/whatsapp-mcp-bridge`,
`ghcr.io/tauri-epo/whatsapp-mcp-server`) are published for `linux/amd64` and
`linux/arm64` with these tags:

| Tag | Meaning | Published by |
| --- | --- | --- |
| `latest` | the last release; what `docker compose pull` gets by default | `release.yml`, when the release PR is merged (`release-cut.yml` does that once a day, 03:00 America/Sao_Paulo, when there is something to release) |
| `vX.Y.Z`, `X.Y` | that release, fixed | `release.yml` |
| `main` | edge: every merge to `main`, the release commit included | `publish.yml` on the push; on a release commit `release.yml` instead, off the same build as `latest` |
| `sha-<7 chars>` | one exact commit | same as `main` |

Versions are computed automatically from the commit titles (release-please:
`feat:` bumps minor, `fix:`/`perf:`/`deps:` patch, breaking changes major); the
[Releases page](https://github.com/Tauri-EPO/whatsapp-mcp/releases) and
`CHANGELOG.md` list what changed. `/api/version` reports `v1.2.3+<sha>` for a
release image and `main+<sha>` for an edge one. A release commit is built once
and carries every tag, so right after a release `main`, `latest` and `v1.2.3`
are the same digest and all three report `v1.2.3+<sha>`; `main` goes back to
`main+<sha>` on the next merge.

**What runs on arm64.** The `linux/arm64` images are built for every merge and
release, and CI executes the bridge on arm64 under QEMU (job "Bridge arm64
(QEMU)"): the image starts, creates its store and reports the FTS5 state, and
the whole Go test suite runs as an arm64 binary inside it. That covers the
pure-Go SQLite (`modernc.org/sqlite`), whose libc is architecture-specific.
Not exercised on arm64: the MCP server image (built, never run), the compose
smoke (`scripts/smoke.sh`), real hardware and real WhatsApp traffic.

The compose file names those images, so you choose per host:

- **Pull mode** (no Go or Python build on the server):
  `docker compose pull && docker compose up -d`. `WHATSAPP_IMAGE_TAG=latest`
  (default) follows releases; `main` follows every merge; pin
  `WHATSAPP_IMAGE_TAG=v1.2.3` or `sha-abc1234` in `.env` to roll back.
  `git pull` is still needed for `docker-compose.yml` and the scripts.
- **Build mode**: `docker compose up -d --build` compiles from the checkout and
  tags the result under the same name, so local changes win until the next
  `docker compose pull`. This is what the README quick start does.

**What the MCP image carries for media.** Besides the static `ffmpeg` binary
(voice notes), the server image installs Pillow (~21 MB with its bundled codecs),
pillow-heif (~26 MB, almost all of it libheif and the HEVC decoders) and
pypdfium2 (~8 MB, Google's PDFium) so `read_media` can downscale a photo to ~1568 px
before it travels instead of shipping 6 MB of base64 into the model's context,
turn a TIFF, BMP or HEIC into something the client renders, and render the pages
of a scanned PDF as pictures. All three ship self-contained manylinux wheels for
`amd64` and `arm64`, so nothing is added to the Debian layer: the runtime is
still the bare interpreter plus `/app/.venv`. The cost at runtime is CPU during
the call (a few hundred milliseconds for a 12 MP photo or a rendered page,
nothing between calls); an image above 64 megapixels is refused rather than
decoded, a PDF render is capped at 20 pages and 8 MB of image per call and runs
one at a time (PDFium is not thread-safe), and
`read_media(max_edge=0)` skips the image path entirely and returns the stored
bytes.

Each push carries a SLSA provenance attestation and an SBOM
(`docker buildx imagetools inspect ghcr.io/tauri-epo/whatsapp-mcp-bridge:latest`
lists them), and the weekly security workflow scans the published images
with Trivy. A packages page must be public for anonymous pulls; that is a one-time
switch in GitHub (Packages > package > Package settings > Change visibility),
otherwise `docker login ghcr.io` with a read-only token first. A pull that fails
with `denied` against a package that *is* public is a stale credential on the
host, not a permission problem — see
[Troubleshooting](TROUBLESHOOTING.md#pulling-the-images-from-ghcr).

## Managed stacks (Komodo, Portainer)

Nothing here needs a stack manager, but the compose file runs happily under one,
and the home server this fork is written for does exactly that: Komodo owns the
checkout in `/etc/komodo/stacks/<stack>` and redeploys it. Three habits
change when the manager owns the stack.

**The compose directory and its `.env` are root-owned.** An operator in the
`docker` group can still drive the containers, but `docker compose` refuses the
directory itself (`open .env: permission denied`), which used to rule out the
post-deploy check. Give `scripts/smoke.sh` the project name instead and it works
entirely through the container labels:

```bash
scripts/smoke.sh --project whatsapp-mcp --url https://box.tailnet.ts.net
```

It resolves the bridge and mcp containers with
`docker ps --filter label=com.docker.compose.project=…`, reads the bridge and MCP
tokens from the container environment, and never opens `.env`
(see [Health and operations](#health-and-operations)).

**Environment changes belong in the manager**, in the stack's Environment
section, not in the `.env` file on disk. A redeploy rewrites that file from what
the manager holds, so an edit made on the server works until the next deploy and
then vanishes without a word — including `WHATSAPP_IMAGE_TAG`, which is how you
pin or roll back an image.

**`docker logs` only covers the container that is running now.** A redeploy
replaces it, and everything the previous one printed is gone unless the manager
keeps its own log history. That is fine for steady-state logs and expensive for
the lines a new release prints exactly once, on its first start: the timestamp
migration ("Timestamp migration: rewrote N messages.timestamp value(s) to UTC")
and the mentions backfill ("Mentions migration: recovered mentions for N
message(s) …") report what they touched and never repeat. (The related
"Timestamp migration: repaired N …" warning does repeat, at every start, until
the rows it names are fixed — see
[Troubleshooting](TROUBLESHOOTING.md#messages-are-out-of-order-after-an-image-rollback).)
Read the once-only lines in the
manager's log view right after an upgrade, or keep a copy before the next
deploy:

```bash
docker logs "$(docker ps --filter label=com.docker.compose.project=whatsapp-mcp \
  --filter label=com.docker.compose.service=bridge --format '{{.Names}}')" \
  > "deploy-$(date +%F).log" 2>&1
```

## Several instances on one host

Two WhatsApp accounts on one box are two compose projects of this repo, one
checkout (or one manager stack) each. Nothing in the code knows about the other
one: a bridge holds one session and one `messages.db`, an MCP server reads one
store and talks to one bridge. What the two share is the host, and that is
where every item of this checklist comes from.

- **One directory per account, one project name.** `COMPOSE_PROJECT_NAME`
  defaults to the directory name; set it in each `.env` anyway
  (`COMPOSE_PROJECT_NAME=wa-sales`) so the name survives a move. It prefixes the
  volumes (`wa-sales_whatsapp-store`), the container names and the compose
  labels that `scripts/smoke.sh --project` and `scripts/backup.sh` resolve. It
  prefixes nothing that is a path: `./outbox` and any other bind mount belong to
  the directory, so two projects started from one checkout share them. One
  checkout per account, always.
- **Per-account values in `.env`.** `WHATSAPP_MCP_PORT`: each stack publishes
  its own, the second `up` fails with "port is already allocated" otherwise.
  `WHATSAPP_MCP_TOKEN`: one credential per account, so a client configured for
  one cannot reach the other. `WHATSAPP_DEVICE_NAME`: the label in the phone's
  Linked Devices list, applied at pair time. `WHATSAPP_PUBLIC_URL` and
  `WHATSAPP_MCP_ALLOWED_HOSTS`: the endpoint each one is reached at.
  `WHATSAPP_OUTBOX` when the outbox lives outside the checkout. The policy
  variables (`WHATSAPP_ALLOWED_CHATS`, `WHATSAPP_READ_ONLY`, the tool lists) are
  per account as well: a second account is a second security boundary, not a
  copy of the first `.env`.
- **One endpoint per instance.** With `tailscale serve`, give each stack its
  own path on the one HTTPS port: `--https=443 --set-path /sales
  http://127.0.0.1:8000` and `--https=443 --set-path /support
  http://127.0.0.1:8001`. Serve strips the prefix before forwarding, so the
  MCP server still sees `/mcp` and clients use
  `https://box.tailnet.ts.net/sales/mcp`; `WHATSAPP_PUBLIC_URL` gets that full
  URL. Separate ports (`--https=8443` for the second) work the same way.
  `WHATSAPP_MCP_ALLOWED_HOSTS=box.tailnet.ts.net` on both (a bare hostname
  matches any port).
- **Checks and backups per project.** `scripts/smoke.sh --project wa-sales
  --url https://box.tailnet.ts.net` and `--project wa-support --url
  https://box.tailnet.ts.net:8443`; `COMPOSE_PROJECT_NAME=wa-sales
  scripts/backup.sh` (or `WHATSAPP_STORE_VOLUME=wa-sales_whatsapp-store`) for
  each. A nightly cron is one line per account. A restore goes back into the
  project the backup came from, and onto one host only
  ([Backup and restore](#backup-and-restore)).
- **Pairing.** Each stack pairs its own phone: `docker compose logs -f bridge`
  in that stack's directory shows its QR code. Two stacks paired to the same
  phone are two linked devices of one account, which works but is rarely what
  you meant.
- **Whisper once.** One whisper.cpp server for the host, attached to every
  stack's network, the same `WHISPER_URL` in each stack
  ([Voice-note transcription](#voice-note-transcription)). Bridge and MCP are
  small; whisper is where a second stack would cost.
- **Managed stacks.** One Komodo (or Portainer) stack per account, its name
  the project name, each with its own Environment section
  ([Managed stacks](#managed-stacks-komodo-portainer)).
- **Not supported.** Two bridges on one store: the second refuses to start
  (`Refusing to start: another whatsapp-bridge already holds this store`,
  `instance_lock.go`). One MCP server for two accounts: it reads one
  `messages.db` and calls one bridge, so an agent that needs both accounts gets
  two MCP endpoints in its client configuration, one per stack.

## Backup and restore

The `whatsapp-store` volume holds everything worth keeping: `whatsapp.db`
(the WhatsApp session keys; losing it means re-pairing), `messages.db`
(local history and read state), the per-chat media directories,
`.bridge-token` (the REST bearer) and `notes.db` (the agent's notes about
media, written by the MCP server).
`scripts/backup.sh` snapshots it **while the stack runs**:

```bash
scripts/backup.sh backup                 # -> ./backups/<UTC timestamp>/
scripts/backup.sh backup /mnt/nas/wamcp  # explicit destination
scripts/backup.sh prune ./backups 7      # keep the newest 7 snapshots
```

It starts a throwaway `alpine` container with the `sqlite3` CLI, copies each
database with `.backup` (a consistent snapshot even mid-write, thanks to WAL
mode), verifies it with `PRAGMA integrity_check`, tars the media directories
and copies the token. Every file of the snapshot is created `0600` and handed
to the owner of the destination directory, so the account that ran the script
can ship it off-box and no other account on the host can read it. A snapshot is
a plain directory:

```
messages.db  whatsapp.db  [notes.db]  media.tar  bridge-token  MANIFEST
```

The script resolves the volume from the compose project labels; outside the
repo directory set `COMPOSE_PROJECT_NAME` or `WHATSAPP_STORE_VOLUME`. A nightly
cron entry, with the snapshot shipped off-box afterwards:

```cron
15 3 * * * cd /opt/whatsapp-mcp && scripts/backup.sh backup >/dev/null && scripts/backup.sh prune backups 14 && rsync -a --delete backups/ nas:/backups/whatsapp-mcp/
```

**The backup is a credential.** `whatsapp.db` lets anyone who has it act as
your linked device and `bridge-token` opens the REST API. Keep snapshots
encrypted at rest (`age`, `restic`, an encrypted NAS share) and out of shared
drives; delete old ones (`prune`). Media can be large: with
`WHATSAPP_MEDIA_RETENTION_DAYS` set, the tree stays bounded and the tar stays
small, and every file can be re-fetched on demand while WhatsApp still serves
it.

Restore onto **one** host only. Restoring `whatsapp.db` on a second machine
while the first still runs makes WhatsApp replace the stream on both, and the
bridge exits until you unlink one of them.

```bash
docker compose stop                       # the script refuses to restore into a running bridge
scripts/backup.sh restore backups/20260904T031500Z
docker compose up -d
docker compose logs -f bridge             # expect "connected", no QR
```

Restore removes stale `-wal`/`-shm` files and the instance lock, copies the
databases, media and token back, and resets ownership to the container user.
To move to a new server, restore the snapshot into the fresh volume before the
first `docker compose up`, and keep the same `WHATSAPP_BRIDGE_TOKEN` in `.env`
if you had set one (otherwise the restored `.bridge-token` is used).

### The store read-only in the MCP container

The default compose file mounts `whatsapp-store` read-write into both services,
as the same user (uid 1000), and that layout reads `messages.db` in every state
of the bridge. The MCP server only reads the databases (it opens them with
`mode=ro`), so mounting the volume `:ro` into `mcp`, or running `mcp` as a
user that cannot write the store directory, looks like a free hardening step.
It works **only while the bridge has the database open**, and the bridge is
stopped on every deploy, `docker compose up -d --build` and crash. Measured with
the real server image (SQLite 3.46.1 in its `python:3.13-slim` base; one
container writing and another reading the same volume), with an idle writer:

| Case | Result |
|---|---|
| bridge running (`messages.db-wal` and `-shm` exist), mount `:ro` | reads work, including rows still only in the `-wal` |
| bridge running, `mcp` as another uid | every read fails: `unable to open database file` (the databases are `0600` since #491; with the `0644` files of earlier releases this case read) |
| bridge stopped cleanly (no `-wal` / `-shm` left), mount `:ro` | every read fails: `unable to open database file` |
| bridge stopped cleanly, `mcp` as another uid | every read fails: `unable to open database file` |
| default compose layout (read-write, same uid), any state | reads work (the reader recreates `-shm` / `-wal` next to the file) |

A WAL database needs its `-shm` file, and a reader that cannot create it can
only use one the writer left behind. Before the server opened the files
read-only (#529) the same two cases failed in exactly the same way (checked in
the same Docker setup with a plain `sqlite3.connect`), so this is SQLite's rule
and not something the server added. The "reads work" rows were measured with an
idle writer: a reader that can neither write nor lock `-shm` was not tried
against a checkpoint happening during its query.

**Do not mount the store `:ro` into `mcp`.** The default layout is the supported
one, and with a read-only mount every read tool, `coverage` included, fails
whenever the bridge is down (see
[Troubleshooting](./TROUBLESHOOTING.md#the-mcp-server-cannot-read-messagesdb-on-a-read-only-store)).

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
