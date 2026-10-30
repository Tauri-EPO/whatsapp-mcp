"""Real SQLite, MCP HTTP dispatch, ingest selection and independent-reader proof."""

import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
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
from tests.conftest import ALICE, BOB, FAMILY
from tests.test_transcribe_ingest import FakeBackend, _add_audio


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
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(runtime_archive.messages_db) + ".missing")
        return selection

    monkeypatch.setattr(transcribe_worker, "find_pending", select_then_break)
    monkeypatch.setattr(
        transcribe_worker, "_record_failure", lambda *args: pytest.fail("policy outage recorded as file failure")
    )
    backend = FakeBackend()
    result = transcribe_worker.run_once(10, transcribe=backend)
    assert result.transcribed == result.failed == 0
    assert backend.runs == []


@pytest.mark.asyncio
async def test_deploy_tool_floor_survives_runtime_empty_and_allow(runtime_archive, monkeypatch):
    monkeypatch.setenv("WHATSAPP_DENY_TOOLS", "delete_message")
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy(deny=frozenset({"delete_message"})))
    for changes in [{"tools.deny": []}, {"tools.allow": ["delete_message"], "tools.deny": []}]:
        patch(runtime_archive, changes)
        snapshot = runtime_settings.snapshot()
        assert "delete_message" in snapshot["settings"]["tools.deny"]["value"]
        assert not tool_policy.active_policy().allows("delete_message")
        assert "WHATSAPP_DENY_TOOLS (deploy)" in tool_policy.active_policy().denial_message("delete_message")
        assert "delete_message" not in {tool.name for tool in await main.mcp.list_tools()}
        result = await main.mcp.call_tool("delete_message", {})
        assert result.is_error and "denied" in result.content[0].text
    monkeypatch.setenv("WHATSAPP_ALLOW_TOOLS", "list_messages")
    patch(runtime_archive, {"tools.allow": ["send_message"], "tools.deny": []})
    assert not tool_policy.active_policy().allows("send_message")
    assert not tool_policy.active_policy().allows("list_messages")
    patch(runtime_archive, {"tools.allow": None})
    assert tool_policy.active_policy().allows("list_messages")


@pytest.mark.parametrize("key", ["tools.allow", "tools.deny", "transcription.ingest_chats"])
def test_corrupt_saved_key_warns_once_and_null_recovers(runtime_archive, caplog, key):
    with runtime_archive.messages() as conn:
        conn.execute("INSERT INTO runtime_settings VALUES (?,?,'2026-10-09',1)", (key, "invalid-json"))
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    assert runtime_settings.snapshot()["settings"][key]["source"] == "default" or key == "tools.deny"
    runtime_settings.snapshot()
    warnings = [record.message for record in caplog.records if "Saved runtime setting" in record.message]
    assert len(warnings) == 1 and key in warnings[0] and "invalid-json" not in warnings[0]
    if key == "tools.allow":
        assert not tool_policy.active_policy().allows("list_messages")
        assert not tool_policy.active_policy().allows("send_message")
    patch(runtime_archive, {key: None})
    assert runtime_settings.snapshot()["version"] == 2
    assert tool_policy.active_policy().allows("list_messages")


def test_unknown_saved_tools_are_dropped_and_bad_allow_keeps_explicit_floor(runtime_archive, monkeypatch, caplog):
    monkeypatch.setenv("WHATSAPP_ALLOW_TOOLS", "list_messages")
    monkeypatch.setenv("WHATSAPP_DENY_TOOLS", "delete_message")
    patch(
        runtime_archive,
        {"tools.deny": ["future_tool", "delete_message"], "tools.allow": ["future_tool", "list_messages"]},
    )
    snapshot = runtime_settings.snapshot()
    assert snapshot["settings"]["tools.allow"]["value"] == ["list_messages"]
    assert snapshot["settings"]["tools.deny"]["value"] == ["delete_message"]
    assert len([r for r in caplog.records if "Saved runtime setting" in r.message]) == 2
    with runtime_archive.messages() as conn:
        conn.execute("UPDATE runtime_settings SET value='invalid-json',version=2 WHERE key='tools.allow'")
    tool_policy.install_runtime_tool_policy(
        main.mcp, tool_policy.ToolPolicy(allow=frozenset({"list_messages"}), deny=frozenset({"delete_message"}))
    )
    assert tool_policy.active_policy().allows("list_messages")
    assert not tool_policy.active_policy().allows("send_message")
    assert not tool_policy.active_policy().allows("delete_message")


def test_bad_ingest_scope_startup_has_one_line_refusal(tmp_path):
    result = subprocess.run(
        [sys.executable, "main.py"],
        env={**os.environ, "TRANSCRIBE_ON_INGEST_CHATS": "invalid", "WHATSAPP_DB_PATH": str(tmp_path / "missing.db")},
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 1
    assert "TRANSCRIBE_ON_INGEST_CHATS: expected all or direct" in result.stderr
    assert "Traceback" not in result.stderr


def test_operator_logout_script_sends_idle_to_private_http_only():
    bash = shutil.which("bash")
    if bash is None or shutil.which("curl") is None:
        pytest.skip("needs bash and curl")
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


@pytest.mark.parametrize(
    ("deploy", "override", "effective"),
    [
        ("", [ALICE], [ALICE]),
        (ALICE, ["*@s.whatsapp.net"], [ALICE]),
        ("*@g.us", [ALICE, FAMILY], [FAMILY]),
        (ALICE, [BOB], []),
        (ALICE, ["*@g.us"], []),
        ("*@g.us", ["*@g.us"], ["*@g.us"]),
    ],
)
def test_runtime_chat_floor_matches_sql(runtime_archive, monkeypatch, deploy, override, effective):
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", deploy)
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy())
    patch(runtime_archive, {"chats.allowed": override})
    assert runtime_settings.snapshot()["settings"]["chats.allowed"] == {"value": effective, "source": "runtime"}
    clause, params = policy.sql_clause("jid")
    with runtime_archive.messages() as conn:
        selected = {row[0] for row in conn.execute(f"SELECT jid FROM chats WHERE {clause}", params)}
    assert selected == {jid for jid in (ALICE, BOB, FAMILY) if policy.allows(jid)}
    assert policy.restricted
    patch(runtime_archive, {"chats.allowed": None})
    assert policy.allows(BOB) == chat_policy.load_chat_policy().allows(BOB)


@pytest.mark.asyncio
async def test_runtime_chat_next_http_tool_call_filters_list_read_search_send(runtime_archive, monkeypatch):
    with runtime_archive.messages() as conn:
        conn.executemany(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me) VALUES (?,?,?,?,?,0)",
            [
                ("A1", ALICE, "5511999999999", "runtime hello", "2026-10-09 00:00:00+00:00"),
                ("B1", BOB, "5511888888888", "runtime hello", "2026-10-09 00:00:00+00:00"),
            ],
        )
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    # Modules hold independent imported references in production.
    import main as tool_main
    import media_inventory
    import media_notes
    import notes

    for module in (tool_main, media_inventory, media_notes, notes, transcribe_worker):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **kw: pytest.fail("denied send reached bridge"))
    app = main.build_http_app(
        main.mcp,
        "streamable-http",
        "fake-mcp-token-0123456789",
        host="0.0.0.0",
        json_response=True,
        stateless_http=True,
    )
    headers = {"Authorization": "Bearer fake-mcp-token-0123456789", "Accept": "application/json, text/event-stream"}
    with TestClient(app) as client:

        def call(name, arguments):
            response = client.post(
                "/mcp",
                headers=headers,
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": name, "arguments": arguments},
                },
            )
            assert response.status_code == 200, response.text
            return response.json()["result"]

        assert BOB in json.dumps(call("list_chats", {}))
        patch(runtime_archive, {"chats.allowed": [ALICE]})
        for name, arguments in [("list_chats", {}), ("list_messages", {"query": "hello", "include_context": False})]:
            result = call(name, arguments)
            assert not result.get("isError"), result
            assert ALICE in json.dumps(result) and BOB not in json.dumps(result)
        for name, arguments in [
            ("get_chat", {"chat_jid": BOB}),
            ("list_messages", {"chat_jid": BOB}),
            ("get_message_context", {"message_id": "B1", "chat_jid": BOB}),
            ("send_message", {"chat_jid": BOB, "message": "fake text"}),
            ("forward_message", {"chat_jid": BOB, "message_id": "B1", "to_chat_jid": ALICE}),
        ]:
            result = call(name, arguments)
            assert result["isError"] and "denied" in result["content"][0]["text"], result
        assert not notes.CHAT_POLICY.allows(BOB)
        assert media_inventory.CHAT_POLICY.allows(ALICE)
        patch(runtime_archive, {"chats.allowed": None})
        assert BOB in json.dumps(call("list_chats", {}))


def test_runtime_chats_ingest_rechecks_selected_candidates(runtime_archive, monkeypatch):
    _add_audio(runtime_archive, "AUD1", ALICE)
    _add_audio(runtime_archive, "AUD2", BOB)
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(transcribe_worker, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    real_select = transcribe_worker.find_pending

    def select_then_narrow(*args, **kwargs):
        selection = real_select(*args, **kwargs)
        assert len(selection.candidates) == 2
        patch(runtime_archive, {"chats.allowed": [ALICE]})
        return selection

    monkeypatch.setattr(transcribe_worker, "find_pending", select_then_narrow)
    backend = FakeBackend()
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 1
    assert len(backend.runs) == 1
    assert transcribe_worker._pending_rows(10) == []
    assert whatsapp.coverage()["audio"]["messages"] == 1


@pytest.mark.parametrize(
    "value",
    [
        [],
        "invalid-json",
        [" "],
        [ALICE + "," + BOB],
        [ALICE, "+55 11 99999-9999"],
        [ALICE, "a@b@c"],
        [ALICE, "@"],
        [ALICE, "1.2@lid"],
    ],
)
def test_bad_saved_chats_fail_closed_and_do_not_log_entries(runtime_archive, caplog, value):
    patch(runtime_archive, {"chats.allowed": value})
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    assert not policy.allows(ALICE) and not policy.allows(BOB)
    assert policy.sql_clause("jid") == ("0=1", [])
    assert ALICE not in caplog.text and BOB not in caplog.text
    assert len([r for r in caplog.records if "Saved runtime setting" in r.message]) == 1
    patch(runtime_archive, {"chats.allowed": None})
    assert policy.allows(ALICE)


@pytest.mark.parametrize(
    ("deploy", "origin"),
    [("", "runtime chats.allowed"), ("*@g.us", "runtime chats.allowed within WHATSAPP_ALLOWED_CHATS (deploy)")],
)
def test_refusal_names_the_list_it_came_from(runtime_archive, deploy, origin):
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": deploy}))
    assert "is not in WHATSAPP_ALLOWED_CHATS;" in policy.denial_message(BOB)  # no override: the wording of before
    patch(runtime_archive, {"chats.allowed": [FAMILY]})
    assert f"is not in {origin};" in policy.denial_message(BOB)
    with pytest.raises(ToolError, match=re.escape(f"is not in {origin}")):
        policy.require_participant(BOB)


def test_sender_name_reads_the_policy_once_for_key_and_query(runtime_archive, monkeypatch):
    class Narrowing:  # A PATCH lands between two reads made outside a pinned operation.
        reads = 0

        @property
        def restricted(self):
            self.reads += 1
            return self.reads == 1

    hidden = BOB.replace("@s.whatsapp.net", "@lid")
    with runtime_archive.messages() as conn:
        conn.execute("INSERT INTO chats(jid,name) VALUES (?,?)", (hidden, "Private cached name"))
        conn.execute("UPDATE chats SET name=NULL WHERE jid=?", (BOB,))
    policy = Narrowing()
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(runtime_archive.whatsmeow_db) + ".missing")
    whatsapp._reset_name_cache()
    assert whatsapp.get_sender_name(BOB) == BOB  # restricted key, restricted query
    assert policy.reads == 1


def test_runtime_chats_restriction_does_not_reuse_cross_namespace_sender_name(runtime_archive, monkeypatch):
    hidden = BOB.replace("@s.whatsapp.net", "@lid")
    with runtime_archive.messages() as conn:
        conn.execute("INSERT INTO chats(jid,name) VALUES (?,?)", (hidden, "Private cached name"))
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me) VALUES (?,?,?,?,?,0)",
            ("B1", FAMILY, BOB, "fake text", "2026-10-09 00:00:00+00:00"),
        )
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    # Hide the legitimate phone book so only the namespace fallback supplies a name.
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(runtime_archive.whatsmeow_db) + ".missing")
    with runtime_archive.messages() as conn:
        conn.execute("UPDATE chats SET name=NULL WHERE jid=?", (BOB,))
    whatsapp._reset_name_cache()
    assert whatsapp.get_sender_name(BOB) == "Private cached name"
    patch(runtime_archive, {"chats.allowed": [BOB, FAMILY]})
    stats = main.message_stats(group_by="sender")
    assert "Private cached name" not in json.dumps(stats)
    assert whatsapp.get_sender_name(BOB) == BOB
    patch(runtime_archive, {"chats.allowed": None})
    assert whatsapp.get_sender_name(BOB) == "Private cached name"


def test_runtime_chat_operation_reads_once_and_refreshes_next_call(runtime_archive, monkeypatch):
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    calls = []
    real_connect = whatsapp._connect_messages_db

    def connect():
        calls.append(1)
        return real_connect()

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    with chat_policy.policy_operation():
        for _ in range(500):
            assert policy.allows(ALICE) and policy.allows(BOB)
        assert not policy.restricted
    assert len(calls) == 1
    patch(runtime_archive, {"chats.allowed": [ALICE]})
    with chat_policy.policy_operation():
        assert policy.allows(ALICE) and not policy.allows(BOB)
    assert len(calls) == 2


def test_runtime_list_chats_pair_scan_has_bounded_connections(runtime_archive, monkeypatch):
    # Use the existing synthetic identity-pair seam; no account or phone is touched.
    pairs = {f"{index}@lid": f"{index}@s.whatsapp.net" for index in range(500)}
    monkeypatch.setattr(whatsapp, "_paired_lid_chats", lambda cur: pairs)
    monkeypatch.setattr(whatsapp, "_paired_phone_chats", lambda cur: {})
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({})))
    calls = []
    real_connect = whatsapp._connect_messages_db

    def connect():
        calls.append(1)
        return real_connect()

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    assert len(whatsapp.list_chats(limit=20)) == 4
    assert len(calls) <= 6
    calls.clear()
    assert len(main.list_chats(limit=20)["items"]) == 4
    assert len(calls) == 2  # One shared settings snapshot and one archive query connection.


def test_runtime_chat_read_failure_never_reuses_cached_sender_metadata(runtime_archive, monkeypatch, caplog):
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    whatsapp._reset_name_cache()
    assert whatsapp.get_sender_name(ALICE) == "Alice"
    patch(runtime_archive, {"chats.allowed": [ALICE]})

    def unavailable():
        raise sqlite3.OperationalError("locked")

    monkeypatch.setattr(whatsapp, "_connect_messages_db", unavailable)
    assert whatsapp.get_sender_name(ALICE) == ALICE
    with pytest.raises(ToolError, match="Runtime chat policy unavailable"):
        policy.allows(ALICE)
    assert ALICE not in caplog.text


@pytest.mark.asyncio
async def test_tool_and_chat_permissions_share_one_atomic_snapshot(runtime_archive, monkeypatch):
    import notes
    import strict_args

    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    for module in (whatsapp, main, notes):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)
    assert main.annotate("chat", BOB, "summary", "fake private Bob note")["success"]
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    patch(runtime_archive, {"chats.allowed": [ALICE], "tools.deny": []})
    real_policy = strict_args.active_policy
    changed = False

    def admit_then_patch():
        nonlocal changed
        admitted = real_policy()
        if not changed:
            changed = True
            patch(runtime_archive, {"chats.allowed": [BOB], "tools.deny": ["get_notes"]})
        return admitted

    monkeypatch.setattr(strict_args, "active_policy", admit_then_patch)
    result = await main.mcp.call_tool("get_notes", {"target_type": "chat", "target_id": BOB})
    assert changed and result.is_error and "denied" in result.content[0].text
    assert "fake private Bob note" not in result.model_dump_json()
    # The next call sees the new tool denial, then a jointly enabled version succeeds.
    assert (await main.mcp.call_tool("get_notes", {"target_type": "chat", "target_id": BOB})).is_error
    patch(runtime_archive, {"chats.allowed": [BOB], "tools.deny": []})
    allowed = await main.mcp.call_tool("get_notes", {"target_type": "chat", "target_id": BOB})
    assert not allowed.is_error and "fake private Bob note" in allowed.model_dump_json()


@pytest.mark.asyncio
async def test_missing_messages_store_cannot_reopen_private_notes(runtime_archive, monkeypatch):
    import notes
    import strict_args

    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    for module in (whatsapp, main, notes):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)
    assert main.annotate("chat", BOB, "summary", "fake private Bob note")["success"]
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    patch(runtime_archive, {"chats.allowed": [ALICE]})
    real_policy = strict_args.active_policy

    def admit_then_lose_store():
        admitted = real_policy()
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(runtime_archive.messages_db) + ".missing")
        return admitted

    monkeypatch.setattr(strict_args, "active_policy", admit_then_lose_store)
    result = await main.mcp.call_tool("get_notes", {"target_type": "chat", "target_id": BOB})
    assert result.is_error and "fake private Bob note" not in result.model_dump_json()
    # Independent chat authorization also fails closed without the outer tool guard.
    main.mcp.runtime_tool_policy = False
    result = await main.mcp.call_tool("get_notes", {"target_type": "chat", "target_id": BOB})
    assert result.is_error and "Runtime chat policy unavailable" in result.content[0].text
    assert "fake private Bob note" not in result.model_dump_json()


def test_ingest_selection_uses_the_same_snapshot_as_tool_admission(runtime_archive, monkeypatch):
    _add_audio(runtime_archive, "AUD1", ALICE)
    _add_audio(runtime_archive, "AUD2", BOB)
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(transcribe_worker, "CHAT_POLICY", policy)
    tool_policy.install_runtime_tool_policy(main.mcp, tool_policy.ToolPolicy())
    patch(runtime_archive, {"chats.allowed": [ALICE], "tools.deny": []})
    real_select = transcribe_worker.find_pending
    selected = []
    changed = False

    def patch_then_select(*args, **kwargs):
        nonlocal changed
        if not changed:
            changed = True
            patch(runtime_archive, {"chats.allowed": [BOB], "tools.deny": ["transcribe_audio"]})
        selection = real_select(*args, **kwargs)
        selected.append({candidate.chat_jid for candidate in selection.candidates})
        return selection

    monkeypatch.setattr(transcribe_worker, "find_pending", patch_then_select)
    backend = FakeBackend()
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 0
    assert selected == [{ALICE}] and backend.runs == []
    patch(runtime_archive, {"chats.allowed": [BOB], "tools.deny": []})
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 1
    assert selected[-1] == {BOB} and len(backend.runs) == 1


def test_unverified_missing_store_snapshot_cannot_authorize_chats(runtime_archive, monkeypatch):
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({}))
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(runtime_archive.messages_db) + ".missing")
    with chat_policy.policy_operation():
        assert runtime_settings.snapshot()["settings"]["chats.allowed"]["source"] == "default"
        with pytest.raises(ToolError, match="Runtime chat policy unavailable"):
            policy.allows(BOB)


def test_permission_store_is_not_implicitly_created(tmp_path, monkeypatch, request):
    assert "auth_runtime_store" not in request.fixturenames
    missing = tmp_path / "missing-messages.db"
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(missing))
    policy = chat_policy.RuntimeChatPolicy(chat_policy.ChatPolicy.unrestricted())
    with pytest.raises(ToolError, match="Runtime chat policy unavailable"):
        policy.allows(ALICE)
    assert not missing.exists()


def test_snapshot_carries_the_single_policy_for_reporting_and_enforcement(runtime_archive, monkeypatch):
    patch(runtime_archive, {"chats.allowed": [ALICE, FAMILY]})
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", "*@g.us")
    calls = 0
    original = chat_policy.intersect_policies

    def counted(*args):
        nonlocal calls
        calls += 1
        return original(*args)

    monkeypatch.setattr(chat_policy, "intersect_policies", counted)
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy())
    with chat_policy.policy_operation():
        report = runtime_settings.snapshot(require_store=True)
        assert policy.effective() is report.chat_policy
        assert report["settings"]["chats.allowed"]["value"] == [FAMILY]
        assert policy.allows(FAMILY) and not policy.allows(ALICE)
        assert calls == 1


def test_captured_deploy_source_survives_environment_changes(runtime_archive, monkeypatch):
    patch(runtime_archive, {"chats.allowed": [ALICE, FAMILY]})
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", "*@g.us")
    runtime_settings.capture_environment()
    policy = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy(runtime_settings._startup_env))
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", ALICE)
    with chat_policy.policy_operation():
        report = runtime_settings.snapshot(require_store=True)
        assert report.chat_policy is policy.effective()
        assert policy.allows(FAMILY) and not policy.allows(ALICE)


def test_operation_keeps_distinct_deploy_policies_separate(runtime_archive, monkeypatch):
    patch(runtime_archive, {"chats.allowed": [ALICE, BOB]})
    a = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE}))
    b = chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": BOB}))
    reads = 0
    original = whatsapp._connect_messages_db

    def counted():
        nonlocal reads
        reads += 1
        return original()

    monkeypatch.setattr(whatsapp, "_connect_messages_db", counted)
    with chat_policy.policy_operation():
        a_value = a.effective()
        assert a_value.allows(ALICE) and not a_value.allows(BOB)
        b_value = b.effective()
        assert b_value.allows(BOB) and not b_value.allows(ALICE)
        assert a.effective() is a_value and b.effective() is b_value
        assert reads == 1


def test_snapshot_uses_the_captured_source_without_reading_global_policy(runtime_archive, monkeypatch):
    patch(runtime_archive, {"chats.allowed": [ALICE, FAMILY]})
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", "*@g.us")
    runtime_settings.capture_environment()
    deploy = chat_policy.load_chat_policy(runtime_settings._startup_env)
    policy = chat_policy.RuntimeChatPolicy(deploy)
    monkeypatch.setattr(
        whatsapp,
        "CHAT_POLICY",
        chat_policy.RuntimeChatPolicy(chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})),
    )
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", ALICE)
    with chat_policy.policy_operation():
        report = runtime_settings.snapshot(require_store=True)
        assert report.chat_policy is policy.effective()
        assert policy.allows(FAMILY) and not policy.allows(ALICE)


@pytest.mark.parametrize("configured", [None, BOB])
def test_env_default_chat_reporting_keeps_its_original_value(runtime_archive, configured):
    env = {} if configured is None else {"WHATSAPP_ALLOWED_CHATS": configured}
    deploy = chat_policy.ChatPolicy.from_entries([ALICE])
    report = runtime_settings.snapshot(env=env, require_store=True, deploy_policy=deploy)
    assert report["settings"]["chats.allowed"] == {
        "value": [] if configured is None else [BOB],
        "source": "default" if configured is None else "env",
    }
    assert report.chat_policy is deploy


def test_snapshot_child_views_reuse_the_root_cache(runtime_archive):
    report = runtime_settings.snapshot(env={})
    alice = chat_policy.ChatPolicy.from_entries([ALICE])
    bob = chat_policy.ChatPolicy.from_entries([BOB])
    alice_view = report.for_deploy(alice)
    bob_view = report.for_deploy(bob)
    assert alice_view.for_deploy(bob) is bob_view
    assert bob_view.for_deploy(alice) is alice_view


def test_snapshot_concurrent_views_share_one_instance(runtime_archive, monkeypatch):
    report = runtime_settings.snapshot(env={})
    child = report.for_deploy(chat_policy.ChatPolicy.from_entries([ALICE]))
    target = chat_policy.ChatPolicy.from_entries([BOB])
    start = threading.Barrier(8)
    entered = threading.Event()
    contended = threading.Event()
    release = threading.Event()
    calls_lock = threading.Lock()
    builds = 0
    original = runtime_settings._effective_chat_setting

    def paused_build(*args):
        nonlocal builds
        with calls_lock:
            builds += 1
            if builds > 1:
                contended.set()
        entered.set()
        assert release.wait(timeout=5)
        return original(*args)

    monkeypatch.setattr(runtime_settings, "_effective_chat_setting", paused_build)

    def resolve(index):
        start.wait(timeout=5)
        source = report if index % 2 else child
        return source.for_deploy(target)

    with ThreadPoolExecutor(max_workers=8) as pool:
        futures = [pool.submit(resolve, index) for index in range(8)]
        try:
            assert entered.wait(timeout=5)
            contended.wait(timeout=0.1)
        finally:
            release.set()
        views = [future.result(timeout=5) for future in futures]
    assert builds == 1
    assert all(view is report.for_deploy(target) for view in views)
    assert child.for_deploy(target) is views[0]
