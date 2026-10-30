"""HTTP session configuration and recovery through the real SDK app."""

import pytest
from mcp.server.mcpserver import MCPServer
from starlette.testclient import TestClient

import mcp_config
from main import build_http_app

ENV = "WHATSAPP_MCP_SESSION_IDLE_TIMEOUT_S"
TOKEN = "test-session-token-0123456789"
HEADERS = {
    "Accept": "application/json, text/event-stream",
    "Authorization": f"Bearer {TOKEN}",
}
INIT = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}},
}


@pytest.mark.parametrize("value,expected", [(None, 86400), ("", 86400), ("  ", 86400), ("0", None), (" 120 ", 120)])
def test_parse_timeout(value, expected):
    assert mcp_config.resolve_session_idle_timeout(value) == expected


@pytest.mark.parametrize("value", ["-1", "1.5", "bad"])
def test_invalid_timeout(value):
    with pytest.raises(ValueError, match=ENV):
        mcp_config.resolve_session_idle_timeout(value)


@pytest.mark.parametrize("value,expected", [(None, 86400), ("0", None), ("120", 120)])
def test_timeout_reaches_sdk_manager(monkeypatch, value, expected):
    monkeypatch.delenv(ENV, raising=False)
    if value is not None:
        monkeypatch.setenv(ENV, value)
    server = MCPServer("test")
    build_http_app(server, "streamable-http", TOKEN)
    assert server._lowlevel_server.session_manager.session_idle_timeout == expected


def test_invalid_timeout_refuses_http_app(monkeypatch):
    monkeypatch.setenv(ENV, "-1")
    with pytest.raises(ValueError, match=ENV):
        build_http_app(MCPServer("test"), "streamable-http", TOKEN)


def test_sse_ignores_http_timeout(monkeypatch):
    monkeypatch.setenv(ENV, "bad")
    build_http_app(MCPServer("test"), "sse", TOKEN)


def test_unknown_session_then_reinitialize_without_restart(monkeypatch, auth_runtime_store):
    monkeypatch.setenv(ENV, "1")
    app = build_http_app(MCPServer("test"), "streamable-http", TOKEN, json_response=True)
    with TestClient(app, base_url="http://127.0.0.1:8000") as client:
        response = client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 2, "method": "ping"},
            headers={**HEADERS, "Mcp-Session-Id": "unknown-session"},
        )
        assert response.status_code == 404
        body = response.json()
        assert body["jsonrpc"] == "2.0"
        assert isinstance(body["error"]["code"], int)
        assert body["error"]["message"]
        response = client.post("/mcp", json=INIT, headers=HEADERS)
        assert response.status_code == 200
        assert "serverInfo" in response.json()["result"]
        session = response.headers["Mcp-Session-Id"]
        response = client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 3, "method": "ping"},
            headers={**HEADERS, "Mcp-Session-Id": session},
        )
        assert response.status_code == 200
        assert response.json()["result"] == {}
