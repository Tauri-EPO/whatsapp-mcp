"""Bound stateful HTTP sessions while leaving opening-request validation to the SDK."""

import hashlib
import json
import logging
import weakref
from dataclasses import dataclass, field
from time import monotonic

from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser, authorization_context
from mcp.server.streamable_http import MCP_SESSION_ID_HEADER, StreamableHTTPServerTransport
from mcp.server.streamable_http_manager import StreamableHTTPSessionManager, _error_response
from mcp_types import INTERNAL_ERROR
from starlette.requests import Request
from starlette.types import Message, Receive, Scope, Send

from http_auth import _bearer_from_headers

DEFAULT_MAX_SESSIONS = 256
FOREIGN_IDLE_GRACE_S = 1800
logger = logging.getLogger("whatsapp_mcp.sessions")


def _owner(scope: Scope, client_name: str | None) -> bytes:
    user = scope.get("user")
    claims = (user.access_token.claims or {}) if isinstance(user, AuthenticatedUser) else {}
    if isinstance(user, AuthenticatedUser) and not claims.get("static"):
        identity = ["oauth", claims.get("iss"), user.access_token.subject, user.access_token.client_id]
    else:
        identity = ["bearer", _bearer_from_headers(scope["headers"])]
    return hashlib.sha256(json.dumps([*identity, client_name]).encode()).digest()


def _client_name(body: bytes) -> str | None:
    # Identity hint only: the SDK still validates and handles the request.
    try:
        name = json.loads(body)["params"]["clientInfo"]["name"]
        return name if isinstance(name, str) else None
    except Exception:
        return None


@dataclass
class _Activity:
    owner: bytes
    active: int = 1  # Reserve the slot while opening/forwarding the SDK response.
    last_active: float = field(default_factory=lambda: monotonic())


class _TrackedRequest:
    def __init__(self, transport: StreamableHTTPServerTransport, owner: bytes):
        self.transport = weakref.ref(transport)
        self.original = type(transport).handle_request  # Unbound: no per-session reference cycle.
        self.activity = _Activity(owner)

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        transport = self.transport()
        assert transport is not None
        self.activity.active += 1
        try:
            await self.original(transport, scope, receive, send)
        finally:
            self.activity.active -= 1
            self.activity.last_active = monotonic()


class _IdleAdmission:
    def __init__(self, manager: StreamableHTTPSessionManager):
        self.manager = manager
        self.original = manager._handle_stateful_request
        self.next_warning = 0.0

    def victim(self, owner: bytes):
        idle = []
        now = monotonic()
        for session, candidate in self.manager._server_instances.items():
            handler = candidate.handle_request
            if isinstance(handler, _TrackedRequest) and handler.activity.active == 0:
                activity = handler.activity
                same_owner = activity.owner == owner
                if same_owner or now - activity.last_active > FOREIGN_IDLE_GRACE_S:
                    idle.append((not same_owner, activity.last_active, session, candidate))
        if not idle:
            return None
        _, _, session, candidate = min(idle, key=lambda item: item[:2])
        return session, candidate

    async def refuse(self, scope: Scope, receive: Receive, send: Send) -> None:
        now = monotonic()
        if now >= self.next_warning:
            self.next_warning = now + 60
            logger.warning(
                "Refusing to open a new session: %d sessions are already open", len(self.manager._server_instances)
            )
        await _error_response("Too many open sessions", 503, INTERNAL_ERROR)(scope, receive, send)

    async def handle(self, scope: Scope, receive: Receive, send: Send) -> None:
        if Request(scope, receive).headers.get(MCP_SESSION_ID_HEADER) is not None:
            await self.original(scope, receive, send)
            return
        try:
            body = await Request(scope, receive).body()  # The SDK already bounded/buffered this body.
        except Exception:
            return  # Disconnected/unreadable opening: no admission, eviction or traceback.
        owner = _owner(scope, _client_name(body))
        manager = self.manager
        user = scope.get("user")
        requestor = authorization_context(user) if isinstance(user, AuthenticatedUser) else None
        async with manager._session_creation_lock:
            cap = manager.max_sessions
            count = len(manager._server_instances)
            full = cap is not None and count >= cap
            # At most one transient extra slot; other openings receive bounded backpressure.
            refused = cap is not None and full and (count > cap or self.victim(owner) is None)
            transport = None
            handler = None
            if not refused:
                try:
                    if full:
                        manager.max_sessions = None
                    transport = manager._admit_session(requestor)
                finally:
                    manager.max_sessions = cap
                if transport is not None:
                    handler = _TrackedRequest(transport, owner)
                    transport.handle_request = handler
        if transport is None:
            await self.refuse(scope, receive, send)
            return
        assert handler is not None
        session = transport.mcp_session_id
        assert session is not None
        pending = True
        messages: list[Message] = []
        status = None

        async def replay() -> Message:
            nonlocal pending
            if pending:
                pending = False
                return {"type": "http.request", "body": body, "more_body": False}
            return await receive()

        async def capture(message: Message) -> None:
            nonlocal status
            if message["type"] == "http.response.start":
                status = message["status"]
            messages.append(message)

        accepted = False
        try:
            await manager._serve_opening_request(transport, scope, replay, capture)
            created = status is not None and status < 400 and session in manager._server_instances
            if created:
                victim = None
                async with manager._session_creation_lock:
                    if full and cap is not None and len(manager._server_instances) > cap:
                        # An old session may have become busy while the SDK initialized.
                        victim = self.victim(owner)
                        if victim is not None:
                            manager._server_instances.pop(victim[0])
                            manager._session_owners.pop(victim[0], None)
                        else:
                            refused = True
                if refused:
                    await self.refuse(scope, receive, send)
                    return
                if victim is not None:
                    await manager._discard_session(*victim)
                    logger.info("Evicted idle MCP session %.8s at session capacity", victim[0])
            for message in messages:
                await send(message)
            accepted = created
        finally:
            handler.activity.active -= 1
            handler.activity.last_active = monotonic()
            if not accepted:
                await manager._discard_session(session, transport)


def install_idle_session_eviction(manager: StreamableHTTPSessionManager) -> None:
    """Install once before serving; real SDK tests guard only the admission contracts."""
    if not manager.stateless:
        manager._handle_stateful_request = _IdleAdmission(manager).handle
