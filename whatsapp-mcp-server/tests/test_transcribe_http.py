"""HTTP provider exercised through multipart, real ffmpeg, tools and notes.db."""

import json
import subprocess
import threading
import time
import wave
from dataclasses import replace
from email.parser import BytesParser
from email.policy import default
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

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


def _audio(path, seconds=0.1):
    with wave.open(str(path), "wb") as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(16000)
        audio.writeframes(b"\0\0" * int(16000 * seconds))


def _add_audio(store, message_id, chat_jid):
    _add_archive_audio(store, message_id, chat_jid)
    _audio(Path(media_inventory.chat_media_dir(chat_jid)) / _media_filename(message_id))


@pytest.mark.parametrize("phase", ["headers", "body"])
def test_http_wall_deadline_includes_trickling_headers_and_body(provider, tmp_path, phase):
    class SlowHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            # Six pieces 0.6 s apart: each gap stays under the 1 s read timeout,
            # and without the wall deadline the response would take 3 s or more.
            pieces = (
                [
                    b"HTTP/1.1 200 OK\r\n",
                    b"Content-Type: application/json\r\n",
                    b"X-Pad-A: 1\r\n",
                    b"X-Pad-B: 1\r\n",
                    b"\r\n",
                    b'{"text":"done"}',
                ]
                if phase == "headers"
                else [
                    b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n",
                    b'{"te',
                    b'xt":',
                    b'"do',
                    b'ne"',
                    b"}",
                ]
            )
            try:
                for piece in pieces:
                    self.wfile.write(piece)
                    self.wfile.flush()
                    time.sleep(0.6)
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), SlowHandler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    source = tmp_path / "note.ogg"
    _audio(source)
    config = replace(load_config(), url=f"http://127.0.0.1:{server.server_port}/transcribe", timeout_s=1)
    started = time.monotonic()
    try:
        with pytest.raises(BackendUnavailableError, match="deadline"):
            transcribe_file(str(source), config=config)
        assert time.monotonic() - started < 2.4
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


@pytest.fixture
def provider(monkeypatch, tmp_path):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    state = {"calls": [], "heads": 0, "status": 200, "body": None, "delay": 0}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_HEAD(self):
            state["heads"] += 1
            state["head_auth"] = self.headers.get("Authorization")
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
            time.sleep(state["delay"])
            self.send_response(state["status"])
            if state["status"] == 302:
                self.send_header("Location", "http://127.0.0.1:1/no-redirect")
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(state["body"] or json.dumps({"text": f"part {len(state['calls'])}"}).encode())

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
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


def test_whole_file_budget_includes_conversion_and_all_parts(provider, tmp_path):
    source = tmp_path / "long.wav"
    _audio(source, seconds=810)
    # Each part answers inside the 3 s timeout, but the two together cannot:
    # a budget per part would take the conversion plus 5.6 s.
    provider["delay"] = 2.8
    started = time.monotonic()
    with pytest.raises(BackendUnavailableError, match="deadline"):
        transcribe_file(str(source), config=replace(load_config(), timeout_s=3))
    assert time.monotonic() - started < 4.8
