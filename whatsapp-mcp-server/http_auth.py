"""Static bearer-token authentication for the http/sse MCP transports.

The MCP SDK's own auth hooks are OAuth-shaped (issuer URLs, resource metadata,
token introspection). For a single-account server fronted by Tailscale or a
reverse proxy a shared secret is the right size: every request to the MCP
endpoints must carry ``Authorization: Bearer <WHATSAPP_MCP_TOKEN>``, compared
in constant time. Unset token = no auth, which is today's behaviour and is
only reasonable on loopback or a tailnet-only listener.

This mirrors what the Go bridge does for its own REST API (see auth.go).
"""

from __future__ import annotations

import secrets
from collections.abc import Awaitable, Callable, MutableMapping
from typing import Any

Scope = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[MutableMapping[str, Any]]]
Send = Callable[[MutableMapping[str, Any]], Awaitable[None]]
ASGIApp = Callable[[Scope, Receive, Send], Awaitable[None]]

MIN_TOKEN_LENGTH = 16
REALM = "whatsapp-mcp"
LOOPBACK_HOSTS = ("127.0.0.1", "localhost", "::1")
# WHATSAPP_MCP_TOKEN values that mean "no auth, and do not fall back to the bridge token".
DISABLE_VALUES = ("off", "none", "disabled")


def resolve_mcp_token(value: str | None) -> str | None:
    """Parse WHATSAPP_MCP_TOKEN. Unset/blank disables auth; short tokens are rejected."""
    token = (value or "").strip()
    if not token:
        return None
    if len(token) < MIN_TOKEN_LENGTH:
        raise ValueError(f"WHATSAPP_MCP_TOKEN is too short (need at least {MIN_TOKEN_LENGTH} characters)")
    return token


def resolve_http_token(
    explicit: str | None,
    host: str,
    read_bridge_token: Callable[[], str | None],
) -> tuple[str | None, str]:
    """Decide which bearer token protects the http/sse transports.

    Returns (token, source). Precedence:
    1. WHATSAPP_MCP_TOKEN set → that token (source "WHATSAPP_MCP_TOKEN").
       The values off/none/disabled turn auth off explicitly (source "disabled").
    2. Loopback bind → no token (source "loopback").
    3. Non-loopback bind → the bridge token (env or .bridge-token file), so a
       deployment that already has one secret does not need a second
       (source "bridge token").
    4. Otherwise no token (source "none"); main.py warns loudly.
    """
    value = (explicit or "").strip()
    if value.lower() in DISABLE_VALUES:
        return None, "disabled"
    if value:
        return resolve_mcp_token(value), "WHATSAPP_MCP_TOKEN"
    if host in LOOPBACK_HOSTS:
        return None, "loopback"
    bridge = (read_bridge_token() or "").strip()
    if len(bridge) >= MIN_TOKEN_LENGTH:
        return bridge, "bridge token"
    return None, "none"


def _bearer_from_headers(headers: list[tuple[bytes, bytes]]) -> str | None:
    for name, value in headers:
        if name.lower() == b"authorization":
            text = value.decode("latin-1").strip()
            scheme, _, credentials = text.partition(" ")
            if scheme.lower() == "bearer" and credentials.strip():
                return credentials.strip()
            return None
    return None


def token_matches(presented: str | None, expected: str) -> bool:
    if not presented:
        return False
    return secrets.compare_digest(presented.encode("utf-8"), expected.encode("utf-8"))


class BearerTokenMiddleware:
    """Pure-ASGI middleware: 401 unless the request carries the expected bearer token.

    Applies to every HTTP request of the wrapped app (the MCP endpoint and, for
    the legacy SSE transport, its message path). WebSocket/lifespan scopes pass
    through untouched. The response body is JSON so MCP clients get a readable
    error instead of an HTML page.
    """

    def __init__(self, app: ASGIApp, token: str):
        if not token:
            raise ValueError("BearerTokenMiddleware requires a non-empty token")
        self.app = app
        self._token = token

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        if token_matches(_bearer_from_headers(scope.get("headers", [])), self._token):
            await self.app(scope, receive, send)
            return
        body = b'{"error":"unauthorized","message":"Missing or invalid bearer token"}'
        await send(
            {
                "type": "http.response.start",
                "status": 401,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"content-length", str(len(body)).encode("ascii")),
                    (b"www-authenticate", f'Bearer realm="{REALM}"'.encode("ascii")),
                    (b"cache-control", b"no-store"),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body})
