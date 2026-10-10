"""Bridge-authenticated private reads; the operator secret stays in Go."""

from __future__ import annotations

import hmac
import ipaddress
import json
import logging
import os
import re
import socket
import struct
import threading
from contextvars import ContextVar
from datetime import UTC, datetime
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import cast
from urllib.parse import urlsplit

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


def admin_endpoint() -> tuple[str, str, str | None, str | None]:
    """Split Compose pins both qualified names through trusted extra_hosts."""
    url = urlsplit(os.getenv("WHATSAPP_API_URL", "http://127.0.0.1:8080/api"))
    name = url.hostname or ""
    if not name.startswith("bridge-agent"):
        return "127.0.0.1", "127.0.0.1", None, None
    if (
        not re.fullmatch(r"bridge-agent\.[a-z0-9][a-z0-9_-]*_agent", name)
        or url.scheme != "http"
        or url.port != 8080
        or url.path != "/api"
        or url.username is not None
        or url.password is not None
        or url.query
        or url.fragment
    ):
        raise OSError("invalid split admin endpoint")
    admin = "mcp-admin." + name.removeprefix("bridge-agent.")

    def resolve(host) -> str:
        addresses = {info[4][0] for info in socket.getaddrinfo(host, None, type=socket.SOCK_STREAM)}
        if len(addresses) != 1:
            raise OSError("split admin requires one IPv4 address")
        address = str(addresses.pop())
        ip = ipaddress.ip_address(address)
        if ip.version != 4 or ip.is_loopback or ip.is_unspecified or ip.is_multicast:
            raise OSError("unsafe split admin address")
        return address

    address, peer = resolve(admin), resolve(name)
    if address == peer:
        raise OSError("split admin and bridge addresses must differ")
    # Linux split containers: require a single local NIC for this pinned IP.
    import fcntl

    devices = []
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
        for _, device in socket.if_nameindex():
            try:
                data = getattr(fcntl, "ioctl")(probe.fileno(), 0x8915, struct.pack("256s", device.encode()))
            except OSError:
                continue  # Interfaces without an IPv4 address.
            if socket.inet_ntoa(data[20:24]) == address:
                devices.append(device)
    if len(devices) != 1:
        raise OSError("split admin IP must belong to exactly one local interface")
    return address, admin, peer, devices[0]


def start_admin(bridge_token: str, mcp_token: str | None, *, port: int = 8091):
    if not mcp_token or len(bridge_token) < 16 or hmac.compare_digest(bridge_token.encode(), mcp_token.encode()):
        raise ValueError("MCP admin requires a bridge token distinct from the MCP bearer; configure WHATSAPP_MCP_TOKEN")
    from http_auth import AuthStateUnavailableError, verify_static_token

    try:
        if verify_static_token(bridge_token, mcp_token):
            raise ValueError("MCP admin requires a bridge token distinct from every effective MCP bearer")
    except AuthStateUnavailableError:
        raise ValueError("MCP admin cannot establish effective credential separation") from None
    address, hostname, peer, device = admin_endpoint()

    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(5)

        def log_message(self, format, *args):
            pass  # No credentials, URLs or request headers in logs.

        def do_GET(self):
            host = f"{hostname}:{cast(HTTPServer, self.server).server_port}"
            headers = self.headers
            auth = headers.get_all("Authorization", [])
            if (
                (
                    self.client_address[0] != peer
                    if peer
                    else not ipaddress.ip_address(self.client_address[0]).is_loopback
                )
                or len(auth) != 1
                or not hmac.compare_digest(auth[0].encode(), f"Bearer {bridge_token}".encode())
            ):
                self.reply(401, {"error": "unauthorized"})
            elif headers.get_all("Host", []) != [host] or headers.get_all("Origin", []) not in ([], [f"http://{host}"]):
                self.reply(403, {"error": "host_origin_refused"})
            elif not self.separated():
                return  # A later runtime rotation can revoke admin immediately.
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

        def separated(self):
            try:
                if verify_static_token(bridge_token, mcp_token):
                    self.reply(401, {"error": "unauthorized"})
                    return False
            except AuthStateUnavailableError:
                self.reply(503, {"error": "authentication_state_unavailable"})
                return False
            return True

        def reply(self, status, payload):
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
            self.close_connection = True

    server = HTTPServer((address, port), Handler, bind_and_activate=False)
    try:
        if device:
            # Reject packets arriving on another NIC for the agent IP. No extra
            # capabilities; if the kernel refuses, keep optional admin closed.
            server.socket.setsockopt(socket.SOL_SOCKET, getattr(socket, "SO_BINDTODEVICE"), device.encode() + b"\0")
        server.server_bind()
        server.server_activate()
    except OSError:
        server.server_close()
        raise
    thread = threading.Thread(target=server.serve_forever, name="mcp-admin", daemon=True)
    thread.start()
    return server


def install_admin(transport: str, port: int):
    if transport == "stdio" or not os.getenv("WHATSAPP_OPERATOR_BIND", "").strip():
        return None
    if port == 8091:
        raise ValueError("WHATSAPP_MCP_PORT: 8091 is reserved for the operator admin listener")
    from http_auth import resolve_http_token
    from whatsapp import _read_bridge_token

    bridge_token = _read_bridge_token() or ""
    token, _ = resolve_http_token(
        os.getenv("WHATSAPP_MCP_TOKEN"), os.getenv("WHATSAPP_MCP_HOST", "127.0.0.1"), _read_bridge_token
    )
    try:
        return start_admin(bridge_token, token)
    except ValueError:
        logging.getLogger("whatsapp_mcp").warning(
            "MCP admin disabled: set WHATSAPP_MCP_TOKEN distinct from the bridge token and clear or expire "
            "any saved bridge-token MCP bearer; data plane remains available"
        )
    except OSError:
        logging.getLogger("whatsapp_mcp").warning(
            "MCP admin disabled: cannot bind private endpoint on port 8091; check split agent address/interface "
            "or free the admin port; data plane remains available"
        )
    return None
