"""block_contact, get_blocklist and delete_chat: payloads, refusals, bridge errors."""

import pytest

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from tool_policy import ToolPolicy

pytestmark = pytest.mark.usefixtures("auth_runtime_store")

CHAT = "5511999999999@s.whatsapp.net"
LID = "100000000000001@lid"


class Response:
    def __init__(self, payload, status=200):
        self.status_code = status
        self.text = ""
        self._payload = payload

    def json(self):
        return self._payload


def _capture(monkeypatch, method, payload, status=200):
    seen = []
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http, method, lambda url, **kw: seen.append((url, kw)) or Response(payload, status)
    )
    return seen


@pytest.mark.parametrize("jid,blocked", [("5511999999999", True), (" " + LID + " ", False), (CHAT, True)])
def test_block_contact_payload(monkeypatch, jid, blocked):
    seen = _capture(monkeypatch, "post", {"success": True, "blocked": blocked, "blocklist": []})
    assert main.block_contact(jid, blocked)["blocked"] is blocked
    assert seen[0][0].endswith("/api/block")
    assert seen[0][1]["json"] == {"jid": jid.strip(), "blocked": blocked}


@pytest.mark.parametrize(
    "jid", ["", "120363000000000001@g.us", "status@broadcast", "bob", "@lid", "5511999999999@s.whatsapp.net@g.us"]
)
def test_block_contact_rejects_non_contacts(monkeypatch, jid):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.block_contact(jid)["error"]["code"] == "invalid_argument"


def test_block_contact_allow_list(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888"]))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.block_contact(CHAT)["error"]["code"] == "denied"


def test_get_blocklist(monkeypatch):
    payload = {"success": True, "blocklist": [{"lid": LID, "phone_jid": CHAT}]}
    seen = _capture(monkeypatch, "get", payload)
    assert main.get_blocklist() == payload
    assert seen[0][0].endswith("/api/blocklist")


def test_delete_chat_payload(monkeypatch):
    seen = _capture(monkeypatch, "post", {"success": True, "deleted": True, "local_messages_removed": 3})
    assert main.delete_chat(CHAT, delete_media=True, confirm=True)["local_messages_removed"] == 3
    assert seen[0][0].endswith("/api/chat/delete")
    assert seen[0][1]["json"] == {"chat_jid": CHAT, "delete_media": True, "confirm": True}


@pytest.mark.parametrize("confirm", [False, "true", 1])
def test_delete_chat_requires_confirm_true(monkeypatch, confirm):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    result = main.delete_chat(CHAT, confirm=confirm)
    assert result["error"]["code"] == "invalid_argument"


def test_delete_chat_validation_and_allow_list(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.delete_chat("", confirm=True)["error"]["code"] == "invalid_argument"
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888"]))
    assert main.delete_chat(CHAT, confirm=True)["error"]["code"] == "denied"


@pytest.mark.parametrize(
    "policy",
    [
        ToolPolicy(read_only=True),
        ToolPolicy(allow=frozenset({"archive_chat"})),
        ToolPolicy(deny=frozenset({"delete_chat", "block_contact"})),
    ],
)
def test_mutating_tools_refused_by_policy(monkeypatch, policy):
    monkeypatch.setattr(tool_policy, "_active", policy)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.delete_chat(CHAT, confirm=True)["error"]["code"] == "denied"
    assert main.block_contact(CHAT)["error"]["code"] == "denied"


@pytest.mark.parametrize("status,code", [(404, "not_found"), (408, "bridge_unavailable"), (502, "bridge_unavailable")])
def test_delete_chat_bridge_errors(monkeypatch, status, code):
    _capture(monkeypatch, "post", {"success": False, "error": {"code": code, "message": "refused"}}, status)
    assert main.delete_chat(CHAT, confirm=True)["error"]["code"] == code


def test_delete_chat_is_annotated_destructive():
    tool = main.mcp._tool_manager.get_tool("delete_chat")
    assert tool.annotations is not None and tool.annotations.destructive_hint is True
