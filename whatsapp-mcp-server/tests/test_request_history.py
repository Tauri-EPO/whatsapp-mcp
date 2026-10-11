"""request_history: POST /api/history through the bridge client."""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

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
        lambda chat_jid, count, before_message_id: (_ for _ in ()).throw(ToolError("not_found", "no anchor")),
    )
    assert main.request_history(CHAT) == {"error": {"code": "not_found", "message": "no anchor"}}


def test_request_history_tool_passes_arguments(monkeypatch):
    import main

    monkeypatch.setattr(
        main, "whatsapp_request_history", lambda chat_jid, count, before_message_id: {"chat_jid": chat_jid, "n": count}
    )
    assert main.request_history(CHAT, 200) == {"chat_jid": CHAT, "n": 200}


@pytest.mark.parametrize("anchor", ["", "   ", 7])
def test_request_history_rejects_empty_anchor(monkeypatch, anchor):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError) as exc:
        whatsapp.request_history(CHAT, before_message_id=anchor)
    assert exc.value.code == "invalid_argument"


def test_request_history_anchor_denied(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge called"))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    with pytest.raises(ToolError) as exc:
        whatsapp.request_history(CHAT, before_message_id="M2")
    assert exc.value.code == "denied"


async def test_request_history_anchor_over_http(monkeypatch):
    """Real MCP argument dispatch -> HTTP -> captured body and error response."""
    import main

    calls = []

    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            calls.append((self.path, payload, self.headers.get("Authorization")))
            invalid = payload["before_message_id"] == "UNKNOWN"
            response = (
                {"success": False, "message": "before_message_id must identify a stored message in this chat"}
                if invalid
                else {"success": True, "message": "Requested target replay"}
            )
            raw = json.dumps(response).encode()
            self.send_response(400 if invalid else 200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{server.server_port}/api")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([]))
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    try:
        result = await main.mcp.call_tool("request_history", {"chat_jid": CHAT, "count": 7, "before_message_id": "M2"})
        assert not result.is_error
        assert result.structured_content["success"] is True
        assert "download_media" in result.structured_content["note"]
        invalid = await main.mcp.call_tool(
            "request_history", {"chat_jid": CHAT, "count": 7, "before_message_id": "UNKNOWN"}
        )
        assert invalid.structured_content["error"]["code"] == "invalid_argument"
        assert "in this chat" in invalid.structured_content["error"]["message"]
        assert calls == [
            (
                "/api/history",
                {"chat_jid": CHAT, "count": 7, "before_message_id": anchor},
                "Bearer test-token-0123456789",
            )
            for anchor in ("M2", "UNKNOWN")
        ]
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
