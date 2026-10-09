"""Actual MCP startup preserves the data plane when optional admin is unavailable."""

import hashlib
import json
import os
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from datetime import UTC, datetime, timedelta

import pytest

BRIDGE_TOKEN = "fake-bridge-0123456789abcdef"
MCP_TOKEN = "fake-mcp-0123456789abcdef"
INITIALIZE = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "test", "version": "1"}},
}


def startup_env(tmp_path, **settings):
    # The real bridge creates this registry before the MCP container starts.
    # Token rotation intentionally refuses a missing archive on protected HTTP.
    with sqlite3.connect(tmp_path / "messages.db") as conn:
        conn.execute(
            "CREATE TABLE IF NOT EXISTS runtime_settings "
            "(key TEXT PRIMARY KEY, value TEXT, updated_at TEXT, version INTEGER)"
        )
    env = {
        key: value for key, value in os.environ.items() if not key.startswith(("WHATSAPP_", "TRANSCRIBE_", "WHISPER_"))
    }
    return {
        **env,
        "WHATSAPP_STORE_DIR": str(tmp_path),
        "WHATSAPP_OPERATOR_BIND": "operator.example",
        "WHATSAPP_BRIDGE_TOKEN": BRIDGE_TOKEN,
        "WHATSAPP_MCP_HOST": "0.0.0.0",
        "WHATSAPP_MCP_TRANSPORT": "http",
        "WHATSAPP_MCP_METRICS": "false",
        **settings,
    }


@contextmanager
def occupied_admin_port():
    with socket.socket() as occupied:
        occupied.bind(("127.0.0.1", 8091))
        occupied.listen()
        yield


@pytest.mark.parametrize(
    "occupied,mcp_token,bridge_grace",
    [
        (False, None, False),
        (True, MCP_TOKEN, False),
        (True, "fake-mcp-ä0123456789abcdef", False),
        (False, MCP_TOKEN, True),
    ],
)
def test_actual_http_startup_without_admin_keeps_mcp_serving_and_warns(tmp_path, occupied, mcp_token, bridge_grace):
    with socket.socket() as free:
        free.bind(("127.0.0.1", 0))
        port = free.getsockname()[1]
    env = startup_env(tmp_path, WHATSAPP_MCP_PORT=str(port))
    if mcp_token:
        env["WHATSAPP_MCP_TOKEN"] = mcp_token
    if bridge_grace:
        state = json.dumps(
            {
                "current": hashlib.sha256(MCP_TOKEN.encode()).hexdigest(),
                "previous": hashlib.sha256(BRIDGE_TOKEN.encode()).hexdigest(),
                "previous_valid_until": (datetime.now(UTC) + timedelta(hours=1)).isoformat(),
            }
        )
        with sqlite3.connect(tmp_path / "messages.db") as conn:
            conn.execute(
                "INSERT INTO runtime_settings VALUES ('auth.mcp_token',?, '2026-10-09T00:00:00Z', 1)", (state,)
            )
    from contextlib import nullcontext

    with occupied_admin_port() if occupied else nullcontext():
        proc = subprocess.Popen(
            [sys.executable, "main.py"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True
        )
        try:
            deadline = time.monotonic() + 25
            while time.monotonic() < deadline:
                try:
                    request = urllib.request.Request(
                        f"http://127.0.0.1:{port}/mcp",
                        data=json.dumps(INITIALIZE).encode(),
                        headers={
                            "Authorization": "Bearer " + (mcp_token or BRIDGE_TOKEN),
                            "Content-Type": "application/json",
                            "Accept": "application/json, text/event-stream",
                        },
                    )
                    with urllib.request.urlopen(request, timeout=2) as response:
                        assert response.status == 200
                        assert '"protocolVersion"' in response.read().decode()
                    break
                except urllib.error.URLError:
                    if proc.poll() is not None:
                        pytest.fail("MCP exited before serving its data plane: " + proc.communicate(timeout=5)[1])
                    time.sleep(0.05)
            else:
                pytest.fail("MCP failed to serve its data plane within the startup budget")
            if not occupied:
                with socket.socket() as probe:
                    assert probe.connect_ex(("127.0.0.1", 8091)) != 0
        finally:
            proc.terminate()
            _, stderr = proc.communicate(timeout=10)
    expected = "free the admin port" if occupied else "set WHATSAPP_MCP_TOKEN distinct from the bridge token"
    assert stderr.count("MCP admin disabled:") == 1 and expected in stderr
    assert "WARNING" in stderr and "Traceback" not in stderr


def test_actual_http_startup_refuses_admin_port_collision_without_traceback(tmp_path):
    result = subprocess.run(
        [sys.executable, "main.py"],
        env=startup_env(tmp_path, WHATSAPP_MCP_PORT="8091", WHATSAPP_MCP_TOKEN=MCP_TOKEN),
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 1
    assert "WHATSAPP_MCP_PORT: 8091 is reserved" in result.stderr
    assert "Traceback" not in result.stderr


def test_actual_stdio_does_not_start_admin_or_validate_http_port(tmp_path):
    with occupied_admin_port():
        result = subprocess.run(
            [sys.executable, "main.py"],
            env=startup_env(
                tmp_path, WHATSAPP_MCP_TRANSPORT="stdio", WHATSAPP_MCP_PORT="not-a-port", WHATSAPP_MCP_TOKEN=MCP_TOKEN
            ),
            input=json.dumps(INITIALIZE) + "\n",
            capture_output=True,
            text=True,
            timeout=30,
        )
    assert result.returncode == 0 and '"protocolVersion"' in result.stdout
    assert "MCP admin disabled:" not in result.stderr and "Traceback" not in result.stderr
