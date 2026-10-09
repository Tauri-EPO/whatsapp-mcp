"""The dispatch permission comes from the token authenticated by the SDK."""

import json
import time

import pytest
from starlette.testclient import TestClient

import http_oauth
import media_resource
import tool_policy
from main import build_http_app
from tests.conftest import ALICE
from tests.test_http_oauth import HEADERS, STATIC, app, configure, signed
from tests.test_http_oauth import isolated_tool_registry as isolated_tool_registry
from tests.test_http_oauth import issuer as issuer
from tests.test_media_resources import store as store


@pytest.mark.parametrize("operation", ["resource", "prompt", "tool"])
def test_dispatch_uses_fresh_sdk_scopes_after_delayed_body(monkeypatch, issuer, store, operation):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    # Actual cache expiry during body consumption, without a minute-long test.
    monkeypatch.setattr(http_oauth, "INTROSPECTION_TTL", 0.03)
    server = media_resource.MediaResourceServer("fake")
    calls = []
    original = media_resource.read_media_resource

    def observed_read(*args):
        calls.append("resource")
        return original(*args)

    monkeypatch.setattr(media_resource, "read_media_resource", observed_read)

    @server.prompt()
    def read_fake_prompt() -> str:
        calls.append("prompt")
        return "fake-private-prompt"

    @server.tool()
    def read_fake() -> str:
        calls.append("tool")
        return "fake-private-tool"

    tool_policy.set_active_policy(tool_policy.ToolPolicy())
    application = build_http_app(
        server, "streamable-http", STATIC, host="0.0.0.0", json_response=True, stateless_http=True
    )
    method, params = {
        "resource": ("resources/read", {"uri": f"whatsapp://media/{ALICE}/TXT1"}),
        "prompt": ("prompts/get", {"name": "read_fake_prompt"}),
        "tool": ("tools/call", {"name": "read_fake", "arguments": {}}),
    }[operation]
    payload = {"jsonrpc": "2.0", "id": 2, "method": method, "params": params}

    def delayed_body():
        issuer["extra"]["scope"] = "base whatsapp:send"
        time.sleep(0.06)
        yield json.dumps(payload).encode()

    with TestClient(application) as client:
        allowed = client.post("/mcp", json=payload, headers={**HEADERS, "Authorization": "Bearer " + STATIC})
        assert allowed.status_code == 200
        assert calls == [operation]  # real resource bytes / actual SDK prompt/tool
        calls.clear()
        denied = client.post(
            "/mcp", content=delayed_body(), headers={**HEADERS, "Authorization": "Bearer opaque-delayed-body"}
        )
    assert issuer["calls"].count("/introspect") == 2
    assert denied.status_code == 403
    assert denied.json() == {"error": "insufficient_scope"}
    assert "whatsapp:read" in denied.headers["www-authenticate"]
    assert "resource_metadata=" in denied.headers["www-authenticate"]
    assert calls == []


@pytest.mark.parametrize("mode", ["opaque", "jwt"])
def test_sdk_revalidation_outage_keeps_503_metadata(monkeypatch, issuer, mode, auth_runtime_store):
    options = (
        {
            "WHATSAPP_MCP_OAUTH_INTROSPECTION_URL": issuer["url"] + "/introspect",
            "WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID": "fake-client",
            "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET": "fake-secret",
        }
        if mode == "opaque"
        else {}
    )
    configure(monkeypatch, issuer, **options)
    monkeypatch.setattr(http_oauth, "INTROSPECTION_TTL", 0.1)
    monkeypatch.setattr(http_oauth, "JWKS_TTL", 0.1)
    application, calls = app()
    token = "opaque-outage-during-body" if mode == "opaque" else signed(issuer)
    payload = {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "read_fake", "arguments": {}}}

    def delayed_body():
        issuer["status"] = 503
        time.sleep(0.2)
        yield json.dumps(payload).encode()

    with TestClient(application, raise_server_exceptions=False) as client:
        allowed = client.post("/mcp", json=payload, headers={**HEADERS, "Authorization": "Bearer " + STATIC})
        assert allowed.status_code == 200 and calls == ["read"]
        calls.clear()
        denied = client.post("/mcp", content=delayed_body(), headers={**HEADERS, "Authorization": "Bearer " + token})
    assert len(issuer["calls"]) >= 2
    assert denied.status_code == 503
    assert denied.json() == {"error": "authorization_unavailable"}
    assert "resource_metadata=" in denied.headers["www-authenticate"]
    assert denied.headers["cache-control"] == "no-store"
    assert calls == []
