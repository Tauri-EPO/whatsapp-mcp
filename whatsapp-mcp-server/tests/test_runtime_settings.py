"""Real SQLite, MCP HTTP dispatch, ingest selection and independent-reader proof."""

import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest
from starlette.testclient import TestClient

import chat_policy
import main
import runtime_settings
import tool_policy
import transcribe_worker
import whatsapp
from errors import ToolError
from strict_args import StrictArgumentServer
from tests.conftest import ALICE, FAMILY
from tests.test_transcribe_ingest import FakeBackend, _add_audio


@pytest.fixture
def runtime_archive(paired_dbs, monkeypatch):
    bridge = Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "runtime_settings.go"
    schema = re.search(r"const runtimeSettingsSchema = `([^`]+)`", bridge.read_text()).group(1)
    with paired_dbs.messages() as conn:
        conn.executescript(schema)
        conn.execute("PRAGMA journal_mode=WAL")
    monkeypatch.setattr(runtime_settings, "_startup_env", None)
    yield paired_dbs
    tool_policy.set_active_policy(None)
    main.mcp.runtime_tool_policy = False


def patch(store, changes):
    """Simulate the bridge's committed transaction; Python production never writes."""
    with store.messages() as conn:
        version = conn.execute("SELECT COALESCE(MAX(version),0)+1 FROM runtime_settings").fetchone()[0]
        for key, value in changes.items():
            conn.execute(
                "INSERT INTO runtime_settings VALUES (?,?,?,?) ON CONFLICT(key) DO UPDATE SET "
                "value=excluded.value,updated_at=excluded.updated_at,version=excluded.version",
                (key, json.dumps(value), "2026-10-09 00:00:00+00:00", version),
            )
    return version


def test_runtime_sources_clears_independent_process_and_read_only(runtime_archive, monkeypatch):
    monkeypatch.setenv("TRANSCRIBE_ON_INGEST_CHATS", "direct")
    monkeypatch.setenv("WHATSAPP_ALLOW_TOOLS", "list_messages")
    initial = runtime_settings.snapshot()
    assert initial["settings"]["transcription.ingest_chats"] == {"value": "direct", "source": "env"}
    assert initial["settings"]["tools.deny"] == {"value": [], "source": "default"}
    assert patch(runtime_archive, {"transcription.ingest_chats": "all", "tools.deny": ["delete_message"]}) == 1
    assert runtime_settings.ingest_setting() == {"value": "all", "source": "runtime"}
    script = "import json,runtime_settings; print(json.dumps(runtime_settings.snapshot()))"
    result = subprocess.run(
        [sys.executable, "-c", script],
        env={**os.environ, "WHATSAPP_DB_PATH": str(runtime_archive.messages_db)},
        capture_output=True,
        text=True,
        timeout=30,
        check=True,
    )
    assert json.loads(result.stdout) == runtime_settings.snapshot()
    assert patch(runtime_archive, {"transcription.ingest_chats": None, "tools.deny": None}) == 2
    assert runtime_settings.snapshot()["version"] == 2
    assert runtime_settings.ingest_setting() == initial["settings"]["transcription.ingest_chats"]
    conn = whatsapp._connect_messages_db()
    try:
        with pytest.raises(sqlite3.OperationalError):
            conn.execute("DELETE FROM runtime_settings")
    finally:
        conn.close()


def test_runtime_lists_filter_http_tools_list_calls_and_restore(runtime_archive):
    server = StrictArgumentServer("runtime-test")
    calls = []

    @server.tool()
    def list_messages() -> dict:
        calls.append("list_messages")
        return {"ok": True}

    @server.tool()
    def send_message() -> dict:
        calls.append("send_message")
        return {"ok": True}

    tool_policy.install_runtime_tool_policy(server, tool_policy.ToolPolicy())
    app = main.build_http_app(
        server, "streamable-http", "fake-mcp-token-0123456789", host="0.0.0.0", json_response=True, stateless_http=True
    )
    headers = {"Authorization": "Bearer fake-mcp-token-0123456789", "Accept": "application/json, text/event-stream"}
    with TestClient(app) as client:

        def rpc(method, params=None):
            response = client.post(
                "/mcp", headers=headers, json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}
            )
            assert response.status_code == 200, response.text
            return response.json()["result"]

        assert {tool["name"] for tool in rpc("tools/list")["tools"]} == {"list_messages", "send_message"}
        patch(runtime_archive, {"tools.allow": ["list_messages", "send_message"], "tools.deny": ["send_message"]})
        assert {tool["name"] for tool in rpc("tools/list")["tools"]} == {"list_messages"}
        denied = rpc("tools/call", {"name": "send_message", "arguments": {}})
        assert denied["isError"] and "denied" in denied["content"][0]["text"]
        assert calls == []
        assert not rpc("tools/call", {"name": "list_messages", "arguments": {}}).get("isError")
        patch(runtime_archive, {"tools.allow": ["send_message"], "tools.deny": []})
        assert {tool["name"] for tool in rpc("tools/list")["tools"]} == {"send_message"}
        assert rpc("tools/call", {"name": "list_messages", "arguments": {}})["isError"]
        patch(runtime_archive, {"tools.allow": None, "tools.deny": None})
        assert len(rpc("tools/list")["tools"]) == 2
        assert not rpc("tools/call", {"name": "send_message", "arguments": {}}).get("isError")
        assert calls == ["list_messages", "send_message"]
        for method, path in [("GET", "settings"), ("PATCH", "settings"), ("POST", "logout")]:
            assert client.request(method, "/operator/v1/" + path, headers=headers, json={}).status_code == 404


@pytest.mark.asyncio
async def test_runtime_allow_cannot_reenable_read_only_tool(runtime_archive):
    patch(runtime_archive, {"tools.allow": ["send_message"]})
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy(read_only=True))
    assert "send_message" not in {tool.name for tool in await main.mcp.list_tools()}
    result = await main.mcp.call_tool("send_message", {"recipient": "5511999999999", "message": "fake text"})
    assert result.is_error
    assert "WHATSAPP_READ_ONLY" in result.content[0].text


def test_runtime_direct_worker_and_coverage_agree_and_explicit_group_is_allowed(runtime_archive, monkeypatch):
    lid = "100000000000006@lid"
    _add_audio(runtime_archive, "AUD1", ALICE)
    _add_audio(runtime_archive, "AUD2", lid)
    _add_audio(runtime_archive, "AUD3", FAMILY)
    _add_audio(runtime_archive, "AUD4", "status@broadcast")
    _add_audio(runtime_archive, "BOB1", "100000000000000@newsletter")
    assert len(transcribe_worker._pending_rows(20)) == 4  # all preserves prior scope.
    assert whatsapp.coverage()["audio"]["messages"] == 4
    patch(runtime_archive, {"transcription.ingest_chats": "direct"})
    assert {row[1] for row in transcribe_worker._pending_rows(20)} == {ALICE, lid}
    assert whatsapp.coverage()["audio"]["messages"] == 2
    backend = FakeBackend()
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 2
    assert len(backend.runs) == 2
    assert whatsapp.coverage()["audio"]["backlog"] == 0
    # The direct scope bounds only background work; the explicit tool stays enabled.
    assert tool_policy.load_tool_policy().allows("transcribe_audio")
    monkeypatch.setattr(main, "_audio_to_transcribe", lambda chat, mid: "fake-group-audio.ogg")
    monkeypatch.setattr(main, "transcribe_file", lambda path, **kw: {"text": "fake transcript", "backend": "server"})
    assert main.transcribe_audio(message_id="AUD3", chat_jid=FAMILY)["text"] == "fake transcript"
    patch(runtime_archive, {"transcription.ingest_chats": "all"})
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 1  # Group was explicitly handled above.
    assert whatsapp.coverage()["audio"]["backlog"] == 0


def test_direct_scope_keeps_chat_allow_list_and_runtime_tool_pause(runtime_archive, monkeypatch):
    _add_audio(runtime_archive, "AUD1", ALICE)
    _add_audio(runtime_archive, "AUD2", "100000000000006@lid")
    patch(runtime_archive, {"transcription.ingest_chats": "direct"})
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    monkeypatch.setattr(transcribe_worker, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    assert {row[1] for row in transcribe_worker._pending_rows(10)} == {ALICE}
    assert whatsapp.coverage()["audio"]["messages"] == 1
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    patch(runtime_archive, {"tools.deny": ["transcribe_audio"]})
    backend = FakeBackend()
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 0
    assert backend.runs == []
    patch(runtime_archive, {"tools.deny": None})
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 1


def test_ingest_env_parser_and_bridge_status(runtime_archive, monkeypatch):
    for bad in ["group", "DIRECT", "false"]:
        with pytest.raises(ValueError, match="TRANSCRIBE_ON_INGEST_CHATS"):
            transcribe_worker.load_ingest_config({"TRANSCRIBE_ON_INGEST_CHATS": bad})
    patch(runtime_archive, {"transcription.ingest_chats": "direct"})
    monkeypatch.setattr(
        whatsapp,
        "_bridge_request",
        lambda *a, **kw: (_ for _ in ()).throw(ToolError("bridge_unavailable", "fake offline")),
    )
    assert whatsapp.bridge_status()["transcription_ingest_chats"] == {"value": "direct", "source": "runtime"}


def test_runtime_policy_failure_between_selection_and_transcription_writes_no_failure(runtime_archive, monkeypatch):
    _add_audio(runtime_archive, "AUD1", ALICE)
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    real_select = transcribe_worker.find_pending

    def select_then_break(*args, **kwargs):
        selection = real_select(*args, **kwargs)
        with runtime_archive.messages() as conn:
            conn.execute("UPDATE runtime_settings SET value='invalid-json'")
            conn.execute("INSERT OR IGNORE INTO runtime_settings VALUES ('tools.deny','invalid-json','2026-10-09',1)")
        return selection

    monkeypatch.setattr(transcribe_worker, "find_pending", select_then_break)
    monkeypatch.setattr(
        transcribe_worker, "_record_failure", lambda *args: pytest.fail("policy outage recorded as file failure")
    )
    backend = FakeBackend()
    result = transcribe_worker.run_once(10, transcribe=backend)
    assert result.transcribed == result.failed == 0
    assert backend.runs == []


def test_operator_logout_script_sends_idle_to_private_http_only():
    bash = shutil.which("bash")
    if bash is None:
        pytest.skip("needs bash")
    received = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            received.append(
                (self.path, self.headers.get("Authorization"), self.rfile.read(int(self.headers["Content-Length"])))
            )
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"server_unlinked":true,"local_session_wiped":true}')

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        root = Path(__file__).resolve().parents[2]
        command = [bash, "scripts/operator-logout.sh"] if os.name == "nt" else ["./scripts/operator-logout.sh"]
        result = subprocess.run(
            [*command, f"http://127.0.0.1:{server.server_port}", "idle"],
            cwd=root,
            env={**os.environ, "OPERATOR_TOKEN": "fake-operator-token-0123456789abcdef"},
            capture_output=True,
            text=True,
            timeout=20,
            check=True,
        )
        assert json.loads(result.stdout)["local_session_wiped"]
        assert received == [("/operator/v1/logout", "Bearer fake-operator-token-0123456789abcdef", b'{"after":"idle"}')]
        assert "fake-operator-token" not in result.stdout + result.stderr
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()
