from pathlib import Path

import pytest
from starlette.testclient import TestClient

from http_auth import RateLimitMiddleware, client_key, resolve_trusted_proxies

pytestmark = pytest.mark.usefixtures("auth_runtime_store")


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


def test_gateway_trusts_one_address_and_separates_forwarded_clients(monkeypatch):
    route = "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 010012AC 0003 0 0 0 00000000\n"
    monkeypatch.setattr(Path, "read_text", lambda *_args, **_kwargs: route)
    trusted = resolve_trusted_proxies("gateway")
    assert tuple(map(str, trusted)) == ("172.18.0.1/32",)
    assert client_key(scope("172.18.0.1", "198.51.100.1"), trusted) == "198.51.100.1"
    assert client_key(scope("172.18.0.2", "198.51.100.1"), trusted) == "172.18.0.2"
    limiter = RateLimitMiddleware(_ok_app, 1, clock=lambda: 0, trusted_proxies=trusted)

    async def socket_app(request, receive, send):
        if request["type"] == "http":
            request["client"] = ("172.18.0.1", 1234)
        await limiter(request, receive, send)

    with TestClient(socket_app) as client:
        assert client.get("/mcp", headers={"X-Forwarded-For": "198.51.100.1"}).status_code == 200
        assert client.get("/mcp", headers={"X-Forwarded-For": "198.51.100.2"}).status_code == 200
        assert client.get("/mcp", headers={"X-Forwarded-For": "198.51.100.1"}).status_code == 429


@pytest.mark.parametrize(
    "route",
    [
        "",
        "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 00000000 0001 0 0 0 00000000\n",
        "invalid\ninvalid",
    ],
)
def test_gateway_requires_a_resolvable_default_route(monkeypatch, route):
    monkeypatch.setattr(Path, "read_text", lambda *_args, **_kwargs: route)
    with pytest.raises(ValueError, match="WHATSAPP_MCP_TRUSTED_PROXIES=gateway cannot resolve"):
        resolve_trusted_proxies("gateway")


def test_gateway_uses_lowest_metric_and_refuses_equal_cost_ambiguity(monkeypatch):
    routes = "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 010012AC 0003 0 0 10 00000000\neth1 00000000 010013AC 0003 0 0 20 00000000\n"
    monkeypatch.setattr(Path, "read_text", lambda *_args, **_kwargs: routes)
    assert tuple(map(str, resolve_trusted_proxies("gateway"))) == ("172.18.0.1/32",)
    routes = routes.replace("0 0 20", "0 0 10")
    with pytest.raises(ValueError, match="cannot resolve"):
        resolve_trusted_proxies("gateway")


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
