"""Opt-in OAuth resource server; credentials never leave the authorization path.

SDK TokenVerifier/AuthSettings retain transport principal/session binding. This
outer layer adds bounded discovery, static-token coexistence, outage responses,
and HTTP scope challenges before a tool can execute.
"""

from __future__ import annotations

import asyncio
import hashlib
import ipaddress
import json
import math
import os
import time
from collections import OrderedDict
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Annotated, Any
from urllib.parse import urlsplit, urlunsplit

import httpx
import jwt
from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.auth.provider import AccessToken, TokenVerifier
from mcp.server.auth.routes import create_protected_resource_routes
from mcp.server.auth.settings import AuthSettings
from pydantic import AnyHttpUrl, TypeAdapter, UrlConstraints
from starlette.routing import Route, Router

from http_auth import (
    ASGIApp,
    AuthStateUnavailableError,
    RateLimitMiddleware,
    Receive,
    Scope,
    Send,
    _bearer_from_headers,
    authentication_unavailable_response,
    client_key,
    verify_static_token,
)

MAX_JSON_BYTES = 256 * 1024
MAX_TOKEN_BYTES = 16 * 1024
JWKS_TTL = 300
INTROSPECTION_TTL = 60
MAX_CACHE_ENTRIES = 1024
CLOCK_LEEWAY = 60
MAX_INTROSPECTIONS = 4
ALGORITHMS = ("RS256", "ES256")
CLIENT_ID_FILE_ENV = "WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID_FILE"
SECRET_FILE_ENV = "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET_FILE"
URL_ADAPTER = TypeAdapter(Annotated[AnyHttpUrl, UrlConstraints(preserve_empty_path=True)])


class AuthorizationUnavailableError(RuntimeError):
    """The authorization service cannot supply a bounded, valid response."""


class VerificationRateLimitError(RuntimeError):
    def __init__(self, wait: float):
        self.wait = wait
        super().__init__("Credential verification is rate limited")


def _url(value: str, name: str) -> str:
    if not isinstance(value, str):
        raise ValueError(f"{name} must be a URL")
    try:
        parts = urlsplit(value)
        loopback = parts.hostname == "localhost"
        if parts.hostname and not loopback:
            try:
                loopback = ipaddress.ip_address(parts.hostname).is_loopback
            except ValueError:
                pass
        valid = (parts.scheme == "https" or parts.scheme == "http" and loopback) and parts.hostname
        valid = valid and not parts.username and not parts.password and not parts.fragment
        parts.port  # validate without echoing the configured URL
    except ValueError:
        valid = False
    if not valid or any(ord(c) < 33 or ord(c) > 126 or c in '\\"' for c in value):
        raise ValueError(f"{name} must be an HTTPS URL without credentials (HTTP allowed on loopback)")
    return value


def _secret(env: Any, name: str, file_name: str) -> str:
    value, file = env.get(name, "").strip(), env.get(file_name, "").strip()
    if value and file:
        raise ValueError(f"Set {name} or {name}_FILE, never both")
    if file:
        try:
            path = Path(file)
            if path.is_symlink() or not path.is_file() or path.stat().st_size > 4096:
                raise ValueError()
            if os.name != "nt" and path.stat().st_mode & 0o077:
                raise ValueError()
            with path.open("rb") as source:
                raw = source.read(4097)
            if len(raw) > 4096:
                raise ValueError()
            value = raw.decode("utf-8").strip()
        except (OSError, UnicodeError, ValueError):
            raise ValueError(f"{name}_FILE must be a private regular UTF-8 file of at most 4096 bytes") from None
    return value


@dataclass(frozen=True)
class OAuthConfig:
    issuer: str
    audience: str
    jwks_url: str | None = None
    scopes: tuple[str, ...] = ()
    subjects: frozenset[str] = frozenset()
    read_scope: str = "whatsapp:read"
    send_scope: str = "whatsapp:send"
    introspection_url: str | None = None
    client_id: str = ""
    client_secret: str = field(default="", repr=False)

    @property
    def metadata_url(self) -> str:
        parts = urlsplit(self.audience)
        return urlunsplit((parts.scheme, parts.netloc, "/.well-known/oauth-protected-resource/mcp", "", ""))

    @property
    def supported_scopes(self) -> list[str]:
        return sorted(set((*self.scopes, self.read_scope, self.send_scope)))

    def auth_settings(self) -> AuthSettings:
        return AuthSettings(
            issuer_url=URL_ADAPTER.validate_python(self.issuer),
            resource_server_url=URL_ADAPTER.validate_python(self.audience),
            required_scopes=list(self.scopes),
            validate_token_resource=True,
        )


def load_oauth_config(env: Any = None) -> OAuthConfig | None:
    env = os.environ if env is None else env
    issuer = env.get("WHATSAPP_MCP_OAUTH_ISSUER", "").strip()
    if not issuer:
        return None  # do not parse any other knob when OAuth is off
    issuer = _url(issuer, "WHATSAPP_MCP_OAUTH_ISSUER")
    audience = env.get("WHATSAPP_MCP_OAUTH_AUDIENCE", "").strip() or env.get("WHATSAPP_PUBLIC_URL", "").strip()
    audience = _url(audience, "WHATSAPP_MCP_OAUTH_AUDIENCE / WHATSAPP_PUBLIC_URL")
    if urlsplit(audience).path != "/mcp" or urlsplit(audience).query or urlsplit(issuer).query:
        raise ValueError("OAuth audience must be the canonical /mcp URL; issuer and audience cannot have a query")
    jwks = env.get("WHATSAPP_MCP_OAUTH_JWKS_URL", "").strip() or None
    introspection = env.get("WHATSAPP_MCP_OAUTH_INTROSPECTION_URL", "").strip() or None
    if jwks:
        _url(jwks, "WHATSAPP_MCP_OAUTH_JWKS_URL")
    if introspection:
        _url(introspection, "WHATSAPP_MCP_OAUTH_INTROSPECTION_URL")
    if jwks and introspection:
        raise ValueError("Choose JWKS or introspection, never both")
    client_id = _secret(env, "WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID", CLIENT_ID_FILE_ENV)
    secret = _secret(env, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET", SECRET_FILE_ENV)
    if introspection and (not client_id or not secret):
        raise ValueError("OAuth introspection requires CLIENT_ID and SECRET (or their _FILE alternatives)")
    scopes = tuple(env.get("WHATSAPP_MCP_OAUTH_SCOPES", "").replace(",", " ").split())
    read = env.get("WHATSAPP_MCP_OAUTH_READ_SCOPE", "").strip() or "whatsapp:read"
    send = env.get("WHATSAPP_MCP_OAUTH_SEND_SCOPE", "").strip() or "whatsapp:send"
    for scope in (*scopes, read, send):
        if any(ord(c) < 33 or ord(c) > 126 or c in '\\"' for c in scope):
            raise ValueError("OAuth scope names must be printable tokens without quotes or backslashes")
    return OAuthConfig(
        issuer,
        audience,
        jwks,
        scopes,
        frozenset(s.strip() for s in env.get("WHATSAPP_MCP_OAUTH_SUBJECTS", "").split(",") if s.strip()),
        read,
        send,
        introspection,
        client_id,
        secret,
    )


class OAuthTokenVerifier(TokenVerifier):
    @staticmethod
    def validate_static_token(static_token: str | None) -> None:
        if static_token and static_token.count(".") == 2:
            raise ValueError("WHATSAPP_MCP_TOKEN must not have two dots when OAuth is enabled")

    def __init__(self, config: OAuthConfig, static_token: str | None = None):
        self.validate_static_token(static_token)
        self.config = config
        self.static_token = static_token
        self._lock = asyncio.Lock()
        self._keys: list[dict[str, Any]] = []
        self._keys_until = 0.0
        self._retry_after = 0.0
        self._refresh_after = 0.0
        self._introspection: OrderedDict[str, tuple[float, dict[str, Any]]] = OrderedDict()
        self._introspection_flights: dict[str, asyncio.Task[dict[str, Any]]] = {}

    def accepts_static(self, token: str) -> bool:
        """Single hook for later runtime rotation; never classify a JWT as static."""
        return token.count(".") != 2 and verify_static_token(token, self.static_token)

    async def _fetch(self, url: str, data: dict[str, str] | None = None) -> dict[str, Any]:
        try:
            return await asyncio.wait_for(self._fetch_json(url, data), timeout=8)
        except TimeoutError:
            raise AuthorizationUnavailableError("Authorization service unavailable") from None

    async def _fetch_json(self, url: str, data: dict[str, str] | None = None) -> dict[str, Any]:
        try:
            async with httpx.AsyncClient(
                trust_env=False,
                follow_redirects=False,
                timeout=httpx.Timeout(5, connect=2),
                headers={"Accept-Encoding": "identity"},
            ) as client:
                kwargs: dict[str, Any] = {}
                if data is not None:
                    kwargs = {"data": data, "auth": (self.config.client_id, self.config.client_secret)}
                async with client.stream("POST" if data is not None else "GET", url, **kwargs) as response:
                    if response.status_code != 200:
                        raise AuthorizationUnavailableError("Authorization service unavailable")
                    if response.headers.get("Content-Encoding", "identity").strip().lower() != "identity":
                        raise AuthorizationUnavailableError("Encoded authorization response refused")
                    body = bytearray()
                    async for chunk in response.aiter_raw():
                        if len(body) + len(chunk) > MAX_JSON_BYTES:
                            raise AuthorizationUnavailableError("Authorization response exceeds its limit")
                        body.extend(chunk)
                    payload = json.loads(body)
                    if not isinstance(payload, dict):
                        raise AuthorizationUnavailableError("Invalid authorization response")
                    return payload
        except (httpx.HTTPError, httpx.InvalidURL, ValueError, RecursionError):
            raise AuthorizationUnavailableError("Authorization service unavailable") from None

    async def _jwks(
        self, before_fetch: Callable[[], None] | None = None, kid: str | None = None
    ) -> list[dict[str, Any]]:
        now = time.monotonic()
        if now < self._keys_until and (
            not kid or any(k.get("kid") == kid for k in self._keys) or now < self._refresh_after
        ):
            return self._keys  # cached known keys never wait for rotation I/O
        async with self._lock:
            now = time.monotonic()
            if now < self._keys_until:
                if not kid or any(k.get("kid") == kid for k in self._keys) or now < self._refresh_after:
                    return self._keys
            if now < self._retry_after:
                raise AuthorizationUnavailableError("Authorization service unavailable")
            if before_fetch:
                before_fetch()
            self._refresh_after = now + 5
            try:
                url = self.config.jwks_url
                if not url:
                    parts = urlsplit(self.config.issuer)
                    # RFC 8414 inserts the well-known segment before the issuer path.
                    metadata_url = urlunsplit(
                        (
                            parts.scheme,
                            parts.netloc,
                            "/.well-known/oauth-authorization-server" + parts.path.rstrip("/"),
                            "",
                            "",
                        )
                    )
                    try:
                        metadata = await self._fetch(metadata_url)
                    except AuthorizationUnavailableError:
                        metadata = await self._fetch(
                            self.config.issuer.rstrip("/") + "/.well-known/openid-configuration"
                        )
                    if metadata.get("issuer") != self.config.issuer:
                        raise AuthorizationUnavailableError("Issuer metadata mismatch")
                    url = _url(metadata.get("jwks_uri", ""), "jwks_uri")
                payload = await self._fetch(url)
                keys = payload.get("keys")
                if (
                    not isinstance(keys, list)
                    or not keys
                    or len(keys) > 64
                    or not all(isinstance(k, dict) for k in keys)
                ):
                    raise AuthorizationUnavailableError("Invalid JWKS")
                self._keys, self._keys_until = keys, now + JWKS_TTL
                return keys
            except (AuthorizationUnavailableError, ValueError):
                self._retry_after = now + 5
                raise AuthorizationUnavailableError("Authorization service unavailable") from None

    async def _inspect(self, token: str, before_fetch: Callable[[], None] | None = None) -> dict[str, Any]:
        key = hashlib.sha256(token.encode()).hexdigest()
        cached = self._introspection.get(key)
        if cached and cached[0] > time.monotonic():
            return cached[1]  # never waits behind another token's remote I/O
        self._introspection.pop(key, None)
        flight = self._introspection_flights.get(key)
        if flight is None:
            if len(self._introspection_flights) >= MAX_INTROSPECTIONS:
                raise AuthorizationUnavailableError("Authorization service busy")
            # No await between lookup/insertion: single flight on this event loop.
            flight = asyncio.create_task(self._inspect_fetch(token, key, before_fetch))
            self._introspection_flights[key] = flight
            flight.add_done_callback(lambda _done: self._introspection_flights.pop(key, None))
        return await asyncio.shield(flight)

    async def _inspect_fetch(self, token: str, key: str, before_fetch: Callable[[], None] | None) -> dict[str, Any]:
        if before_fetch:
            before_fetch()
        payload = await self._fetch(self.config.introspection_url or "", {"token": token})
        if payload.get("active") is True and self._access(token, payload) is not None:
            self._introspection[key] = (time.monotonic() + INTROSPECTION_TTL, payload)
            while len(self._introspection) > MAX_CACHE_ENTRIES:
                self._introspection.popitem(last=False)
        return payload

    def _access(self, token: str, claims: dict[str, Any]) -> AccessToken | None:
        sub, aud, exp, nbf, iat = (claims.get(k) for k in ("sub", "aud", "exp", "nbf", "iat"))
        now = time.time()
        token_type = claims.get("token_type")
        # RFC 7662 token_type names the authorization scheme (usually Bearer).
        # Retain access_token compatibility, but never accept refresh tokens.
        if token_type is not None and (
            not isinstance(token_type, str) or token_type.lower() not in {"bearer", "access_token"}
        ):
            return None
        if not isinstance(sub, str) or not sub or len(sub) > 1024:
            return None
        if self.config.subjects and sub not in self.config.subjects:
            return None
        client_id = claims.get("client_id", claims.get("azp", sub))
        if not isinstance(client_id, str) or not client_id or len(client_id) > 1024:
            return None
        # RFC 7662 permits absent exp/iss; JWT verification requires both.
        if "iss" in claims and claims["iss"] != self.config.issuer:
            return None
        if aud != self.config.audience and not (isinstance(aud, list) and self.config.audience in aud):
            return None
        for value in (exp, nbf, iat):
            if value is not None and (
                isinstance(value, bool)
                or not isinstance(value, (int, float))
                or not 0 <= value <= 253402300799
                or not math.isfinite(value)
            ):
                return None
        if (
            exp is not None
            and exp <= now - CLOCK_LEEWAY
            or any(v is not None and v > now + CLOCK_LEEWAY for v in (nbf, iat))
        ):
            return None
        scope = claims.get("scope", "")
        if not isinstance(scope, str):
            return None
        scopes = scope.split()
        if not set(self.config.scopes).issubset(scopes):
            return None
        return AccessToken(
            token=token,
            client_id=client_id,
            subject=sub,
            scopes=scopes,
            # Native SDK expiry checks must observe the same clock-skew window.
            expires_at=int(exp + CLOCK_LEEWAY) if exp is not None else None,
            resource=self.config.audience,
            claims={
                "iss": self.config.issuer,
                "read_scope": self.config.read_scope,
                "send_scope": self.config.send_scope,
            },
        )

    async def verify_token(self, token: str, before_fetch: Callable[[], None] | None = None) -> AccessToken | None:
        if len(token.encode()) > MAX_TOKEN_BYTES:
            return None
        if token.count(".") != 2 and before_fetch:
            before_fetch()  # Static registry reads are verification I/O too.
        static_unavailable = None
        try:
            is_static = token.count(".") != 2 and await asyncio.to_thread(self.accepts_static, token)
        except AuthStateUnavailableError as exc:
            static_unavailable = exc
            is_static = False
        if is_static:
            return AccessToken(
                token=token,
                client_id="static",
                subject="static",
                scopes=self.config.supported_scopes,
                resource=self.config.audience,
                claims={"iss": "urn:whatsapp-mcp:static", "static": True},
            )
        if self.config.introspection_url:
            payload = await self._inspect(token, before_fetch)
            access = self._access(token, payload) if payload.get("active") is True else None
            if access is None and static_unavailable is not None:
                raise static_unavailable
            return access
        if static_unavailable is not None:
            raise static_unavailable
        try:
            header = jwt.get_unverified_header(token)
            alg = header.get("alg")
            if alg not in ALGORITHMS or not isinstance(header.get("kid"), str) or header.get("crit"):
                return None
            keys = await self._jwks(before_fetch, header["kid"])
            candidates = [
                k
                for k in keys
                if k.get("kid") == header["kid"]
                and k.get("use", "sig") == "sig"
                and k.get("alg", alg) == alg
                and k.get("kty") == ("RSA" if alg == "RS256" else "EC")
                and "verify" in k.get("key_ops", ["verify"])
            ]
            if len(candidates) != 1:
                return None
            key = jwt.PyJWK.from_dict(candidates[0], algorithm=alg).key
            claims = jwt.decode(
                token,
                key,
                algorithms=[alg],
                issuer=self.config.issuer,
                audience=self.config.audience,
                leeway=CLOCK_LEEWAY,
                options={"require": ["iss", "sub", "exp", "aud"]},
            )
            return self._access(token, claims)
        except (jwt.PyJWTError, ValueError, TypeError, KeyError, OverflowError):
            return None


def required_tool_scope(config: OAuthConfig, name: str) -> str:
    from tool_policy import mutating_tools

    return config.send_scope if name in mutating_tools() else config.read_scope


class OAuthSDKAvailabilityMiddleware:
    """Turn SDK authentication outages into the resource-server 503 envelope."""

    def __init__(self, app: ASGIApp, reject: Callable[[Send, int, list[str] | None], Awaitable[None]]) -> None:
        self.app = app
        self.reject = reject

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        response_started = False

        async def tracked_send(message: Any) -> None:
            nonlocal response_started
            if message["type"] == "http.response.start":
                response_started = True
            await send(message)

        try:
            await self.app(scope, receive, tracked_send)
        except AuthStateUnavailableError as exc:
            if scope.get("type") != "http" or response_started:
                raise
            await authentication_unavailable_response(send, str(exc))
        except AuthorizationUnavailableError:
            if scope.get("type") != "http" or response_started:
                raise
            await self.reject(send, 503, None)


class OAuthSDKScopeMiddleware:
    """Check operation scopes after the SDK authenticates the current token."""

    def __init__(self, app: ASGIApp, reject: Callable[[Send, int, list[str] | None], Awaitable[None]]) -> None:
        self.app = app
        self.reject = reject

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        access = get_access_token()
        required = scope.get("oauth_required_scopes", [])
        if scope.get("type") == "http" and access is not None and not set(required).issubset(access.scopes):
            await self.reject(send, 403, required)
            return
        await self.app(scope, receive, send)


class OAuthMiddleware:
    def __init__(
        self,
        app: ASGIApp,
        verifier: OAuthTokenVerifier,
        per_minute: int = 0,
        max_body: int = 4 * 1024 * 1024,
        trusted_proxies: Any = (),
    ):
        self.app, self.verifier, self.max_body = app, verifier, max_body
        self.limiter = RateLimitMiddleware(app, per_minute, trusted_proxies=trusted_proxies) if per_minute else None
        self.peer_limiter = (
            RateLimitMiddleware(app, per_minute, trusted_proxies=trusted_proxies) if per_minute else None
        )
        routes = create_protected_resource_routes(
            URL_ADAPTER.validate_python(verifier.config.audience),
            [URL_ADAPTER.validate_python(verifier.config.issuer)],
            verifier.config.supported_scopes,
        )
        self.metadata_app = Router(
            routes=[
                *routes,
                Route("/.well-known/oauth-protected-resource", endpoint=routes[0].endpoint, methods=["GET", "OPTIONS"]),
            ]
        )

    def _guard_fetch(self, scope: Scope) -> None:
        if self.peer_limiter and not scope.get("oauth_verification_reserved"):
            key = client_key(scope, self.peer_limiter._trusted_proxies)
            wait = self.peer_limiter._take(key)
            if wait > 0:
                raise VerificationRateLimitError(wait)
            scope["oauth_verification_reserved"] = key

    async def _limited(self, scope: Scope, send: Send, peer: bool = False) -> bool:
        if peer and scope.pop("oauth_verification_reserved", None):
            return False  # already reserved before remote verification
        limiter = self.peer_limiter if peer else self.limiter
        if not limiter:
            return False
        wait = limiter._take(client_key(scope, limiter._trusted_proxies))
        if wait <= 0:
            return False
        await self._rate_error(send, wait)
        return True

    async def _rate_error(self, send: Send, wait: float) -> None:
        await send(
            {
                "type": "http.response.start",
                "status": 429,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"cache-control", b"no-store"),
                    (b"retry-after", str(max(1, math.ceil(wait))).encode()),
                ],
            }
        )
        await send({"type": "http.response.body", "body": b'{"error":"rate_limited"}'})

    async def _error(self, send: Send, status: int, required: list[str] | None = None) -> None:
        challenge = f'Bearer resource_metadata="{self.verifier.config.metadata_url}"'
        if required:
            challenge += f', error="insufficient_scope", scope="{" ".join(required)}"'
        body = json.dumps(
            {
                "error": "insufficient_scope"
                if required
                else "authorization_unavailable"
                if status == 503
                else "unauthorized"
            }
        ).encode()
        await send(
            {
                "type": "http.response.start",
                "status": status,
                "headers": [
                    (b"content-type", b"application/json"),
                    (b"cache-control", b"no-store"),
                    (b"www-authenticate", challenge.encode("ascii")),
                ],
            }
        )
        await send({"type": "http.response.body", "body": body})

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        config = self.verifier.config
        if scope.get("path") in ("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"):
            await self.metadata_app(scope, receive, send)
            return
        presented = _bearer_from_headers(scope.get("headers", []))
        try:
            access = (
                await self.verifier.verify_token(presented, lambda: self._guard_fetch(scope)) if presented else None
            )
        except AuthStateUnavailableError as exc:
            if not await self._limited(scope, send, peer=True):
                await authentication_unavailable_response(send, str(exc))
            return
        except VerificationRateLimitError as exc:
            await self._rate_error(send, exc.wait)
            return
        except AuthorizationUnavailableError:
            if not await self._limited(scope, send, peer=True):
                await self._error(send, 503)
            return
        if access is None:
            if not await self._limited(scope, send, peer=True):
                await self._error(send, 401)
            return
        reservation = scope.pop("oauth_verification_reserved", None)
        if reservation and self.peer_limiter:
            self.peer_limiter.refund(reservation)
        if not (access.claims or {}).get("static"):
            scope["oauth_subject"] = access.subject
        if await self._limited(scope, send):
            return
        # SDK's own middleware revalidates and binds the principal to each session.
        # Preflight tool scopes produces an HTTP challenge rather than a JSON-RPC error.
        if not (access.claims or {}).get("static"):
            required: set[str] = set()
            if scope.get("path") == "/upload":
                required.add(config.send_scope)
            elif scope.get("method") == "POST":
                body = bytearray()
                while True:
                    message = await receive()
                    if message["type"] == "http.disconnect":
                        return
                    body.extend(message.get("body", b""))
                    if len(body) > self.max_body:
                        await send(
                            {
                                "type": "http.response.start",
                                "status": 413,
                                "headers": [(b"content-type", b"application/json")],
                            }
                        )
                        await send({"type": "http.response.body", "body": b'{"error":"body_too_large"}'})
                        return
                    if not message.get("more_body", False):
                        break
                try:
                    payload = json.loads(body)
                    requests = payload if isinstance(payload, list) else [payload]
                    for request in requests:
                        if not isinstance(request, dict):
                            continue
                        method = request.get("method", "")
                        if method == "tools/call":
                            params = request.get("params")
                            name = params.get("name", "") if isinstance(params, dict) else ""
                            if isinstance(name, str):
                                required.add(required_tool_scope(config, name))
                        elif isinstance(method, str) and method.startswith(("resources/", "prompts/")):
                            required.add(config.read_scope)
                except (ValueError, TypeError, RecursionError):
                    pass  # SDK supplies the protocol error
                original_receive = receive
                pending = [{"type": "http.request", "body": bytes(body), "more_body": False}]

                async def replay() -> Any:
                    return pending.pop(0) if pending else await original_receive()

                receive = replay
            if not required.issubset(access.scopes):
                await self._error(send, 403, sorted(set(config.scopes) | required))
                return
            # A body can outlast the cache: SDK authentication may then obtain
            # newer claims. Its inner guard must authorize that exact snapshot.
            scope["oauth_required_scopes"] = sorted(set(config.scopes) | required)
        await self.app(scope, receive, send)
