"""Read and synthetic-test tools cannot configure or redirect the receiver."""

import json

import httpx
import pytest
from mcp.server.auth.middleware.auth_context import auth_context_var
from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
from mcp.server.auth.provider import AccessToken

import main
import tool_policy
import whatsapp
from http_oauth import load_oauth_config, required_tool_scope
from tests.test_bridge_status_webhook import STATES
from tool_policy import ToolPolicy

pytestmark = pytest.mark.usefixtures("auth_runtime_store")

VIEW = {
    **STATES[2],
    "events": ["message.received"],
    "forward_self": False,
    "forward_channels": False,
    "forward_broadcasts": False,
    "authorization_set": True,
}


@pytest.fixture(autouse=True)
def isolated(monkeypatch):
    monkeypatch.setattr(tool_policy, "_active", ToolPolicy())
    monkeypatch.setattr(tool_policy, "_runtime", False)
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "fake-bridge-token-issue-834")
    monkeypatch.setenv("WHATSAPP_OPERATOR_TOKEN", "fake-operator-token-never-forwarded")


def response(monkeypatch, method, path, status, payload):
    calls = []

    def fake(url, **kwargs):
        assert url == whatsapp.WHATSAPP_API_BASE_URL + path
        bridge_credential = kwargs["headers"]["Authorization"] == "Bearer fake-bridge-token-issue-834"
        assert bridge_credential, "webhook request did not use the bridge credential"
        assert "/operator/" not in url
        calls.append((method, path))
        return httpx.Response(status, json=payload)

    monkeypatch.setattr(whatsapp.bridge_http, method.lower(), fake)
    return calls


def test_get_webhook_same_sanitized_view(monkeypatch):
    calls = response(
        monkeypatch,
        "GET",
        "/webhook",
        200,
        {
            **VIEW,
            "secret": "fake-secret",
            "secret_fingerprint": "fake-fingerprint",
            "authorization": "Bearer fake-auth",
        },
    )
    result = main.get_webhook()
    assert result == VIEW
    assert calls == [("GET", "/webhook")]
    serialized = json.dumps(result)
    assert all(value not in serialized for value in ("fake-secret", "fake-fingerprint", "Bearer fake-auth"))


def test_get_webhook_without_destination(monkeypatch):
    absent = {**VIEW, **STATES[1], "events": [], "authorization_set": False}
    response(monkeypatch, "GET", "/webhook", 200, absent)
    assert main.get_webhook() == absent


def test_detail_status_and_get_share_the_sanitized_view(monkeypatch):
    def fake(url, **_kwargs):
        if url.endswith("/health"):
            return httpx.Response(200, json={"paired": True, "connected": True, "webhook": VIEW})
        if url.endswith("/webhook"):
            return httpx.Response(200, json=VIEW)
        return httpx.Response(503, json={})

    monkeypatch.setattr(whatsapp.bridge_http, "get", fake)
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, "true")
    assert main.bridge_status(detail=True)["webhook"] == main.get_webhook() == VIEW


@pytest.mark.parametrize(
    "delivered,reason", [(True, ""), (False, "http_403"), (False, "receiver_refused"), (False, "delivery_failed")]
)
def test_synthetic_result(monkeypatch, delivered, reason):
    result = {"delivered": delivered, "reason": reason, "event_id": "fake-event-id"}
    calls = response(monkeypatch, "POST", "/webhook/test", 200, result)
    assert main.test_webhook() == result
    assert calls == [("POST", "/webhook/test")]


@pytest.mark.parametrize(
    "tool,method,path", [(main.get_webhook, "GET", "/webhook"), (main.test_webhook, "POST", "/webhook/test")]
)
@pytest.mark.parametrize(
    "status,code,text",
    [
        (404, "not_found", "Runtime webhook is disabled"),
        (409, "conflict", "Configure an enabled webhook first"),
        (401, "denied", "denied"),
        (403, "denied", "denied"),
        (500, "bridge_unavailable", "failed"),
    ],
)
def test_clear_safe_error_envelopes(monkeypatch, tool, method, path, status, code, text):
    response(monkeypatch, method, path, status, {"message": "fake-private-credential"})
    result = tool()
    assert result["error"]["code"] == code
    assert text in result["error"]["message"]
    assert "fake-private-credential" not in json.dumps(result)


def test_synthetic_reason_cannot_echo_private_text(monkeypatch):
    response(
        monkeypatch,
        "POST",
        "/webhook/test",
        200,
        {"delivered": False, "reason": "fake-private-credential", "event_id": "fake-event-id"},
    )
    result = main.test_webhook()
    assert result["error"]["code"] == "bridge_unavailable"
    assert "fake-private-credential" not in json.dumps(result)


@pytest.mark.asyncio
@pytest.mark.parametrize("name", ["get_webhook", "test_webhook"])
@pytest.mark.parametrize(
    "policy",
    [ToolPolicy(deny=frozenset({"get_webhook", "test_webhook"})), ToolPolicy(allow=frozenset({"bridge_status"}))],
)
async def test_policy_hides_and_refuses_tools(monkeypatch, name, policy):
    monkeypatch.setattr(tool_policy, "_active", policy)
    monkeypatch.setattr(main.mcp, "runtime_tool_policy", True)
    assert name not in {tool.name for tool in await main.mcp.list_tools()}
    result = await main.mcp.call_tool(name, {})
    assert result.is_error and result.structured_content["error"]["code"] == "denied"


@pytest.mark.asyncio
async def test_read_only_hides_and_refuses_synthetic_test(monkeypatch):
    monkeypatch.setattr(tool_policy, "_active", ToolPolicy(read_only=True))
    monkeypatch.setattr(main.mcp, "runtime_tool_policy", True)
    names = {tool.name for tool in await main.mcp.list_tools()}
    assert "get_webhook" in names and "test_webhook" not in names
    assert main.test_webhook()["error"]["code"] == "denied"
    result = await main.mcp.call_tool("test_webhook", {})
    assert result.is_error and result.structured_content["error"]["code"] == "denied"
    response(monkeypatch, "GET", "/webhook", 200, VIEW)
    assert main.get_webhook() == VIEW


@pytest.mark.asyncio
@pytest.mark.parametrize("scope", ["whatsapp:read", "whatsapp:send"])
async def test_oauth_read_and_send_scopes(monkeypatch, scope):
    config = load_oauth_config(
        {
            "WHATSAPP_MCP_OAUTH_ISSUER": "https://example.com",
            "WHATSAPP_MCP_OAUTH_AUDIENCE": "https://example.ts.net/mcp",
        }
    )
    assert required_tool_scope(config, "get_webhook") == "whatsapp:read"
    assert required_tool_scope(config, "test_webhook") == "whatsapp:send"
    response(monkeypatch, "GET", "/webhook", 200, VIEW)
    sent = {"delivered": True, "reason": "", "event_id": "fake-event-id"}
    response(monkeypatch, "POST", "/webhook/test", 200, sent)
    allowed_name, denied_name, expected = (
        ("get_webhook", "test_webhook", VIEW) if scope == "whatsapp:read" else ("test_webhook", "get_webhook", sent)
    )
    token = AccessToken(
        token="fake",
        client_id="fake",
        subject="Alice",
        scopes=[scope],
        claims={"read_scope": "whatsapp:read", "send_scope": "whatsapp:send"},
    )
    context = auth_context_var.set(AuthenticatedUser(token))
    try:
        assert denied_name not in {tool.name for tool in await main.mcp.list_tools()}
        denied = await main.mcp.call_tool(denied_name, {})
        assert denied.is_error and denied.structured_content["error"]["code"] == "denied"
        allowed = await main.mcp.call_tool(allowed_name, {})
        assert not allowed.is_error and allowed.structured_content == expected
    finally:
        auth_context_var.reset(context)


def test_no_configuration_tool_exists():
    names = tool_policy.registered_tool_names(main.mcp)
    assert "set_webhook" not in names and "delete_webhook" not in names


@pytest.mark.asyncio
@pytest.mark.parametrize("name", ["get_webhook", "test_webhook"])
async def test_tools_accept_no_destination_argument(name):
    result = await main.mcp.call_tool(name, {"url": "https://example.com"})
    assert result.is_error and result.structured_content["error"]["code"] == "invalid_argument"
