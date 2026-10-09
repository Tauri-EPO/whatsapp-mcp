"""Real local AS HTTP + signed JWTs + the pinned SDK's HTTP transport."""

import base64
import hashlib
import hmac
import json
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
                self.reply({"keys": [state["public"]]}, 302 if state["redirect"] else state["status"])

        def do_POST(self):
            payload = parse_qs(self.rfile.read(int(self.headers["Content-Length"])).decode())
            state["calls"].append(self.path)
            if self.path == "/introspect":
                state["basic"] = self.headers.get("Authorization")
                token = payload["token"][0]
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


def app(rate=0, static=STATIC, stateless=True):
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
