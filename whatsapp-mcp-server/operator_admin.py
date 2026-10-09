"""Bridge-authenticated, loopback-only reads; the operator secret stays in Go."""

from __future__ import annotations

import hmac
import ipaddress
import json
import os
import threading
from contextvars import ContextVar
from datetime import UTC, datetime
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import cast

authenticated_transport: ContextVar[bool] = ContextVar("authenticated_transport", default=False)
_lock = threading.Lock()
_last_call: str | None = None


def record_call():
    global _last_call
    if authenticated_transport.get():
        with _lock:
            _last_call = datetime.now(UTC).isoformat().replace("+00:00", "Z")


class AuthenticatedCalls:
    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        token = authenticated_transport.set(scope.get("type") == "http")
        try:
            await self.app(scope, receive, send)
        finally:
            authenticated_transport.reset(token)


def start_admin(bridge_token: str, mcp_token: str | None, *, port: int = 8091):
    if len(bridge_token) < 16 or (mcp_token and hmac.compare_digest(bridge_token, mcp_token)):
        raise ValueError("MCP admin requires a bridge token distinct from the MCP bearer; configure WHATSAPP_MCP_TOKEN")

    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(5)

        def log_message(self, format, *args):
            pass  # No credentials, URLs or request headers in logs.

        def do_GET(self):
            host = f"127.0.0.1:{cast(HTTPServer, self.server).server_port}"
            headers = self.headers
            auth = headers.get_all("Authorization", [])
            if (
                not ipaddress.ip_address(self.client_address[0]).is_loopback
                or len(auth) != 1
                or not hmac.compare_digest(auth[0], f"Bearer {bridge_token}")
            ):
                self.reply(401, {"error": "unauthorized"})
            elif headers.get_all("Host", []) != [host] or headers.get_all("Origin", []) not in ([], [f"http://{host}"]):
                self.reply(403, {"error": "host_origin_refused"})
            elif headers.get("Content-Length", "0") != "0" or headers.get("Transfer-Encoding"):
                self.reply(400, {"error": "body_refused"})
            elif self.path == "/admin/v1/transcription/usage":
                from transcription_usage import current_usage

                try:
                    self.reply(200, current_usage())
                except Exception:  # noqa: BLE001 - no database paths in a control-plane error
                    self.reply(503, {"error": "usage_unavailable"})
            elif self.path == "/admin/v1/health":
                with _lock:
                    self.reply(200, {"last_mcp_call_at": _last_call})
            else:
                self.reply(404, {"error": "not_found"})

        def reply(self, status, payload):
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
            self.close_connection = True

    server = HTTPServer(("127.0.0.1", port), Handler)
    thread = threading.Thread(target=server.serve_forever, name="mcp-admin", daemon=True)
    thread.start()
    return server


def install_admin():
    if not os.getenv("WHATSAPP_OPERATOR_BIND", "").strip():
        return None
    from http_auth import resolve_http_token
    from whatsapp import _read_bridge_token

    bridge_token = _read_bridge_token() or ""
    token, _ = resolve_http_token(
        os.getenv("WHATSAPP_MCP_TOKEN"), os.getenv("WHATSAPP_MCP_HOST", "127.0.0.1"), _read_bridge_token
    )
    return start_admin(bridge_token, token)
