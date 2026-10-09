"""Real local AS HTTP + signed JWTs + the pinned SDK's HTTP transport."""

import asyncio
import base64
import hashlib
import hmac
import json
import os
import sqlite3
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric import rsa
from starlette.testclient import TestClient

import http_oauth
import tool_policy
from http_oauth import load_oauth_config
from main import build_http_app
from strict_args import StrictArgumentServer
from tests.test_transcribe_http import provider as provider
from tool_policy import ToolPolicy, mutating_tool, set_active_policy

STATIC = "fake-static-token-0123456789abcdef"
AUDIENCE = "https://example.ts.net/mcp"
HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}
INIT = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {"protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "fake", "version": "1"}},
}


@pytest.fixture(autouse=True)
def isolated_tool_registry(monkeypatch):
    monkeypatch.setattr(tool_policy, "_MUTATING", set(tool_policy._MUTATING))
    monkeypatch.setattr(tool_policy, "_active", tool_policy._active)


@pytest.fixture
def issuer():
    private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    public = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(private.public_key()))
    public.update(kid="test-key", alg="RS256", use="sig")
    state = {
        "private": private,
        "public": public,
        "calls": [],
        "status": 200,
        "active": True,
        "extra": {},
        "oversize": False,
        "redirect": False,
        "codes": {},
        "refresh": {},
        "revoked": set(),
        "slow_started": threading.Event(),
        "slow_release": threading.Event(),
        "slow_jwks": False,
    }

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def reply(self, payload, status=200):
            self.send_response(status)
            if state["redirect"]:
                self.send_header("Location", "http://localhost:1/never-follow")
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(
                b" " * (http_oauth.MAX_JSON_BYTES + 1) if state["oversize"] else json.dumps(payload).encode()
            )

        def do_GET(self):
            state["calls"].append(self.path)
            if self.path.startswith("/authorize?"):
                query = parse_qs(urlsplit(self.path).query)
                assert query["code_challenge_method"] == ["S256"]
                assert query["response_type"] == ["code"]
                assert query["resource"] == [AUDIENCE]
                state["codes"]["code-1"] = query["code_challenge"][0]
                self.send_response(302)
                self.send_header("Location", "http://localhost/callback?code=code-1")
                self.end_headers()
            elif "well-known" in self.path:
                self.reply(
                    {
                        "issuer": state["url"],
                        "jwks_uri": state["url"] + "/jwks",
                        "authorization_endpoint": state["url"] + "/authorize",
                        "token_endpoint": state["url"] + "/token",
                        "code_challenge_methods_supported": ["S256"],
                    },
                    state["status"],
                )
            else:
                if state["slow_jwks"]:
                    state["slow_started"].set()
                    state["slow_release"].wait(timeout=3)
                self.reply({"keys": [state["public"]]}, 302 if state["redirect"] else state["status"])

        def do_POST(self):
            payload = parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode())
            state["calls"].append(self.path)
            if self.path == "/introspect":
                state["basic"] = self.headers.get("Authorization")
                token = payload["token"][0]
                if token.startswith("slow-"):
                    state["slow_started"].set()
                    state["slow_release"].wait(timeout=3)
                self.reply(
                    {
                        "active": state["active"] and token not in state["revoked"],
                        "aud": AUDIENCE,
                        "sub": "Alice",
                        "scope": "base whatsapp:read whatsapp:send",
                        **state["extra"],
                    },
                    state["status"],
                )
            elif payload.get("grant_type") == ["authorization_code"]:
                code, verifier = payload["code"][0], payload["code_verifier"][0]
                challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
                assert state["codes"].pop(code) == challenge
                state["refresh"]["refresh-1"] = "access-1"
                self.reply({"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer"})
            else:
                refresh = payload["refresh_token"][0]
                if refresh not in state["refresh"]:
                    state["revoked"].update(("access-1", "access-2"))
                    self.reply({"error": "invalid_grant"}, 400)
                else:
                    state["refresh"].pop(refresh)
                    state["refresh"]["refresh-2"] = "access-2"
                    self.reply({"access_token": "access-2", "refresh_token": "refresh-2", "token_type": "Bearer"})

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    state["url"] = f"http://127.0.0.1:{server.server_port}"
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield state
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def configure(monkeypatch, issuer, **extra):
    env = {
        "WHATSAPP_MCP_OAUTH_ISSUER": issuer["url"],
        "WHATSAPP_MCP_OAUTH_AUDIENCE": AUDIENCE,
        "WHATSAPP_MCP_OAUTH_SCOPES": "base",
        **extra,
    }
    for name, value in env.items():
        monkeypatch.setenv(name, value)
    return load_oauth_config(env)


def signed(issuer, **extra):
    return jwt.encode(
        {
            "iss": issuer["url"],
            "aud": AUDIENCE,
            "sub": "Alice",
            "exp": time.time() + 600,
            "scope": "base whatsapp:read whatsapp:send",
            **extra,
        },
        issuer["private"],
        algorithm="RS256",
        headers={"kid": "test-key"},
    )


def request(client, token=None, payload=INIT):
    headers = dict(HEADERS)
    if token:
        headers["Authorization"] = "Bearer " + token
    return client.post("/mcp", json=payload, headers=headers)


def app(rate=0, static=STATIC, stateless=True, max_body=4 * 1024 * 1024):
    server = StrictArgumentServer("fake")
    calls = []

    @server.tool()
    def read_fake() -> str:
        calls.append("read")
        return "read ok"

    @server.tool()
    @mutating_tool
    def send_fake() -> str:
        calls.append("send")
        return "send ok"

    set_active_policy(ToolPolicy())
    return build_http_app(
        server,
        "streamable-http",
        static,
        rate_limit_per_minute=rate,
        host="0.0.0.0",
        json_response=True,
        stateless_http=stateless,
        max_request_body_size=max_body,
    ), calls


@pytest.mark.parametrize(
    "claim,value",
    [
        ("aud", "https://other.example/mcp"),
        ("iss", "https://other.example"),
        ("exp", 1),
        ("nbf", 9999999999),
        ("scope", "whatsapp:read"),
        ("sub", "Bob"),
        ("sub", ""),
        ("exp", "bad"),
        ("nbf", "bad"),
        ("scope", ["base"]),
        ("exp", True),
    ],
)
def test_deny_claims(monkeypatch, issuer, claim, value):
    configure(monkeypatch, issuer, WHATSAPP_MCP_OAUTH_SUBJECTS="Alice")
    application, calls = app()
    with TestClient(application) as client:
        response = request(client, signed(issuer, **{claim: value}))
        assert response.status_code == 401
        assert "resource_metadata=" in response.headers["www-authenticate"]
    assert calls == []


@pytest.mark.parametrize("missing", ["iss", "aud", "exp", "sub"])
def test_missing_claim(monkeypatch, issuer, missing):
    configure(monkeypatch, issuer)
    claims = {
        "iss": issuer["url"],
        "aud": AUDIENCE,
        "sub": "Alice",
        "exp": time.time() + 60,
        "scope": "base whatsapp:read",
    }
    claims.pop(missing)
    token = jwt.encode(claims, issuer["private"], algorithm="RS256", headers={"kid": "test-key"})
    application, _ = app()
    with TestClient(application) as client:
        assert request(client, token).status_code == 401


def test_signature_algorithms_and_key_confusion(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    claims = {
        "iss": issuer["url"],
        "aud": AUDIENCE,
        "sub": "Alice",
        "exp": time.time() + 60,
        "scope": "base whatsapp:read",
    }
    from cryptography.hazmat.primitives import serialization

    public_der = (
        issuer["private"]
        .public_key()
        .public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
    )
    other = rsa.generate_private_key(public_exponent=65537, key_size=2048)

    def part(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).rstrip(b"=")

    unsigned = part({"alg": "HS256", "kid": "test-key"}) + b"." + part(claims)
    confused = (
        unsigned + b"." + base64.urlsafe_b64encode(hmac.digest(public_der, unsigned, "sha256")).rstrip(b"=")
    ).decode()
    tokens = [
        jwt.encode(claims, "", algorithm="none", headers={"kid": "test-key"}),
        confused,
        jwt.encode(claims, other, algorithm="RS256", headers={"kid": "test-key"}),
    ]
    application, _ = app()
    with TestClient(application) as client:
        for token in tokens:
            assert request(client, token).status_code == 401


def test_metadata_discovery_jwks_cache_and_static(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    application, _ = app()
    with TestClient(application) as client:
        denied = request(client)
        assert denied.status_code == 401
        assert (
            AUDIENCE.replace("/mcp", "/.well-known/oauth-protected-resource/mcp") in denied.headers["www-authenticate"]
        )
        docs = [
            client.get(path).json()
            for path in ("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp")
        ]
        assert docs[0] == docs[1]
        assert docs[0]["resource"] == AUDIENCE
        assert docs[0]["authorization_servers"] == [issuer["url"]]
        assert "whatsapp:send" in docs[0]["scopes_supported"]
        path = "/.well-known/oauth-protected-resource/mcp"
        cors = client.get(path, headers={"Origin": "https://client.example.com"})
        assert cors.headers["access-control-allow-origin"] == "*"
        assert client.head(path).content == b""
        assert client.post(path).status_code == 405
        assert request(client, STATIC).status_code == 200
        for _ in range(3):
            assert request(client, signed(issuer)).status_code == 200
    assert issuer["calls"] == ["/.well-known/oauth-authorization-server", "/jwks"]


@pytest.mark.parametrize("broken", ["redirect", "oversize"])
def test_bounded_jwks_no_redirect(monkeypatch, issuer, broken):
    configure(monkeypatch, issuer, WHATSAPP_MCP_OAUTH_JWKS_URL=issuer["url"] + "/jwks")
    issuer[broken] = True
    application, _ = app()
    with TestClient(application) as client:
        assert request(client, signed(issuer)).status_code == 503
        assert request(client, signed(issuer)).status_code == 503
    assert issuer["calls"] == ["/jwks"]  # failure cooldown, no per-request fetch


def test_tool_scope_challenge_listing_and_policy(monkeypatch, issuer):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_READ_SCOPE="whatsapp.read",
        WHATSAPP_MCP_OAUTH_SEND_SCOPE="whatsapp.send",
    )
    application, calls = app()
    read = signed(issuer, scope="base whatsapp.read")
    full = signed(issuer, scope="base whatsapp.read whatsapp.send")
    with TestClient(application) as client:
        listing = request(client, read, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        assert listing.status_code == 200
        assert [t["name"] for t in listing.json()["result"]["tools"]] == ["read_fake"]
        send_call = {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": "send_fake", "arguments": {}},
        }
        denied = request(client, read, send_call)
        assert denied.status_code == 403
        challenge = denied.headers["www-authenticate"]
        assert 'error="insufficient_scope"' in challenge and 'scope="base whatsapp.send"' in challenge
        assert calls == []
        assert request(client, full, send_call).status_code == 200
        assert calls == ["send"]
        set_active_policy(ToolPolicy(deny=frozenset({"send_fake"})))
        refused = request(client, full, send_call)
        assert refused.json()["result"]["isError"] is True
        assert calls == ["send"]
    set_active_policy(None)


def test_subject_rate_limit(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    application, _ = app(rate=1)
    with TestClient(application) as client:
        assert request(client, signed(issuer)).status_code == 200
        assert request(client, signed(issuer)).status_code == 429
        assert request(client, signed(issuer, sub="Bob")).status_code == 200


@pytest.mark.parametrize(
    "extra",
    [
        {"active": False},
        {"active": "true"},
        {"aud": "other"},
        {"scope": "whatsapp:read"},
        {"sub": ""},
        {"exp": 1},
        {"nbf": 9999999999},
        {"iss": "other"},
    ],
)
def test_introspection_deny_not_cached(monkeypatch, issuer, extra):
    config = configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    issuer["extra"] = extra
    application, _ = app()
    with TestClient(application) as client:
        assert request(client, "opaque-test").status_code == 401
        assert request(client, "opaque-test").status_code == 401
    assert issuer["calls"] == ["/introspect", "/introspect"]
    assert base64.b64decode(issuer["basic"][6:]) == b"fake-client:fake-secret"
    assert config.introspection_url.endswith("/introspect")


def test_introspection_outage_is_503(monkeypatch, issuer):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    issuer["status"] = 503
    application, calls = app()
    with TestClient(application) as client:
        assert request(client, "opaque-test").status_code == 503
    assert calls == []


def test_local_as_pkce_refresh_reuse_revocation(monkeypatch, issuer):
    import httpx

    config = configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    verifier = "fake-pkce-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    application, calls = app()
    original = http_oauth.time.monotonic
    with TestClient(application) as client, httpx.Client(trust_env=False) as as_client:
        metadata = client.get("/.well-known/oauth-protected-resource/mcp").json()
        assert metadata["authorization_servers"] == [config.issuer]
        discovery = as_client.get(
            metadata["authorization_servers"][0] + "/.well-known/oauth-authorization-server"
        ).json()
        authorization = as_client.get(
            discovery["authorization_endpoint"],
            params={
                "response_type": "code",
                "client_id": "fake-client",
                "resource": AUDIENCE,
                "code_challenge": challenge,
                "code_challenge_method": "S256",
                "redirect_uri": "http://localhost/callback",
            },
        )
        assert authorization.status_code == 302
        code = parse_qs(urlsplit(authorization.headers["Location"]).query)["code"][0]
        pair = as_client.post(
            discovery["token_endpoint"],
            data={"grant_type": "authorization_code", "code": code, "code_verifier": verifier},
        ).json()
        call = {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "read_fake", "arguments": {}}}
        assert request(client, pair["access_token"], call).status_code == 200
        assert request(client, pair["access_token"], call).status_code == 200
        assert issuer["calls"].count("/introspect") == 1
        rotated = as_client.post(
            issuer["url"] + "/token", data={"grant_type": "refresh_token", "refresh_token": pair["refresh_token"]}
        ).json()
        assert request(client, rotated["access_token"], call).status_code == 200
        assert (
            as_client.post(
                issuer["url"] + "/token", data={"grant_type": "refresh_token", "refresh_token": pair["refresh_token"]}
            ).status_code
            == 400
        )
        monkeypatch.setattr(http_oauth.time, "monotonic", lambda: original() + 61)
        assert request(client, rotated["access_token"], call).status_code == 401
        assert request(client, pair["access_token"], call).status_code == 401
    assert calls == ["read", "read", "read"]


def test_oauth_off_static_response_bytes(monkeypatch):
    monkeypatch.delenv("WHATSAPP_MCP_OAUTH_ISSUER", raising=False)
    monkeypatch.setenv("WHATSAPP_MCP_OAUTH_AUDIENCE", "invalid-but-unused")
    application, _ = app()
    with TestClient(application) as client:
        response = request(client)
        assert response.content == b'{"error":"unauthorized","message":"Missing or invalid bearer token"}'
        assert response.headers["www-authenticate"] == 'Bearer realm="whatsapp-mcp"'
        assert request(client, STATIC).status_code == 200


def test_secret_files_and_url_config(tmp_path):
    path = tmp_path / "secret"
    path.write_text("fake-secret")
    path.chmod(0o600)
    env = {
        "WHATSAPP_MCP_OAUTH_ISSUER": "https://issuer.example",
        "WHATSAPP_PUBLIC_URL": AUDIENCE,
        "WHATSAPP_MCP_OAUTH_INTROSPECTION_URL": "https://issuer.example/introspect",
        "WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID": "fake-client",
        "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET_FILE": str(path),
    }
    assert load_oauth_config(env).client_secret == "fake-secret"
    with pytest.raises(ValueError, match="never both"):
        load_oauth_config({**env, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET": "fake-secret"})
    for bad in ("http://issuer.example", "https://user:secret@issuer.example", 'https://issuer.example/"'):
        with pytest.raises(ValueError):
            load_oauth_config({**env, "WHATSAPP_MCP_OAUTH_ISSUER": bad})


def test_stateful_session_scope_downgrade_and_subject_binding(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    application, calls = app(stateless=False)
    with TestClient(application) as client:
        initialized = request(client, signed(issuer))
        assert initialized.status_code == 200
        session = initialized.headers["mcp-session-id"]
        headers = {
            **HEADERS,
            "mcp-session-id": session,
            "Authorization": "Bearer " + signed(issuer, scope="base whatsapp:read"),
        }
        listed = client.post("/mcp", headers=headers, json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
        assert listed.status_code == 200
        assert [t["name"] for t in listed.json()["result"]["tools"]] == ["read_fake"]
        headers["Authorization"] = "Bearer " + signed(issuer, sub="Bob")
        foreign = client.post(
            "/mcp",
            headers=headers,
            json={"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "read_fake", "arguments": {}}},
        )
        assert foreign.status_code in (403, 404)
        assert calls == []


def test_invalid_and_scope_denied_requests_are_rate_limited(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    application, _ = app(rate=1)
    with TestClient(application) as client:
        token = signed(issuer, scope="base")
        call = {"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "read_fake", "arguments": {}}}
        assert request(client, token, call).status_code == 403
        assert request(client, token, call).status_code == 429
        assert request(client).status_code == 401
        assert request(client).status_code == 429


@pytest.mark.asyncio
async def test_jwks_expiry_key_confusion_and_proxy_isolation(monkeypatch, issuer):
    config = configure(monkeypatch, issuer, WHATSAPP_MCP_OAUTH_JWKS_URL=issuer["url"] + "/jwks")
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.setenv(name, "http://127.0.0.1:1")
    monkeypatch.setenv("NO_PROXY", "")
    verifier = http_oauth.OAuthTokenVerifier(config)
    assert await verifier.verify_token(signed(issuer)) is not None
    verifier._keys[0]["kty"] = "oct"
    assert await verifier.verify_token(signed(issuer)) is None  # even a trusted kid cannot change key family
    assert issuer["calls"] == ["/jwks"]
    verifier._keys_until = 0
    issuer["status"] = 503
    with pytest.raises(http_oauth.AuthorizationUnavailableError):
        await verifier.verify_token(signed(issuer))
    assert issuer["calls"] == ["/jwks", "/jwks"]


def test_introspection_invalid_guesses_stop_before_remote_fetch(monkeypatch, issuer):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    issuer["active"] = False
    application, _ = app(rate=1)
    with TestClient(application) as client:
        assert [request(client, "invalid-" + str(i)).status_code for i in range(4)] == [401, 429, 429, 429]
    assert issuer["calls"] == ["/introspect"]


def test_introspection_distinct_subjects_keep_independent_limits(monkeypatch, issuer):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    application, _ = app(rate=1)
    with TestClient(application) as client:
        assert request(client, "opaque-alice").status_code == 200
        assert request(client, "opaque-alice").status_code == 429
        issuer["extra"] = {"sub": "Bob"}
        assert request(client, "opaque-bob").status_code == 200
    assert issuer["calls"] == ["/introspect", "/introspect"]


def test_concurrent_invalid_tokens_cannot_queue_introspection_calls(monkeypatch, issuer):
    from concurrent.futures import ThreadPoolExecutor

    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    issuer["active"] = False
    application, _ = app(rate=1)
    with TestClient(application) as client, ThreadPoolExecutor(max_workers=4) as pool:
        statuses = list(pool.map(lambda i: request(client, "invalid-" + str(i)).status_code, range(4)))
    assert sorted(statuses) == [401, 429, 429, 429]
    assert issuer["calls"] == ["/introspect"]


def test_private_secret_file_deny_paths(tmp_path):
    import os

    from http_oauth import SECRET_FILE_ENV, _secret

    path = tmp_path / "secret"
    path.write_bytes(b"x" * 4097)
    path.chmod(0o600)
    with pytest.raises(ValueError, match="private regular"):
        _secret({SECRET_FILE_ENV: str(path)}, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET", SECRET_FILE_ENV)
    with pytest.raises(ValueError, match="private regular"):
        _secret({SECRET_FILE_ENV: str(tmp_path)}, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET", SECRET_FILE_ENV)
    if os.name != "nt":
        path.write_text("fake-secret")
        path.chmod(0o644)
        with pytest.raises(ValueError, match="private regular"):
            _secret({SECRET_FILE_ENV: str(path)}, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET", SECRET_FILE_ENV)
        path.chmod(0o600)
        link = tmp_path / "symlink-secret"
        link.symlink_to(path)
        with pytest.raises(ValueError, match="private regular"):
            _secret({SECRET_FILE_ENV: str(link)}, "WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET", SECRET_FILE_ENV)


def test_introspection_session_is_bound_to_client_and_subject(monkeypatch, issuer):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-resource",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    issuer["extra"] = {"client_id": "client-a"}
    application, calls = app(stateless=False)
    with TestClient(application) as client:
        initialized = request(client, "opaque-client-a")
        assert initialized.status_code == 200
        session = initialized.headers["mcp-session-id"]
        issuer["extra"] = {"client_id": "client-b"}
        headers = {**HEADERS, "mcp-session-id": session, "Authorization": "Bearer opaque-client-b"}
        foreign = client.post(
            "/mcp",
            headers=headers,
            json={"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "read_fake", "arguments": {}}},
        )
        assert foreign.status_code == 404
    assert calls == []


@pytest.mark.parametrize("claim", ["nbf", "iat", "exp"])
@pytest.mark.parametrize("offset,expected", [(3, 200), (120, 401)])
def test_clock_skew_on_native_sdk_path(monkeypatch, issuer, claim, offset, expected):
    configure(monkeypatch, issuer)
    application, _ = app()
    value = time.time() + (-offset if claim == "exp" else offset)
    with TestClient(application) as client:
        assert request(client, signed(issuer, **{claim: value})).status_code == expected


@pytest.mark.parametrize(
    "kind,expected", [(None, 200), ("access_token", 200), ("ACCESS_TOKEN", 200), ("refresh_token", 401), (7, 401)]
)
def test_introspection_only_access_token_type(monkeypatch, issuer, kind, expected):
    configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    if kind is not None:
        issuer["extra"]["token_type"] = kind
    application, calls = app()
    with TestClient(application) as client:
        assert request(client, "opaque-token-kind").status_code == expected
        assert request(client, "opaque-token-kind").status_code == expected
    assert len(issuer["calls"]) == (1 if expected == 200 else 2)
    assert calls == []


@pytest.mark.asyncio
async def test_cached_introspection_bypasses_slow_token_and_single_flight(monkeypatch, issuer):
    config = configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    verifier = http_oauth.OAuthTokenVerifier(config)
    assert await verifier.verify_token("cached-valid") is not None
    issuer["active"] = False
    slow = [asyncio.create_task(verifier.verify_token("slow-same")) for _ in range(3)]
    assert await asyncio.to_thread(issuer["slow_started"].wait, 2)
    try:
        assert await asyncio.wait_for(verifier.verify_token("cached-valid"), 0.5) is not None
        assert issuer["calls"] == ["/introspect", "/introspect"]
        assert not issuer["slow_release"].is_set()
    finally:
        issuer["slow_release"].set()
        assert await asyncio.gather(*slow) == [None] * 3


@pytest.mark.asyncio
async def test_introspection_concurrency_is_bounded(monkeypatch, issuer):
    config = configure(
        monkeypatch,
        issuer,
        WHATSAPP_MCP_OAUTH_INTROSPECTION_URL=issuer["url"] + "/introspect",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_CLIENT_ID="fake-client",
        WHATSAPP_MCP_OAUTH_INTROSPECTION_SECRET="fake-secret",
    )
    verifier = http_oauth.OAuthTokenVerifier(config)
    tasks = [asyncio.create_task(verifier.verify_token(f"slow-{i}")) for i in range(6)]
    assert await asyncio.to_thread(issuer["slow_started"].wait, 2)
    try:
        await asyncio.sleep(0.1)
        assert len(verifier._introspection_flights) == http_oauth.MAX_INTROSPECTIONS
        assert len(issuer["calls"]) <= http_oauth.MAX_INTROSPECTIONS
    finally:
        issuer["slow_release"].set()
        results = await asyncio.gather(*tasks, return_exceptions=True)
    assert sum(isinstance(r, http_oauth.AuthorizationUnavailableError) for r in results) == 2
    assert len(issuer["calls"]) == http_oauth.MAX_INTROSPECTIONS


@pytest.mark.asyncio
async def test_tool_scope_refusal_without_http_preflight(monkeypatch):
    from mcp.server.auth.middleware.auth_context import auth_context_var
    from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
    from mcp.server.auth.provider import AccessToken

    server = StrictArgumentServer("fake")
    calls = []
    assert "send_message" in tool_policy.mutating_tools()

    @server.tool()
    def send_message() -> str:
        # A bare SDK callable isolates this guard from the decorator's policy
        # guard; the production tool name is still classified as mutating.
        calls.append("send")
        return "sent"

    token = AccessToken(
        token="fake",
        client_id="fake",
        subject="Alice",
        scopes=["whatsapp:read"],
        claims={"read_scope": "whatsapp:read", "send_scope": "whatsapp:send"},
    )
    context = auth_context_var.set(AuthenticatedUser(token))
    try:
        result = await server.call_tool("send_message", {})
    finally:
        auth_context_var.reset(context)
    assert result.is_error
    payload = result.structured_content or json.loads(result.content[0].text)
    assert payload["error"]["code"] == "denied"
    assert "OAuth scope" in payload["error"]["message"]
    assert calls == []


@pytest.mark.asyncio
async def test_unknown_kid_refresh_is_bounded_and_accepts_rotation(monkeypatch, issuer):
    config = configure(monkeypatch, issuer, WHATSAPP_MCP_OAUTH_JWKS_URL=issuer["url"] + "/jwks")
    verifier = http_oauth.OAuthTokenVerifier(config)
    assert await verifier.verify_token(signed(issuer)) is not None
    private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    rotated = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(private.public_key()))
    rotated.update(kid="rotated-key", alg="RS256", use="sig")
    claims = {
        "iss": issuer["url"],
        "aud": AUDIENCE,
        "sub": "Alice",
        "exp": time.time() + 60,
        "scope": "base whatsapp:read",
    }
    new = jwt.encode(claims, private, algorithm="RS256", headers={"kid": "rotated-key"})
    issuer["public"] = rotated
    assert await verifier.verify_token(new) is None  # initial fetch owns this cooldown
    verifier._refresh_after = 0  # observe the next eligible cooldown window
    assert await verifier.verify_token(new) is not None
    for i in range(5):
        unknown = jwt.encode(claims, private, algorithm="RS256", headers={"kid": f"unknown-{i}"})
        assert await verifier.verify_token(unknown) is None
    assert issuer["calls"] == ["/jwks", "/jwks"]


def test_dotted_static_token_refused_only_when_oauth_on(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    dotted = "fake-static.fake-middle.fake-end"
    with pytest.raises(ValueError, match="two dots"):
        app(static=dotted)
    monkeypatch.delenv("WHATSAPP_MCP_OAUTH_ISSUER")
    application, _ = app(static=dotted)
    with TestClient(application) as client:
        assert request(client, dotted).status_code == 200


def test_body_413_has_no_authentication_error_or_challenge(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    application, calls = app(max_body=128)
    with TestClient(application) as client:
        response = request(client, signed(issuer), {"padding": "x" * 256})
    assert response.status_code == 413
    assert "www-authenticate" not in response.headers
    assert response.json() == {"error": "body_too_large"}
    assert calls == []


@pytest.mark.asyncio
async def test_known_jwks_key_does_not_wait_for_unknown_kid_refresh(monkeypatch, issuer):
    config = configure(monkeypatch, issuer, WHATSAPP_MCP_OAUTH_JWKS_URL=issuer["url"] + "/jwks")
    verifier = http_oauth.OAuthTokenVerifier(config)
    known = signed(issuer)
    assert await verifier.verify_token(known) is not None
    verifier._refresh_after = 0
    issuer["slow_jwks"] = True
    claims = jwt.decode(known, options={"verify_signature": False})
    unknown = jwt.encode(claims, issuer["private"], algorithm="RS256", headers={"kid": "unknown"})
    refresh = asyncio.create_task(verifier.verify_token(unknown))
    assert await asyncio.to_thread(issuer["slow_started"].wait, 2)
    try:
        assert await asyncio.wait_for(verifier.verify_token(known), 0.5) is not None
        assert not issuer["slow_release"].is_set()
    finally:
        issuer["slow_release"].set()
        assert await refresh is None
    assert issuer["calls"] == ["/jwks", "/jwks"]


def test_dotted_static_token_startup_has_one_line_error(monkeypatch, issuer):
    configure(monkeypatch, issuer)
    env = dict(
        os.environ,
        WHATSAPP_MCP_TRANSPORT="http",
        WHATSAPP_MCP_HOST="127.0.0.1",
        WHATSAPP_MCP_TOKEN="fake-static.fake-middle.fake-end",
        TRANSCRIBE_ON_INGEST="0",
    )
    result = subprocess.run([sys.executable, "main.py"], env=env, capture_output=True, text=True, timeout=15)
    assert result.returncode == 1
    assert result.stderr.rstrip().splitlines()[-1] == "WHATSAPP_MCP_TOKEN must not have two dots when OAuth is enabled"
    assert "Traceback" not in result.stderr and "listening on" not in result.stderr
    assert issuer["calls"] == []


def test_read_only_oauth_cannot_upload_store_secret(monkeypatch, issuer, provider, tmp_path):
    import main
    import whatsapp

    configure(monkeypatch, issuer)
    store = tmp_path / "archive"
    store.mkdir()
    secret = store / "whatsapp.db"
    with sqlite3.connect(secret) as conn:
        conn.execute("CREATE TABLE fake_session (key TEXT)")
        conn.execute("INSERT INTO fake_session VALUES ('fake-session-secret-not-audio')")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store / "messages.db"))
    server = StrictArgumentServer("fake")
    server.add_tool(main.transcribe_audio)
    set_active_policy(ToolPolicy(read_only=True))
    application = build_http_app(
        server, "streamable-http", None, host="0.0.0.0", json_response=True, stateless_http=True
    )
    with TestClient(application) as client:
        token = signed(issuer, scope="base whatsapp:read")
        assert request(client, token).status_code == 200
        response = request(
            client,
            token,
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "transcribe_audio", "arguments": {"file_path": str(secret)}},
            },
        )
    assert response.status_code == 200 and response.json()["result"]["isError"]
    assert "not supported audio" in response.text
    assert provider["calls"] == []
