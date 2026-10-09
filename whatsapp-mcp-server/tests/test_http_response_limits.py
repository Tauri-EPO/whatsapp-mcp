"""Real HTTP compression, header trickling and deep-JSON denial regressions."""

import gzip
import threading
import time
import tracemalloc
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from starlette.testclient import TestClient

import http_oauth
import transcribe
from tests.test_http_oauth import AUDIENCE, HEADERS, STATIC, app, configure, signed
from tests.test_http_oauth import isolated_tool_registry as isolated_tool_registry
from tests.test_http_oauth import issuer as issuer


@pytest.fixture
def endpoint():
    state = {"body": b"{}", "encoding": None, "slow_head": False, "accept_encoding": []}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def reply(self):
            state["accept_encoding"].append(self.headers.get("Accept-Encoding"))
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            if state["encoding"]:
                self.send_header("Content-Encoding", state["encoding"])
            self.end_headers()
            try:
                self.wfile.write(state["body"])
            except (BrokenPipeError, ConnectionResetError):
                pass

        def do_GET(self):
            self.reply()

        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            self.reply()

        def do_HEAD(self):
            if not state["slow_head"]:
                self.reply()
                return
            pieces = [b"HTTP/1.1 200 OK\r\n"] + [b"X-Tick: 1\r\n"] * 7 + [b"\r\n"]
            try:
                for piece in pieces:
                    self.wfile.write(piece)
                    self.wfile.flush()
                    time.sleep(0.15)
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = True
    state["url"] = f"http://127.0.0.1:{server.server_port}/response"
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield state
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


@pytest.mark.asyncio
@pytest.mark.parametrize("consumer", ["authorization", "transcription"])
async def test_compressed_http_response_refused_before_expansion(endpoint, tmp_path, consumer):
    endpoint["encoding"] = "gzip"
    endpoint["body"] = gzip.compress(b'{"text":"' + b"x" * (16 * 1024 * 1024) + b'"}')
    source = tmp_path / "prepared-part.ogg"
    source.write_bytes(b"fake-prepared-part")
    config = transcribe.WhisperConfig(endpoint["url"], None, "fake-model", "auto", 2, "openai_compatible")
    verifier = http_oauth.OAuthTokenVerifier(http_oauth.OAuthConfig(endpoint["url"], AUDIENCE))
    tracemalloc.start()
    try:
        if consumer == "authorization":
            with pytest.raises(http_oauth.AuthorizationUnavailableError):
                await verifier._fetch(endpoint["url"])
        else:
            with pytest.raises(transcribe.BackendUnavailableError):
                await transcribe._transcribe_http_request(source, config, "auto")
        _, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    assert peak < 4 * 1024 * 1024, f"compressed response allocated {peak} bytes before refusal"
    assert endpoint["accept_encoding"] == ["identity"]


def test_http_provider_probe_whole_request_deadline(endpoint, monkeypatch):
    endpoint["slow_head"] = True
    monkeypatch.setattr(transcribe, "STATUS_PROBE_TIMEOUT_S", 0.3)
    config = transcribe.WhisperConfig(endpoint["url"], None, "fake-model", "auto", 2, "openai_compatible")
    started = time.monotonic()
    assert transcribe.probe_http_provider(config) is False
    assert time.monotonic() - started < 0.75


@pytest.mark.asyncio
@pytest.mark.parametrize("consumer", ["authorization", "transcription"])
async def test_deep_json_backend_response_has_classified_error(endpoint, tmp_path, consumer):
    endpoint["body"] = b'{"text":' + b"[" * 10000 + b"0" + b"]" * 10000 + b"}"
    if consumer == "authorization":
        verifier = http_oauth.OAuthTokenVerifier(http_oauth.OAuthConfig(endpoint["url"], AUDIENCE))
        with pytest.raises(http_oauth.AuthorizationUnavailableError):
            await verifier._fetch(endpoint["url"])
    else:
        source = tmp_path / "prepared-part.ogg"
        source.write_bytes(b"fake-prepared-part")
        config = transcribe.WhisperConfig(endpoint["url"], None, "fake-model", "auto", 2, "openai_compatible")
        with pytest.raises(transcribe.TranscriptionError, match="invalid JSON"):
            await transcribe._transcribe_http_request(source, config, "auto")


def test_oauth_deep_json_preflight_keeps_sdk_parse_error(monkeypatch, issuer, auth_runtime_store):
    configure(monkeypatch, issuer)
    body = b'{"jsonrpc":"2.0","id":2,"method":"ping","params":{"nested":' + b"[" * 10000 + b"0" + b"]" * 10000 + b"}}"
    application, calls = app()
    with TestClient(application, raise_server_exceptions=False) as client:
        response = client.post("/mcp", content=body, headers={**HEADERS, "Authorization": "Bearer " + signed(issuer)})
    assert response.status_code == 400
    assert response.json()["error"]["code"] == -32700
    assert calls == []
    monkeypatch.delenv("WHATSAPP_MCP_OAUTH_ISSUER")
    application, _ = app()
    with TestClient(application, raise_server_exceptions=False) as client:
        legacy = client.post("/mcp", content=body, headers={**HEADERS, "Authorization": "Bearer " + STATIC})
    assert legacy.status_code == response.status_code
    assert legacy.content == response.content
