"""_bridge_request: every bridge call carries a timeout; connection errors retry, read timeouts do not."""

import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx
import pytest

import whatsapp
from errors import ToolError


def test_bridge_requests_never_send_credentials_to_environment_proxy(monkeypatch):
    bridge_calls, proxy_calls = [], []

    def handler(calls):
        class Receiver(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append((self.command, self.path, self.headers.get("Authorization")))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"{}")

            def do_POST(self):
                self.do_GET()

            def log_message(self, *_args):
                pass

        return Receiver

    bridge = ThreadingHTTPServer(("127.0.0.1", 0), handler(bridge_calls))
    proxy = ThreadingHTTPServer(("127.0.0.1", 0), handler(proxy_calls))
    threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in (bridge, proxy)]
    for thread in threads:
        thread.start()
    transport = whatsapp._BridgeHTTP()
    monkeypatch.setattr(whatsapp, "bridge_http", transport)
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{bridge.server_port}/api")
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "fake-proxy-test-token-0123456789")
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        monkeypatch.setenv(name, f"http://127.0.0.1:{proxy.server_port}")
    for name in ("NO_PROXY", "no_proxy"):
        monkeypatch.setenv(name, "")
    try:
        assert whatsapp._bridge_request("GET", "/health").status_code == 200
        assert whatsapp._bridge_request("POST", "/send", json={}).status_code == 200
        assert not proxy_calls, "a bridge request reached the environment proxy"
        assert bridge_calls == [
            ("GET", "/api/health", "Bearer fake-proxy-test-token-0123456789"),
            ("POST", "/api/send", "Bearer fake-proxy-test-token-0123456789"),
        ]
    finally:
        transport._client_or_new().close()
        for server in (bridge, proxy):
            server.shutdown()
            server.server_close()
        for thread in threads:
            thread.join(timeout=5)


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

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake)
    monkeypatch.setattr(whatsapp.bridge_http, "get", fake)
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
        raise httpx.ConnectError("refused")

    monkeypatch.setattr(whatsapp.bridge_http, "post", refused)
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
        raise httpx.ReadTimeout("slow")

    monkeypatch.setattr(whatsapp.bridge_http, "post", slow)
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
            raise httpx.ConnectError("reset")
        return _Resp()

    monkeypatch.setattr(whatsapp.bridge_http, "get", flaky)
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

    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: Sent())
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    ok, msg, sent = whatsapp.send_message("5511999999999", "hi")
    assert ok and sent == {
        "message_id": "3EB0ABC",
        "chat_jid": "5511999999999@s.whatsapp.net",
        "timestamp": "2026-09-04T12:00:00Z",
    }


class _Failed:
    """A bridge failure body: `message` at the top, `error: {code, message}` under it."""

    def __init__(self, status: int, code: str, message: str) -> None:
        self.status_code = status
        self.text = message
        self._body = {"success": False, "message": message, "error": {"code": code, "message": message}}

    def json(self):
        return self._body


def test_the_bridge_can_name_a_code_its_status_does_not_carry(monkeypatch, paired_dbs):
    """`/api/download` answers 500 for both a CDN failure and a file the phone lost (#378)."""
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(whatsapp, "_policy_denied", lambda *_a, **_k: None, raising=False)

    gone = _Failed(500, "media_unavailable", "Failed to download media: sender's phone declined media retry: NOT_FOUND")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: gone)
    with pytest.raises(ToolError) as exc:
        whatsapp.download_media("MSG1", "5511999999999@s.whatsapp.net")
    assert exc.value.code == "media_unavailable"
    assert "NOT_FOUND" in exc.value.message

    refused = _Failed(500, "media_refused", "unsafe message identity")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: refused)
    with pytest.raises(ToolError) as exc:
        whatsapp.download_media("MSG1", "5511999999999@s.whatsapp.net")
    assert exc.value.code == "media_refused"

    # Without a name, the status still decides.
    cdn = _Failed(500, "internal", "Failed to download media: CDN says 410")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: cdn)
    with pytest.raises(ToolError) as exc:
        whatsapp.download_media("MSG1", "5511999999999@s.whatsapp.net")
    assert exc.value.code == "bridge_unavailable"

    # A code this server does not know is not passed through.
    odd = _Failed(500, "teapot", "Failed to download media: ?")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: odd)
    with pytest.raises(ToolError) as exc:
        whatsapp.download_media("MSG1", "5511999999999@s.whatsapp.net")
    assert exc.value.code == "bridge_unavailable"
