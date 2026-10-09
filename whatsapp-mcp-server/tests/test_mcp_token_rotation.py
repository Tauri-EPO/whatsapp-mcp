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
from http_auth import BearerTokenMiddleware, verify_static_token
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


def test_corrupt_or_unreadable_auth_fails_closed(auth_store):
    rotate(auth_store)
    with closing(auth_store.messages()) as conn, conn:
        conn.execute("UPDATE runtime_settings SET value='{}' WHERE key='auth.mcp_token'")
    assert not verify_static_token(OLD, OLD)
    Path(whatsapp.MESSAGES_DB_PATH).write_bytes(b"not a database")
    assert not verify_static_token(OLD, OLD)


@pytest.mark.parametrize("expected", [OLD, None])
def test_missing_store_or_registry_never_restores_deploy_auth(auth_store, expected):
    client = TestClient(BearerTokenMiddleware(_ok_app, expected, runtime_rotation=True))
    rotate(auth_store, until=datetime.now(UTC) - timedelta(seconds=1))
    assert response(client, OLD).status_code == 401
    assert response(client, NEW).status_code == 200
    path = Path(whatsapp.MESSAGES_DB_PATH)
    saved = path.with_suffix(".saved")
    path.rename(saved)
    try:
        assert response(client, OLD).status_code == 401
        assert client.get("/mcp").status_code == 401
        fresh = TestClient(BearerTokenMiddleware(_ok_app, expected, runtime_rotation=True))
        assert response(fresh, OLD).status_code == 401
    finally:
        saved.rename(path)
    with closing(auth_store.messages()) as conn, conn:
        conn.execute("DROP TABLE runtime_settings")
    assert response(client, OLD).status_code == 401
    assert client.get("/mcp").status_code == 401


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
