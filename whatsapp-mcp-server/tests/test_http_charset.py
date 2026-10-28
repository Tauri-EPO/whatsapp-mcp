"""HTTP responses declare charset=utf-8 (issue #758).

The bodies were always UTF-8, but `text/event-stream` and `application/json` went out
without a charset, and a client that falls back to ISO-8859-1 showed `histórico` as
`histÃ³rico`. These tests read the bytes as the header declares them."""

import asyncio
import json
import re
from pathlib import Path

import pytest
from mcp.server.mcpserver import MCPServer
from starlette.testclient import TestClient

import main
import runtime_settings
from http_auth import Utf8CharsetMiddleware

TOKEN = "fake-mcp-token-0123456789"
ACCENTED = "histórico → você"
HEADERS = {"Authorization": f"Bearer {TOKEN}", "Accept": "application/json, text/event-stream"}


@pytest.fixture(autouse=True)
def runtime_archive(paired_dbs, monkeypatch):
    """The bearer check reads the runtime settings table (token rotation): same setup as test_runtime_settings."""
    bridge = Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "runtime_settings.go"
    schema = re.search(r"const runtimeSettingsSchema = `([^`]+)`", bridge.read_text()).group(1)
    with paired_dbs.messages() as conn:
        conn.executescript(schema)
    monkeypatch.setattr(runtime_settings, "_startup_env", None)
    monkeypatch.setattr(runtime_settings, "_warned", {})
    return paired_dbs


def _app(json_response: bool):
    server = MCPServer("charset-test")

    @server.tool()
    def echo_accents() -> str:
        """Return text with accents: histórico → você."""
        return ACCENTED

    return main.build_http_app(
        server, "streamable-http", TOKEN, host="0.0.0.0", json_response=json_response, stateless_http=True
    )


def _rpc(client, method: str, params: dict | None = None):
    response = client.post(
        "/mcp", headers=HEADERS, json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}
    )
    assert response.status_code == 200, response.text
    content_type = response.headers["content-type"]
    charset = content_type.split("charset=", 1)[1]
    text = response.content.decode(charset)  # read the bytes the way the header tells a client to
    if content_type.startswith("text/event-stream"):
        text = next(line[len("data: ") :] for line in text.splitlines() if line.startswith("data: "))
    return content_type, json.loads(text)["result"]


@pytest.mark.parametrize(("json_response", "media_type"), [(True, "application/json"), (False, "text/event-stream")])
def test_tools_list_and_call_declare_utf8_and_keep_accents(json_response, media_type):
    with TestClient(_app(json_response)) as client:
        content_type, listing = _rpc(client, "tools/list")
        assert content_type == f"{media_type}; charset=utf-8"
        [tool] = listing["tools"]
        assert tool["description"] == "Return text with accents: histórico → você."

        content_type, result = _rpc(client, "tools/call", {"name": "echo_accents", "arguments": {}})
        assert content_type == f"{media_type}; charset=utf-8"
        assert result["content"][0]["text"] == ACCENTED


def test_own_json_replies_declare_utf8():
    with TestClient(_app(True)) as client:
        response = client.post("/mcp", headers={"Accept": "application/json"}, json={})
    assert response.status_code == 401
    assert response.headers["content-type"] == "application/json; charset=utf-8"


@pytest.mark.parametrize(
    ("sent", "expected"),
    [
        (b"application/json", b"application/json; charset=utf-8"),
        (b"text/event-stream", b"text/event-stream; charset=utf-8"),
        (b"Application/JSON", b"Application/JSON; charset=utf-8"),
        (b"application/json; charset=utf-8", b"application/json; charset=utf-8"),
        (b"application/json; charset=iso-8859-1", b"application/json; charset=iso-8859-1"),  # declared: left alone
        (b"text/plain; version=0.0.4", b"text/plain; version=0.0.4"),  # other types untouched
    ],
)
def test_middleware_adds_only_a_missing_charset_and_keeps_the_body(sent, expected):
    body = "histórico".encode()
    messages = []

    async def app(scope, receive, send):
        await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", sent), (b"x-a", b"1")]})
        await send({"type": "http.response.body", "body": body})

    async def send(message):
        messages.append(message)

    async def receive():
        return {"type": "http.request"}

    asyncio.run(Utf8CharsetMiddleware(app)({"type": "http"}, receive, send))
    assert messages[0]["headers"] == [(b"content-type", expected), (b"x-a", b"1")]
    assert messages[1] == {"type": "http.response.body", "body": body}
