"""HTTP provider exercised through multipart, real ffmpeg, tools and notes.db."""

import json
import math
import subprocess
import threading
import time
import wave
from contextlib import contextmanager
from dataclasses import replace
from email.parser import BytesParser
from email.policy import default
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace

import pytest

import main
import media_inventory
import media_notes
import tool_policy
import transcribe
import transcribe_worker
import whatsapp
from tests.conftest import ALICE
from tests.test_transcribe_ingest import SHA, _media_filename
from tests.test_transcribe_ingest import _add_audio as _add_archive_audio
from tool_policy import ToolPolicy
from transcribe import BackendUnavailableError, TranscriptionError, load_config, transcribe_file

KEY = "fake-provider-key-0123456789abcdef"


def _audio(path, seconds=0.1, sample_rate=16000):
    with wave.open(str(path), "wb") as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(sample_rate)
        audio.writeframes(b"\0\0" * int(sample_rate * seconds))


def _add_audio(store, message_id, chat_jid):
    _add_archive_audio(store, message_id, chat_jid)
    _audio(Path(media_inventory.chat_media_dir(chat_jid)) / _media_filename(message_id))


def test_whole_file_budget_reaches_trickling_multipart_http(tmp_path, monkeypatch):
    import transcription_usage

    # Follow the public call through real request/cancellation code. Preparation
    # consumes 1s; the first trickling response consumes 1.2s, leaving 0.8s for
    # the second. Renewing the whole budget for either part must fail this proof.
    clock = SimpleNamespace(now=100.0, deadline=None)
    budgets, reads, closed = [], [], []
    source = tmp_path / "source.wav"
    source.touch()
    parts = [tmp_path / f"part-{i}.ogg" for i in range(2)]
    for part in parts:
        part.write_bytes(b"fake-audio")

    def conversion(audio_path, work_dir, deadline):
        assert audio_path == str(source) and deadline == 103.0
        clock.now += 1.0
        return parts, 810.0

    @contextmanager
    def admission(*args, deadline):
        assert deadline == 103.0
        yield

    async def wait_for(request, timeout):
        budgets.append(timeout)
        clock.deadline = clock.now + timeout
        try:
            return await request
        finally:
            clock.deadline = None

    class Response:
        status_code = 200
        headers = {}

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            closed.append("response")

        async def aiter_raw(self):
            for chunk in [b'{"text":', b'"done"}']:
                clock.now += 0.6
                if clock.deadline is not None and clock.now >= clock.deadline:
                    raise TimeoutError
                yield chunk

    class Client:
        def __init__(self, **kwargs):
            reads.append(kwargs["timeout"].read)

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            closed.append("client")

        def stream(self, *args, **kwargs):
            return Response()

    monkeypatch.setattr(transcribe, "time", SimpleNamespace(monotonic=lambda: clock.now))
    monkeypatch.setattr(transcribe, "_http_parts", conversion)
    monkeypatch.setattr(transcription_usage, "admission", admission)
    monkeypatch.setattr(transcribe.asyncio, "wait_for", wait_for)
    monkeypatch.setattr(transcribe.httpx, "AsyncClient", Client)
    config = replace(
        transcribe.load_config({}),
        provider="openai_compatible",
        url="http://example.invalid/transcribe",
        model="fake-speech-model",
        timeout_s=3,
    )
    with pytest.raises(transcribe.BackendUnavailableError, match="deadline"):
        transcribe.transcribe_file(str(source), config=config)
    assert budgets == pytest.approx([2.0, 0.8])
    assert reads == pytest.approx([2.0, 0.8])
    assert closed == ["response", "client"] * 2


@pytest.mark.parametrize("phase", ["headers", "body"])
def test_http_request_deadline_covers_the_entire_stream(tmp_path, monkeypatch, phase):
    # A virtual transport advances time at each receive, so the proof checks the
    # scope and single budget of wait_for without relying on scheduler speed.
    clock = SimpleNamespace(now=100.0, deadline=None)
    timeouts = []

    def receive():
        clock.now += 0.6
        if clock.deadline is not None and clock.now >= clock.deadline:
            raise TimeoutError

    async def wait_for(request, timeout):
        timeouts.append(timeout)
        clock.deadline = clock.now + timeout
        try:
            return await request
        finally:
            clock.deadline = None

    class Response:
        status_code = 200
        headers = {}

        async def __aenter__(self):
            if phase == "headers":
                for _ in range(6):
                    receive()
            return self

        async def __aexit__(self, *args):
            pass

        async def aiter_raw(self):
            for chunk in [b'{"te', b'xt":', b'"do', b'ne"', b"}"]:
                if phase == "body":
                    receive()
                yield chunk

    class Client:
        def __init__(self, **kwargs):
            assert kwargs["timeout"].read == 1

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        def stream(self, *args, **kwargs):
            return Response()

    monkeypatch.setattr(transcribe.asyncio, "wait_for", wait_for)
    monkeypatch.setattr(transcribe.httpx, "AsyncClient", Client)
    source = tmp_path / "note.ogg"
    source.write_bytes(b"fake-audio")
    config = replace(load_config({}), timeout_s=1, url="http://example.invalid/transcribe")
    with pytest.raises(BackendUnavailableError, match="deadline"):
        transcribe._transcribe_http(source, config, "auto")
    assert timeouts == [1]
    assert clock.now == pytest.approx(101.2)


@pytest.mark.parametrize("phase", ["headers", "body"])
def test_http_wall_deadline_includes_trickling_headers_and_body(tmp_path, phase):
    stop = threading.Event()
    received = threading.Event()

    class SlowHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            received.set()
            # Pieces one second apart take at least 11 s without one
            # request deadline. The 9 s bound gives the 3 s budget ample slack
            # and still rejects a budget renewed for each chunk.
            pieces = (
                [b"HTTP/1.1 200 OK\r\n", b"Content-Type: application/json\r\n"]
                + [b"X-Pad: 1\r\n"] * 8
                + [b"\r\n", b'{"text":"done"}']
                if phase == "headers"
                else [b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n"]
                + [bytes([byte]) for byte in b'{"text":"done"}']
            )
            try:
                for piece in pieces:
                    self.wfile.write(piece)
                    self.wfile.flush()
                    if stop.wait(1):
                        break
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), SlowHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    source = tmp_path / "note.ogg"
    source.write_bytes(b"fake-audio")
    config = replace(load_config({}), url=f"http://127.0.0.1:{server.server_port}/transcribe", timeout_s=3)
    started = time.monotonic()
    try:
        with pytest.raises(BackendUnavailableError, match="deadline"):
            transcribe._transcribe_http(source, config, "auto")
        elapsed = time.monotonic() - started
        print(f"{phase} trickle elapsed={elapsed:.3f}s (budget=3s, bound=9s)")
        assert received.is_set(), "the HTTP provider was never reached"
        assert elapsed < 9
    finally:
        stop.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


@pytest.fixture
def provider(monkeypatch, tmp_path):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    monkeypatch.setattr(transcribe, "_activity", transcribe.OrderedDict())
    state = {
        "calls": [],
        "heads": 0,
        "status": 200,
        "body": None,
        "delay": 0,
        "head_delay": 0,
        "release_delay": threading.Event(),
    }

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_HEAD(self):
            state["heads"] += 1
            state["head_auth"] = self.headers.get("Authorization")
            time.sleep(state["head_delay"])
            self.send_response(state["status"])
            self.end_headers()

        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            message = BytesParser(policy=default).parsebytes(
                ("Content-Type: " + self.headers["Content-Type"] + "\r\n\r\n").encode() + body
            )
            fields = {
                part.get_param("name", header="Content-Disposition"): part.get_payload(decode=True)
                for part in message.iter_parts()
            }
            file = next(part for part in message.iter_parts() if part.get_filename())
            state["calls"].append(
                {
                    "path": self.path,
                    "auth": self.headers.get("Authorization"),
                    "fields": fields,
                    "filename": file.get_filename(),
                    "mime": file.get_content_type(),
                }
            )
            state["release_delay"].wait(state["delay"])
            self.send_response(state["status"])
            if state["status"] == 302:
                self.send_header("Location", "http://127.0.0.1:1/no-redirect")
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(state["body"] or json.dumps({"text": f"part {len(state['calls'])}"}).encode())

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = True
    state["url"] = f"http://127.0.0.1:{server.server_port}/v1/audio/transcriptions"
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    env = {
        "WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible",
        "WHATSAPP_TRANSCRIPTION_URL": state["url"],
        "WHATSAPP_TRANSCRIPTION_MODEL": "fake-speech-model",
        "WHATSAPP_TRANSCRIPTION_LANGUAGE": "en",
        "WHATSAPP_TRANSCRIPTION_API_KEY": KEY,
    }
    for name, value in env.items():
        monkeypatch.setenv(name, value)
    # A functioning upload despite these values proves ambient auth/proxies are ignored.
    netrc = tmp_path / "netrc"
    netrc.write_text("machine 127.0.0.1 login unwanted password fake-netrc-secret\n")
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.setenv(name, "http://127.0.0.1:1")
    monkeypatch.setenv("NO_PROXY", "")
    monkeypatch.setenv("NETRC", str(netrc))
    try:
        yield state
    finally:
        state["release_delay"].set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_transcoded_audio_model_language_auth_and_status(provider, tmp_path, caplog):
    source = tmp_path / "note.ogg"
    _audio(source)
    with source.open("ab") as original:
        original.write(b"fake-sensitive-original-trailer")
    result = transcribe_file(str(source))
    assert result == {
        "text": "part 1",
        "language": "en",
        "backend": "openai_compatible",
        "provider": "openai_compatible",
        "model": "fake-speech-model",
        "duration_s": 0.1,
    }
    sent = provider["calls"][0]
    assert sent["path"] == "/v1/audio/transcriptions"
    assert sent["auth"] == "Bearer " + KEY
    assert sent["filename"] == "part-0000.ogg" and sent["mime"] == "audio/ogg"
    uploaded = sent["fields"].pop("file")
    assert uploaded.startswith(b"OggS") and uploaded != source.read_bytes()
    assert b"fake-sensitive-original-trailer" not in uploaded
    assert sent["fields"] == {
        "model": b"fake-speech-model",
        "language": b"en",
        "response_format": b"json",
    }
    status = transcribe.describe_status()
    assert status["provider"] == "openai_compatible" and status["endpoint_host"] == "127.0.0.1"
    assert status["reachable"] is True
    assert provider["heads"] == 1 and provider["head_auth"] == "Bearer " + KEY
    # Feed actual Opus/OGG back through the same pipeline, as a phone voice note.
    source.write_bytes(uploaded)
    assert transcribe_file(str(source))["text"] == "part 2"
    assert provider["calls"][1]["fields"]["file"].startswith(b"OggS")
    assert KEY not in json.dumps(status) + caplog.text


def test_probe_has_independent_five_second_default(provider, monkeypatch):
    monkeypatch.setenv("WHISPER_TIMEOUT_S", "1")
    provider["head_delay"] = 2.2
    assert load_config().probe_timeout_s == 5
    assert transcribe.describe_status()["reachable"] is True
    assert provider["heads"] == 1


def test_probe_timeouts_retry_and_preserve_real_activity(provider, tmp_path, monkeypatch):
    source = tmp_path / "voice.wav"
    _audio(source)
    transcribe_file(str(source))
    success = transcribe.describe_status()["last_success_at"]
    assert success and transcribe.describe_status()["last_failure_at"] is None
    before = provider["heads"]
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROBE_TIMEOUT_S", "1")
    provider["head_delay"] = 1.5
    status = transcribe.describe_status()
    assert provider["heads"] == before + 2
    assert status["reachable"] is None and status["probe"] == "timeout"
    assert status["last_success_at"] == success and status["last_failure_at"] is None
    provider["head_delay"], provider["status"] = 0, 503
    assert transcribe.describe_status()["reachable"] is None  # recent actual success
    provider["status"] = 500
    with pytest.raises(BackendUnavailableError):
        transcribe_file(str(source))
    failed = transcribe.describe_status()
    assert failed["last_success_at"] == success and failed["last_failure_at"]


@pytest.mark.parametrize("status_code", [401, 403])
def test_probe_auth_failure_is_false_even_after_success(provider, tmp_path, status_code):
    source = tmp_path / "voice.wav"
    _audio(source)
    transcribe_file(str(source))
    provider["status"] = status_code
    status = transcribe.describe_status()
    assert status["reachable"] is False and status["probe"] == "unauthorized"
    assert status["last_success_at"] and status["last_failure_at"] is None
    assert provider["heads"] == 1


def test_old_success_and_other_endpoint_do_not_mask_failure(provider, tmp_path, monkeypatch):
    from datetime import UTC, datetime, timedelta

    source = tmp_path / "voice.wav"
    _audio(source)
    transcribe_file(str(source))
    config = load_config()
    transcribe._activity[(config.provider, config.url, config.model)]["last_success_at"] = datetime.now(
        UTC
    ) - timedelta(minutes=16)
    provider["status"] = 503
    assert transcribe.describe_status()["reachable"] is False
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_URL", provider["url"] + "-other")
    status = transcribe.describe_status()
    assert status["reachable"] is False and status["last_success_at"] is None


@pytest.mark.parametrize("value", ["0", "-1", "nan", "inf", "bad"])
def test_invalid_probe_timeout_is_rejected(provider, monkeypatch, value):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROBE_TIMEOUT_S", value)
    with pytest.raises(TranscriptionError, match="PROBE_TIMEOUT"):
        load_config()


def test_auto_language_omitted(provider, tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_LANGUAGE", "auto")
    source = tmp_path / "note.ogg"
    _audio(source)
    transcribe_file(str(source))
    assert "language" not in provider["calls"][0]["fields"]


def test_large_audio_real_ffmpeg_chunk_order_and_limits(provider, tmp_path):
    source = tmp_path / "long.wav"
    subprocess.run(
        [
            "ffmpeg",
            "-nostdin",
            "-v",
            "error",
            "-f",
            "lavfi",
            "-i",
            "anullsrc=r=16000:cl=mono",
            "-t",
            "810",
            "-c:a",
            "pcm_s16le",
            str(source),
        ],
        check=True,
        timeout=30,
        capture_output=True,
    )
    assert source.stat().st_size > transcribe.HTTP_UPLOAD_LIMIT
    result = transcribe_file(str(source))
    assert result["text"] == "part 1 part 2"
    assert [c["filename"] for c in provider["calls"]] == ["part-0000.ogg", "part-0001.ogg"]
    assert all(0 < len(c["fields"]["file"]) < transcribe.HTTP_UPLOAD_LIMIT for c in provider["calls"])


@pytest.mark.parametrize("status", [401, 403, 408, 429, 500, 502, 503, 302])
def test_backend_outages_never_park_audio(provider, paired_dbs, status, caplog):
    provider["status"] = status
    provider["body"] = json.dumps({"error": KEY}).encode()
    _add_audio(paired_dbs, "AUD1", ALICE)
    result = transcribe_worker.run_once(10)
    assert result.failed == 0 and result.transcribed == 0
    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}
    assert KEY not in caplog.text
    assert len(provider["calls"]) == 1  # redirects never followed


def test_connection_refused_is_outage(provider, paired_dbs, monkeypatch):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_URL", "http://127.0.0.1:1/transcriptions")
    _add_audio(paired_dbs, "AUD1", ALICE)
    result = transcribe_worker.run_once(10)
    assert result.failed == 0 and result.transcribed == 0
    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}


def test_400_parks_only_this_file_and_omits_body(provider, paired_dbs, caplog):
    provider["status"] = 400
    provider["body"] = json.dumps({"error": KEY}).encode()
    _add_audio(paired_dbs, "AUD1", ALICE)
    result = transcribe_worker.run_once(10)
    assert result.failed == 1 and not result.outage
    notes = media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]
    assert "HTTP 400" in notes["transcript_error"]
    assert KEY not in json.dumps(notes) + caplog.text


def test_worker_and_tool_share_provider_and_cache_notes(provider, paired_dbs, monkeypatch):
    # Exercise the actual cached-only tool path; never contact a paired bridge.
    monkeypatch.setattr(tool_policy, "_active", ToolPolicy(deny=frozenset({"download_media"})))
    _add_audio(paired_dbs, "AUD1", ALICE)
    assert transcribe_worker.run_once(1).transcribed == 1
    notes = media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]
    assert notes["transcript_provider"] == "openai_compatible"
    assert notes["transcript_model"] == "fake-speech-model"
    _add_audio(paired_dbs, "AUD2", ALICE)
    result = main.transcribe_audio(chat_jid=ALICE, message_id="AUD2")
    assert result["text"] == "part 2"
    assert media_notes.fetch_notes([SHA["AUD2"]])[SHA["AUD2"]]["transcript_model"] == "fake-speech-model"
    assert len(provider["calls"]) == 2


@pytest.mark.parametrize("body", [b"not JSON", b'{"text": 7}', b'{"text": ""}'])
def test_per_file_response_errors_do_not_expose_response(provider, tmp_path, body):
    provider["body"] = body
    source = tmp_path / "note.ogg"
    _audio(source)
    with pytest.raises(TranscriptionError) as exc:
        transcribe_file(str(source))
    assert not isinstance(exc.value, BackendUnavailableError)
    assert KEY not in str(exc.value)


def test_bounded_response(provider, tmp_path):
    provider["body"] = b" " * (transcribe.HTTP_RESPONSE_LIMIT + 1)
    source = tmp_path / "note.ogg"
    _audio(source)
    with pytest.raises(BackendUnavailableError, match="limits"):
        transcribe_file(str(source))


def test_explicit_configuration_no_fallback():
    with pytest.raises(BackendUnavailableError, match="requires"):
        load_config(
            {"WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible", "WHISPER_URL": "http://localhost/inference"}
        )
    with pytest.raises(BackendUnavailableError, match="without credentials"):
        load_config(
            {
                "WHATSAPP_TRANSCRIPTION_PROVIDER": "openai_compatible",
                "WHATSAPP_TRANSCRIPTION_URL": "https://user:fake-secret@example.com/transcribe",
                "WHATSAPP_TRANSCRIPTION_MODEL": "fake-model",
            }
        )


def test_local_replacement_clears_http_provenance(provider, paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE)
    assert transcribe_worker.run_once(1).transcribed == 1
    before = media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]
    assert before["transcript_provider"] == "openai_compatible"
    media_notes.store_transcript(
        SHA["AUD1"], {"text": "replacement local transcript", "backend": "cli", "language": "en"}
    )
    after = media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]
    assert after["transcript"] == "replacement local transcript"
    assert after["transcript_backend"] == "cli"
    assert "transcript_provider" not in after and "transcript_model" not in after


@pytest.mark.parametrize("backend", ["openai_compatible", "whisper_cpp"])
@pytest.mark.parametrize("name", ["whatsapp.db", ".bridge-token", "run-secret", "symlink"])
def test_explicit_secret_and_symlink_paths_never_upload(provider, tmp_path, monkeypatch, backend, name):
    store = tmp_path / "archive"
    store.mkdir()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store / "messages.db"))
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path / "media"))
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_PROVIDER", backend)
    monkeypatch.setenv("WHISPER_URL", provider["url"])
    if name in ("whatsapp.db", ".bridge-token"):
        source = store / name
        source.write_bytes(b"fake-session-secret-not-audio")
    else:
        outside = tmp_path / "run" / "secrets"
        outside.mkdir(parents=True)
        source = outside / "fake-key"
        source.write_bytes(b"fake-mounted-secret-not-audio")
        if name == "symlink":
            link = store / "escape.ogg"
            try:
                link.symlink_to(source)
            except OSError:
                pytest.skip("Symlinks unavailable on this platform")
            source = link
    result = main.transcribe_audio(file_path=str(source))
    assert result["error"]["code"] == ("internal" if name in ("whatsapp.db", ".bridge-token") else "denied")
    assert provider["calls"] == [] and provider["heads"] == 0


def test_non_audio_never_uploaded_even_with_audio_extension(provider, tmp_path):
    source = tmp_path / "secret.ogg"
    source.write_bytes(b"fake-session-secret-not-audio")
    with pytest.raises(TranscriptionError):
        transcribe_file(str(source))
    assert provider["calls"] == []


def test_unicode_audio_upload_with_non_utf8_default_encoding(provider, tmp_path, monkeypatch):
    # Reproduce the Windows ANSI default on every platform; ffmpeg emits UTF-8.
    monkeypatch.setattr(subprocess, "_text_encoding", lambda: "cp1252")
    source = tmp_path / "fake-\u0101.wav"
    _audio(source)
    assert transcribe_file(str(source))["text"] == "part 1"
    assert len(provider["calls"]) == 1
    assert provider["calls"][0]["fields"]["file"].startswith(b"OggS")


def test_whole_file_budget_with_real_conversion_and_slow_http(provider, tmp_path, monkeypatch):
    source = tmp_path / "long-real.wav"
    # Scale only the chunk interval for this budget test: real ffmpeg and HTTP
    # still run, but two tiny silence parts avoid encoding ten minutes of audio.
    _audio(source, seconds=0.15, sample_rate=8000)

    def short_segments(command, **kwargs):
        command = list(command)
        if "-segment_time" in command:
            command[command.index("-segment_time") + 1] = "0.1"
        return subprocess.run(command, **kwargs)

    monkeypatch.setattr(
        transcribe,
        "subprocess",
        SimpleNamespace(
            run=short_segments,
            DEVNULL=subprocess.DEVNULL,
            TimeoutExpired=subprocess.TimeoutExpired,
            CalledProcessError=subprocess.CalledProcessError,
        ),
    )
    probe_dir = tmp_path / "conversion-probe"
    probe_dir.mkdir()
    started = time.monotonic()
    parts, _ = transcribe._http_parts(str(source), str(probe_dir), started + 120)
    preparation_s = time.monotonic() - started
    assert len(parts) == 2
    budget = math.ceil(2 * preparation_s) + 2
    prepare = transcribe._http_parts

    def measured_preparation(audio_path, work_dir, deadline):
        parts, duration = prepare(audio_path, work_dir, deadline)
        assert len(parts) == 2
        # Size the real provider delay from what conversion actually left, so
        # even a faster warmed conversion still requires one shared deadline.
        provider["delay"] = (deadline - time.monotonic()) * 0.75
        return parts, duration

    monkeypatch.setattr(transcribe, "_http_parts", measured_preparation)
    with pytest.raises(BackendUnavailableError, match="deadline"):
        transcribe_file(str(source), config=replace(load_config(), timeout_s=budget))
    assert provider["calls"], "the real HTTP provider was never reached"
    assert all(call["fields"]["file"].startswith(b"OggS") for call in provider["calls"])


@pytest.mark.parametrize("part_count", [2, 4])
def test_whole_file_budget_includes_conversion_and_all_parts(tmp_path, monkeypatch, part_count):
    # This proves deadline propagation without ffmpeg/process scheduling. Other
    # tests in this module exercise real conversion and HTTP wall deadlines.
    import transcription_usage

    clock = SimpleNamespace(now=100.0)
    monkeypatch.setattr(transcribe, "time", SimpleNamespace(monotonic=lambda: clock.now))
    source = tmp_path / "long.wav"
    source.touch()
    parts = [tmp_path / f"part-{n}.ogg" for n in range(part_count)]
    seen = []

    def conversion(audio_path, work_dir, deadline):
        assert audio_path == str(source)
        assert deadline == 103.0
        clock.now += 1.0
        return parts, 810.0

    @contextmanager
    def admission(*args, deadline):
        assert deadline == 103.0  # Accounting gets the same absolute deadline.
        yield

    def upload(part, config, language):
        seen.append((part, config.timeout_s))
        clock.now += 0.75
        return "part"

    monkeypatch.setattr(transcribe, "_http_parts", conversion)
    monkeypatch.setattr(transcribe, "_transcribe_http", upload)
    monkeypatch.setattr(transcription_usage, "admission", admission)
    config = replace(
        load_config({}),
        provider="openai_compatible",
        url="http://example.invalid/transcribe",
        model="fake-speech-model",
        timeout_s=3,
    )
    if part_count == 4:
        with pytest.raises(BackendUnavailableError, match="deadline"):
            transcribe_file(str(source), config=config)
    else:
        assert transcribe_file(str(source), config=config)["text"] == "part part"
    assert seen == list(zip(parts, [2.0, 1.25, 0.5]))


def test_each_part_request_gets_only_the_remaining_budget(provider, tmp_path, monkeypatch):
    source = tmp_path / "long.wav"
    _audio(source, seconds=810)
    seen = []

    def recording_http(part, config, language):
        seen.append((time.monotonic(), config.timeout_s))
        time.sleep(0.3)
        return "part"

    monkeypatch.setattr(transcribe, "_transcribe_http", recording_http)
    started = time.monotonic()
    assert transcribe_file(str(source), config=replace(load_config(), timeout_s=30))["text"] == "part part"
    assert len(seen) == 2
    for called_at, timeout_s in seen:
        assert timeout_s <= 30 - (called_at - started) + 0.05
    assert seen[1][1] < seen[0][1] - 0.25
