"""Disposable split-network proof; initialize only, never invoke MCP tools."""

import errno
import json
import os
import socket
import sqlite3
import time
import urllib.error
import urllib.request
from urllib.parse import urlsplit

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def get(url, token="", host=None):
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    if host:
        headers["Host"] = host
    return OPENER.open(urllib.request.Request(url, headers=headers), timeout=5)


def refused(host, ports):
    # Resolve first: a missing alias does not prove listener isolation.
    address = socket.gethostbyname(host)
    for port in ports:
        try:
            with socket.create_connection((address, port), timeout=3):
                pass
        except OSError as exc:
            assert exc.errno == errno.ECONNREFUSED, (host, port, exc)
        else:
            raise AssertionError(f"{host}:{port} unexpectedly reachable")
        print(f"{host} ({address}):{port} -> ECONNREFUSED", flush=True)


def mcp():
    import export
    import media_notes
    import private_files
    import whatsapp

    assert os.getuid() == 1000
    url = whatsapp.WHATSAPP_API_BASE_URL + "/health"
    assert socket.gethostbyname(urlsplit(url).hostname) == os.environ["PROBE_BRIDGE_IP"]
    # Use the production token resolver (including the generated token file).
    token = whatsapp._bridge_headers()["Authorization"].removeprefix("Bearer ")
    response = whatsapp._bridge_request("GET", "/health", timeout=5)
    assert response.status_code == 200 and response.json()["paired"] is False
    for label, credential, host in (
        ("missing token", "", None),
        ("wrong token", "other-instance-token", None),
        ("wrong Host", token, "example.test:8080"),
    ):
        try:
            get(url, credential, host).close()
        except urllib.error.HTTPError as exc:
            assert exc.code == (403 if host else 401), (label, exc.code)
        else:
            raise AssertionError(label + " accepted")
        print(f"bridge {label} -> denied", flush=True)
    for connect in (whatsapp._connect_messages_db, whatsapp._connect_whatsmeow_db):
        with connect() as conn:
            assert conn.execute("SELECT count(*) FROM sqlite_master").fetchone()[0] > 0
            try:
                conn.execute("CREATE TABLE split_smoke_forbidden(x)")
            except sqlite3.OperationalError as exc:
                assert "readonly" in str(exc)
            else:
                raise AssertionError("bridge-owned DB accepted write")
    with media_notes._connect(create=True) as conn:
        assert conn.execute("SELECT count(*) FROM sqlite_master").fetchone()[0] > 0
    import transcription_usage

    with transcription_usage._connection() as conn:
        conn.execute(
            "INSERT INTO transcription_usage VALUES (?, 'whisper_cpp', 'fake-model', 'manual', 12, 1)",
            (transcription_usage.month_now(),),
        )
    assert export.export_dir() == "/app/outbox/exports"
    with private_files.private_open(export.resolve_export_path("split-smoke.ndjson"), "w") as handle:
        handle.write('{"fake":true}\n')
    with private_files.private_open("/app/outbox/.uploads/split-smoke.txt", "w") as handle:
        handle.write(os.environ["PROBE_INSTANCE"])
    with get(os.environ["WHISPER_URL"].replace("/inference", "/")) as response:
        assert response.status == 200
    print(
        "MCP uid=1000: authenticated bridge, two read-only databases, writable notes/exports/uploads, whisper HTTP -> PASS",
        flush=True,
    )


def operator():
    for instance in ("A", "B"):
        alias = os.environ[f"OPERATOR_ALIAS_{instance}"]
        port = os.environ[f"OPERATOR_PORT_{instance}"]
        with get(
            f"http://{alias}:{port}/operator/v1/health",
            os.environ["WHATSAPP_OPERATOR_TOKEN"],
        ) as response:
            assert response.status == 200
            health = json.load(response)
            assert "last_mcp_call_at" in health and health["last_mcp_call_at"] is None
        started = time.monotonic()
        with get(f"http://{alias}:{port}/operator/v1/transcription/usage", os.environ["WHATSAPP_OPERATOR_TOKEN"]) as response:
            usage = json.load(response)
            assert response.status == 200 and usage["seconds"] == 12 and usage["requests"] == 1, usage
        elapsed = time.monotonic() - started
        assert elapsed < 5, elapsed
        print(f"{alias}:{port} usage -> 200 seconds=12 requests=1 in {elapsed:.3f}s", flush=True)
        print(f"{alias}:{port} authenticated operator health -> 200", flush=True)
        refused(alias, (8000, 8080, 8091))
    admin_isolation()


def proxy():
    for instance in ("A", "B"):
        refused(os.environ[f"PROBE_ALIAS_{instance}"], (int(os.environ["OPERATOR_PORT_B"]), 8091))
    admin_isolation()


def admin():
    host = os.environ["PROBE_ADMIN_NAME"] + ":8091"
    url = "http://" + os.environ["PROBE_ADMIN_IP"] + ":8091/admin/v1/transcription/usage"
    token = os.environ["PROBE_BRIDGE_TOKEN"]
    with get(url, token, host) as response:
        assert response.status == 200 and json.load(response)["seconds"] == 12
    for label, credential, authority, expected in (
        ("MCP bearer", os.environ["WHATSAPP_MCP_TOKEN"], host, 401),
        ("operator token", os.environ["WHATSAPP_OPERATOR_TOKEN"], host, 401),
        ("wrong Host", token, "example.test:8091", 403),
        ("loopback Host", token, "127.0.0.1:8091", 403),
    ):
        try:
            get(url, credential, authority).close()
        except urllib.error.HTTPError as exc:
            assert exc.code == expected, (label, exc.code)
        else:
            raise AssertionError(label + " accepted")
        print(f"admin {label} -> {expected}", flush=True)


def admin_isolation():
    for instance in ("A", "B"):
        address = os.environ[f"PROBE_ADMIN_IP_{instance}"]
        try:
            with socket.create_connection((address, 8091), timeout=2):
                pass
        except OSError as exc:
            assert isinstance(exc, TimeoutError) or exc.errno in (errno.ECONNREFUSED, errno.EHOSTUNREACH, errno.ETIMEDOUT), exc
        else:
            raise AssertionError("admin reachable by agent IP from another network")
        name = os.environ[f"PROBE_ADMIN_NAME_{instance}"]
        # The intentional same-named peer answers: prove it is not the admin.
        try:
            get(f"http://{name}:8091/admin/v1/transcription/usage").close()
        except urllib.error.HTTPError as exc:
            assert exc.code == 409 and json.load(exc) == {"name_collision": True}
        else:
            raise AssertionError("admin name reached an unexpected responder")
        print(f"admin {name} name-collision check -> 409; agent IP {address}:8091 -> blocked", flush=True)


if __name__ == "__main__":
    {"mcp": mcp, "operator": operator, "proxy": proxy, "admin": admin}[os.environ["PROBE_ROLE"]]()
