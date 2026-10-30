"""HTTP session configuration and recovery through the real SDK app."""

import logging
import os
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import anyio
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


@pytest.mark.parametrize(
    "value,expected", [(None, 86400), ("", 86400), ("  ", 86400), ("0", None), (" 120 ", 120), ("315360000", 315360000)]
)
def test_parse_timeout(value, expected):
    assert mcp_config.resolve_session_idle_timeout(value) == expected


@pytest.mark.parametrize("value", ["-1", "1.5", "bad", str(315360001), "9" * 400])
def test_invalid_timeout(value):
    with pytest.raises(ValueError, match=ENV):
        mcp_config.resolve_session_idle_timeout(value)


@pytest.mark.parametrize("value,expected", [(None, 86400), ("0", None), ("120", 120)])
def test_timeout_reaches_sdk_manager(monkeypatch, value, expected):
    monkeypatch.delenv(ENV, raising=False)
    if value is not None:
        monkeypatch.setenv(ENV, value)
    server = MCPServer("test")
    build_http_app(
        server, "streamable-http", TOKEN, session_idle_timeout=mcp_config.resolve_session_idle_timeout(value)
    )
    assert server._lowlevel_server.session_manager.session_idle_timeout == expected
    assert server._lowlevel_server.session_manager.max_sessions == 10000


@pytest.mark.parametrize("value", ["-1", "9" * 400])
def test_invalid_timeout_exits_before_admin_or_app(tmp_path, value):
    # Run the real entry point; sentinels make an early side effect observable.
    script = """
import operator_admin, mcp_config, runpy
def forbidden(*args, **kwargs):
    raise RuntimeError('LISTENER_STARTED_BEFORE_VALIDATION')
operator_admin.install_admin = forbidden
mcp_config.run_http_listeners = forbidden
runpy.run_path('main.py', run_name='__main__')
"""
    result = subprocess.run(
        [sys.executable, "-c", script],
        env={**os.environ, ENV: value, "WHATSAPP_MCP_TRANSPORT": "http", "WHATSAPP_STORE_DIR": str(tmp_path)},
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 1
    assert f"Invalid {ENV}" in result.stderr
    assert "LISTENER_STARTED_BEFORE_VALIDATION" not in result.stderr
    assert "listening on" not in result.stderr


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


def initialize(client):
    response = client.post("/mcp", json=INIT, headers=HEADERS)
    assert response.status_code == 200, response.text
    session = response.headers["Mcp-Session-Id"]
    response = client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "method": "notifications/initialized"},
        headers={**HEADERS, "Mcp-Session-Id": session},
    )
    assert response.status_code == 202
    return session


def ping(client, session):
    return client.post(
        "/mcp", json={"jsonrpc": "2.0", "id": 3, "method": "ping"}, headers={**HEADERS, "Mcp-Session-Id": session}
    )


def assert_missing(response):
    assert response.status_code == 404
    assert response.json()["jsonrpc"] == "2.0"
    assert isinstance(response.json()["error"]["code"], int)


def test_real_idle_expiration_then_reinitialize(auth_runtime_store):
    app = build_http_app(MCPServer("test"), "streamable-http", TOKEN, json_response=True, session_idle_timeout=0.2)
    with TestClient(app, base_url="http://127.0.0.1:8000") as client:
        session = initialize(client)
        time.sleep(0.4)
        assert_missing(ping(client, session))
        fresh = initialize(client)
        assert fresh != session
        assert ping(client, fresh).status_code == 200


@pytest.mark.parametrize("timeout", [None, 86400])
def test_sdk_contract_and_least_recent_idle_eviction(auth_runtime_store, caplog, timeout):
    server = MCPServer("test")
    app = build_http_app(
        server, "streamable-http", TOKEN, json_response=True, max_sessions=2, session_idle_timeout=timeout
    )
    manager = server._lowlevel_server.session_manager
    for name in (
        "_session_creation_lock",
        "_server_instances",
        "_session_owners",
        "_admit_session",
        "_serve_opening_request",
        "_discard_session",
        "_handle_stateful_request",
    ):
        assert hasattr(manager, name), f"SDK session contract changed: {name}"
    with (
        TestClient(app, base_url="http://127.0.0.1:8000") as client,
        caplog.at_level(logging.INFO, logger="whatsapp_mcp.sessions"),
    ):
        first, second = initialize(client), initialize(client)
        victim = manager._server_instances[second]
        assert hasattr(victim, "_requests_in_flight")
        assert ping(client, first).status_code == 200  # first is now the most recent
        third = initialize(client)
        assert victim.is_terminated
        assert second not in manager._session_owners
        assert len(manager._server_instances) == 2
        assert_missing(ping(client, second))
        assert ping(client, first).status_code == ping(client, third).status_code == 200
        logs = [record.getMessage() for record in caplog.records if record.name == "whatsapp_mcp.sessions"]
        assert len(logs) == 1 and second[:8] in logs[0] and second not in logs[0]


@pytest.mark.parametrize("timeout", [None, 86400])
@pytest.mark.parametrize("cap", [1, 2])
def test_inflight_session_is_not_evicted(auth_runtime_store, timeout, cap):
    server = MCPServer("test")
    started, release = threading.Event(), threading.Event()

    @server.tool()
    async def hold() -> str:
        started.set()
        await anyio.to_thread.run_sync(lambda: release.wait(10))
        return "done"

    app = build_http_app(
        server, "streamable-http", TOKEN, json_response=True, max_sessions=cap, session_idle_timeout=timeout
    )
    with TestClient(app, base_url="http://127.0.0.1:8000") as client, ThreadPoolExecutor(1) as pool:
        session = initialize(client)
        idle = initialize(client) if cap == 2 else None
        future = pool.submit(
            client.post,
            "/mcp",
            json={"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "hold", "arguments": {}}},
            headers={**HEADERS, "Mcp-Session-Id": session},
        )
        try:
            assert started.wait(5)
            if idle is None:
                response = client.post("/mcp", json=INIT, headers=HEADERS)
                assert response.status_code == 503
                assert response.json()["jsonrpc"] == "2.0"
            else:
                fresh = initialize(client)
                assert_missing(ping(client, idle))
        finally:
            release.set()
        assert future.result(timeout=10).status_code == 200
        if cap == 1:
            fresh = initialize(client)
            assert_missing(ping(client, session))
        else:
            assert ping(client, session).status_code == 200
        assert ping(client, fresh).status_code == 200
