"""SDK 2.3 stateful admission: evict the least recently active idle session."""

import logging
import time
from dataclasses import dataclass, field

from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser, authorization_context
from mcp.server.streamable_http import MCP_SESSION_ID_HEADER, StreamableHTTPServerTransport
from mcp.server.streamable_http_manager import StreamableHTTPSessionManager, _error_response
from mcp_types import INTERNAL_ERROR
from starlette.requests import Request
from starlette.types import Receive, Scope, Send

logger = logging.getLogger("whatsapp_mcp.sessions")


@dataclass
class _Activity:
    # Reserve a newly admitted session while the SDK starts its server task.
    active: int = 1
    last_active: float = field(default_factory=time.monotonic)


class _IdleAdmission:
    def __init__(self, manager: StreamableHTTPSessionManager):
        self.manager = manager
        self.original = manager._handle_stateful_request

    @staticmethod
    def track(transport: StreamableHTTPServerTransport) -> _Activity:
        activity = _Activity()
        setattr(transport, "_whatsapp_session_activity", activity)
        original = transport.handle_request

        async def handle(scope: Scope, receive: Receive, send: Send) -> None:
            # SDK activity accounting is disabled when expiry is None; ours
            # protects in-flight POSTs and open GET streams in both modes.
            activity.active += 1
            try:
                await original(scope, receive, send)
            finally:
                activity.active -= 1
                activity.last_active = time.monotonic()

        transport.handle_request = handle
        return activity

    async def handle(self, scope: Scope, receive: Receive, send: Send) -> None:
        if Request(scope, receive).headers.get(MCP_SESSION_ID_HEADER) is not None:
            # Keep SDK owner checks, DELETE handling and JSON-RPC 404s intact.
            await self.original(scope, receive, send)
            return
        manager = self.manager
        user = scope.get("user")
        requestor = authorization_context(user) if isinstance(user, AuthenticatedUser) else None
        async with manager._session_creation_lock:
            if manager.max_sessions is not None and len(manager._server_instances) >= manager.max_sessions:
                idle = []
                for session, candidate in manager._server_instances.items():
                    activity = getattr(candidate, "_whatsapp_session_activity", None)
                    if activity is not None and activity.active == 0 and candidate._requests_in_flight == 0:
                        idle.append((activity.last_active, session, candidate))
                if idle:
                    _, session, candidate = min(idle, key=lambda item: item[0])
                    await manager._discard_session(session, candidate)
                    logger.info("Evicted idle MCP session %.8s at session capacity", session)
            transport = manager._admit_session(requestor)
            activity = self.track(transport) if transport is not None else None
        if transport is None:
            await _error_response("Too many open sessions", 503, INTERNAL_ERROR)(scope, receive, send)
            return
        try:
            await manager._serve_opening_request(transport, scope, receive, send)
        finally:
            assert activity is not None
            activity.active -= 1
            activity.last_active = time.monotonic()


def install_idle_session_eviction(manager: StreamableHTTPSessionManager) -> None:
    """Adapt only new-session admission, retaining the SDK app/auth/lifespan.

    Private SDK contracts are guarded by real tiny-cap/in-flight tests. Install
    once, before serving requests; no global SDK factories are patched.
    """
    if not manager.stateless:
        manager._handle_stateful_request = _IdleAdmission(manager).handle
