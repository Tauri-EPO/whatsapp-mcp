"""request_history: POST /api/history through the bridge client."""

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


def test_request_history_posts_expected_payload(monkeypatch):
    calls = []

    def fake_post(url, json=None, headers=None, timeout=None):
        calls.append((url, json, headers))
        return Resp(
            payload={
                "success": True,
                "message": "Requested up to 50 messages older than 2026-06-07T10:00:00Z for " + CHAT,
            }
        )

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    result = whatsapp.request_history(CHAT)

    assert calls[0][0].endswith("/history")
    assert calls[0][1] == {"chat_jid": CHAT, "count": 50}
    assert calls[0][2] == {"Authorization": "Bearer test-token-0123456789"}
    assert result["success"] is True
    assert result["chat_jid"] == CHAT and result["requested_count"] == 50
    assert "Requested up to 50 messages older than" in result["message"]
    assert "asynchronous" in result["note"] and "list_messages" in result["note"]


def test_request_history_clamps_and_validates_count(monkeypatch):
    seen = {}
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda url, json=None, **k: seen.update(json) or Resp(payload={"success": True, "message": "ok"}),
    )

    assert whatsapp.request_history(CHAT, count=5000)["requested_count"] == whatsapp.HISTORY_MAX_COUNT
    assert seen["count"] == whatsapp.HISTORY_MAX_COUNT

    for bad in (0, -1, "many"):
        with pytest.raises(ToolError) as exc:
            whatsapp.request_history(CHAT, count=bad)  # type: ignore[arg-type]
        assert exc.value.code == "invalid_argument"


def test_request_history_no_anchor_message_is_not_found(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *a, **k: Resp(
            404,
            {
                "success": False,
                "message": "No stored message for this chat to anchor the request; send or receive one message first",
            },
        ),
    )
    with pytest.raises(ToolError) as exc:
        whatsapp.request_history(CHAT)
    assert exc.value.code == "not_found"
    assert "anchor the request" in exc.value.message


def test_request_history_not_connected_is_bridge_unavailable(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *a, **k: Resp(503, {"success": False, "message": "Not connected to WhatsApp"}),
    )
    with pytest.raises(ToolError) as exc:
        whatsapp.request_history(CHAT)
    assert exc.value.code == "bridge_unavailable"
    assert exc.value.message == "Not connected to WhatsApp"


def test_request_history_validation_and_policy(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError) as exc:
        whatsapp.request_history("   ")
    assert exc.value.code == "invalid_argument"

    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    with pytest.raises(ToolError, match="WHATSAPP_ALLOWED_CHATS") as exc:
        whatsapp.request_history(CHAT)
    assert exc.value.code == "denied"


def test_request_history_tool_envelope_on_error(monkeypatch):
    import main

    monkeypatch.setattr(
        main,
        "whatsapp_request_history",
        lambda chat_jid, count: (_ for _ in ()).throw(ToolError("not_found", "no anchor")),
    )
    assert main.request_history(CHAT) == {"error": {"code": "not_found", "message": "no anchor"}}


def test_request_history_tool_passes_arguments(monkeypatch):
    import main

    monkeypatch.setattr(main, "whatsapp_request_history", lambda chat_jid, count: {"chat_jid": chat_jid, "n": count})
    assert main.request_history(CHAT, 200) == {"chat_jid": CHAT, "n": 200}
