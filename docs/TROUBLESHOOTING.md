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

To fill a hole, ask the phone for one chat with `request_history(chat_jid)`
(or `POST /api/history`, see
[CONFIGURATION.md](CONFIGURATION.md#requesting-history-for-a-single-chat-on-demand)).
It is anchored on the oldest message already stored, arrives asynchronously, and
the phone decides how much it returns — messages it deleted itself are gone. For
a full backfill instead, re-pair once with `--full-history-pair`.

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

  Note that `bridge_status` still reports `ok: true` here, correctly: it checks
  the bridge over loopback and never inspects the published endpoint.

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
