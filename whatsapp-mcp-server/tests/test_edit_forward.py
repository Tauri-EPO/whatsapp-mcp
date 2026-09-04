"""edit_message / forward_message: payloads, validation, allow-list on both ends."""

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

CHAT = "5511999999999@s.whatsapp.net"


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload if payload is not None else {"success": True, "message_id": "X"}
        self.text = ""

    def json(self):
        return self._payload


def test_payloads(monkeypatch):
    seen = []
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda url, **kw: seen.append((url.rsplit("/api", 1)[-1], kw.get("json"), kw.get("timeout"))) or Resp(),
    )
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    assert main.edit_message(CHAT, "M1", "fixed")["success"] is True
    assert seen[-1][:2] == ("/edit", {"chat_jid": CHAT, "message_id": "M1", "text": "fixed"})
    out = main.forward_message(CHAT, "M1", "120363000000000001@g.us")
    assert out["message_id"] == "X"
    assert seen[-1] == (
        "/forward",
        {"chat_jid": CHAT, "message_id": "M1", "to_chat_jid": "120363000000000001@g.us"},
        whatsapp.BRIDGE_MEDIA_TIMEOUT_S,
    )


def test_validation(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    assert main.edit_message(CHAT, "M1", "  ")["error"]["code"] == "invalid_argument"
    assert main.edit_message("", "M1", "x")["error"]["code"] == "invalid_argument"
    assert main.forward_message(CHAT, "M1", "")["error"]["code"] == "invalid_argument"


def test_allow_list_covers_destination(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError) as exc:
        whatsapp.forward_message(CHAT, "M1", "5511000000000")
    assert exc.value.code == "denied"
    with pytest.raises(ToolError) as exc:
        whatsapp.edit_message("5511000000000@s.whatsapp.net", "M1", "x")
    assert exc.value.code == "denied"


def test_bridge_refusals_map(monkeypatch):
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *a, **k: Resp(403, {"success": False, "message": "Only messages sent by this account can be edited"}),
    )
    out = main.edit_message(CHAT, "THEIRS", "x")
    assert out["error"]["code"] == "denied" and "Only messages" in out["error"]["message"]
    monkeypatch.setattr(
        whatsapp.bridge_http, "post", lambda *a, **k: Resp(404, {"success": False, "message": "not found"})
    )
    assert main.forward_message(CHAT, "NOPE", "5511888888888")["error"]["code"] == "not_found"
