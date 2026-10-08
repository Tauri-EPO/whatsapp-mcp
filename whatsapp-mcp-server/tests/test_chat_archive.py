"""Archive tool payloads and refusals, from model-facing entry to bridge HTTP."""

import pytest

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from tool_policy import ToolPolicy

CHAT = "5511999999999@s.whatsapp.net"


@pytest.mark.parametrize("archived", [True, False])
def test_archive_payload(monkeypatch, archived):
    seen = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "archived": archived}

    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kw: seen.append((url, kw)) or Response())
    assert main.archive_chat(" " + CHAT + " ", archived) == {"success": True, "archived": archived}
    assert seen[0][0].endswith("/api/chat/archive")
    assert seen[0][1]["json"] == {"chat_jid": CHAT, "archived": archived}
    assert seen[0][1]["headers"]["Authorization"] == "Bearer " + "t" * 32


@pytest.mark.parametrize(
    "policy",
    [
        ToolPolicy(read_only=True),
        ToolPolicy(allow=frozenset({"send_message"})),
        ToolPolicy(deny=frozenset({"archive_chat"})),
    ],
)
def test_archive_tool_refusals(monkeypatch, policy):
    monkeypatch.setattr(tool_policy, "_active", policy)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.archive_chat(CHAT)["error"]["code"] == "denied"


def test_archive_chat_policy_and_validation(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888"]))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.archive_chat(CHAT)["error"]["code"] == "denied"
    assert main.archive_chat("")["error"]["code"] == "invalid_argument"


@pytest.mark.parametrize("status,code", [(404, "not_found"), (503, "bridge_unavailable")])
def test_archive_bridge_errors(monkeypatch, status, code):
    class Response:
        status_code = status
        text = ""

        def json(self):
            return {"success": False, "error": {"code": code, "message": "archive refused"}}

    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: Response())
    assert main.archive_chat(CHAT)["error"]["code"] == code
