"""Tests for the static bearer-token auth on the http/sse transports."""

import pytest
from starlette.testclient import TestClient

from http_auth import MIN_TOKEN_LENGTH, BearerTokenMiddleware, resolve_mcp_token, token_matches

TOKEN = "s3cret-token-0123456789abcdef"


class TestResolveMcpToken:
    @pytest.mark.parametrize("value", [None, "", "   "])
    def test_unset_disables_auth(self, value):
        assert resolve_mcp_token(value) is None

    def test_strips_whitespace(self):
        assert resolve_mcp_token(f"  {TOKEN}\n") == TOKEN

    def test_short_token_rejected(self):
        with pytest.raises(ValueError, match="WHATSAPP_MCP_TOKEN"):
            resolve_mcp_token("x" * (MIN_TOKEN_LENGTH - 1))


def test_token_matches_is_strict():
    assert token_matches(TOKEN, TOKEN)
    assert not token_matches(TOKEN + "x", TOKEN)
    assert not token_matches("", TOKEN)
    assert not token_matches(None, TOKEN)


async def _ok_app(scope, receive, send):
    if scope["type"] == "lifespan":
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            elif message["type"] == "lifespan.shutdown":
                await send({"type": "lifespan.shutdown.complete"})
                return
    await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"text/plain")]})
    await send({"type": "http.response.body", "body": b"ok"})


class TestBearerTokenMiddleware:
    def _client(self):
        return TestClient(BearerTokenMiddleware(_ok_app, TOKEN))

    def test_missing_header_is_401_with_challenge(self):
        response = self._client().get("/mcp")
        assert response.status_code == 401
        assert response.headers["www-authenticate"] == 'Bearer realm="whatsapp-mcp"'
        assert response.json()["error"] == "unauthorized"

    @pytest.mark.parametrize(
        "header",
        ["Bearer wrong-token-0123456789", "Basic dXNlcjpwYXNz", f"Token {TOKEN}", "Bearer", f"Bearer {TOKEN}x"],
    )
    def test_bad_credentials_are_401(self, header):
        response = self._client().get("/mcp", headers={"Authorization": header})
        assert response.status_code == 401

    def test_valid_token_passes_through(self):
        response = self._client().post("/mcp", headers={"Authorization": f"Bearer {TOKEN}"})
        assert response.status_code == 200
        assert response.text == "ok"

    def test_scheme_is_case_insensitive(self):
        response = self._client().get("/mcp", headers={"Authorization": f"bearer {TOKEN}"})
        assert response.status_code == 200

    def test_empty_token_is_a_programming_error(self):
        with pytest.raises(ValueError):
            BearerTokenMiddleware(_ok_app, "")


class TestBuildHttpApp:
    """The real MCP app wrapped the way main.py does it."""

    INIT = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}},
    }
    HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}

    def _post(self, client, extra_headers=None):
        return client.post("/mcp", json=self.INIT, headers={**self.HEADERS, **(extra_headers or {})})

    def test_token_required_when_configured(self):
        from mcp.server.mcpserver import MCPServer

        from main import build_http_app

        app = build_http_app(MCPServer("t"), "streamable-http", TOKEN, host="0.0.0.0")
        with TestClient(app) as client:  # one lifespan: the session manager runs once per app
            assert self._post(client).status_code == 401
            assert self._post(client, {"Authorization": f"Bearer {TOKEN}"}).status_code == 200

    def test_no_token_keeps_open_behaviour(self):
        from mcp.server.mcpserver import MCPServer

        from main import build_http_app

        app = build_http_app(MCPServer("t"), "streamable-http", None, host="0.0.0.0")
        with TestClient(app) as client:
            assert self._post(client).status_code == 200

    def test_sse_app_is_wrapped_too(self):
        from mcp.server.mcpserver import MCPServer

        from main import build_http_app

        app = build_http_app(MCPServer("t"), "sse", TOKEN, host="0.0.0.0")
        with TestClient(app) as client:
            assert client.post("/messages/?session_id=x", json={}).status_code == 401


class TestResolveHttpToken:
    def _resolve(self, explicit, host, bridge):
        from http_auth import resolve_http_token

        return resolve_http_token(explicit, host, lambda: bridge)

    def test_explicit_token_wins(self):
        assert self._resolve(TOKEN, "0.0.0.0", "bridge-token-0123456789") == (TOKEN, "WHATSAPP_MCP_TOKEN")

    def test_loopback_needs_no_token(self):
        assert self._resolve(None, "127.0.0.1", "bridge-token-0123456789") == (None, "loopback")

    def test_non_loopback_reuses_bridge_token(self):
        assert self._resolve("", "0.0.0.0", "bridge-token-0123456789") == ("bridge-token-0123456789", "bridge token")

    def test_non_loopback_without_any_token(self):
        assert self._resolve(None, "0.0.0.0", None) == (None, "none")
        assert self._resolve(None, "0.0.0.0", "short") == (None, "none")

    @pytest.mark.parametrize("value", ["off", "OFF", " none ", "disabled"])
    def test_explicit_off_disables_even_with_bridge_token(self, value):
        assert self._resolve(value, "0.0.0.0", "bridge-token-0123456789") == (None, "disabled")

    def test_explicit_short_token_still_rejected(self):
        with pytest.raises(ValueError):
            self._resolve("short", "0.0.0.0", None)
