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
./whatsapp-bridge    # or `go run -tags sqlite_fts5 .` during development
```

Do not remove `whatsapp-bridge/store/messages.db` for this recovery unless you
also want to delete the local message archive.
