import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

CHAT = "5511888888888@s.whatsapp.net"


class Resp:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def test_delete_message_posts_expected_payload(monkeypatch):
    calls = []

    def fake_post(url, json=None, headers=None, timeout=None):
        calls.append((url, json, headers))
        return Resp(payload={"success": True, "message": "Message deleted for everyone", "for_everyone": True})

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    ok, msg = whatsapp.delete_message(CHAT, "m1", for_everyone=True)

    assert ok is True and msg == "Message deleted for everyone"
    assert calls[0][0].endswith("/delete")
    assert calls[0][1] == {"chat_jid": CHAT, "message_id": "m1", "for_everyone": True}
    assert calls[0][2] == {"Authorization": "Bearer test-token-0123456789"}


def test_delete_message_default_is_local_only(monkeypatch):
    seen = {}
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda url, json=None, **k: seen.update(json) or Resp(payload={"success": True, "message": "ok"}),
    )
    assert whatsapp.delete_message(CHAT, "m1")[0] is True
    assert seen["for_everyone"] is False


def test_delete_message_bridge_refusal_is_surfaced(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *a, **k: Resp(
            403, {"success": False, "message": "Only messages sent by this account can be deleted for everyone"}
        ),
    )
    with pytest.raises(ToolError) as exc:
        whatsapp.delete_message(CHAT, "theirs", for_everyone=True)
    assert exc.value.code == "denied" and "Only messages sent by this account" in exc.value.message


def test_delete_message_validation_and_policy(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    for chat, mid in (("", "m1"), (CHAT, "  ")):
        with pytest.raises(ToolError) as exc:
            whatsapp.delete_message(chat, mid)
        assert exc.value.code == "invalid_argument"
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    with pytest.raises(ToolError, match="WHATSAPP_ALLOWED_CHATS") as exc:
        whatsapp.delete_message(CHAT, "m1")
    assert exc.value.code == "denied"


def test_delete_message_tool_shape(monkeypatch):
    import main

    monkeypatch.setattr(main, "whatsapp_delete_message", lambda c, m, f: (True, "done"))
    assert main.delete_message(CHAT, "m1", True) == {"success": True, "message": "done", "for_everyone": True}
