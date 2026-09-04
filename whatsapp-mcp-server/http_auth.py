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
import threading
import time
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
# Requests per minute per client when a token is enforced and nothing is configured.
DEFAULT_RATE_LIMIT_PER_MINUTE = 120
DEFAULT_MAX_BODY_BYTES = 4 * 1024 * 1024  # the SDK's own default


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


def resolve_rate_limit(value: str | None, token_enforced: bool) -> int:
    """Parse WHATSAPP_MCP_RATE_LIMIT (requests per minute per client). 0 disables.

    Unset → DEFAULT_RATE_LIMIT_PER_MINUTE when a bearer token is enforced (the
    endpoint is reachable from beyond loopback), 0 otherwise.
    """
    raw = (value or "").strip().lower()
    if not raw:
        return DEFAULT_RATE_LIMIT_PER_MINUTE if token_enforced else 0
    if raw in DISABLE_VALUES:
        return 0
    try:
        limit = int(raw)
    except ValueError:
        raise ValueError(
            f"Invalid WHATSAPP_MCP_RATE_LIMIT={value!r}; must be an integer (requests/minute) or off"
        ) from None
    if limit < 0:
        raise ValueError(f"Invalid WHATSAPP_MCP_RATE_LIMIT={value!r}; must be >= 0")
    return limit


def resolve_max_body_bytes(value: str | None) -> int:
    """Parse WHATSAPP_MCP_MAX_BODY_BYTES (default 4 MiB, the SDK default)."""
    raw = (value or "").strip()
    if not raw:
        return DEFAULT_MAX_BODY_BYTES
    try:
        size = int(raw)
    except ValueError:
        raise ValueError(f"Invalid WHATSAPP_MCP_MAX_BODY_BYTES={value!r}; must be an integer") from None
    if size < 1024:
        raise ValueError(f"Invalid WHATSAPP_MCP_MAX_BODY_BYTES={value!r}; must be at least 1024")
    return size


def client_key(scope: Scope) -> str:
    """Identify the caller: first X-Forwarded-For hop (Tailscale Serve / a proxy
    set it) or the socket peer."""
    for name, value in scope.get("headers", []):
        if name.lower() == b"x-forwarded-for":
            first = value.decode("latin-1").split(",")[0].strip()
            if first:
                return first
    client = scope.get("client")
    return client[0] if client else "unknown"


class RateLimitMiddleware:
    """Pure-ASGI token bucket per client: `per_minute` requests sustained, a burst
    of the same size, 429 + Retry-After when exhausted. Sits in front of the
    bearer check so credential guessing is throttled too."""

    def __init__(self, app: ASGIApp, per_minute: int, clock: Callable[[], float] = time.monotonic):
        if per_minute <= 0:
            raise ValueError("RateLimitMiddleware requires per_minute > 0")
        self.app = app
        self.capacity = float(per_minute)
        self.refill_per_second = per_minute / 60.0
        self._clock = clock
        self._buckets: dict[str, tuple[float, float]] = {}  # key -> (tokens, last_refill)
        self._lock = threading.Lock()
        self._last_prune = clock()

    def _take(self, key: str) -> float:
        """Consume one token; return 0 when allowed, else seconds until one is available."""
        now = self._clock()
        with self._lock:
            tokens, last = self._buckets.get(key, (self.capacity, now))
            tokens = min(self.capacity, tokens + (now - last) * self.refill_per_second)
            if tokens >= 1.0:
                self._buckets[key] = (tokens - 1.0, now)
                allowed = 0.0
            else:
                self._buckets[key] = (tokens, now)
                allowed = (1.0 - tokens) / self.refill_per_second
            if now - self._last_prune > 300:
                self._last_prune = now
                stale = [k for k, (t, ts) in self._buckets.items() if t >= self.capacity - 0.01 or now - ts > 600]
                for k in stale:
                    self._buckets.pop(k, None)
        return allowed

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        wait = self._take(client_key(scope))
        if wait <= 0:
            await self.app(scope, receive, send)
            return
        retry_after = max(1, int(wait + 0.999))
        body = b'{"error":"rate_limited","message":"Too many requests; slow down"}'
        await send(
            {
                "type": "http.response.start",
                "status": 429,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"content-length", str(len(body)).encode("ascii")),
                    (b"retry-after", str(retry_after).encode("ascii")),
                    (b"cache-control", b"no-store"),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body})
