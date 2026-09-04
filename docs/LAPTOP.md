# Running on a laptop (stdio, no Docker)

The server can run as a local process launched by Claude Desktop, Cursor or Claude Code over stdio, the way the upstream project was designed. The Docker Compose route in [DOCKER.md](DOCKER.md) is the primary path for this fork; this page keeps the laptop workflow complete.

## Prerequisites

- Go 1.26+
- Python 3.11+
- [uv](https://docs.astral.sh/uv/) package manager
- Claude Desktop or Cursor
- FFmpeg (optional, for voice message conversion)

## Quick start

1. **Clone the repository**

   ```bash
   git clone https://github.com/Tauri-EPO/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Start the WhatsApp bridge**

   ```bash
   cd whatsapp-bridge
   go run .   # the tag enables full-text message search
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

## Updating

Pull the latest changes, then refresh whichever components moved:

```bash
git pull
```

| You changed                                                              | What to do                                                                                                                                            |
| ------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Bridge code** (`whatsapp-bridge/*.go`) and you run `go run .` | Nothing — `go run` recompiles each launch. Just restart the bridge.                                                                          |
| **Bridge code** and you run a built binary                               | `cd whatsapp-bridge && go build -o whatsapp-bridge && ./whatsapp-bridge`                                                            |
| **MCP server** (`whatsapp-mcp-server/*.py`, `pyproject.toml`, `uv.lock`) | Restart Claude Desktop / Cursor — `uv` re-resolves from the lockfile on next launch. Force a sync with `cd whatsapp-mcp-server && uv sync` if needed. |

Updates do **not** require re-pairing or deleting `whatsapp.db` — your session and message history are preserved. Re-pairing is only needed when explicitly requesting full history (see [Requesting full history](CONFIGURATION.md#requesting-full-history)).

For `v0.2.1` and later, restart both the bridge and MCP server after updating
so the MCP server can read the bridge token. If the two components do not share
the same checkout, set the same `WHATSAPP_BRIDGE_TOKEN` value in both
environments.

## Cursor IDE Configuration

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

## Run automatically on macOS

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

## Windows

The bridge is pure Go (modernc.org/sqlite), so no C toolchain or MSYS2 is needed:

```bash
go run .
```
