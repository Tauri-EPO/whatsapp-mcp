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
  cd /etc/komodo/stacks/whatsapp-mcp           # wherever the stack lives
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
