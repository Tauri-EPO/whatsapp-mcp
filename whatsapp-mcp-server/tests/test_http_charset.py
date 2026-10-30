"""HTTP responses declare charset=utf-8 (issue #758).

The bodies were always UTF-8, but `text/event-stream` and `application/json` went out
without a charset, and a client that falls back to ISO-8859-1 showed `histórico` as
`histÃ³rico`. These tests read the bytes as the header declares them."""

import asyncio
import json
import os

import httpx
import pytest
from mcp.server.mcpserver import MCPServer
from starlette.testclient import TestClient

import main
from http_auth import Utf8CharsetMiddleware
from mcp_config import resolve_transport

TOKEN = "fake-mcp-token-0123456789"
ACCENTED = "histórico → você"
HEADERS = {"Authorization": f"Bearer {TOKEN}", "Accept": "application/json, text/event-stream"}


pytestmark = pytest.mark.usefixtures("runtime_archive")


def _server():
    server = MCPServer("charset-test")

    @server.tool()
    def echo_accents() -> str:
        """Return text with accents: histórico → você."""
        return ACCENTED

    return server


def _app(json_response: bool):
    return main.build_http_app(
        _server(), "streamable-http", TOKEN, host="0.0.0.0", json_response=json_response, stateless_http=True
    )


def _rpc(client, method: str, params: dict | None = None):
    response = client.post(
        "/mcp", headers=HEADERS, json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}
    )
    assert response.status_code == 200, response.text
    content_type = response.headers["content-type"]
    assert "charset=utf-8" in content_type, f"Missing UTF-8 charset in Content-Type: {content_type!r}"
    charset = content_type.split("charset=", 1)[1]
    text = response.content.decode(charset)  # read the bytes the way the header tells a client to
    if content_type.startswith("text/event-stream"):
        text = next(line[len("data: ") :] for line in text.splitlines() if line.startswith("data: "))
    return content_type, json.loads(text)["result"]


def test_rpc_reports_missing_charset_clearly():
    transport = httpx.MockTransport(
        lambda request: httpx.Response(200, headers={"content-type": "application/json"}, content=b'{"result": {}}')
    )
    with httpx.Client(transport=transport, base_url="http://testserver") as client:
        with pytest.raises(AssertionError, match="Missing UTF-8 charset in Content-Type: 'application/json'"):
            _rpc(client, "tools/list")


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


@pytest.mark.parametrize("rate_limit", [False, True])
def test_middleware_covers_own_json_replies_without_literal_charset(rate_limit):
    app = main.build_http_app(
        MCPServer("charset-test"), "streamable-http", TOKEN, rate_limit_per_minute=1 if rate_limit else 0
    )
    with TestClient(app) as client:
        response = client.post("/mcp", headers={"Accept": "application/json"}, json={})
        assert response.status_code == 401
        assert response.headers["content-type"] == "application/json; charset=utf-8"
        if rate_limit:
            response = client.post("/mcp", headers={"Accept": "application/json"}, json={})
    assert response.status_code == (429 if rate_limit else 401)
    assert response.headers["content-type"] == "application/json; charset=utf-8"


@pytest.mark.asyncio
async def test_legacy_sse_stream_and_message_posts_declare_utf8(monkeypatch):
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "sse")
    app = main.build_http_app(_server(), resolve_transport(os.getenv("WHATSAPP_MCP_TRANSPORT")), TOKEN, host="0.0.0.0")
    incoming, outgoing = asyncio.Queue(), asyncio.Queue()
    await incoming.put({"type": "http.request", "body": b""})
    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": "GET",
        "scheme": "http",
        "path": "/sse",
        "raw_path": b"/sse",
        "root_path": "",
        "query_string": b"",
        "headers": [
            (b"host", b"testserver"),
            (b"authorization", f"Bearer {TOKEN}".encode()),
            (b"accept", b"text/event-stream"),
        ],
        "client": ("127.0.0.1", 12345),
        "server": ("testserver", 80),
    }

    async def event():
        message = await outgoing.get()
        assert message["type"] == "http.response.body", message
        lines = message["body"].decode("utf-8").splitlines()
        return next(line.removeprefix("data: ") for line in lines if line.startswith("data: "))

    stream = asyncio.create_task(app(scope, incoming.get, outgoing.put))
    try:
        async with (
            asyncio.timeout(10),
            httpx.AsyncClient(transport=httpx.ASGITransport(app), base_url="http://testserver") as client,
        ):
            start = await outgoing.get()
            assert start["status"] == 200
            assert dict(start["headers"])[b"content-type"] == b"text/event-stream; charset=utf-8"
            endpoint = await event()
            denied = await client.post(endpoint, json={})
            assert denied.status_code == 401
            assert denied.headers["content-type"] == "application/json; charset=utf-8"

            async def post(message):
                response = await client.post(endpoint, headers=HEADERS, json=message)
                assert response.status_code == 202, response.text
                # The SDK's accepted POST has no media type; JSON/SSE replies must declare UTF-8.
                content_type = response.headers.get("content-type", "")
                if content_type.startswith(("application/json", "text/event-stream")):
                    assert "charset=utf-8" in content_type, content_type

            await post(
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": "2025-03-26",
                        "capabilities": {},
                        "clientInfo": {"name": "charset-test", "version": "1"},
                    },
                }
            )
            assert "result" in json.loads(await event())
            await post({"jsonrpc": "2.0", "method": "notifications/initialized"})
            await post({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
            [tool] = json.loads(await event())["result"]["tools"]
            assert tool["description"] == "Return text with accents: histórico → você."
            await post(
                {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "echo_accents", "arguments": {}}}
            )
            assert json.loads(await event())["result"]["content"][0]["text"] == ACCENTED
    finally:
        await incoming.put({"type": "http.disconnect"})
        try:
            await asyncio.wait_for(stream, timeout=5)
        finally:
            if not stream.done():
                stream.cancel()


@pytest.mark.parametrize(
    ("sent", "expected"),
    [
        (b"application/json", b"application/json; charset=utf-8"),
        (b"text/event-stream", b"text/event-stream; charset=utf-8"),
        (b"Application/JSON", b"Application/JSON; charset=utf-8"),
        (b"application/problem+json", b"application/problem+json; charset=utf-8"),
        (
            b"Application/Vnd.Example+JSON; profile=example",
            b"Application/Vnd.Example+JSON; profile=example; charset=utf-8",
        ),
        (b"application/*+json", b"application/*+json; charset=utf-8"),
        (b"application/problem+json; charset=utf-8", b"application/problem+json; charset=utf-8"),
        (b"text/example+json", b"text/example+json"),
        (b"application/json;", b"application/json; charset=utf-8"),
        (b"application/json; \t; ", b"application/json; charset=utf-8"),
        (b"text/event-stream \t", b"text/event-stream; charset=utf-8"),
        (b"application/json; xcharset=1", b"application/json; xcharset=1; charset=utf-8"),
        (
            b'application/json; profile="urn:charset=example"',
            b'application/json; profile="urn:charset=example"; charset=utf-8',
        ),
        (
            b'application/json; profile="urn:example;charset=legacy"',
            b'application/json; profile="urn:example;charset=legacy"; charset=utf-8',
        ),
        (
            b'application/json; profile="urn:example;\\"charset=legacy"',
            b'application/json; profile="urn:example;\\"charset=legacy"; charset=utf-8',
        ),
        (b"application/json; charset*=utf-8", b"application/json; charset*=utf-8; charset=utf-8"),
        (b"application/json; CHARSET = utf-8", b"application/json; CHARSET = utf-8"),
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
