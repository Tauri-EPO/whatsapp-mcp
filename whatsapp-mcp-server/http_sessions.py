"""Bound legacy HTTP sessions without evicting busy or recently foreign-owned ones."""

import hashlib
import json
import logging
from dataclasses import dataclass, field
from time import monotonic

from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser, authorization_context
from mcp.server.streamable_http import MCP_SESSION_ID_HEADER, StreamableHTTPServerTransport
from mcp.server.streamable_http_manager import StreamableHTTPSessionManager, _error_response
from mcp_types import INTERNAL_ERROR, InitializeRequest, JSONRPCRequest, jsonrpc_message_adapter
from pydantic import ValidationError
from starlette.requests import Request
from starlette.types import Receive, Scope, Send

from http_auth import _bearer_from_headers

DEFAULT_MAX_SESSIONS = 256
FOREIGN_IDLE_GRACE_S = 1800
logger = logging.getLogger("whatsapp_mcp.sessions")


def _owner(scope: Scope) -> bytes:
    user = scope.get("user")
    claims = (user.access_token.claims or {}) if isinstance(user, AuthenticatedUser) else {}
    if isinstance(user, AuthenticatedUser) and not claims.get("static"):
        identity = ["oauth", claims.get("iss"), user.access_token.subject]
    else:
        identity = ["bearer", _bearer_from_headers(scope["headers"])]
    return hashlib.sha256(json.dumps(identity).encode()).digest()


@dataclass
class _Activity:
    owner: bytes
    # Reserve the session while the SDK starts its server task.
    active: int = 1
    last_active: float = field(default_factory=lambda: monotonic())


class _IdleAdmission:
    def __init__(self, manager: StreamableHTTPSessionManager):
        self.manager = manager
        self.original = manager._handle_stateful_request
        self.validator = StreamableHTTPServerTransport(
            None, is_json_response_enabled=manager.json_response, security_settings=manager.security_settings
        )

    @staticmethod
    def track(transport: StreamableHTTPServerTransport, owner: bytes) -> _Activity:
        activity = _Activity(owner)
        setattr(transport, "_whatsapp_session_activity", activity)
        original = transport.handle_request

        async def handle(scope: Scope, receive: Receive, send: Send) -> None:
            # This also counts open GET streams when SDK expiry is disabled.
            activity.active += 1
            try:
                await original(scope, receive, send)
            finally:
                activity.active -= 1
                activity.last_active = monotonic()

        transport.handle_request = handle
        return activity

    async def validate_initialize(self, scope: Scope, receive: Receive, send: Send) -> Receive | None:
        request = Request(scope, receive)
        error = await self.validator._security.validate_request(request, is_post=request.method == "POST")
        if error is not None:
            await error(scope, receive, send)
            return None
        if request.method != "POST":
            await _error_response("A new session requires an initialize POST", 400)(scope, receive, send)
            return None
        if not await self.validator._validate_accept_header(request, scope, send):
            return None
        if not self.validator._check_content_type(request):
            await _error_response("Content-Type must be application/json", 415)(scope, receive, send)
            return None
        body = await request.body()  # Already bounded by the SDK body-limit middleware.
        try:
            message = jsonrpc_message_adapter.validate_json(body, by_name=False)
            if not isinstance(message, JSONRPCRequest) or message.method != "initialize":
                raise ValueError("initialize request required")
            InitializeRequest.model_validate(message.model_dump(by_alias=True), by_name=False)
        except (ValidationError, ValueError):
            await _error_response("A valid initialize request is required", 400)(scope, receive, send)
            return None
        pending = True

        async def replay():
            nonlocal pending
            if pending:
                pending = False
                return {"type": "http.request", "body": body, "more_body": False}
            return await receive()

        return replay

    async def handle(self, scope: Scope, receive: Receive, send: Send) -> None:
        if Request(scope, receive).headers.get(MCP_SESSION_ID_HEADER) is not None:
            # Keep SDK owner checks, DELETE handling and JSON-RPC 404s intact.
            await self.original(scope, receive, send)
            return
        replay = await self.validate_initialize(scope, receive, send)
        if replay is None:
            return
        manager = self.manager
        user = scope.get("user")
        requestor = authorization_context(user) if isinstance(user, AuthenticatedUser) else None
        owner = _owner(scope)
        victim = None
        async with manager._session_creation_lock:
            if manager.max_sessions is not None and len(manager._server_instances) >= manager.max_sessions:
                idle = []
                now = monotonic()
                for session, candidate in manager._server_instances.items():
                    activity = getattr(candidate, "_whatsapp_session_activity", None)
                    if activity is not None and activity.active == 0:
                        same_owner = activity.owner == owner
                        if same_owner or now - activity.last_active > FOREIGN_IDLE_GRACE_S:
                            idle.append((not same_owner, activity.last_active, session, candidate))
                if idle:
                    _, _, session, candidate = min(idle, key=lambda item: item[:2])
                    # Detach atomically; termination can await outside the creation lock.
                    manager._server_instances.pop(session)
                    manager._session_owners.pop(session, None)
                    victim = (session, candidate)
            transport = manager._admit_session(requestor)
            activity = self.track(transport, owner) if transport is not None else None
        if transport is None:
            await _error_response("Too many open sessions", 503, INTERNAL_ERROR)(scope, receive, send)
            return
        opening_started = False
        try:
            if victim is not None:
                await manager._discard_session(*victim)
                logger.info("Evicted idle MCP session %.8s at session capacity", victim[0])
            opening_started = True
            await manager._serve_opening_request(transport, scope, replay, send)
        finally:
            assert activity is not None
            activity.active -= 1
            activity.last_active = monotonic()
            # Also clean up if cancellation happened before the SDK opening call.
            if not opening_started:
                assert transport.mcp_session_id is not None
                await manager._discard_session(transport.mcp_session_id, transport)


def install_idle_session_eviction(manager: StreamableHTTPSessionManager) -> None:
    """Install once before serving; real SDK tests guard these private contracts."""
    if not manager.stateless:
        manager._handle_stateful_request = _IdleAdmission(manager).handle
