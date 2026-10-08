"""Group management tools: payloads, validation, allow-list, bridge errors."""

import json
import threading
from contextlib import asynccontextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import anyio
import pytest
from mcp.client.session import ClientSession
from mcp.shared.memory import create_client_server_memory_streams

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tool_policy import ToolPolicy

GROUP = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload if payload is not None else {"success": True}
        self.text = ""

    def json(self):
        return self._payload


@pytest.fixture
def calls(monkeypatch):
    seen = []

    def fake_post(url, json=None, headers=None, timeout=None):
        seen.append((url.rsplit("/api", 1)[-1], json))
        if url.endswith("/group/invite"):
            return Resp(payload={"success": True, "link": "https://chat.whatsapp.com/ABC"})
        return Resp()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    return seen


def test_participants_payload(calls):
    out = main.manage_group_participants(GROUP, "Add", ["+5511999999999", " 5511888888888@s.whatsapp.net "])
    assert out["success"] is True
    assert calls[-1] == (
        "/group/participants",
        {"group_jid": GROUP, "action": "add", "participants": ["+5511999999999", "5511888888888@s.whatsapp.net"]},
    )


def test_participants_validation(calls):
    assert (
        main.manage_group_participants("5511999999999@s.whatsapp.net", "add", ["x"])["error"]["code"]
        == "invalid_argument"
    )
    assert main.manage_group_participants(GROUP, "kick", ["x"])["error"]["code"] == "invalid_argument"
    assert main.manage_group_participants(GROUP, "add", [" "])["error"]["code"] == "invalid_argument"
    assert calls == []


def test_update_invite_leave_typing(calls):
    assert main.update_group(GROUP, name="Família")["success"] is True
    assert calls[-1] == ("/group/subject", {"group_jid": GROUP, "name": "Família"})
    assert main.update_group(GROUP, description="")["success"] is True
    assert calls[-1][1] == {"group_jid": GROUP, "description": ""}
    assert main.update_group(GROUP)["error"]["code"] == "invalid_argument"

    out = main.get_group_invite_link(GROUP, reset=True)
    assert out["link"].startswith("https://chat.whatsapp.com/") and calls[-1][1] == {"group_jid": GROUP, "reset": True}

    assert main.leave_group(GROUP)["success"] is True and calls[-1][0] == "/group/leave"

    assert main.send_typing("5511999999999")["success"] is True
    assert calls[-1] == ("/typing", {"recipient": "5511999999999", "is_typing": True})
    assert main.send_typing("", False)["error"]["code"] == "invalid_argument"


def test_allow_list_blocks_before_bridge(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    for fn, args in (
        (whatsapp.manage_group_participants, (GROUP, "add", ["5511999999999"])),
        (whatsapp.update_group, (GROUP, "x", None)),
        (whatsapp.get_group_invite_link, (GROUP,)),
        (whatsapp.leave_group, (GROUP,)),
        (whatsapp.send_typing, (GROUP,)),
    ):
        with pytest.raises(ToolError) as exc:
            fn(*args)
        assert exc.value.code == "denied", fn.__name__


def test_bridge_refusal_maps_to_code(monkeypatch):
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http, "post", lambda *a, **k: Resp(502, {"success": False, "message": "not connected"})
    )
    out = main.leave_group(GROUP)
    assert out["error"]["code"] == "bridge_unavailable" and "not connected" in out["error"]["message"]


@asynccontextmanager
async def group_sdk_client():
    """Reach tools/call over the real SDK protocol and registration adapter."""
    server = main.mcp._lowlevel_server
    async with create_client_server_memory_streams() as (client_streams, server_streams):
        async with anyio.create_task_group() as tasks:
            tasks.start_soon(server.run, *server_streams, server.create_initialization_options())
            async with ClientSession(*client_streams) as client:
                await client.initialize()
                await client.list_tools()
                yield client
            tasks.cancel_scope.cancel()


@pytest.fixture
def group_http_bridge(monkeypatch):
    """Use actual HTTP bytes between the MCP tool and a local bridge stand-in."""
    observed = []
    reply = {"status": 502, "payload": {}}

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            observed.append((self.path, body, self.headers.get("Authorization")))
            payload = json.dumps(reply["payload"]).encode()
            self.send_response(reply["status"])
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{server.server_port}/api")
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    try:
        yield reply, observed
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        assert not thread.is_alive()


@pytest.mark.parametrize("stage", ["could not read group info", "set description failed"])
async def test_update_group_partial_failure_through_sdk_and_http(group_http_bridge, stage):
    reply, observed = group_http_bridge
    message = f"group was renamed; {stage}: synthetic failure; retry only description"
    reply["payload"] = {"success": False, "message": message, "changed": ["name"]}
    async with group_sdk_client() as client:
        result = await client.call_tool("update_group", {"chat_jid": GROUP, "name": "New group", "description": "new"})
    assert result.is_error is True
    envelope = json.loads(result.content[0].text)
    assert envelope["error"] == {"code": "bridge_unavailable", "message": message}
    assert "group was renamed" in envelope["error"]["message"]
    assert observed == [
        ("/api/group/subject", {"group_jid": GROUP, "name": "New group", "description": "new"}, "Bearer " + "t" * 32)
    ]


@pytest.mark.parametrize("description", ["new description", ""])
async def test_update_group_description_only_retry_through_sdk_and_http(group_http_bridge, description):
    reply, observed = group_http_bridge
    reply.update(status=200, payload={"success": True, "group_jid": GROUP, "changed": ["description"]})
    async with group_sdk_client() as client:
        result = await client.call_tool("update_group", {"chat_jid": GROUP, "description": description})
    assert not result.is_error
    assert json.loads(result.content[0].text)["changed"] == ["description"]
    assert observed == [("/api/group/subject", {"group_jid": GROUP, "description": description}, "Bearer " + "t" * 32)]


@pytest.mark.parametrize("denial", ["chat", "read-only", "allow-list", "deny-list"])
async def test_update_group_sdk_denies_before_http(monkeypatch, group_http_bridge, denial):
    _, observed = group_http_bridge
    if denial == "chat":
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    else:
        policy = ToolPolicy(
            read_only=denial == "read-only",
            allow=frozenset({"send_reaction"}) if denial == "allow-list" else frozenset(),
            deny=frozenset({"update_group"}) if denial == "deny-list" else frozenset(),
        )
        monkeypatch.setattr(tool_policy, "_active", policy)
    async with group_sdk_client() as client:
        result = await client.call_tool("update_group", {"chat_jid": GROUP, "name": "New group", "description": "new"})
    assert result.is_error is True
    assert json.loads(result.content[0].text)["error"]["code"] == "denied"
    assert observed == []
