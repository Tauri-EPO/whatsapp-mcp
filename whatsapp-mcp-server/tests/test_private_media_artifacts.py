"""Actual MCP-owned files, HTTP uploads, payload namespaces and parser results."""

import base64
import errno
import json
import logging
import os
import sqlite3
import stat
import subprocess
import sys
import time
from pathlib import Path

import pytest

import chat_policy
import export
import media_notes
import media_read
import media_text
import media_upload
import notes
import private_files
import whatsapp
from errors import ToolError
from tests.conftest import ALICE
from tests.test_export_messages import store as store
from tests.test_http_upload import HEADERS, client
from tests.test_read_media_text import _cache, _insert, make_pdf
from tests.test_send_inline import DummyResponse


@pytest.mark.parametrize(
    "raw,expected",
    [
        ("photo\u202eexe.png", "photoexe.png"),
        ("report\u200b.pdf", "report.pdf"),
        ("CO\u200bN.txt", "_CON.txt"),
        ("CON.tar.gz", "_CON.tar.gz"),
        ("COM1.ogg", "_COM1.ogg"),
        ("com0.ogg", "_com0.ogg"),
        ("LpT0.pdf", "_LpT0.pdf"),
        ("conin$.txt", "_conin$.txt"),
        ("ConOut$.tar.gz", "_ConOut$.tar.gz"),
        ("report\u2028part\u2029.pdf", "reportpart.pdf"),
        ("lpt³.pdf", "_lpt³.pdf"),
        ("NUL .txt", "_NUL .txt"),
        ("COM10.ogg", "COM10.ogg"),
        ("family\u200dphoto.png", "family\u200dphoto.png"),
        ("flag\U000e0067.png", "flag\U000e0067.png"),
        ("../report.pdf", "report.pdf"),
        ("a\\report.pdf", "report.pdf"),
    ],
)
def test_filename_uses_glyph_rules_and_reserved_device_detection(raw, expected):
    assert media_upload.safe_filename(raw, "file") == expected


def test_long_filename_preserves_extension_after_invisible_cleanup():
    name = media_upload.safe_filename("á" * 400 + "\u202e.pdf", "file")
    assert name.endswith(".pdf") and len(name.encode()) <= 200


POSIX = pytest.mark.skipif(os.name != "posix", reason="POSIX file modes/symlinks")


def mode(path):
    return stat.S_IMODE(os.stat(path).st_mode)


@POSIX
@pytest.mark.parametrize("connector", [notes._connect, media_notes._connect])
@pytest.mark.parametrize("mask", [0o022, 0o077, 0o777])
def test_both_notes_factories_create_private_database_and_live_siblings(tmp_path, monkeypatch, connector, mask):
    store = tmp_path / "store"
    store.mkdir(mode=0o755)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store / "messages.db"))
    previous = os.umask(mask)
    conn = None
    try:
        conn = connector(create=True)
        conn.execute("CREATE TABLE permission_probe (value TEXT)")
        conn.execute("INSERT INTO permission_probe VALUES ('synthetic')")
        conn.commit()
        path = store / "notes.db"
        assert [mode(str(path) + suffix) for suffix in ("", "-wal", "-shm")] == [0o600] * 3
        assert mode(store) == 0o755
    finally:
        if conn is not None:
            conn.close()
        os.umask(previous)


@POSIX
def test_export_and_upload_paths_are_private_without_changing_existing_ancestors(tmp_path, monkeypatch):
    ancestor = tmp_path / "existing"
    ancestor.mkdir(mode=0o755)
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(ancestor / "exports"))
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(ancestor / "outbox"))
    previous = os.umask(0o022)
    try:
        destination = export.resolve_export_path("nested/archive.ndjson")
        with private_files.private_open(destination + ".part", "w", encoding="utf-8") as handle:
            handle.write('{"synthetic":true}\n')
        uploaded = Path(media_upload.write_inline(b"synthetic", "report.pdf"))
        for folder in (
            ancestor / "exports",
            Path(destination).parent,
            ancestor / "outbox",
            uploaded.parent.parent,
            uploaded.parent,
        ):
            assert mode(folder) == 0o700
        assert mode(destination + ".part") == mode(uploaded) == 0o600
        assert mode(ancestor) == 0o755
        media_upload.discard(str(uploaded))
    finally:
        os.umask(previous)


@POSIX
def test_actual_export_archive_has_private_final_mode(store, tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(tmp_path / "fresh-exports"))
    result = export.export_messages(out_path="nested/archive.ndjson")
    destination = Path(result["path"])
    assert result["count"] == 5
    assert len(destination.read_text().splitlines()) == 5
    assert mode(destination) == 0o600
    assert mode(destination.parent) == mode(destination.parent.parent) == 0o700


@POSIX
def test_startup_tightens_existing_owned_files_once_and_skips_links(tmp_path, caplog):
    bridge = tmp_path / "messages.db"
    bridge.write_bytes(b"bridge-owned")
    os.chmod(bridge, 0o644)
    database = tmp_path / "notes.db"
    for suffix in ("", "-wal", "-shm"):
        Path(str(database) + suffix).write_bytes(b"synthetic")
        os.chmod(str(database) + suffix, 0o644)
    outside = tmp_path / "outside"
    outside.mkdir(mode=0o755)
    external = outside / "public"
    external.write_bytes(b"untouched")
    os.chmod(external, 0o644)
    roots = [tmp_path / "exports", tmp_path / ".uploads"]
    for index, root in enumerate(roots):
        root.mkdir(mode=0o755)
        child = root if index == 0 else root / ("20261008T100000Z-" + "a" * 32)
        if child != root:
            child.mkdir(mode=0o755)
        (child / ("messages-all-20261008T100000Z.ndjson" if index == 0 else "bytes")).write_bytes(b"private")
        (root / "link").symlink_to(outside, target_is_directory=True)
        (root / "linked-file").symlink_to(external)
    with caplog.at_level(logging.INFO, logger="whatsapp_mcp"):
        for _ in range(2):
            private_files.tighten_existing_artifacts(str(database), *(str(root) for root in roots))
    assert len(caplog.records) == 1
    assert "5 files, 2 directories" in caplog.records[0].message
    assert [mode(str(database) + suffix) for suffix in ("", "-wal", "-shm")] == [0o600] * 3
    assert mode(external) == mode(bridge) == 0o644 and mode(outside) == 0o755
    missing = tmp_path / "absent" / "notes.db"
    private_files.tighten_existing_artifacts(str(missing), str(tmp_path / "none"), str(tmp_path / "none2"))
    assert not missing.parent.exists()


@POSIX
def test_private_file_and_notes_factory_refuse_final_symlink(tmp_path):
    outside = tmp_path / "outside"
    outside.write_bytes(b"untouched")
    target = tmp_path / "notes.db"
    target.symlink_to(outside)
    with pytest.raises(OSError):
        private_files.private_open(str(target), "w")
    with pytest.raises(OSError):
        private_files.notes_connection(str(target), create=True, timeout=1)
    assert outside.read_bytes() == b"untouched"


@pytest.mark.parametrize(
    "destination",
    [
        ".mcp-export-artifacts",
        ".mcp-export-artifacts.part",
        ".mcp-export-artifacts/archive.ndjson",
        ".mcp-export-artifacts.part/archive.ndjson",
        "nested/../.mcp-export-artifacts",
    ],
)
def test_export_metadata_namespace_is_refused_before_directory_or_data_effects(tmp_path, monkeypatch, destination):
    root = tmp_path / "not-created"
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(root))
    with pytest.raises(ToolError) as refused:
        export.export_messages(out_path=destination)
    assert refused.value.code == "invalid_argument"
    assert not root.exists()


def test_unavailable_export_manifest_keeps_completed_archive_valid(paired_dbs, tmp_path, monkeypatch, caplog):
    root = tmp_path / "exports"
    root.mkdir()
    (root / private_files.EXPORT_MANIFEST).mkdir()
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(root))
    with caplog.at_level(logging.WARNING, logger="whatsapp_mcp"):
        result = export.export_messages(out_path="normal.ndjson")
    records = [json.loads(line) for line in Path(result["path"]).read_text().splitlines()]
    assert len(records) == result["count"] and all(isinstance(row, dict) for row in records)
    assert any("ownership was not recorded" in row.message for row in caplog.records)


def test_unavailable_filesystem_root_returns_error_without_directory_effects(tmp_path, monkeypatch):
    root = tmp_path / "unavailable" / "exports"
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(root))
    monkeypatch.setattr(Path, "exists", lambda _path: False)
    created = []
    monkeypatch.setattr(Path, "mkdir", lambda path, **_kwargs: created.append(path))
    with pytest.raises(ToolError) as refused:
        export.export_messages(out_path="normal.ndjson")
    assert refused.value.code == "internal"
    assert "filesystem root" in str(refused.value) and created == []


@POSIX
def test_migration_shared_export_root_preserves_other_files_and_directories(tmp_path):
    database = tmp_path / "notes.db"
    database.write_bytes(b"notes")
    shared = tmp_path / "shared"
    shared.mkdir(mode=0o755)
    unrelated = [shared / "messages.db", shared / "whatsapp.db", shared / "runner", shared / "archive.ndjson"]
    for path in unrelated:
        path.write_bytes(b"non-owned")
        os.chmod(path, 0o755)
    (shared / "cached-media").mkdir(mode=0o755)
    custom = shared / "custom-name"
    custom.write_bytes(b"export")
    private_files.record_export(str(shared), str(custom))
    os.chmod(custom, 0o644)
    default = shared / "messages-all-20261008T100000Z.ndjson"
    default.write_bytes(b"export")
    private_files.tighten_existing_artifacts(str(database), str(shared), str(tmp_path / "missing"))
    assert mode(default) == mode(custom) == mode(database) == 0o600
    assert all(mode(path) == 0o755 for path in unrelated)
    assert mode(shared) == mode(shared / "cached-media") == 0o755


@POSIX
def test_symlinked_upload_root_does_not_prevent_real_read_only_startup(tmp_path, monkeypatch):
    external = tmp_path / "external"
    external.mkdir(mode=0o755)
    untouched = external / "public"
    untouched.write_bytes(b"untouched")
    os.chmod(untouched, 0o644)
    outbox = tmp_path / "outbox"
    outbox.mkdir()
    (outbox / ".uploads").symlink_to(external, target_is_directory=True)
    env = {
        **os.environ,
        "WHATSAPP_MEDIA_ROOTS": str(outbox),
        "WHATSAPP_EXPORT_DIR": str(tmp_path / "missing"),
        "WHATSAPP_DB_PATH": str(tmp_path / "messages.db"),
        "WHATSAPP_MCP_TRANSPORT": "stdio",
        "WHATSAPP_READ_ONLY": "1",
        "WHATSAPP_PARENT_WATCHDOG_S": "0",
        "TRANSCRIBE_ON_INGEST": "0",
    }
    result = subprocess.run(
        [sys.executable, str(Path(media_upload.__file__).with_name("main.py"))],
        input="",
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout == "" and mode(untouched) == 0o644 and mode(external) == 0o755
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(outbox))
    with pytest.raises(ToolError) as refused:
        media_upload.checked_upload_root()
    assert refused.value.code == "denied"


@POSIX
def test_real_startup_tightens_existing_artifacts_before_stdio_serves(tmp_path):
    store = tmp_path / "store"
    store.mkdir(mode=0o755)
    database = store / "notes.db"
    database.write_bytes(b"synthetic notes")
    bridge_db = store / "messages.db"
    bridge_db.write_bytes(b"bridge owned")
    os.chmod(database, 0o644)
    os.chmod(bridge_db, 0o644)
    exports = store / "exports"
    exports.mkdir(mode=0o755)
    archived = exports / "messages-all-20261008T100000Z.ndjson"
    archived.write_text('{"synthetic":true}\n')
    os.chmod(archived, 0o644)
    uploads = tmp_path / "outbox" / ".uploads"
    uploads.mkdir(parents=True, mode=0o755)
    env = {
        **os.environ,
        "WHATSAPP_DB_PATH": str(bridge_db),
        "WHATSMEOW_DB_PATH": str(store / "whatsapp.db"),
        "WHATSAPP_EXPORT_DIR": str(exports),
        "WHATSAPP_MEDIA_ROOTS": str(uploads.parent),
        "WHATSAPP_MCP_TRANSPORT": "stdio",
        "WHATSAPP_PARENT_WATCHDOG_S": "0",
        "TRANSCRIBE_ON_INGEST": "0",
    }
    result = subprocess.run(
        [sys.executable, str(Path(media_upload.__file__).with_name("main.py"))],
        input="",
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    assert mode(database) == mode(archived) == 0o600
    assert mode(exports) == 0o755 and mode(uploads) == 0o700
    assert mode(store) == 0o755 and mode(bridge_db) == 0o644
    assert result.stderr.count("Tightened MCP-owned permissions:") == 1
    assert result.stdout == ""


@POSIX
def test_new_upload_folder_mode_is_independent_of_restrictive_umask(tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path / "outbox"))
    previous = os.umask(0o777)
    try:
        folder = Path(media_upload.create_upload())
    finally:
        os.umask(previous)
    assert mode(folder) == 0o700


@POSIX
@pytest.mark.usefixtures("auth_runtime_store")
def test_configured_symlink_spelling_for_inline_preview_http_id_and_audio(tmp_path, monkeypatch):
    physical = tmp_path / "mnt" / "example-storage" / "outbox"
    physical.mkdir(parents=True)
    configured = tmp_path / "srv" / "outbox"
    configured.parent.mkdir()
    configured.symlink_to(physical, target_is_directory=True)
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(configured))
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "http")
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "fake-token")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    calls = []

    def bridge(method, endpoint, **kwargs):
        path = Path(kwargs["json"]["media_path"])
        assert path.is_relative_to(configured / ".uploads")
        calls.append((path, path.read_bytes()))
        assert mode(path) == 0o600
        assert mode(path.parent) == mode(path.parent.parent) == 0o700
        return DummyResponse()

    monkeypatch.setattr(whatsapp, "_bridge_request", bridge)
    data = base64.b64encode(b"synthetic").decode()
    preview = whatsapp.send_file(ALICE, media_base64=data, filename="CO\u200bN.pdf", dry_run=True)[2]
    assert preview["payload"]["media_path"] == str(configured / ".uploads" / "<upload>" / "_CON.pdf")
    assert not (physical / ".uploads").exists()
    whatsapp.send_file(ALICE, media_base64=data, filename="report.pdf")
    with client(stateless_http=True, json_response=True) as c:
        response = c.post("/upload", content=b"HTTP-synthetic", headers=HEADERS)
        assert response.status_code == 200
        receipt = response.json()
        whatsapp.send_file(ALICE, upload_id=receipt["upload_id"])
    whatsapp.send_audio_message(ALICE, media_base64=base64.b64encode(b"OggS" + bytes(64)).decode())
    assert [content for _, content in calls] == [b"synthetic", b"HTTP-synthetic", b"OggS" + bytes(64)]
    assert all(not path.exists() for path, _ in calls)
    second = tmp_path / "second-root" / "caller.pdf"
    assert media_upload.bridge_media_path(str(second)) == str(second)
    assert media_upload.bridge_media_path(str(tmp_path / "outbox-other" / "caller.pdf")) == str(
        tmp_path / "outbox-other" / "caller.pdf"
    )
    (physical / ".uploads" / "linked").symlink_to(tmp_path, target_is_directory=True)
    with pytest.raises(ToolError, match="symlinks"):
        media_upload.bridge_media_path(str(configured / ".uploads" / "linked" / "escape.pdf"))


def damaged_pdf(count=500, missing=499, declared=None, indirect=False):
    # Rewrite the page tree before reconstructing xref, preserving readable Kids.
    raw = make_pdf([f"Synthetic page {i}" for i in range(count)])
    objects = raw.split(b"endobj\n")[:-1]
    bodies = [part.split(b" obj\n", 1)[1] for part in objects]
    declared = count if declared is None else declared
    count_value = f"{declared}"
    if indirect:
        count_value = f"{len(bodies) + 1} 0 R"
        bodies.append(f"{declared}\n".encode())
    kids = [f"{4 + 2 * i} 0 R" for i in range(count - missing)]
    kids.extend(f"{10000 + i} 0 R" for i in range(missing))
    bodies[1] = f"<< /Type /Pages /Count {count_value} /Kids [{' '.join(kids)}] >>\n".encode()
    result = bytearray(b"%PDF-1.4\n")
    offsets = []
    for number, body in enumerate(bodies, 1):
        offsets.append(len(result))
        result += f"{number} 0 obj\n".encode() + body + b"endobj\n"
    start = len(result)
    result += f"xref\n0 {len(bodies) + 1}\n0000000000 65535 f \n".encode()
    result += b"".join(f"{offset:010d} 00000 n \n".encode() for offset in offsets)
    result += f"trailer\n<< /Size {len(bodies) + 1} /Root 1 0 R >>\nstartxref\n{start}\n%%EOF\n".encode()
    return bytes(result)


def test_actual_media_read_healthy_500_pages_and_damaged_tree(paired_dbs, monkeypatch, caplog):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "HEALTHY500", "healthy.pdf")
        _insert(conn, "DAMAGED500", "damaged.pdf")
    _cache("document_20260904_100000_HEALTHY500.pdf", make_pdf([f"Synthetic page {i}" for i in range(500)]))
    _cache("document_20260904_100000_DAMAGED500.pdf", damaged_pdf())
    with caplog.at_level(logging.ERROR, logger="pypdf"):
        start = time.perf_counter()
        blocks = media_read.read_media(ALICE, "HEALTHY500", as_text=True, max_pages=500)
        healthy_s = time.perf_counter() - start
        assert json.loads(blocks[-1].text)["pages_total"] == 500
        start = time.perf_counter()
        damaged = media_read.read_media(ALICE, "DAMAGED500", as_text=True, max_pages=500)
        damaged_s = time.perf_counter() - start
    assert json.loads(damaged[-1].text)["pages_missing"] == 499
    assert json.loads(damaged[-1].text)["pages_total"] == 1
    assert any("499 page-tree reference(s)" in block.text for block in damaged)
    print(f"actual read_media 500 pages: healthy={healthy_s:.6f}s damaged={damaged_s:.6f}s")


def test_majority_extraction_failures_refused_without_losing_minority_details(tmp_path, monkeypatch):
    import pypdf

    class Page:
        def __init__(self, failed):
            self.failed = failed

        def extract_text(self):
            if self.failed:
                raise KeyError("synthetic")
            return "synthetic"

    class Reader:
        def __init__(self, path):
            self.pages = [Page(True), Page(True), Page(True), Page(False)]

    monkeypatch.setattr(pypdf, "PdfReader", Reader)
    with pytest.raises(ToolError, match="as_images") as failure:
        media_text._pdf(str(tmp_path / "synthetic.pdf"), 500)
    assert failure.value.code == "invalid_argument"


@pytest.mark.parametrize("indirect", [False, True])
def test_wrong_pdf_count_keeps_all_healthy_pages(tmp_path, indirect):
    path = tmp_path / "wrong-count.pdf"
    path.write_bytes(damaged_pdf(count=10, missing=0, declared=30, indirect=indirect))
    result = media_text.extract(str(path), media_text.PDF_MIME)
    assert result.units_total == 10 and len(result.sections) == 10
    assert result.pages_missing == 0 and result.note is None


@pytest.mark.parametrize("indirect", [False, True])
def test_actual_media_read_names_missing_pdf_kids(paired_dbs, monkeypatch, indirect):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "MISSINGKIDS", "missing.pdf")
    _cache("document_20260904_100000_MISSINGKIDS.pdf", damaged_pdf(count=10, missing=4, indirect=indirect))
    blocks = media_read.read_media(ALICE, "MISSINGKIDS", as_text=True)
    metadata = json.loads(blocks[-1].text)
    assert metadata["pages_total"] == 6 and metadata["pages_missing"] == 4
    assert any("4 page-tree reference(s)" in block.text for block in blocks)
    assert sum("--- page " in block.text for block in blocks) == 6


def test_all_pdf_kids_missing_is_invalid_input(tmp_path):
    path = tmp_path / "all-missing.pdf"
    path.write_bytes(damaged_pdf(count=10, missing=10))
    with pytest.raises(ToolError, match="no readable pages") as failure:
        media_text.extract(str(path), media_text.PDF_MIME)
    assert failure.value.code == "invalid_argument"


@POSIX
def test_existing_export_root_and_nested_directories_keep_modes(tmp_path, monkeypatch):
    root = tmp_path / "exports"
    nested = root / "unrelated" / "deep"
    nested.mkdir(parents=True)
    for folder in (root, nested.parent, nested):
        os.chmod(folder, 0o755)
    archive = root / "messages-all-20261008T100000Z.ndjson"
    archive.write_text("synthetic")
    os.chmod(archive, 0o644)
    deep_archive = nested / archive.name
    deep_archive.write_text("not registered")
    os.chmod(deep_archive, 0o644)
    custom = nested / "registered.ndjson"
    custom.write_text("synthetic")
    private_files.record_export(str(root), str(custom))
    os.chmod(custom, 0o644)
    # Even a scan of the unrelated subtree is a regression.
    monkeypatch.setattr(os, "walk", lambda *_args, **_kwargs: pytest.fail("recursive export traversal"))
    private_files.tighten_existing_artifacts(str(tmp_path / "absent"), str(root), str(tmp_path / "absent2"))
    assert mode(archive) == mode(custom) == 0o600
    assert mode(deep_archive) == 0o644
    assert [mode(folder) for folder in (root, nested.parent, nested)] == [0o755] * 3


@POSIX
def test_export_continues_when_fchmod_is_unsupported(store, tmp_path, monkeypatch, caplog):
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(tmp_path / "exports"))
    private_files._warn_permissions.cache_clear()

    def refused(*_args):
        raise PermissionError(errno.EPERM, "synthetic private path")

    monkeypatch.setattr(os, "fchmod", refused)
    with caplog.at_level(logging.WARNING, logger="whatsapp_mcp"):
        result = export.export_messages(out_path="archive.ndjson")
        again = export.export_messages(out_path="second.ndjson")
    assert len(Path(result["path"]).read_text().splitlines()) == result["count"] == 5
    assert again["count"] == 5
    assert len([row for row in caplog.records if "artifact permissions" in row.message]) == 1
    assert all(
        "synthetic private path" not in row.message and str(tmp_path) not in row.message for row in caplog.records
    )


@POSIX
def test_old_notes_database_is_tightened_before_open_and_first_query(tmp_path, monkeypatch):
    database = tmp_path / "notes.db"
    with sqlite3.connect(database) as conn:
        conn.execute("CREATE TABLE legacy (value TEXT)")
        conn.execute("INSERT INTO legacy VALUES ('synthetic')")
    os.chmod(database, 0o644)
    original_fchmod = os.fchmod
    original_connect = sqlite3.connect
    observed = []

    def check_fd(fd, permissions):
        observed.append("before fchmod")
        assert stat.S_IMODE(os.fstat(fd).st_mode) == 0o600
        original_fchmod(fd, permissions)

    def check_connect(*args, **kwargs):
        observed.append("before connect")
        assert mode(database) == 0o600
        return original_connect(*args, **kwargs)

    monkeypatch.setattr(os, "fchmod", check_fd)
    monkeypatch.setattr(sqlite3, "connect", check_connect)
    conn = private_files.notes_connection(str(database), create=False, timeout=1)
    try:
        assert conn.execute("SELECT value FROM legacy").fetchone() == ("synthetic",)
    finally:
        conn.close()
    assert observed == ["before fchmod", "before connect"]


@POSIX
@pytest.mark.parametrize("connector", [notes._connect, media_notes._connect])
def test_notes_queries_continue_when_permission_adjustments_fail(tmp_path, monkeypatch, caplog, connector):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    database = tmp_path / "notes.db"
    with sqlite3.connect(database) as conn:
        conn.execute("CREATE TABLE legacy (value TEXT)")
        conn.execute("INSERT INTO legacy VALUES ('synthetic')")
    os.chmod(database, 0o644)
    private_files._warn_permissions.cache_clear()

    def refused(*_args, **_kwargs):
        raise PermissionError(errno.EPERM, "synthetic private path")

    monkeypatch.setattr(os, "chmod", refused)
    monkeypatch.setattr(os, "fchmod", refused)
    with caplog.at_level(logging.WARNING, logger="whatsapp_mcp"):
        conn = connector(create=True)
        try:
            assert conn.execute("SELECT value FROM legacy").fetchone() == ("synthetic",)
        finally:
            conn.close()
    assert len(caplog.records) == 1 and "notes database" in caplog.records[0].message
    assert str(tmp_path) not in caplog.records[0].message


@POSIX
@pytest.mark.parametrize(
    "failure",
    ["notes", "archive", "uploads", "manifest_read", "manifest_utf8", "readonly", "unsupported"],
)
def test_real_startup_survives_permission_and_manifest_failures(tmp_path, failure):
    store = tmp_path / "store"
    store.mkdir()
    database = store / "notes.db"
    database.write_bytes(b"synthetic")
    exports = store / "exports"
    exports.mkdir()
    archive = exports / "messages-all-20261008T100000Z.ndjson"
    archive.write_text("synthetic")
    uploads = tmp_path / "outbox" / ".uploads"
    uploads.mkdir(parents=True)
    manifest = exports / private_files.EXPORT_MANIFEST
    manifest.write_bytes(b"\xff" if failure == "manifest_utf8" else b'"custom.ndjson"\n')
    for path in (database, archive):
        os.chmod(path, 0o644)
    os.chmod(uploads, 0o755)
    os.chmod(manifest, 0o600)
    target = {"notes": database, "archive": archive, "uploads": uploads}.get(failure, database)
    # Fault injection occurs in the real stdio entrypoint process. Files and
    # startup are real; only the unavailable filesystem operation is replaced.
    runner = """
import builtins, errno, os, runpy, sys
failure, target, manifest, entrypoint = sys.argv[1:]
chmod, opened = os.chmod, builtins.open
def denied_chmod(path, *args, **kwargs):
    if os.fspath(path) == target:
        code = {'readonly': errno.EROFS, 'unsupported': errno.ENOTSUP}.get(failure, errno.EPERM)
        raise OSError(code, 'synthetic private path')
    return chmod(path, *args, **kwargs)
def denied_open(path, *args, **kwargs):
    if os.fspath(path) == manifest:
        raise PermissionError(errno.EACCES, 'synthetic private path')
    return opened(path, *args, **kwargs)
if failure not in ('manifest_read', 'manifest_utf8'):
    os.chmod = denied_chmod
if failure == 'manifest_read':
    builtins.open = denied_open
runpy.run_path(entrypoint, run_name='__main__')
"""
    env = {
        **os.environ,
        "WHATSAPP_DB_PATH": str(store / "messages.db"),
        "WHATSMEOW_DB_PATH": str(store / "whatsapp.db"),
        "WHATSAPP_EXPORT_DIR": str(exports),
        "WHATSAPP_MEDIA_ROOTS": str(uploads.parent),
        "WHATSAPP_MCP_TRANSPORT": "stdio",
        "WHATSAPP_PARENT_WATCHDOG_S": "0",
        "TRANSCRIBE_ON_INGEST": "0",
    }
    result = subprocess.run(
        [
            sys.executable,
            "-c",
            runner,
            failure,
            str(target),
            str(manifest),
            str(Path(media_upload.__file__).with_name("main.py")),
        ],
        input="",
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout == "" and "Traceback" not in result.stderr
    warnings = [line for line in result.stderr.splitlines() if "WARNING" in line]
    assert len(warnings) == 1, result.stderr
    assert str(tmp_path) not in warnings[0] and "synthetic private path" not in warnings[0]
