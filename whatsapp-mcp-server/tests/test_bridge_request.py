"""_bridge_request: every bridge call carries a timeout; connection errors retry, read timeouts do not."""

import pytest
import requests

import whatsapp
from errors import ToolError


class _Resp:
    status_code = 200

    def json(self):
        return {"success": True, "message": "ok"}

    text = "ok"


def test_every_bridge_call_passes_a_timeout(monkeypatch, tmp_path):
    seen = []

    def fake(url, **kwargs):
        seen.append((url.rsplit("/", 1)[-1], kwargs.get("timeout")))
        return _Resp()

    monkeypatch.setattr(whatsapp.requests, "post", fake)
    monkeypatch.setattr(whatsapp.requests, "get", fake)
    monkeypatch.setattr(whatsapp, "_policy_denied", lambda *_a, **_k: None, raising=False)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)

    whatsapp.send_message("5511999999999", "hi")
    whatsapp.send_reaction("5511999999999", "MSG1", "👍")
    whatsapp.mark_messages_read(["MSG1"], "5511999999999@s.whatsapp.net")
    whatsapp.download_media("MSG1", "5511999999999@s.whatsapp.net")
    whatsapp.get_group_members("120363000000000001@g.us")
    whatsapp.get_poll_results("MSG1", "5511999999999@s.whatsapp.net")
    whatsapp.delete_message("5511999999999@s.whatsapp.net", "MSG1", False)

    assert seen, "no bridge call recorded"
    assert all(timeout is not None and timeout > 0 for _, timeout in seen), seen
    by_path = dict(seen)
    assert by_path["download"] == whatsapp.BRIDGE_MEDIA_TIMEOUT_S
    assert by_path["send"] == whatsapp._bridge_timeout()


def test_timeout_env_override(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TIMEOUT_S", "7.5")
    assert whatsapp._bridge_timeout() == 7.5
    monkeypatch.setenv("WHATSAPP_BRIDGE_TIMEOUT_S", "nope")
    assert whatsapp._bridge_timeout() == 30.0
    monkeypatch.setenv("WHATSAPP_BRIDGE_TIMEOUT_S", "-1")
    assert whatsapp._bridge_timeout() == 30.0


def test_connection_errors_retry_then_raise(monkeypatch):
    calls = []
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: calls.append(("sleep", s)))

    def refused(url, **kwargs):
        calls.append(("post", url))
        raise requests.ConnectionError("refused")

    monkeypatch.setattr(whatsapp.requests, "post", refused)
    with pytest.raises(ToolError) as exc:
        whatsapp._bridge_request("POST", "/send", json={})
    assert exc.value.code == "bridge_unavailable"
    posts = [c for c in calls if c[0] == "post"]
    sleeps = [c for c in calls if c[0] == "sleep"]
    assert len(posts) == whatsapp.BRIDGE_CONNECT_RETRIES + 1
    assert len(sleeps) == whatsapp.BRIDGE_CONNECT_RETRIES


def test_read_timeout_is_not_retried(monkeypatch):
    calls = []

    def slow(url, **kwargs):
        calls.append(url)
        raise requests.exceptions.ReadTimeout("slow")

    monkeypatch.setattr(whatsapp.requests, "post", slow)
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: (_ for _ in ()).throw(AssertionError("no sleep")))
    with pytest.raises(ToolError) as exc:
        whatsapp._bridge_request("POST", "/send", json={})
    assert exc.value.code == "bridge_unavailable"
    assert len(calls) == 1, "a POST that may have reached the bridge must not be re-sent"


def test_connection_error_recovers(monkeypatch):
    attempts = []
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: None)

    def flaky(url, **kwargs):
        attempts.append(url)
        if len(attempts) == 1:
            raise requests.ConnectionError("reset")
        return _Resp()

    monkeypatch.setattr(whatsapp.requests, "get", flaky)
    resp = whatsapp._bridge_request("GET", "/poll", params={})
    assert resp.status_code == 200 and len(attempts) == 2


def test_send_message_returns_bridge_message_id(monkeypatch):
    class Sent:
        status_code = 200
        text = ""

        def json(self):
            return {
                "success": True,
                "message": "Message sent",
                "message_id": "3EB0ABC",
                "chat_jid": "5511999999999@s.whatsapp.net",
                "timestamp": "2026-09-04T12:00:00Z",
            }

    monkeypatch.setattr(whatsapp.requests, "post", lambda url, **kwargs: Sent())
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    ok, msg, sent = whatsapp.send_message("5511999999999", "hi")
    assert ok and sent == {
        "message_id": "3EB0ABC",
        "chat_jid": "5511999999999@s.whatsapp.net",
        "timestamp": "2026-09-04T12:00:00Z",
    }
