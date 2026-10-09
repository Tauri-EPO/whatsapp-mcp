"""Real SQLite and HTTP auth consumers observe persisted operator rotations."""

import hashlib
import json
import re
import sqlite3
from contextlib import closing
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from starlette.testclient import TestClient

import main
import whatsapp
from http_auth import AuthStateUnavailableError, BearerTokenMiddleware, RuntimeRateLimitMiddleware, verify_static_token
from tests.conftest import PairedStore
from tests.test_http_auth import TestBuildHttpApp as BuildProbe
from tests.test_http_auth import _ok_app

OLD = "fake-original-mcp-token"
NEW = "fake-next-mcp-token"
THIRD = "fake-third-mcp-token"


def digest(token):
    return hashlib.sha256(token.encode()).hexdigest()


@pytest.fixture
def auth_store(tmp_path, monkeypatch):
    bridge = Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "runtime_settings.go"
    schema = re.search(r"const runtimeSettingsSchema = `([^`]+)`", bridge.read_text()).group(1)
    path = tmp_path / "messages.db"
    with closing(sqlite3.connect(path)) as conn:
        conn.executescript(schema)
        conn.execute("PRAGMA journal_mode=WAL")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return PairedStore(path, tmp_path / "whatsapp.db")


def rotate(store, current=NEW, previous=OLD, until=None):
    state = (
        None
        if current is None
        else {
            "current": digest(current),
            "previous": digest(previous) if previous else "",
            "previous_valid_until": (until or datetime.now(UTC) + timedelta(hours=1)).isoformat(),
        }
    )
    with closing(store.messages()) as conn, conn:
        conn.execute(
            "INSERT INTO runtime_settings VALUES ('auth.mcp_token',?,'2026-10-09 00:00:00+00:00',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (json.dumps(state),),
        )


def response(client, token):
    return client.get("/mcp", headers={"Authorization": f"Bearer {token}"})


def test_rotation_http_grace_expiry_second_rotation_restart_delete(auth_store):
    client = TestClient(BearerTokenMiddleware(_ok_app, OLD))
    assert response(client, OLD).status_code == 200
    assert response(client, NEW).status_code == 401
    rotate(auth_store)
    assert response(client, OLD).status_code == 200
    assert response(client, NEW).status_code == 200
    assert client.get("/mcp").status_code == 401
    fresh = TestClient(BearerTokenMiddleware(_ok_app, OLD))
    assert response(fresh, NEW).status_code == 200
    rotate(auth_store, until=datetime.now(UTC) - timedelta(seconds=1))
    assert response(client, OLD).status_code == 401
    assert response(client, NEW).status_code == 200
    rotate(auth_store, THIRD, NEW)
    assert response(client, OLD).status_code == 401
    assert response(client, NEW).status_code == 200
    assert response(client, THIRD).status_code == 200
    rotate(auth_store, None)
    assert response(client, OLD).status_code == 200
    assert response(client, THIRD).status_code == 401


@pytest.mark.parametrize("damage", ["corrupt_rotation", "invalid_database", "missing_database", "locked_database"])
def test_auth_unavailable_is_503_warns_without_credentials(auth_store, caplog, monkeypatch, damage):
    import http_auth

    monkeypatch.setattr(http_auth, "_AUTH_WARN_AT", float("-inf"))
    rotate(auth_store)
    lock = None
    if damage == "corrupt_rotation":
        with closing(auth_store.messages()) as conn, conn:
            conn.execute("UPDATE runtime_settings SET value='{}' WHERE key='auth.mcp_token'")
    elif damage == "invalid_database":
        Path(whatsapp.MESSAGES_DB_PATH).write_bytes(b"not a database")
    elif damage == "missing_database":
        Path(whatsapp.MESSAGES_DB_PATH).unlink()
    else:
        lock = auth_store.messages()
        lock.execute("PRAGMA journal_mode=DELETE")
        lock.execute("BEGIN EXCLUSIVE")
    try:
        client = TestClient(BearerTokenMiddleware(_ok_app, OLD))
        for _ in range(2):
            result = response(client, OLD)
            assert result.status_code == 503
            assert result.json()["message"] == "authentication state unavailable"
        warnings = [r for r in caplog.records if "Authentication state unavailable" in r.message]
        assert len(warnings) == 1
        assert all(value not in caplog.text for value in (OLD, NEW, digest(NEW)))
        with pytest.raises(AuthStateUnavailableError):
            verify_static_token(OLD, OLD)
    finally:
        if lock is not None:
            lock.rollback()
            lock.close()


@pytest.mark.parametrize("expected", [OLD, None])
def test_pre_registry_store_falls_back_to_deploy_policy(auth_store, expected):
    with closing(auth_store.messages()) as conn, conn:
        conn.execute("DROP TABLE runtime_settings")
    client = TestClient(BearerTokenMiddleware(_ok_app, expected, runtime_rotation=True))
    assert response(client, OLD).status_code == 200
    assert client.get("/mcp").status_code == (200 if expected is None else 401)


def test_missing_database_preserves_explicit_anonymous_mode(auth_store):
    Path(whatsapp.MESSAGES_DB_PATH).unlink()
    client = TestClient(BearerTokenMiddleware(_ok_app, None, runtime_rotation=True))
    assert client.get("/mcp").status_code == 200


def test_rotation_enforces_auth_when_initially_disabled(auth_store):
    assert not verify_static_token("arbitrary-fake-token", None)
    client = TestClient(BearerTokenMiddleware(_ok_app, None, runtime_rotation=True))
    assert client.get("/mcp").status_code == 200
    rotate(auth_store, previous=None)
    assert client.get("/mcp").status_code == 401
    assert response(client, NEW).status_code == 200
    rotate(auth_store, None)
    assert client.get("/mcp").status_code == 200


def test_real_mcp_transport_rotates_and_operator_routes_are_absent(auth_store):
    from mcp.server.mcpserver import MCPServer

    app = main.build_http_app(MCPServer("rotation-test"), "streamable-http", OLD, host="127.0.0.1")
    with TestClient(app, base_url="http://127.0.0.1:8000") as client:
        rotate(auth_store)
        probe = BuildProbe()
        assert probe._post(client, {"Authorization": f"Bearer {NEW}"}).status_code == 200
        assert probe._post(client, {"Authorization": "Bearer wrong-fake-token"}).status_code == 401
        for method, path in (
            ("POST", "/operator/v1/mcp-token"),
            ("DELETE", "/operator/v1/mcp-token"),
            ("GET", "/operator/v1/send/usage"),
        ):
            assert client.request(method, path, headers={"Authorization": f"Bearer {NEW}"}).status_code == 404


def test_auth_reader_uses_read_only_connection(auth_store):
    rotate(auth_store)
    conn = whatsapp._connect_messages_db()
    with pytest.raises(sqlite3.OperationalError, match="readonly"):
        conn.execute("DELETE FROM runtime_settings")
    conn.close()


@pytest.mark.parametrize("configured", [None, 0, 2])
def test_rotation_activates_default_guessing_throttle(auth_store, configured):
    bearer = BearerTokenMiddleware(_ok_app, None, runtime_rotation=True)
    limited = RuntimeRateLimitMiddleware(bearer, configured, None)
    limited.limited._clock = lambda: 1.0
    client = TestClient(limited)
    if configured is None:
        for _ in range(121):
            assert client.get("/mcp").status_code == 200
    rotate(auth_store, previous=None)
    count = configured or 120
    for _ in range(count):
        assert response(client, "fake-wrong-token").status_code == 401
    assert response(client, "fake-wrong-token").status_code == (401 if configured == 0 else 429)


def test_auth_reader_does_not_block_event_loop(auth_store, monkeypatch):
    import asyncio
    import threading

    import http_auth

    entered, release = threading.Event(), threading.Event()
    verify = http_auth.verify_static_token

    def held(*args, **kwargs):
        entered.set()
        assert release.wait(2)
        return verify(*args, **kwargs)

    monkeypatch.setattr(http_auth, "verify_static_token", held)

    async def probe():
        messages = []

        async def send(message):
            messages.append(message)

        task = asyncio.create_task(
            BearerTokenMiddleware(_ok_app, OLD)(
                {"type": "http", "headers": [(b"authorization", f"Bearer {OLD}".encode())]}, None, send
            )
        )
        try:
            assert await asyncio.to_thread(entered.wait, 1)
            assert not task.done()
        finally:
            release.set()
        await task
        assert messages[0]["status"] == 200

    asyncio.run(probe())
