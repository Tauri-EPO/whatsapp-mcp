"""HTTP bytes -> opaque upload ID -> MCP send -> fake bridge -> cleanup."""

import asyncio
import hashlib
import os
from datetime import UTC, datetime, timedelta
from pathlib import Path
from urllib.parse import quote

import pytest
from mcp.server.mcpserver import MCPServer
from starlette.testclient import TestClient

import main
import media_upload
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from http_auth import MAX_UPLOAD_ENV, resolve_upload_max_bytes
from http_upload import UploadApp
from mcp_config import build_transport_security

TOKEN = "fake-upload-token-0123456789"
HEADERS = {"Authorization": f"Bearer {TOKEN}", "X-Filename": "report.pdf"}
ALICE = "12025551234@s.whatsapp.net"


@pytest.fixture(autouse=True)
def isolated(monkeypatch, tmp_path):
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path / "outbox"))
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "http")
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "fake-token")
    monkeypatch.delenv(MAX_UPLOAD_ENV, raising=False)

    def unexpected_bridge(*args, **kwargs):
        raise AssertionError("Every bridge call in an upload test must use a fake")

    monkeypatch.setattr(whatsapp, "_bridge_request", unexpected_bridge)
    tool_policy.set_active_policy(tool_policy.ToolPolicy())
    yield
    tool_policy.set_active_policy(None)


def app(transport="http", **kwargs):
    server = MCPServer("uploads")
    server.tool()(main.send_file)
    server.tool()(main.send_audio_message)
    if "upload_max_bytes" not in kwargs:
        kwargs["upload_max_bytes"] = resolve_upload_max_bytes(os.getenv(MAX_UPLOAD_ENV))
    return main.build_http_app(server, transport, TOKEN, host="127.0.0.1", **kwargs)


def client(transport="http", **kwargs):
    return TestClient(app(transport, **kwargs), base_url="http://localhost:8000")


def remaining():
    root = Path(media_upload.upload_dir())
    return list(root.iterdir()) if root.exists() else []


def test_upload_policy_read_failure_is_denied_without_storing(monkeypatch):
    import http_upload

    def unavailable():
        raise ToolError("denied", "Runtime tool policy unavailable")

    monkeypatch.setattr(http_upload, "active_policy", unavailable)
    with client(stateless_http=True, json_response=True) as c:
        response = c.post("/upload", content=b"fake bytes", headers=HEADERS)
    assert response.status_code == 403
    assert response.json()["error"]["code"] == "denied"
    assert remaining() == []


@pytest.mark.parametrize("tool,filename", [("send_file", "report.pdf"), ("send_audio_message", "voice.ogg")])
def test_upload_then_real_mcp_send_removes_file(monkeypatch, tool, filename):
    content = b"%PDF-fake\n" + b"x" * (5 * 1024 * 1024)
    calls = []

    def bridge(method, endpoint, **kwargs):
        path = Path(kwargs["json"]["media_path"])
        assert path.is_relative_to(Path(media_upload.upload_dir()))
        calls.append((endpoint, path.read_bytes(), path.name))

        class Response:
            status_code = 200

            def json(self):
                return {"success": True, "message": "sent"}

        return Response()

    monkeypatch.setattr(whatsapp, "_bridge_request", bridge)
    with client(stateless_http=True, json_response=True) as c:
        uploaded = c.post("/upload", content=content, headers={**HEADERS, "X-Filename": "../" + filename})
        assert uploaded.status_code == 200
        receipt = uploaded.json()
        assert set(receipt) == {"upload_id", "filename", "bytes", "sha256", "expires_at"}
        assert receipt["filename"] == filename
        assert receipt["bytes"] == len(content)
        assert receipt["sha256"] == hashlib.sha256(content).hexdigest()
        assert datetime.fromisoformat(receipt["expires_at"]) > datetime.now(UTC)
        assert len(remaining()) == 1
        sent = c.post(
            "/mcp",
            headers={**HEADERS, "Accept": "application/json, text/event-stream"},
            json={
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": tool, "arguments": {"chat_jid": ALICE, "upload_id": receipt["upload_id"]}},
            },
        )
        assert sent.status_code == 200
        assert sent.json()["result"]["structuredContent"]["success"] is True
    assert calls == [("/send", content, filename)]
    assert remaining() == []


@pytest.mark.parametrize("transport", ["http", "sse"])
@pytest.mark.parametrize(
    "headers,status",
    [
        ({}, 401),
        ({"Authorization": "Bearer wrong-token"}, 401),
        ({**HEADERS, "Host": "evil.example.com"}, 421),
        ({**HEADERS, "Origin": "https://evil.example.com"}, 403),
    ],
)
def test_auth_host_and_origin_denials(transport, headers, status):
    with client(transport) as c:
        assert c.post("/upload", content=b"secret bytes", headers=headers).status_code == status
    assert remaining() == []


def test_custom_host_origin_and_sse_upload():
    security = build_transport_security("0.0.0.0", "example.ts.net")
    with client("sse", transport_security=security) as c:
        response = c.post(
            "/upload", content=b"abc", headers={**HEADERS, "Host": "example.ts.net", "Origin": "https://example.ts.net"}
        )
        assert response.status_code == 200
    assert len(remaining()) == 1


def test_rate_limit_is_shared_with_mcp_and_runs_before_auth():
    with client(rate_limit_per_minute=1) as c:
        assert c.post("/upload", content=b"abc").status_code == 401
        assert c.post("/mcp", json={}, headers=HEADERS).status_code == 429
    assert remaining() == []


@pytest.mark.parametrize(
    "policy",
    [
        tool_policy.ToolPolicy(read_only=True),
        tool_policy.ToolPolicy(allow=frozenset({"send_message"})),
        tool_policy.ToolPolicy(deny=frozenset({"send_file", "send_audio_message"})),
    ],
)
def test_policy_denials(policy):
    tool_policy.set_active_policy(policy)
    with client() as c:
        assert c.post("/upload", content=b"abc", headers=HEADERS).status_code == 403
    assert remaining() == []


@pytest.mark.parametrize("allowed", ["send_file", "send_audio_message"])
def test_either_sending_tool_allows_upload(allowed):
    tool_policy.set_active_policy(tool_policy.ToolPolicy(allow=frozenset({allowed})))
    with client() as c:
        assert c.post("/upload", content=b"abc", headers=HEADERS).status_code == 200


@pytest.mark.parametrize("declared", [None, "1", "9999"])
async def test_stream_limit_ignores_content_length_hint_and_cleans_partial(monkeypatch, declared):
    monkeypatch.setenv(MAX_UPLOAD_ENV, "8")
    upload = UploadApp(None, None, limit=8)
    chunks = iter(
        [
            {"type": "http.request", "body": b"12345", "more_body": True},
            {"type": "http.request", "body": b"6789", "more_body": False},
        ]
    )
    headers = [(b"x-filename", b"file.bin")]
    if declared is not None:
        headers.append((b"content-length", declared.encode()))
    response = []

    async def receive():
        # There really is a partial file between the two ASGI chunks.
        if remaining():
            assert (remaining()[0] / ".pending").exists()
        return next(chunks)

    async def send(message):
        response.append(message)

    await upload({"type": "http", "method": "POST", "path": "/upload", "headers": headers}, receive, send)
    assert response[0]["status"] == 413
    assert remaining() == []


def test_own_limit_and_empty_and_method(monkeypatch):
    monkeypatch.setenv(MAX_UPLOAD_ENV, "8")
    with client() as c:
        assert c.post("/upload", content=b"12345678", headers=HEADERS).status_code == 200
        saved = remaining()
        assert c.post("/upload", content=b"123456789", headers=HEADERS).status_code == 413
        assert c.post("/upload", content=b"", headers=HEADERS).status_code == 400
        assert c.get("/upload", headers=HEADERS).status_code == 405
        assert remaining() == saved


def test_json_rpc_cap_still_applies_only_to_mcp():
    with client(max_request_body_size=1024) as c:
        assert c.post("/upload", content=b"x" * 2048, headers=HEADERS).status_code == 200
        assert c.post("/mcp", content=b"x" * 2048, headers=HEADERS).status_code == 413


@pytest.mark.parametrize("value", ["no", "0", "-1"])
def test_bad_limit_is_startup_error(monkeypatch, value):
    monkeypatch.setenv(MAX_UPLOAD_ENV, value)
    with pytest.raises(ValueError, match=MAX_UPLOAD_ENV):
        resolve_upload_max_bytes(value)


def test_ttl_startup_and_new_upload_sweep():
    root = Path(media_upload.upload_dir())
    old_stamp = (datetime.now(UTC) - timedelta(hours=2)).strftime("%Y%m%dT%H%M%SZ")
    expired = root / (old_stamp + "-" + "a" * 32)
    expired.mkdir(parents=True)
    (expired / "report.pdf").write_bytes(b"abc")
    with client() as c:
        assert not expired.exists()
        expired.mkdir()
        (expired / "report.pdf").write_bytes(b"abc")
        assert c.post("/upload", content=b"new", headers=HEADERS).status_code == 200
        assert not expired.exists()


@pytest.mark.parametrize(
    "upload_id",
    ["missing", "../report.pdf", "/etc/passwd", "99999999T999999Z-" + "a" * 32, "20000101T000000Z-" + "a" * 32],
)
def test_unknown_traversal_and_expired_ids(upload_id):
    with pytest.raises(ToolError) as exc:
        whatsapp.send_file(ALICE, upload_id=upload_id)
    assert exc.value.code == "not_found"


@pytest.mark.parametrize("send", [main.send_file, main.send_audio_message])
def test_stdio_refuses_upload_id(monkeypatch, send):
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "stdio")
    result = send(ALICE, upload_id="missing")
    assert result["error"]["code"] == "invalid_argument"
    assert "stdio" in result["error"]["message"]


@pytest.mark.parametrize("send", [main.send_file, main.send_audio_message])
@pytest.mark.parametrize("kwargs", [{"media_path": "x"}, {"media_base64": "eA=="}])
def test_exactly_one_source(send, kwargs):
    result = send(ALICE, upload_id="missing", **kwargs)
    assert result["error"]["code"] == "invalid_argument"


def test_send_failure_preserves_upload_for_retry(monkeypatch):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()

    def fail(*args, **kwargs):
        raise ToolError("bridge_unavailable", "fake failure")

    monkeypatch.setattr(whatsapp, "_bridge_request", fail)
    assert main.send_file(ALICE, upload_id=receipt["upload_id"])["error"]["code"] == "bridge_unavailable"
    assert len(remaining()) == 1
    assert (remaining()[0] / "report.pdf").read_bytes() == b"abc"


def test_dry_run_preserves_upload(monkeypatch):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()
    monkeypatch.setattr(whatsapp, "_recipient_preview", lambda recipient: {})
    assert main.send_file(ALICE, upload_id=receipt["upload_id"], dry_run=True)["dry_run"]
    assert len(remaining()) == 1


def test_denied_recipient_does_not_consume_upload(monkeypatch):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888@s.whatsapp.net"]))
    assert main.send_file(ALICE, upload_id=receipt["upload_id"])["error"]["code"] == "denied"
    assert len(remaining()) == 1


def test_windows_drive_case_is_not_a_symlink(monkeypatch):
    root = media_upload.media_root()
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", root[:1].lower() + root[1:])
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()
    with media_upload.uploaded_path(receipt["upload_id"]) as path:
        assert Path(path).read_bytes() == b"abc"
    assert remaining() == []


def test_missing_server_path_error_suggests_upload():
    result = main.send_file(ALICE, media_path="/missing/report.pdf")
    assert result["error"]["code"] == "not_found"
    assert "upload_id" in result["error"]["message"] and "media_base64" in result["error"]["message"]


async def test_disconnect_removes_partial_and_does_not_log_body(caplog):
    upload = UploadApp(None, None)
    events = iter(
        [
            {"type": "http.request", "body": b"private fake body", "more_body": True},
            {"type": "http.disconnect"},
        ]
    )
    response = []

    async def receive():
        return next(events)

    async def send(message):
        response.append(message)

    await upload(
        {"type": "http", "method": "POST", "path": "/upload", "headers": [(b"x-filename", b"file.bin")]}, receive, send
    )
    assert response[0]["status"] == 500
    assert remaining() == []
    assert "private fake body" not in caplog.text


def test_active_send_is_not_swept_or_consumed_twice(monkeypatch):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()
    with media_upload.uploaded_path(receipt["upload_id"]) as path:
        with pytest.raises(ToolError, match="in use; retry the same ID") as busy:
            whatsapp.send_file(ALICE, upload_id=receipt["upload_id"])
        assert busy.value.code == "conflict"
        monkeypatch.setattr(media_upload, "upload_expiry", lambda upload_id: 0)
        media_upload.sweep_uploads()
        assert Path(path).read_bytes() == b"abc"
    assert remaining() == []


def test_partial_is_not_sendable():
    folder = Path(media_upload.create_upload())
    (folder / ".pending").write_bytes(b"partial")
    result = main.send_file(ALICE, upload_id=folder.name)
    assert result["error"]["code"] == "not_found"


@pytest.mark.parametrize("link_location", ["root", "folder", "file"])
def test_symlinks_refused(tmp_path, link_location):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers=HEADERS).json()
    root = Path(media_upload.upload_dir())
    folder = root / receipt["upload_id"]
    target = tmp_path / "outside"
    target.mkdir()
    (target / "report.pdf").write_bytes(b"outside")
    link = {"root": root, "folder": folder, "file": folder / "report.pdf"}[link_location]
    moved = link.with_name(link.name + "-moved")
    link.rename(moved)
    try:
        link.symlink_to(
            target / "report.pdf" if link_location == "file" else target, target_is_directory=link_location != "file"
        )
    except OSError:
        moved.rename(link)
        pytest.skip("This Windows host does not permit symlink creation")
    result = main.send_file(ALICE, upload_id=receipt["upload_id"])
    assert result["error"]["code"] in ("denied", "not_found")
    assert (target / "report.pdf").read_bytes() == b"outside"


@pytest.mark.parametrize("name", ["relatório-ação.pdf", "字" * 200 + ".pdf"])
def test_percent_encoded_utf8_name_survives_upload_and_send(monkeypatch, name):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers={**HEADERS, "X-Filename": quote(name)}).json()
    expected = media_upload.safe_filename(name, "file")
    assert receipt["filename"] == expected
    assert len(expected.encode("utf-8")) <= 200
    seen = []
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: {"success": True})
    monkeypatch.setattr(
        whatsapp, "_bridge_request", lambda *args, **kwargs: seen.append(Path(kwargs["json"]["media_path"]).name)
    )
    assert main.send_file(ALICE, upload_id=receipt["upload_id"])["success"]
    assert seen == [expected]
    assert remaining() == []


@pytest.mark.parametrize(
    "headers,status",
    [
        ({"Authorization": f"Bearer {TOKEN}"}, 400),
        ({**HEADERS, "X-Filename": "%FF.pdf"}, 400),
        ({**HEADERS, "Content-Type": "multipart/form-data; boundary=fake"}, 415),
        ({**HEADERS, "Content-Length": "-1"}, 400),
    ],
)
def test_bad_upload_metadata_creates_nothing(headers, status):
    with client() as c:
        response = c.post("/upload", content=b"abc", headers=headers)
        assert response.status_code == status
        assert response.json()["error"]["code"] == "invalid_argument"
    assert remaining() == []


async def test_honest_oversize_header_is_rejected_before_reading():
    upload = UploadApp(None, None, limit=8)
    response = []

    async def receive():
        raise AssertionError("No body should be read for an oversized declared length")

    async def send(message):
        response.append(message)

    await upload(
        {
            "type": "http",
            "method": "POST",
            "path": "/upload",
            "headers": [(b"x-filename", b"file.bin"), (b"content-length", b"9")],
        },
        receive,
        send,
    )
    assert response[0]["status"] == 413
    assert remaining() == []


def test_aggregate_limit_keeps_existing_upload_and_cleans_new_partial(monkeypatch):
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", 5)
    with client(upload_max_bytes=5) as c:
        assert c.post("/upload", content=b"abc", headers=HEADERS).status_code == 200
        response = c.post("/upload", content=b"def", headers=HEADERS)
        assert response.status_code == 413
        assert response.json()["error"]["code"] == "too_large"
        assert response.json()["limit_bytes"] == 5
    assert len(remaining()) == 1
    assert (remaining()[0] / "report.pdf").read_bytes() == b"abc"


def test_aggregate_limit_counts_other_receiving_transfers(monkeypatch):
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", 5)
    with media_upload.receiving_upload() as upload:
        with open(upload.pending, "xb", buffering=0) as handle:
            upload.write(handle, b"ab")
        with client(upload_max_bytes=5) as c:
            assert c.post("/upload", content=b"cdef", headers=HEADERS).status_code == 413
        assert Path(upload.pending).read_bytes() == b"ab"
    assert remaining() == []


def test_audio_conversion_failure_preserves_id(monkeypatch):
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers={**HEADERS, "X-Filename": "voice.wav"}).json()

    def unavailable(*args, **kwargs):
        raise FileNotFoundError("fake ffmpeg unavailable")

    monkeypatch.setattr(whatsapp.audio, "convert_to_opus_ogg_temp", unavailable)
    assert main.send_audio_message(ALICE, upload_id=receipt["upload_id"])["error"]["code"] == "internal"
    assert (remaining()[0] / "voice.wav").read_bytes() == b"abc"


def test_configured_root_may_cross_symlink_for_http_and_inline(monkeypatch, tmp_path):
    target = tmp_path / "resolved"
    target.mkdir()
    alias = tmp_path / "alias"
    try:
        alias.symlink_to(target, target_is_directory=True)
    except OSError:
        pytest.skip("This Windows host does not permit symlink creation")
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(alias / "outbox"))
    path = media_upload.write_inline(b"inline", "file.pdf")
    assert Path(path).read_bytes() == b"inline"
    assert Path(path).is_relative_to(target)
    media_upload.discard(path)
    with client() as c:
        receipt = c.post("/upload", content=b"http", headers=HEADERS).json()
    with media_upload.uploaded_path(receipt["upload_id"]) as path:
        assert Path(path).read_bytes() == b"http"
    assert remaining() == []


def test_undeletable_expired_upload_does_not_break_startup_or_upload(monkeypatch):
    root = Path(media_upload.upload_dir())
    expired = root / ("20000101T000000Z-" + "b" * 32)
    expired.mkdir(parents=True)
    (expired / "file.pdf").write_bytes(b"old")
    original = media_upload.shutil.rmtree

    def denied(path, **kwargs):
        if Path(path) == expired:
            if kwargs.get("ignore_errors"):
                return
            raise PermissionError("fake denied")
        return original(path, **kwargs)

    monkeypatch.setattr(media_upload.shutil, "rmtree", denied)
    with client() as c:
        assert c.post("/upload", content=b"new", headers=HEADERS).status_code == 200
    assert expired.exists()


def test_unreadable_sweep_directory_is_best_effort(monkeypatch):
    Path(media_upload.upload_dir()).mkdir(parents=True)
    monkeypatch.setattr(Path, "iterdir", lambda path: (_ for _ in ()).throw(PermissionError("fake denied")))
    media_upload.sweep_uploads()


def test_periodic_sweep_removes_expired_upload_without_another_request(monkeypatch):
    import http_upload

    monkeypatch.setattr(http_upload, "SWEEP_INTERVAL_S", 0.01)
    with client() as c:
        expired = Path(media_upload.upload_dir()) / ("20000101T000000Z-" + "c" * 32)
        expired.mkdir(parents=True)
        (expired / "file.pdf").write_bytes(b"old")

        async def observe():
            for _ in range(100):
                if not expired.exists():
                    return
                await asyncio.sleep(0.01)
            raise AssertionError("Periodic sweep never removed the expired upload")

        c.portal.call(observe)
    assert remaining() == []


def test_invalid_upload_limit_exits_cleanly_before_serving(tmp_path):
    import subprocess
    import sys

    result = subprocess.run(
        [sys.executable, str(Path(main.__file__).resolve())],
        env={**os.environ, MAX_UPLOAD_ENV: "64MiB", "WHATSAPP_MCP_TRANSPORT": "http"},
        cwd=tmp_path,
        capture_output=True,
        text=True,
        timeout=15,
    )
    assert result.returncode == 1
    assert MAX_UPLOAD_ENV + " must be a positive integer" in result.stderr
    assert "Traceback" not in result.stderr


def test_storage_failure_uses_error_envelope_and_cleans_partial(monkeypatch):
    def fail(*args):
        raise OSError("fake disk failure")

    monkeypatch.setattr(media_upload.ReceivingUpload, "write", fail)
    with client() as c:
        response = c.post("/upload", content=b"abc", headers=HEADERS)
        assert response.status_code == 500
        assert response.json() == {"error": {"code": "internal", "message": "Upload could not be stored"}}
    assert remaining() == []


def test_control_characters_removed_from_decoded_name():
    with client() as c:
        receipt = c.post("/upload", content=b"abc", headers={**HEADERS, "X-Filename": "fake%C2%80.pdf"}).json()
        assert receipt["filename"] == "fake.pdf"


@pytest.mark.parametrize("name", ["..", "%20", "reports/"])
def test_names_without_a_usable_basename_are_refused(name):
    with client() as c:
        response = c.post("/upload", content=b"x", headers={**HEADERS, "X-Filename": name})
        assert response.status_code == 400
    assert remaining() == []


@pytest.mark.parametrize("send", [main.send_file, main.send_audio_message])
def test_upload_id_cannot_silently_ignore_a_filename(send):
    with client() as c:
        receipt = c.post("/upload", content=b"x", headers=HEADERS).json()
    response = send(ALICE, upload_id=receipt["upload_id"], filename="photo.jpg")
    assert response["error"]["code"] == "invalid_argument"
    assert "fixed at upload" in response["error"]["message"]
    assert len(remaining()) == 1


def test_limit_cannot_exceed_shared_capacity():
    with pytest.raises(ValueError, match="536870912.*268435456"):
        resolve_upload_max_bytes("536870912")


def test_stdio_first_write_sweeps_legacy_and_conversion_orphans(monkeypatch):
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "stdio")
    root = Path(media_upload.upload_dir())
    legacy = root / ("20000101T000000Z-" + "d" * 8)
    legacy.mkdir(parents=True)
    (legacy / "old.pdf").write_bytes(b"x" * 9)
    orphan = root / "tmp-orphan.ogg"
    orphan.write_bytes(b"x" * 9)
    os.utime(orphan, (1, 1))
    unrelated = root / "operator-file.txt"
    unrelated.write_bytes(b"x" * 100)
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", 10)
    path = media_upload.write_inline(b"new", "new.pdf")
    assert not legacy.exists() and not orphan.exists()
    assert Path(path).read_bytes() == b"new"
    assert unrelated.exists()  # Only entries the module can expire are charged/swept.
    media_upload.discard(path)


def test_concurrent_inline_cleanup_and_capacity_walk_are_safe():
    import threading
    from concurrent.futures import ThreadPoolExecutor

    start = threading.Barrier(2)

    def writer():
        start.wait()
        for _ in range(100):
            path = media_upload.write_inline(b"inline", "file.pdf")
            media_upload.discard(path)

    def observer():
        start.wait()
        for _ in range(200):
            with media_upload._lock:
                media_upload.check_outbox_capacity(0)

    with ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(writer), pool.submit(observer)]
        for future in futures:
            future.result(timeout=15)
    assert remaining() == []


def test_capacity_walk_tolerates_an_entry_removed_outside_the_lock(monkeypatch):
    path = Path(media_upload.write_inline(b"x", "file.pdf"))
    original = Path.stat
    stats = 0

    def vanished(file, *args, **kwargs):
        nonlocal stats
        if file == path:
            stats += 1
            if stats == 2:
                raise FileNotFoundError("fake race after is_file but before size")
        return original(file, *args, **kwargs)

    monkeypatch.setattr(Path, "stat", vanished)
    with media_upload._lock:
        media_upload.check_outbox_capacity(0)
    assert stats == 2
    media_upload.discard(str(path))


def test_stream_capacity_walk_runs_once_per_eight_mib_not_per_chunk(monkeypatch):
    calls = []
    original = media_upload.check_outbox_capacity

    def counted(additional):
        calls.append(additional)
        return original(additional)

    monkeypatch.setattr(media_upload, "check_outbox_capacity", counted)
    chunk = b"x" * (64 * 1024)
    with media_upload.receiving_upload() as upload:
        with open(upload.pending, "xb", buffering=0) as handle:
            for _ in range(256):  # 16 MiB, 256 network-sized chunks
                upload.write(handle, chunk)
        upload.finish("file.pdf")
        assert Path(upload.folder, "file.pdf").stat().st_size == 16 * 1024 * 1024
    assert calls == [1, 1]
    assert media_upload._reserved == {}


def test_partial_reservations_prevent_concurrent_capacity_overshoot(monkeypatch):
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", 10)
    with media_upload.receiving_upload(limit=6) as first:
        with open(first.pending, "xb", buffering=0) as handle:
            first.write(handle, b"abc")
        with media_upload.receiving_upload(limit=6) as second:
            with open(second.pending, "xb", buffering=0) as handle:
                second.write(handle, b"defg")
                with pytest.raises(ToolError, match="outbox is full"):
                    second.write(handle, b"h")
            assert Path(first.pending).read_bytes() == b"abc"
    assert remaining() == []
    assert media_upload._reserved == {}


def test_capacity_error_is_not_cached(monkeypatch):
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", 1)
    with client(upload_max_bytes=1) as c:
        assert c.post("/upload", content=b"x", headers=HEADERS).status_code == 200
        response = c.post("/upload", content=b"y", headers=HEADERS)
        assert response.status_code == 413
        assert response.headers["Cache-Control"] == "no-store"


def test_audio_missing_path_suggests_http_upload():
    response = main.send_audio_message(ALICE, media_path="/missing/voice.ogg")
    assert response["error"]["code"] == "not_found"
    assert "upload_id" in response["error"]["message"]


@pytest.mark.parametrize("budget,sent", [(50000, False), (100000, True)])
def test_real_ffmpeg_output_obeys_shared_quota_and_preserves_failed_id(monkeypatch, tmp_path, budget, sent):
    import shutil
    import subprocess

    if not shutil.which("ffmpeg"):
        pytest.skip("Real ffmpeg is exercised in the shipped-image gate")
    source = tmp_path / "voice.mp3"
    subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=440:duration=5",
            "-ar",
            "8000",
            "-b:a",
            "8k",
            "-y",
            str(source),
        ],
        check=True,
        capture_output=True,
        timeout=15,
    )
    monkeypatch.setattr(media_upload, "MAX_OUTBOX_BYTES", budget)
    with client(upload_max_bytes=budget) as c:
        receipt = c.post("/upload", content=source.read_bytes(), headers={**HEADERS, "X-Filename": "voice.mp3"}).json()
        assert c.post("/upload", content=b"x" * 40000, headers=HEADERS).status_code == 200
    calls = []

    def bridge(method, endpoint, **kwargs):
        used = sum(path.stat().st_size for path in Path(media_upload.upload_dir()).rglob("*") if path.is_file())
        calls.append(used)
        assert used <= budget
        assert Path(kwargs["json"]["media_path"]).read_bytes().startswith(b"OggS")

        class Response:
            status_code = 200

            def json(self):
                return {"success": True}

        return Response()

    monkeypatch.setattr(whatsapp, "_bridge_request", bridge)
    response = main.send_audio_message(ALICE, upload_id=receipt["upload_id"])
    original = Path(media_upload.upload_dir()) / receipt["upload_id"] / "voice.mp3"
    if sent:
        assert response["success"] and len(calls) == 1
        assert not original.exists()
        assert len(remaining()) == 1
    else:
        assert response["error"]["code"] == "too_large"
        assert calls == []
        assert original.read_bytes() == source.read_bytes()
        assert len(remaining()) == 2
    assert media_upload._reserved == {}


@pytest.mark.parametrize("transport", ["http", "sse"])
@pytest.mark.parametrize("header,status", [("Host", 421), ("Origin", 403)])
def test_explicit_none_security_uses_same_loopback_defaults_as_sdk(transport, header, status):
    bad = "evil.example.com" if header == "Host" else "https://evil.example.com"
    with client(transport, transport_security=None) as c:
        assert c.post("/upload", content=b"fake", headers={**HEADERS, header: bad}).status_code == status
    assert remaining() == []
