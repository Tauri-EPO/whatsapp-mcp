import pytest
from starlette.testclient import TestClient

from http_auth import RateLimitMiddleware, client_key, resolve_trusted_proxies


async def _ok_app(scope, receive, send):
    if scope["type"] == "lifespan":
        while True:
            message = await receive()
            if message["type"] == "lifespan.startup":
                await send({"type": "lifespan.startup.complete"})
            else:
                await send({"type": "lifespan.shutdown.complete"})
                return
    await send({"type": "http.response.start", "status": 200, "headers": []})
    await send({"type": "http.response.body", "body": b"ok"})


def scope(peer, *headers):
    return {"client": (peer, 1234), "headers": [(b"x-forwarded-for", value.encode()) for value in headers]}


@pytest.mark.parametrize(
    "peer,headers,expected",
    [
        ("192.0.2.5", ["198.51.100.1"], "192.0.2.5"),
        ("127.0.0.1", ["203.0.113.1, 198.51.100.1, 10.0.0.1"], "198.51.100.1"),
        ("127.0.0.1", ["203.0.113.2, 198.51.100.1", "10.0.0.1"], "198.51.100.1"),
        ("127.0.0.1", ["garbage, 198.51.100.1"], "127.0.0.1"),
        ("127.0.0.1", ["10.0.0.1"], "127.0.0.1"),
        ("::1", ["2001:db8::1"], "2001:db8::1"),
    ],
)
def test_chain(peer, headers, expected):
    assert client_key(scope(peer, *headers), resolve_trusted_proxies("loopback,10.0.0.0/8")) == expected


def test_default_does_not_trust_even_loopback():
    assert client_key(scope("127.0.0.1", "198.51.100.1")) == "127.0.0.1"


@pytest.mark.parametrize("value", ["any", "*", "10.0.0.1/8", "loopback,invalid"])
def test_invalid_config_fails_closed(value):
    with pytest.raises(ValueError, match="WHATSAPP_MCP_TRUSTED_PROXIES"):
        resolve_trusted_proxies(value)


def test_appending_proxy_does_not_let_leftmost_spoof_bypass_bucket():
    limiter = RateLimitMiddleware(
        _ok_app, 1, clock=lambda: 0, trusted_proxies=resolve_trusted_proxies("loopback,10.0.0.0/8")
    )

    async def socket_app(request, receive, send):
        if request["type"] == "http":
            request["client"] = ("127.0.0.1", 1234)
        await limiter(request, receive, send)

    with TestClient(socket_app) as client:
        assert client.get("/mcp", headers={"X-Forwarded-For": "203.0.113.1, 198.51.100.1, 10.0.0.1"}).status_code == 200
        assert client.get("/mcp", headers={"X-Forwarded-For": "203.0.113.2, 198.51.100.1, 10.0.0.1"}).status_code == 429
        assert client.get("/mcp", headers={"X-Forwarded-For": "198.51.100.2, 10.0.0.1"}).status_code == 200


@pytest.mark.parametrize("peer,expected", [("127.0.0.1", "https"), ("192.0.2.1", "http")])
def test_trusted_scheme_preserves_real_mcp_redirect(peer, expected):
    from mcp.server.mcpserver import MCPServer

    from main import build_http_app

    token = "fake-test-token-at-least-16"
    app = build_http_app(
        MCPServer("proxy-test"),
        "streamable-http",
        token,
        trusted_proxies=resolve_trusted_proxies("loopback"),
        host="0.0.0.0",
    )

    async def socket_app(request, receive, send):
        if request["type"] == "http":
            request["client"] = (peer, 1234)
        await app(request, receive, send)

    with TestClient(socket_app) as client:
        response = client.get(
            "/mcp/", headers={"Authorization": f"Bearer {token}", "X-Forwarded-Proto": "https"}, follow_redirects=False
        )
        assert response.status_code == 307
        assert response.headers["location"] == f"{expected}://testserver/mcp"
