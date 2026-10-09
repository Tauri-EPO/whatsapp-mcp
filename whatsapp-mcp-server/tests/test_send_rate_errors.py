"""Send refusal crosses real HTTP and MCP error consumers with retry metadata."""

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

import whatsapp
from errors import ToolError, tool_errors


def test_send_429_real_http_tool_envelope(monkeypatch):
    class Refusal(BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "37")
            self.end_headers()
            self.wfile.write(
                json.dumps(
                    {"success": False, "error": "send_rate_limited", "limit": "per_day", "retry_after_s": 37}
                ).encode()
            )

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Refusal)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{server.server_port}/api")
    monkeypatch.setattr(whatsapp, "bridge_http", whatsapp._BridgeHTTP())
    try:
        with pytest.raises(ToolError) as error:
            whatsapp._bridge_json(
                whatsapp._bridge_request("POST", "/send", json={"recipient": "5511999999999", "message": "hello"})
            )
        assert error.value.code == "rate_limited"
        assert error.value.extra == {"retry_after_s": 37, "limit": "per_day"}
        assert "stop and report" in error.value.message

        @tool_errors
        def call():
            raise error.value

        result = call()
        assert result["error"]["code"] == "rate_limited"
        assert result["retry_after_s"] == 37
    finally:
        if whatsapp.bridge_http._client is not None:
            whatsapp.bridge_http._client.close()
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def test_oversized_batch_has_no_finite_retry(monkeypatch):
    import httpx

    response = httpx.Response(
        429, json={"error": "send_rate_limited", "limit": "limit_exceeds_batch", "retry_after_s": None}
    )
    with pytest.raises(ToolError) as error:
        whatsapp._bridge_json(response)
    assert error.value.code == "rate_limited"
    assert error.value.extra["retry_after_s"] is None
    assert "split the batch" in error.value.message
