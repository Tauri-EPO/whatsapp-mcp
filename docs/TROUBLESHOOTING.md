# Troubleshooting

Symptoms and fixes for pairing, auth, sync and app-state problems. For container-specific checks (`docker compose ps`, health endpoints, logs) see [DOCKER.md](DOCKER.md).

## Authentication Issues

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

## "My agent says a chat is quiet"

An empty `list_messages` result means "nothing in the archive", which is not the
same as "nothing was said". The archive holds what the phone pushed at pair time
plus everything that arrived since, so it can start late, miss the days the
bridge was down, and list chats whose metadata synced while their history never
did. An agent that does not check will state a hole as fact.

Ask the archive what it actually has, with the `coverage` tool
([TOOLS.md](TOOLS.md#coverage)) — read-only, works while the bridge is down:

- `first_message_time` earlier than the period in question? If not, the answer
  is "never synced", not "quiet".
- `gaps`: windows with no message in **any** chat. A multi-day gap across every
  conversation is a sync outage, not silence.
- `chats_without_messages`: chats known by name with zero messages stored.

Scope it with `after`/`before` when old sync artefacts crowd out the hole you
care about, or with `chat_jid` to ask about one conversation. Then
`coverage(by_chat=True)` lists *which* chats to backfill, worst first, with a
`stub_only` flag for the ones holding nothing but a pair-time history-sync stub.

To fill a hole, ask the phone for one chat with `request_history(chat_jid)`
(or `POST /api/history`, see
[CONFIGURATION.md](CONFIGURATION.md#requesting-history-for-a-single-chat-on-demand)).
It is anchored on the oldest message already stored, arrives asynchronously, and
the phone decides how much it returns — messages it deleted itself are gone. For
a full backfill instead, re-pair once with `--full-history-pair`.

## "The number was added to a group and the earlier messages are missing"

When someone adds a number to a group, WhatsApp can offer to share the group's
recent messages with the new member. The archive of a bridge linked to that
number still starts at the moment it joined: the shared messages are **not**
stored today.

That share does not travel as a history sync (the companion sync at pair time,
or the on-demand request `request_history` makes). The adder's client uploads
the messages as one encrypted bundle and sends a message that only points at
it. The bridge recognises that message and says so, but it does not download or
decode the bundle yet (issue #468):

```text
Group history bundle seen in <group>@g.us (message <id>, from_me=false): 42 messages, oldest in window 1700000000, oldest in bundle 1700003600, 1 history receivers, 3 other receivers; the shared messages are not downloaded or stored
```

How to read it:

- The line means a share message reached this bridge. `from_me=true` is the
  copy of a share this account made itself; the two receiver counts say how many
  accounts were to get the history and how many were not. The timestamps are the
  values WhatsApp sent, as sent.
- Every such message also increments
  `whatsapp_bridge_group_history_shares_total` on `/metrics`. It counts
  messages, not adds: one add can produce more than one (a bundle and a notice,
  or a redelivery), and the counter starts again at `0` when the bridge
  restarts.
- **No line is weaker evidence than a line.** It needs `WHATSAPP_LOG_LEVEL` at
  `INFO` or lower, the share has to arrive on the live connection while the
  bridge is running (a share replayed inside a history sync is not reported),
  and a message this device could not decrypt never gets that far.

`request_history` does not fetch the bundle: it asks the account's **own
phone** for messages older than the oldest one stored, and it cannot name a
bundle. Whether that phone, once it has processed the share itself, returns the
shared messages on such a request (or at a re-pair with `--full-history-pair`)
has not been observed; do not count on it.

Until the bundle is decoded, the copy that is known to exist is the archive of
an account that was already in the group: export it there with
`export_messages`.

## "Messages are out of order after an image rollback"

Every timestamp in `messages.db` is stored in one spelling (UTC, `+00:00`, fixed
width — see [ARCHITECTURE.md](ARCHITECTURE.md)), because the filters and
`ORDER BY` compare those strings directly. Releases before the canonical
spelling let the SQLite driver stamp the local offset of the machine on the row,
so an operator who pins the image back to one of them for a day and then rolls
forward leaves rows the current release sorts and filters by wall clock: a row
written in a `-03:00` zone carries the local hour, so it sorts three hours
earlier than the instant it stands for, and an `after` / `before` bound can miss
it entirely.

Nothing to do — the bridge repairs itself. Every start probes each time column
for a value that is not in the canonical spelling and re-runs the rewrite for
the columns that hit, logging what it changed:

```text
[WARN] Timestamp migration: repaired 412 messages.timestamp value(s) written by an older bridge after the migration ran
```

If instead the log says values *could not be parsed*, those rows keep their old
spelling and stay wrong, and the bridge re-reads that column at every start. The
line just above each summary names the table, the column and the rowid:

```text
[WARN] Timestamp migration: leaving messages.timestamp rowid=871 unchanged: unrecognised timestamp "20/09/2026"
```

Fix or delete those rows with the bridge stopped
(`sqlite3 store/messages.db "UPDATE messages SET timestamp = '2026-09-04 20:13:09+00:00' WHERE rowid = 871"`);
the next start picks the change up and stops warning.

## Pulling the images from GHCR

- **`docker compose pull` fails with `denied`**:

  ```text
  Head "https://ghcr.io/v2/tauri-epo/whatsapp-mcp-server/manifests/latest": denied: denied
  ```

  Both packages are public and anonymous pulls work, so this is almost never
  the package. It is the Docker client on the host presenting a **stale
  credential**: an old `docker login ghcr.io` whose token has since expired or
  lost `read:packages` still sits in `~/.docker/config.json`. GHCR rejects that
  token instead of falling back to anonymous access, and the rejection reads
  exactly like a private-package error. Mind whose config it is: a stack
  deployed by a manager (Komodo, Portainer) pulls as root, so root's
  `config.json` is the one that counts, not yours.

  **Fix**, as the user that runs the deploy (`sudo -i` for a root-owned stack)
  and from the stack's own directory, because `docker compose` needs the file:

  ```bash
  docker logout ghcr.io                        # drop the stale credential
  cd /path/to/stack                            # wherever the stack lives
  docker compose pull                          # anonymous pull; public package
  ```

  With a stack manager, redeploying from its UI after the `docker logout` does
  the same thing.

  **Confirm the package is public** before chasing anything else — an anonymous
  pull token and one manifest request, from any machine:

  ```bash
  token=$(curl -s "https://ghcr.io/token?scope=repository:tauri-epo/whatsapp-mcp-server:pull" \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  curl -sI -H "Authorization: Bearer $token" \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    https://ghcr.io/v2/tauri-epo/whatsapp-mcp-server/manifests/latest | head -1
  # HTTP/2 200 (or HTTP/1.1 200 OK) <- public. A 401 or 403 means it is private.
  ```

  A 200 there next to a `denied` from `docker compose pull` proves the
  credential is at fault. Visibility itself is a one-time switch in GitHub
  (Packages > package > Package settings > Change visibility), see
  [DOCKER.md](DOCKER.md#published-images).

## Published HTTPS endpoint

- **`certificate verify failed: certificate has expired`**: the certificate
  serving your published MCP endpoint (`https://<host>.<tailnet>.ts.net/mcp`)
  has expired. Direct clients fail at the TLS handshake, before any MCP or HTTP
  error, so the message names no chat, tool or container:

  ```text
  ssl.SSLCertVerificationError: [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: certificate has expired
  ```

  PowerShell reports the same condition as
  `Invoke-WebRequest: The remote certificate is invalid according to the
  validation procedure`, and `curl` as
  `SSL certificate problem: certificate has expired`.

  Expect this to be invisible at first. Clients that reuse a long-lived session
  or their own trust store (`npx mcp-remote`, for example) can keep working
  after the certificate expires, so the first symptom is usually a *new* direct
  client — a bulk-export script, a monitor — failing while your agent still
  answers.

  **Cause.** The bridge and MCP containers never terminate TLS; they bind
  loopback and something on the host publishes them (`tailscale serve`, a
  reverse proxy). Tailscale's certificates are short-lived, and renewing them
  is the **host's** job, not the container's. `tailscale serve` renews while
  `tailscaled` keeps running; a certificate fetched once with `tailscale cert`
  into files for a proxy is *not* renewed by anything unless you scheduled it.
  Restarting or rebuilding the containers changes nothing.

  Note that `bridge_status` reports `ok: true` here, correctly: it checks the
  bridge over loopback and knows nothing about the published endpoint — unless
  you tell it where that endpoint is. Set `WHATSAPP_PUBLIC_URL` to the URL your
  clients use and the same call answers the question before a client does:

  ```jsonc
  "endpoint_cert_expires_at": "2026-10-07T21:52:11Z",
  "endpoint_cert_days_left": -3,
  "endpoint_cert_error": "certificate verify failed for myserver.tail1234.ts.net:443: certificate has expired"
  ```

  It is one TLS handshake with no request, cached for an hour, and it cannot
  make `bridge_status` fail; see
  [Watching the published certificate](CONFIGURATION.md#watching-the-published-certificate).
  It only watches — renewing is still the host's job, below.

  **Fix**, on the host that serves the endpoint:

  ```bash
  # Serving through tailscale serve: confirm what is published, then re-apply.
  tailscale status                                       # tailscaled up?
  sudo tailscale serve status
  sudo tailscale serve --bg --https=443 http://127.0.0.1:8000

  # Serving through a reverse proxy from certificate files: reissue and reload.
  sudo tailscale cert <host>.<tailnet>.ts.net            # writes .crt / .key
  sudo systemctl reload nginx                            # or your proxy
  ```

  **Verify** the certificate the endpoint actually serves, from any machine on
  the tailnet:

  ```bash
  echo | openssl s_client -connect <host>.<tailnet>.ts.net:443 \
      -servername <host>.<tailnet>.ts.net 2>/dev/null \
    | openssl x509 -noout -dates
  # notBefore=...
  # notAfter=...   <- must be in the future
  ```

  Then re-run the client that failed. If you front the endpoint with a proxy
  and reissue certificates by hand, put that `openssl` line in a cron job on
  the host: nothing in this repo watches the expiry date for you.

## The MCP server cannot read `messages.db` on a read-only store

- **Reads fail with `database error: unable to open database file` or
  `attempt to write a readonly database` although the file exists**, and you
  mounted the store `:ro` into `mcp` (or run it as a different user). SQLite
  must create `messages.db-shm` to read a WAL database; when the bridge is
  running it has already created it and the reader only attaches, but once the
  bridge has stopped cleanly the `-wal` / `-shm` files are gone and a reader
  that cannot write the directory has nothing to attach to. To confirm: the
  error is `unable to open database file` although `messages.db` exists (a
  missing file says `messages.db not found` instead), `ls` of the store shows no
  `messages.db-wal` / `messages.db-shm`, and `docker inspect <mcp container>`
  lists the store mount with `"RW": false`. Start the bridge, or give `mcp` a
  writable store (the default compose layout). The measurements
  are in [DOCKER.md](./DOCKER.md#the-store-read-only-in-the-mcp-container).
- **Every read fails with `unable to open database file`, bridge running or
  not, and `mcp` runs as another user than the bridge**: the bridge keeps
  `messages.db` and `whatsapp.db` `0600` and sets them back to that at every
  start (its log says `Tightened ... to 0600` the first time). Run both
  processes as the same user, as the images and the compose file do (uid 1000);
  a group that used to be able to read the files no longer can.

## `messages.db not found at <path>`

- **Every read tool answers `internal: messages.db not found at <path>: the
  bridge has not created it there, or the path is wrong. The path comes from
  WHATSAPP_DB_PATH, ...`** (a tool that wraps database errors prefixes it with
  `database error:`). The MCP server only
  *reads* `messages.db` (the bridge owns and writes it) and opens it read-only,
  so a path where nothing exists is an error instead of a new empty database
  that answers every query with "no such table" or an empty list. The path in
  the message is the one the server resolved: `WHATSAPP_DB_PATH` if set,
  otherwise `WHATSAPP_STORE_DIR/messages.db`, otherwise `../whatsapp-bridge/store/messages.db`
  relative to the server. Check that it is the directory the bridge writes to
  (`docker compose exec bridge ls /app/store`; in compose both services mount
  the `whatsapp-store` volume at `/app/store`) and that the bridge has started
  at least once. `bridge_status` keeps working without the file, and
  `whatsapp.db` (`WHATSMEOW_DB_PATH`) is opened the same way but its absence is
  tolerated by the tools that use it. No `messages.db` is created in the wrong place, so
  there is nothing to delete once the path is fixed. (Reading a database that
  does exist may still create its `-shm` file next to it.)

## whisper configured but not reachable

- **`bridge_status` answers `"whisper": { "configured": true, "backend": "url",
  "reachable": false }`**, and with `TRANSCRIBE_ON_INGEST=1` the MCP log repeats
  `transcribe_on_ingest: the whisper backend is unavailable (...)` once per
  round. The MCP server probes `WHISPER_URL` from inside the mcp container,
  which shares the bridge's network namespace. Check from the same place:

  ```bash
  scripts/smoke.sh --project whatsapp-mcp     # step 5 probes WHISPER_URL and names the address
  docker compose exec mcp python -c 'import urllib.request; urllib.request.urlopen("http://whisper:8178/")'
  ```

  The usual causes, in order: the whisper project is not running (`docker ps`
  from its directory); it is not on this stack's network (`docker network
  inspect whatsapp-mcp_default` must list it; a stack recreated with `down` and
  `up` gets a fresh network the whisper container is no longer attached to, so
  restart the whisper project); `WHISPER_URL` names something the bridge cannot
  reach (a service name only resolves on a shared network, and the host's own
  loopback is not reachable from a container); or the server is still
  downloading its model on first start (`scripts/smoke.sh --wait 120` gives it
  time). The compose for such a server is in
  [Voice-note transcription](DOCKER.md#voice-note-transcription).

## Two accounts on one host

Two compose projects of this repo on one box
([Several instances on one host](DOCKER.md#several-instances-on-one-host)).

- **`Bind for 127.0.0.1:8000 failed: port is already allocated` on the second
  `up`**: both stacks publish the MCP endpoint on the same host port. Set a
  different `WHATSAPP_MCP_PORT` in the second stack's `.env` (or its manager
  Environment) and point that stack's `tailscale serve` mapping at it.
- **A file dropped in the outbox for one account is visible to the other
  agent**, or `send_file` from one stack finds the other's files: the two
  projects were started from the same checkout, so `./outbox` (and any other
  bind mount) is the same directory. `COMPOSE_PROJECT_NAME` separates volumes,
  not paths. Give each account its own checkout, or set a distinct
  `WHATSAPP_OUTBOX` per stack.
- **`Refusing to start: another whatsapp-bridge already holds this store`**:
  two bridges were pointed at one store directory; see
  [Authentication Issues](#authentication-issues). Two accounts need two
  stores, which two projects get by default.

## The phone says the bridge "will be disconnected in 1 day"

Under Linked devices the phone shows, for the bridge's device, "open WhatsApp
on this device to keep it connected". WhatsApp logs a linked device out about a
month after it was last opened, and a connection does not count as opening.

A bridge from v2.1 on prevents it by itself (`WHATSAPP_SESSION_KEEPALIVE_HOURS`,
see [Keeping the linked device](CONFIGURATION.md#keeping-the-linked-device)):
check the log for `Session keepalive: told WhatsApp this linked device is in
use` and reopen the Linked devices screen; the last connection of the device
moves to that minute. If the variable is `0`, or the bridge is older, update
and restart it before the day runs out.

Restarts now honour the last blip saved in the store's `.session-keepalive` file;
the new INFO line says when the next blip is due. To force one after the session
settles, delete that file in the store directory and restart the bridge.

If the device was already logged out, the bridge exits and waits for a new QR
scan: `docker compose logs -f bridge`, scan, done. The archive in `messages.db`
is kept; only the session in `whatsapp.db` is replaced.

## Media downloads fail for one chat

- **`download_media` answers `Failed to download media: failed to create chat
  directory inside the store: ...` or `... chat directory is not a real
  directory inside the store`**, and the bridge log repeats it for every
  inbound photo of that chat: `store/<chat_jid>/` is a symlink. The bridge
  writes media through a handle on the store directory and follows no link on
  the way, so a chat directory linked onto another disk no longer receives
  files, and the files behind the link are no longer served. Mount the other
  disk at that path instead (a volume or a bind mount on
  `/app/store/<chat_jid>`), or move the whole store with `WHATSAPP_STORE_DIR`;
  the reasoning is in [the store root](ARCHITECTURE.md#the-store-root).
- **`Refusing to cache media for message ...: refusing media path`** (WARN in
  the bridge log, and the same text from `download_media`): the chat JID or the
  message ID of that row is not a plain file name (a path separator, `..` or a
  control character). The message row is stored as usual; only its file is not
  cached, by design.

## A recent file cannot be downloaded

`download_media` / `read_media` answer `bridge_unavailable` with **"the WhatsApp
CDN refused the request (HTTP 403) for a message only 4m old; its link cannot
have expired yet, so this is not a lost file: try again later"** (404 and 410
read the same way).

The bridge downloads a file by the message's own direct path
(`messages.direct_path`; rows stored before that column existed use the path
cut out of `url`). When the CDN refuses that path and the `url` names something
else, the url's path is tried once as well, which is what the bridge asked for
before it kept the direct path. If both are refused: WhatsApp's CDN keeps a
file for days, so for a message less than six hours old the request is what
failed. The bridge then does not ask the sender's phone to re-upload, and does
not mark the file `media_unavailable`: the next call tries again. Past six
hours, or when the link itself is stamped as expired, the media retry runs as
it always did.

Each refusal leaves one WARN line in the bridge log (`docker compose logs
bridge | grep "CDN refused media"`):

```
CDN refused media for message <message-id> with HTTP 403: message age 4m12s, asked for the message's direct path: /v/ (3 segments, query ccb,oh,oe,_nc_sid, expiry stamp in the future), same object as the url's path, other parameters; url host mmg.whatsapp.net; key 32, sha256 32, enc sha256 32 bytes
```

How to read it:

- **`asked for the message's direct path` / `the path cut out of the stored
  url`**: which of the two the request used. The second means the row has no
  `direct_path` (an older row, or a message that carried none).
- **`/v/ (3 segments, query ccb,oh,oe,_nc_sid, …)`**: the first segment of the
  path, how deep it is, and the names of its query parameters. `no query` is a
  path the CDN cannot authorise; a `?` in the list is a piece that was not
  `name=value`.
- **`expiry stamp in the future` / `in the past` / `no expiry stamp`**: what
  the link's own `oe` parameter says. `in the past` on a recent message is an
  old upload sent again, and goes to the media retry.
- **`same as the url's path`**, **`same object as the url's path, other
  parameters`** (the ordinary case: clients add a parameter of their own to
  the url) or **`another object than the url's path`**: how the message's
  `url` relates to its direct path. Only the last one means the two disagree.
- **`url host …`**: the host of the stored url, or `not a plain host name`.
- **`key 32, sha256 32, enc sha256 32 bytes`**: the sizes of the media key and
  the two hashes the row holds. Anything but 32 is a row that was stored wrong.

A second line, `Message <message-id> was downloaded through the path cut out of
its url after its direct path was refused`, means the file arrived anyway. The
pair is still worth reporting: it says which of the two paths the CDN accepts.

The line carries no token, file name or hash, only the shape above, so it can
be attached to a report once the message ID is replaced by a placeholder. A
file that keeps failing with this line while the phone opens it is a bug worth
reporting with it.

## App State / LTHash Conflicts

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
./whatsapp-bridge    # or `go run .` during development
```

Do not remove `whatsapp-bridge/store/messages.db` for this recovery unless you
also want to delete the local message archive.
