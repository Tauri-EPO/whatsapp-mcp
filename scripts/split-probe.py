"""Disposable split-network proof; initialize only, never invoke MCP tools."""

import errno
import json
import os
import socket
import sqlite3
import urllib.error
import urllib.request

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def get(url, token="", host=None):
    headers = {"Authorization": f"Bearer {token}"}
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
    # Use the production token resolver (including the generated token file).
    token = whatsapp._bridge_headers()["Authorization"].removeprefix("Bearer ")
    with get(url, token) as response:
        assert response.status == 200 and json.load(response)["paired"] is False
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
    assert export.export_dir() == "/app/outbox/exports"
    with private_files.private_open(
        export.resolve_export_path("split-smoke.ndjson"), "w"
    ) as handle:
        handle.write('{"fake":true}\n')
    with private_files.private_open(
        "/app/outbox/.uploads/split-smoke.txt", "w"
    ) as handle:
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
        print(f"{alias}:{port} authenticated operator health -> 200", flush=True)
        refused(alias, (8000, 8080))


if __name__ == "__main__":
    {"mcp": mcp, "operator": operator}[os.environ["PROBE_ROLE"]]()
