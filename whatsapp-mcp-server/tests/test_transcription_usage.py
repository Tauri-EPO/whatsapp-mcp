"""Real audio, provider HTTP, SQLite, worker/tool consumers and UTC quota proof."""

import concurrent.futures
import http.client
import io
import json
import os
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.request
import wave
from datetime import UTC, datetime
from pathlib import Path

import pytest
from starlette.testclient import TestClient

import main
import media_inventory
import media_notes
import observability
import operator_admin
import runtime_settings
import transcribe
import transcribe_worker
import transcription_usage as usage
import whatsapp
from errors import ToolError
from strict_args import StrictArgumentServer
from tests.conftest import ALICE
from tests.test_runtime_settings import patch
from tests.test_runtime_settings import runtime_archive as archive_fixture
from tests.test_transcribe_http import _add_audio, _audio
from tests.test_transcribe_http import provider as provider_fixture
from tests.test_transcribe_ingest import SHA, _media_filename

runtime_archive = archive_fixture
provider = provider_fixture


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
@pytest.mark.parametrize("source_kind", ["tool", "ingest"])
def test_exact_decimal_quota_fits_real_decoded_files(
    provider, runtime_archive, monkeypatch, tmp_path, provider_name, source_kind
):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.08, "transcription.cap_scope": "all"})
    source = tmp_path / "exact.wav"
    _audio(source, 1.6)
    for _ in range(3):
        assert not usage.ingest_quota_exhausted(1.6)
        assert transcribe.transcribe_file(str(source), source=source_kind)["duration_s"] == 1.6
    assert len(provider["calls"]) == 3
    assert usage.current_usage()["seconds"] == pytest.approx(4.8)
    assert usage.current_usage()["remaining_seconds"] == 0
    assert usage.ingest_quota_exhausted(1.6)
    _audio(source, 1 / 16000)
    with pytest.raises(ToolError, match="quota"):
        transcribe.transcribe_file(str(source), source=source_kind)
    assert len(provider["calls"]) == 3


def test_admin_non_ascii_tokens_and_headers_never_crash(paired_dbs, monkeypatch):
    monkeypatch.setenv("WHATSAPP_OPERATOR_BIND", "operator.example")
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "fake-bridge-0123456789abcdef")
    monkeypatch.setenv("WHATSAPP_MCP_TOKEN", "fake-mcp-ä0123456789abcdef")
    start = operator_admin.start_admin
    monkeypatch.setattr(operator_admin, "start_admin", lambda bridge, mcp: start(bridge, mcp, port=0))
    admin = operator_admin.install_admin("http", 8000)
    assert admin is not None
    try:
        for token, expected in [("fake-mcp-ä0123456789abcdef", 401), ("fake-bridge-0123456789abcdef", 200)]:
            client = http.client.HTTPConnection("127.0.0.1", admin.server_port, timeout=2)
            try:
                client.request("GET", "/admin/v1/health", headers={"Authorization": "Bearer " + token})
                response = client.getresponse()
                assert response.status == expected
                response.read()
            finally:
                client.close()
    finally:
        admin.shutdown()
        admin.server_close()
    with pytest.raises(ValueError, match="distinct"):
        start("fake-same-ä0123456789abcdef", "fake-same-ä0123456789abcdef", port=0)


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
def test_real_tool_worker_duration_notes_metrics_and_restart(
    provider, paired_dbs, monkeypatch, tmp_path, provider_name
):
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    path = tmp_path / "tool.wav"
    _audio(path, 2)
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path))
    result = main.transcribe_audio(file_path=str(path))
    assert result["duration_s"] == 2 and result["provider"] == provider_name
    _add_audio(paired_dbs, "AUD1", ALICE)
    assert transcribe_worker.run_once(10).transcribed == 1
    value = usage.current_usage()
    monkeypatch.setattr(
        whatsapp,
        "_bridge_request",
        lambda *args, **kwargs: (_ for _ in ()).throw(ToolError("bridge_unavailable", "fake offline")),
    )
    assert whatsapp.bridge_status()["transcription_usage"]["minutes"] == 2.1 / 60
    assert value["by_source"] == {"tool": {"seconds": 2, "requests": 1}, "ingest": {"seconds": 0.1, "requests": 1}}
    notes = media_notes.get_media_notes(SHA["AUD1"])["notes"]
    assert notes["duration_s"]["value"] == "0.1"
    assert notes["transcript_provider"]["value"] == provider_name
    assert notes["transcript_model"]["value"] == ("unknown" if provider_name == "whisper_cpp" else "fake-speech-model")
    assert len(provider["calls"]) == 2
    cached = main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")
    assert cached["cached"] and cached["duration_s"] == 0.1
    assert cached["provider"] == provider_name and cached["model"] == notes["transcript_model"]["value"]
    assert usage.current_usage() == value
    text = usage.metrics_text()
    assert f'whatsapp_mcp_transcription_seconds_total{{provider="{provider_name}",source="tool"}} 2' in text
    assert f'whatsapp_mcp_transcription_requests_total{{provider="{provider_name}",outcome="success"}} 2' in text
    with sqlite3.connect(media_notes.notes_db_path()) as conn:
        assert conn.execute("SELECT SUM(seconds),SUM(requests) FROM transcription_usage").fetchone() == (2.1, 2)
    script = (
        "import whatsapp,transcription_usage,json; whatsapp.MESSAGES_DB_PATH="
        + repr(str(paired_dbs.messages_db))
        + "; print(json.dumps(transcription_usage.current_usage()))"
    )
    restarted = subprocess.run([sys.executable, "-c", script], capture_output=True, text=True, check=True, timeout=20)
    assert json.loads(restarted.stdout) == value


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
def test_cap_pauses_without_note_rolls_utc_resumes_and_all_refuses(
    provider, runtime_archive, monkeypatch, caplog, tmp_path, provider_name
):
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    monkeypatch.setattr(usage, "month_now", lambda: "2026-10")
    monkeypatch.setattr(usage, "_paused", False)
    caplog.set_level("INFO")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.05, "transcription.cap_scope": "all"})
    # Exhaust the UTC month through the same real tool path the cap guards.
    path = tmp_path / "three.wav"
    _audio(path, 3)
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path))
    assert main.transcribe_audio(file_path=str(path))["duration_s"] == 3
    _add_audio(runtime_archive, "AUD1", ALICE)
    for _ in range(2):
        batch = transcribe_worker.run_once(10)
        assert batch.transcribed == batch.failed == 0 and batch.position is None
    assert media_notes.get_media_notes(SHA["AUD1"])["notes"] == {}
    assert len(provider["calls"]) == 1
    refused = main.transcribe_audio(file_path=str(path))
    assert refused["error"]["code"] == "transcription_quota_exceeded"
    assert usage.current_usage()["remaining_seconds"] == 0
    monkeypatch.setenv("TZ", "Pacific/Honolulu")  # Owner decision: TZ does not alter month.
    monkeypatch.setattr(usage, "month_now", lambda: "2026-11")
    assert transcribe_worker.run_once(10).transcribed == 1
    assert usage.current_usage()["seconds"] == 0.1
    assert sum("monthly quota paused" in r.message for r in caplog.records) == 1
    assert sum("monthly quota resumed" in r.message for r in caplog.records) == 1


def test_concurrent_reservations_no_overshoot_release_failure_and_crash_is_conservative(runtime_archive):
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.05, "transcription.cap_scope": "all"})
    admitted = threading.Event()
    release = threading.Event()

    def first():
        with usage.admission(2, "whisper_cpp", "", "ingest"):
            admitted.set()
            assert release.wait(5)

    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        future = pool.submit(first)
        assert admitted.wait(5)
        with pytest.raises(ToolError, match="remaining monthly"):
            with usage.admission(2, "openai_compatible", "fake-model", "tool"):
                pytest.fail("reservation overshoot")
        release.set()
        future.result()
    assert usage.current_usage()["seconds"] == 2
    with pytest.raises(RuntimeError):
        with usage.admission(1, "openai_compatible", "fake-model", "tool"):
            raise RuntimeError("provider failed")
    assert usage.current_usage()["remaining_seconds"] == 1
    with usage._connection() as conn:
        conn.execute("INSERT INTO transcription_reservations VALUES ('crashed',?,'tool',1)", (usage.month_now(),))
    assert usage.current_usage()["remaining_seconds"] == 0
    assert 'outcome="error"} 1' in usage.metrics_text()


def test_barrier_concurrent_admissions_never_exceed_cap(runtime_archive, monkeypatch):
    patch(runtime_archive, {"transcription.monthly_max_minutes": 4 / 60, "transcription.cap_scope": "all"})
    # First use creates its schema; the next test isolates concurrent DDL.
    conn = usage._connection()
    conn.close()
    barrier = threading.Barrier(8)
    used = usage._used

    def widen_read_write_race(conn, month, scope):
        result = used(conn, month, scope)
        time.sleep(0.1)  # A real writer must retain the admission lock during this gap.
        return result

    monkeypatch.setattr(usage, "_used", widen_read_write_race)

    def attempt():
        barrier.wait(timeout=10)
        try:
            with usage.admission(2, "whisper_cpp", "", "ingest"):
                time.sleep(0.05)
            return True
        except ToolError as exc:
            assert exc.code == "transcription_quota_exceeded"
            return False

    with concurrent.futures.ThreadPoolExecutor(8) as pool:
        assert sum(pool.map(lambda _: attempt(), range(8))) == 2
    assert usage.current_usage()["seconds"] == 4
    assert usage.current_usage()["remaining_seconds"] == 0


def test_concurrent_first_use_creates_schema_without_preexisting_tables(paired_dbs):
    assert not Path(media_notes.notes_db_path()).exists()
    barrier = threading.Barrier(8)

    def first_use(_):
        barrier.wait(timeout=10)
        conn = usage._connection(timeout=10)
        try:
            assert conn.execute("SELECT COUNT(*) FROM transcription_reservations").fetchone() == (0,)
            assert conn.execute("SELECT COUNT(*) FROM transcription_usage").fetchone() == (0,)
            assert conn.execute("SELECT COUNT(*) FROM transcription_outcomes").fetchone() == (0,)
        finally:
            conn.close()

    with concurrent.futures.ThreadPoolExecutor(8) as pool:
        list(pool.map(first_use, range(8)))


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
def test_exhausted_worker_never_reconverts_head_and_resumes_after_raise(
    provider, runtime_archive, monkeypatch, provider_name
):
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    _add_audio(runtime_archive, "AUD1", ALICE)
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0})
    conversions = []
    original = subprocess.run

    def count_conversion(argv, *args, **kwargs):
        if argv[0] == "ffmpeg":
            conversions.append(argv)
        return original(argv, *args, **kwargs)

    monkeypatch.setattr(subprocess, "run", count_conversion)
    for _ in range(3):
        batch = transcribe_worker.run_once(10)
        assert batch.transcribed == batch.failed == 0 and batch.position is None
    assert conversions == [] and provider["calls"] == []
    assert media_notes.get_media_notes(SHA["AUD1"])["notes"] == {}
    patch(runtime_archive, {"transcription.monthly_max_minutes": 1})
    assert transcribe_worker.run_once(10).transcribed == 1
    assert conversions and len(provider["calls"]) == 1


def test_transcription_metrics_have_help_for_every_family(paired_dbs):
    text = usage.metrics_text()
    for name in ("usage_available", "seconds_total", "requests_total", "quota_remaining_seconds"):
        assert f"# HELP whatsapp_mcp_transcription_{name} " in text


def test_initialized_usage_metrics_and_admin_read_during_real_writer_transaction(paired_dbs):
    with usage.admission(2, "whisper_cpp", "", "tool"):
        pass
    writer = usage._connection()
    writer.execute("BEGIN IMMEDIATE")
    writer.execute("UPDATE transcription_usage SET seconds=99")
    token = "fake-bridge-0123456789abcdef"
    admin = operator_admin.start_admin(token, "fake-mcp-0123456789abcdef", port=0)
    try:
        assert usage.current_usage()["seconds"] == 2  # Committed WAL snapshot, never the writer's pending value.
        assert 'source="tool"} 2' in usage.metrics_text()
        client = http.client.HTTPConnection("127.0.0.1", admin.server_port, timeout=2)
        try:
            client.request("GET", "/admin/v1/transcription/usage", headers={"Authorization": "Bearer " + token})
            response = client.getresponse()
            assert response.status == 200 and json.loads(response.read())["seconds"] == 2
        finally:
            client.close()
    finally:
        writer.rollback()
        writer.close()
        admin.shutdown()
        admin.server_close()


def test_http_meter_and_upload_share_snapshot_when_original_is_replaced(
    provider, runtime_archive, monkeypatch, tmp_path
):
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.05, "transcription.cap_scope": "all"})
    source = tmp_path / "mutable.wav"
    _audio(source, 1)
    duration = usage.audio_duration

    def replace_after_probe(path, deadline=None):
        measured = duration(path, deadline)
        _audio(source, 10)
        return measured

    monkeypatch.setattr(usage, "audio_duration", replace_after_probe)
    result = transcribe.transcribe_file(str(source))
    uploaded = tmp_path / "uploaded.ogg"
    uploaded.write_bytes(provider["calls"][0]["fields"]["file"])
    assert duration(str(source)) == 10
    # Opus can pad its last frame; the ten-second replacement is never encoded.
    assert duration(str(uploaded)) == pytest.approx(1, abs=0.02)
    assert result["duration_s"] == 1
    assert usage.current_usage()["seconds"] == 1
    assert usage.current_usage()["remaining_seconds"] == 2


@pytest.mark.skipif(not hasattr(os, "O_NOFOLLOW"), reason="POSIX no-follow descriptor contract")
def test_http_snapshot_refuses_final_symlink(tmp_path):
    target, source, work = tmp_path / "private.wav", tmp_path / "input.wav", tmp_path / "work"
    _audio(target)
    source.symlink_to(target)
    work.mkdir()
    with pytest.raises(transcribe.TranscriptionError, match="snapshot"):
        transcribe._freeze_http_audio(str(source), str(work), time.monotonic() + 5)
    assert list((work / "original").iterdir()) == []


def test_http_snapshot_bounds_bytes_when_input_grows_after_initial_stat(tmp_path, monkeypatch):
    source, work = tmp_path / "growing.wav", tmp_path / "work"
    _audio(source, 0.01)
    work.mkdir()
    monkeypatch.setattr(transcribe, "MAX_HTTP_AUDIO_BYTES", 1024)
    fdopen = os.fdopen

    class GrowingInput:
        def __init__(self, original):
            self.original = original

        def __enter__(self):
            return self

        def __exit__(self, *args):
            self.original.close()

        def fileno(self):
            return self.original.fileno()

        def read(self, size):
            with source.open("ab") as file:
                file.write(b"\0" * 2048)
            return self.original.read(size)

    monkeypatch.setattr(
        os,
        "fdopen",
        lambda fd, mode, **kwargs: (
            GrowingInput(fdopen(fd, mode, **kwargs)) if mode == "rb" else fdopen(fd, mode, **kwargs)
        ),
    )
    with pytest.raises(transcribe.TranscriptionError, match="256 MiB"):
        transcribe._freeze_http_audio(str(source), str(work), time.monotonic() + 5)
    assert (work / "original" / source.name).stat().st_size == 0


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
@pytest.mark.parametrize("resume", ["raise", "month", "replace"])
def test_positive_insufficient_quota_reuses_measured_duration_and_rechecks_file_identity(
    provider, runtime_archive, monkeypatch, provider_name, resume
):
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    monkeypatch.setattr(transcribe_worker, "_blocked_quota", None)
    monkeypatch.setattr(usage, "month_now", lambda: "2026-10")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 3 / 60})
    with usage.admission(2, provider_name, "", "ingest"):
        pass
    _add_audio(runtime_archive, "AUD1", ALICE)
    path = Path(media_inventory.chat_media_dir(ALICE)) / _media_filename("AUD1")
    _audio(path, 2)
    conversions = []
    original = subprocess.run

    def count_conversion(argv, *args, **kwargs):
        if argv[0] == "ffmpeg":
            conversions.append(argv)
        return original(argv, *args, **kwargs)

    monkeypatch.setattr(subprocess, "run", count_conversion)
    conversion_counts = []
    for _ in range(3):
        batch = transcribe_worker.run_once(10)
        assert batch.transcribed == batch.failed == 0 and batch.position is None
        conversion_counts.append(len(conversions))
    assert conversion_counts[0] > 0 and conversion_counts == [conversion_counts[0]] * 3
    assert provider["calls"] == []
    assert media_notes.get_media_notes(SHA["AUD1"])["notes"] == {}
    if resume == "raise":
        patch(runtime_archive, {"transcription.monthly_max_minutes": 5 / 60})
    elif resume == "month":
        monkeypatch.setattr(usage, "month_now", lambda: "2026-11")
    else:
        # Replacement bytes have a different archive hash as well as size/mtime.
        _audio(path, 0.5)
        with runtime_archive.messages() as conn:
            conn.execute("UPDATE messages SET file_sha256=? WHERE id='AUD1'", (bytes.fromhex(SHA["AUD2"]),))
    assert transcribe_worker.run_once(10).transcribed == 1
    assert len(conversions) > conversion_counts[0]
    assert len(provider["calls"]) == 1
    assert media_notes.get_media_notes(SHA["AUD2"] if resume == "replace" else SHA["AUD1"])["notes"]["duration_s"][
        "value"
    ] == ("0.5" if resume == "replace" else "2.0")


@pytest.mark.parametrize("value", ["0x1p4", "1_0", "0b10", "NaN", "Inf", "١"])
def test_cap_environment_refuses_non_decimal_forms(value):
    with pytest.raises(ValueError):
        runtime_settings.parse_cap(value)


@pytest.mark.parametrize(
    "value,expected",
    [
        (None, None),
        ("", None),
        (" \t", None),
        (" 1.5 ", 1.5),
        ("+1", 1),
        (".5", 0.5),
        ("1.", 1),
        ("1e2", 100),
        ("01", 1),
    ],
)
def test_cap_environment_decimal_forms(value, expected):
    assert runtime_settings.parse_cap(value) == expected


def test_runtime_deploy_ceiling_scope_clear_restart_and_raise(runtime_archive, monkeypatch):
    monkeypatch.setenv("TRANSCRIBE_MONTHLY_MAX_MINUTES", "10")
    monkeypatch.setenv("TRANSCRIBE_CAP_SCOPE", "all")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 20, "transcription.cap_scope": "ingest"})
    assert usage.limits() == (600, "all")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 1})
    assert usage.limits() == (60, "all")
    with usage.admission(60, "whisper_cpp", "", "ingest"):
        pass
    patch(runtime_archive, {"transcription.monthly_max_minutes": 2})
    assert usage.current_usage()["remaining_seconds"] == 60
    patch(runtime_archive, {"transcription.monthly_max_minutes": None, "transcription.cap_scope": None})
    assert usage.current_usage()["remaining_seconds"] == 540
    assert runtime_settings.snapshot()["settings"]["transcription.monthly_max_minutes"]["source"] == "env"
    monkeypatch.delenv("TRANSCRIBE_CAP_SCOPE")
    assert usage.limits() == (600, "ingest")
    with usage.admission(700, "whisper_cpp", "", "tool"):
        pass
    assert usage.current_usage()["remaining_seconds"] == 540  # tool usage outside ingest cap


@pytest.mark.parametrize("extension,codec", [("wma", "wmav2"), ("caf", "pcm_s16le")])
def test_cpp_metering_preserves_previously_supported_formats(
    provider, paired_dbs, monkeypatch, tmp_path, extension, codec
):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", "whisper_cpp")
    monkeypatch.setenv("WHISPER_URL", provider["url"])
    monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    wav = tmp_path / "source.wav"
    _audio(wav, 1)
    source = tmp_path / f"source.{extension}"
    subprocess.run(
        ["ffmpeg", "-nostdin", "-v", "error", "-i", str(wav), "-c:a", codec, str(source)],
        check=True,
        capture_output=True,
        timeout=10,
    )
    conversions = []
    original_run = subprocess.run

    def observe(command, *args, **kwargs):
        if command[0] == "ffmpeg":
            conversions.append(command)
        return original_run(command, *args, **kwargs)

    monkeypatch.setattr(subprocess, "run", observe)
    result = transcribe.transcribe_file(str(source))
    assert len(conversions) == 1
    assert result["duration_s"] == pytest.approx(1, abs=0.1)
    with wave.open(io.BytesIO(provider["calls"][0]["fields"]["file"]), "rb") as uploaded:
        assert result["duration_s"] == uploaded.getnframes() / uploaded.getframerate()
    assert usage.current_usage()["seconds"] == result["duration_s"]
    assert len(provider["calls"]) == 1


def test_empty_cpp_worker_result_releases_quota_and_records_error(provider, runtime_archive, monkeypatch):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", "whisper_cpp")
    monkeypatch.setenv("WHISPER_URL", provider["url"])
    monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    provider["body"] = b'{"text":"   "}'
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.1})
    _add_audio(runtime_archive, "AUD1", ALICE)
    batch = transcribe_worker.run_once(10)
    assert batch.transcribed == 0 and batch.failed == 1
    assert usage.current_usage()["requests"] == usage.current_usage()["seconds"] == 0
    assert usage.current_usage()["remaining_seconds"] == 6
    assert 'provider="whisper_cpp",outcome="error"} 1' in usage.metrics_text()


def test_http_deadline_includes_accounting_writer_wait(provider, paired_dbs, monkeypatch, tmp_path):
    import time

    monkeypatch.setenv("WHISPER_TIMEOUT_S", "1")
    path = tmp_path / "locked.wav"
    _audio(path, 0.1)
    conn = usage._connection()
    conn.execute("BEGIN IMMEDIATE")
    started = time.monotonic()
    try:
        with pytest.raises(transcribe.BackendUnavailableError, match="deadline"):
            transcribe.transcribe_file(str(path))
        assert time.monotonic() - started < 1.6
        assert not provider["calls"]
    finally:
        conn.rollback()
        conn.close()


@pytest.mark.parametrize("outcome", ["success", "error"])
def test_http_completion_reconciles_after_writer_recovery(provider, runtime_archive, monkeypatch, tmp_path, outcome):
    import time

    monkeypatch.setenv("WHISPER_TIMEOUT_S", "3")
    patch(runtime_archive, {"transcription.monthly_max_minutes": 0.05, "transcription.cap_scope": "all"})
    source = tmp_path / "recovery.wav"
    _audio(source, 1)
    original_http = transcribe._transcribe_http
    writers = []

    def contended_http(*args, **kwargs):
        conn = sqlite3.connect(media_notes.notes_db_path())
        conn.execute("BEGIN IMMEDIATE")
        writers.append(conn)
        return original_http(*args, **kwargs)

    monkeypatch.setattr(transcribe, "_transcribe_http", contended_http)
    provider["delay"] = 4 if outcome == "error" else 0
    started = time.monotonic()
    try:
        if outcome == "error":
            with pytest.raises(transcribe.BackendUnavailableError, match="deadline"):
                transcribe.transcribe_file(str(source))
        else:
            with pytest.raises(transcribe.BackendUnavailableError, match="accounting pending"):
                transcribe.transcribe_file(str(source))
        assert time.monotonic() - started < 3.6
        token, month = writers[0].execute("SELECT id,month FROM transcription_reservations").fetchone()
        assert writers[0].execute("SELECT COUNT(*) FROM transcription_usage").fetchone()[0] == 0
    finally:
        for conn in writers:
            conn.rollback()
            conn.close()
    until = time.monotonic() + 5
    while usage._pending.unfinished_tasks and time.monotonic() < until:
        time.sleep(0.05)
    assert usage._pending.unfinished_tasks == 0
    value = usage.current_usage()
    assert value["seconds"] == value["requests"] == (1 if outcome == "success" else 0)
    assert value["remaining_seconds"] == (2 if outcome == "success" else 3)
    assert f'provider="openai_compatible",outcome="{outcome}"}} 1' in usage.metrics_text()
    with usage._connection() as conn:
        assert conn.execute("SELECT COUNT(*) FROM transcription_reservations").fetchone()[0] == 0
        usage._finish(
            conn,
            usage.Completion(
                media_notes.notes_db_path(), token, month, 1, "openai_compatible", "fake-speech-model", "tool", outcome
            ),
        )
    assert usage.current_usage() == value  # replay is idempotent


def test_accounting_admission_bounds_inflight_and_deferred_work(paired_dbs, monkeypatch):
    monkeypatch.setattr(usage, "_completion_slots", threading.BoundedSemaphore(1))
    with usage.admission(1, "whisper_cpp", "unknown", "tool"):
        with pytest.raises(ToolError, match="accounting busy"):
            with usage.admission(1, "whisper_cpp", "unknown", "tool"):
                pytest.fail("accounting queue admission exceeded its bound")
    with usage.admission(1, "whisper_cpp", "unknown", "tool"):
        pass
    assert usage.current_usage()["requests"] == 2


def test_month_clock_is_utc_even_when_tz_differs(monkeypatch):
    class Clock:
        @staticmethod
        def now(zone):
            assert zone is UTC
            return datetime(2026, 11, 1, tzinfo=UTC)

    monkeypatch.setenv("TZ", "Pacific/Honolulu")
    monkeypatch.setattr(usage, "datetime", Clock)
    assert usage.month_now() == "2026-11"


def test_chained_ogg_counts_all_decoded_frames_and_cannot_bypass_cap(provider, runtime_archive, monkeypatch, tmp_path):
    chunks = []
    for seconds in (2, 3):
        wav, ogg = tmp_path / f"{seconds}.wav", tmp_path / f"{seconds}.ogg"
        _audio(wav, seconds)
        subprocess.run(
            ["ffmpeg", "-nostdin", "-v", "error", "-i", str(wav), "-c:a", "libopus", str(ogg)],
            check=True,
            capture_output=True,
            timeout=10,
        )
        chunks.append(ogg.read_bytes())
    chained = tmp_path / "chained.ogg"
    chained.write_bytes(b"".join(chunks))
    assert usage.audio_duration(str(chained)) == 5
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path))
    patch(runtime_archive, {"transcription.monthly_max_minutes": 4 / 60, "transcription.cap_scope": "all"})
    result = main.transcribe_audio(file_path=str(chained))
    assert result["error"]["code"] == "transcription_quota_exceeded"
    assert provider["calls"] == []
    assert usage.current_usage()["seconds"] == 0


@pytest.mark.parametrize("provider_name", ["whisper_cpp", "openai_compatible"])
def test_multiple_audio_tracks_meter_the_same_first_track_as_upload(
    provider, runtime_archive, monkeypatch, tmp_path, provider_name
):
    if provider_name == "whisper_cpp":
        monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", provider_name)
        monkeypatch.setenv("WHISPER_URL", provider["url"])
        monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    first, second = tmp_path / "first.wav", tmp_path / "second.wav"
    _audio(first, 5)
    _audio(second, 1)
    source = tmp_path / "multiple.mka"
    subprocess.run(
        [
            "ffmpeg",
            "-nostdin",
            "-v",
            "error",
            "-i",
            str(first),
            "-i",
            str(second),
            "-map",
            "0:a",
            "-map",
            "1:a",
            "-disposition:a:0",
            "0",
            "-disposition:a:1",
            "default",
            "-c:a",
            "pcm_s16le",
            str(source),
        ],
        check=True,
        capture_output=True,
        timeout=10,
    )
    assert usage.audio_duration(str(source)) == 5
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path))
    patch(runtime_archive, {"transcription.monthly_max_minutes": 4 / 60, "transcription.cap_scope": "all"})
    assert main.transcribe_audio(file_path=str(source))["error"]["code"] == "transcription_quota_exceeded"
    assert provider["calls"] == []


def test_corrupt_optional_usage_preserves_bridge_status_and_other_metrics(paired_dbs, monkeypatch):
    Path(media_notes.notes_db_path()).write_bytes(b"not a SQLite database")

    def down(*args, **kwargs):
        raise ToolError("bridge_unavailable", "fake offline bridge")

    monkeypatch.setattr(whatsapp, "_bridge_request", down)
    status = whatsapp.bridge_status()
    assert status["transcription_usage"] == {"error": "Transcription usage unavailable"}
    assert status["reason"] == "fake offline bridge"
    text = observability.Metrics().render()
    assert "whatsapp_mcp_uptime_seconds " in text
    assert "whatsapp_mcp_transcription_usage_available 0" in text
    assert "whatsapp_mcp_transcription_seconds_total{" not in text


def test_real_mcp_http_responds_while_legacy_notes_metrics_waits_for_writer(paired_dbs, monkeypatch):
    import time

    from private_files import notes_connection

    writer = notes_connection(media_notes.notes_db_path(), create=True, timeout=5)
    with writer:
        writer.execute("CREATE TABLE legacy_notes (value TEXT)")
    writer.execute("BEGIN IMMEDIATE")
    entered = threading.Event()
    original_render = observability.Metrics.render

    def observe(registry):
        entered.set()
        return original_render(registry)

    monkeypatch.setattr(observability.Metrics, "render", observe)
    monkeypatch.setenv("WHATSAPP_MCP_METRICS", "true")
    server = StrictArgumentServer("metrics-contention")

    @server.tool()
    def echo() -> dict:
        return {"ok": True}

    app = main.build_http_app(
        server, "streamable-http", "fake-mcp-token-0123456789", host="0.0.0.0", json_response=True, stateless_http=True
    )
    headers = {"Authorization": "Bearer fake-mcp-token-0123456789", "Accept": "application/json, text/event-stream"}
    try:
        with TestClient(app) as client, concurrent.futures.ThreadPoolExecutor(1) as pool:
            scrape = pool.submit(client.get, "/metrics")
            try:
                assert entered.wait(2)
                started = time.monotonic()
                response = client.post(
                    "/mcp",
                    headers=headers,
                    json={
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": "tools/call",
                        "params": {"name": "echo", "arguments": {}},
                    },
                )
                assert response.status_code == 200 and not response.json()["result"]["isError"]
                assert time.monotonic() - started < 1.5
                assert not scrape.done()  # the real SQLite writer still blocks schema creation
            finally:
                writer.rollback()
            response = scrape.result(timeout=3)
            assert response.status_code == 200
            assert "whatsapp_mcp_transcription_usage_available 1" in response.text
    finally:
        writer.rollback()
        writer.close()


def test_admin_real_socket_auth_host_origin_only_routes_and_no_mcp_surface(paired_dbs):
    bridge, mcp_token = "fake-bridge-0123456789abcdef", "fake-mcp-0123456789abcdef"
    server = operator_admin.start_admin(bridge, mcp_token, port=0)
    host = f"127.0.0.1:{server.server_address[1]}"
    assert server.server_address[0] == "127.0.0.1"
    try:
        cases = [
            (None, {}, 401),
            (mcp_token, {}, 401),
            ("fake-operator-0123456789abcdef", {}, 401),
            (bridge, {"Host": "wrong.example"}, 403),
            (bridge, {"Origin": "http://wrong.example"}, 403),
            (bridge, {}, 200),
        ]
        for token, extra, expected in cases:
            request = urllib.request.Request(
                f"http://{host}/admin/v1/transcription/usage",
                headers={**extra, **({"Authorization": "Bearer " + token} if token else {})},
            )
            try:
                with urllib.request.urlopen(request, timeout=5) as response:
                    assert response.status == expected
                    assert json.load(response) == usage.current_usage()
            except urllib.error.HTTPError as exc:
                assert exc.code == expected
        for path in ("/api/send", "/admin/v1/settings", "/mcp", "/metrics"):
            with pytest.raises(urllib.error.HTTPError) as exc:
                urllib.request.urlopen(
                    urllib.request.Request(f"http://{host}{path}", headers={"Authorization": "Bearer " + bridge}),
                    timeout=5,
                )
            assert exc.value.code == 404
    finally:
        server.shutdown()
        server.server_close()
    with pytest.raises(ValueError, match="distinct"):
        operator_admin.start_admin(bridge, bridge, port=0)


def test_authenticated_mcp_tool_activity_and_operator_routes_absent(paired_dbs, monkeypatch):
    monkeypatch.setattr(operator_admin, "_last_call", None)
    server = StrictArgumentServer("activity-test")

    @server.tool()
    def echo() -> dict:
        return {"ok": True}

    app = main.build_http_app(
        server, "streamable-http", "fake-mcp-token-0123456789", host="0.0.0.0", json_response=True, stateless_http=True
    )
    headers = {"Authorization": "Bearer fake-mcp-token-0123456789", "Accept": "application/json, text/event-stream"}
    with TestClient(app) as client:
        call = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "echo", "arguments": {}}}
        assert client.post("/mcp", json=call).status_code == 401
        assert operator_admin._last_call is None
        assert client.post("/mcp", headers=headers, json=call).status_code == 200
        assert operator_admin._last_call is not None
        for path in ("/operator/v1/transcription/usage", "/admin/v1/transcription/usage", "/operator/v1/settings"):
            assert client.get(path, headers=headers).status_code == 404


def test_unauthenticated_http_tool_calls_never_record_activity(paired_dbs, monkeypatch):
    monkeypatch.setattr(operator_admin, "_last_call", None)
    server = StrictArgumentServer("unauthenticated-activity")

    @server.tool()
    def echo() -> dict:
        return {"ok": True}

    app = main.build_http_app(server, "streamable-http", None, host="0.0.0.0", json_response=True, stateless_http=True)
    with TestClient(app) as client:
        call = {"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "echo", "arguments": {}}}
        for headers in ({}, {"Authorization": "Bearer fake-unverified-0123456789"}):
            response = client.post(
                "/mcp", headers={"Accept": "application/json, text/event-stream", **headers}, json=call
            )
            assert response.status_code == 200 and not response.json()["result"]["isError"]
            assert operator_admin._last_call is None


def test_go_operator_to_python_admin_actual_processes(paired_dbs):
    binary = Path(__file__).resolve().parents[2] / ".tmp" / "operator-e2e.exe"
    if not binary.exists():
        pytest.skip("opt-in cross-process proof requires the locally cross-compiled Go test binary")
    bridge = "fake-bridge-0123456789abcdef"
    server = operator_admin.start_admin(bridge, "fake-mcp-0123456789abcdef")
    try:
        with usage.admission(12, "whisper_cpp", "", "ingest"):
            pass
        import os

        result = subprocess.run(
            [str(binary), "-test.run", "^TestOperatorMCPRealAdminIntegration$", "-test.v"],
            env={
                **os.environ,
                "WAMCP_REAL_ADMIN_TEST": "1",
                "WAMCP_REAL_MESSAGES_TEST": str(paired_dbs.messages_db.parent),
            },
            capture_output=True,
            text=True,
            timeout=20,
        )
        assert result.returncode == 0, result.stdout + result.stderr
        assert "seconds=12 requests=1" in result.stdout
        assert "cap_seconds=6 remaining_seconds=0" in result.stdout
        assert usage.current_usage()["cap_seconds"] == 6
    finally:
        server.shutdown()
        server.server_close()
