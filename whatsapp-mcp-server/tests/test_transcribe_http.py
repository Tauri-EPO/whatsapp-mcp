"""HTTP provider exercised through multipart, real ffmpeg, tools and notes.db."""

import json
import subprocess
import threading
import time
from dataclasses import replace
from email.parser import BytesParser
from email.policy import default
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

import main
import media_notes
import tool_policy
import transcribe
import transcribe_worker
from tests.conftest import ALICE
from tests.test_transcribe_ingest import SHA, _add_audio
from tool_policy import ToolPolicy
from transcribe import BackendUnavailableError, TranscriptionError, load_config, transcribe_file

KEY = "fake-provider-key-0123456789abcdef"


@pytest.mark.parametrize("phase", ["headers", "body"])
def test_http_wall_deadline_includes_trickling_headers_and_body(provider, tmp_path, phase):
    class SlowHandler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            pieces = (
                [b"HTTP/1.1 200 OK\r\n", b"Content-Type: application/json\r\n", b"\r\n", b'{"text":"done"}']
                if phase == "headers"
                else [b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n", b'{"text":', b'"done"', b"}"]
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
    source.write_bytes(b"OggS")
    config = replace(load_config(), url=f"http://127.0.0.1:{server.server_port}/transcribe", timeout_s=1)
    started = time.monotonic()
    try:
        with pytest.raises(BackendUnavailableError, match="deadline"):
            transcribe_file(str(source), config=config)
        assert time.monotonic() - started < 1.6
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


@pytest.fixture
def provider(monkeypatch, tmp_path):
    state = {"calls": [], "heads": 0, "status": 200, "body": None}

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


def test_original_opus_model_language_auth_and_status(provider, tmp_path, caplog):
    source = tmp_path / "note.ogg"
    source.write_bytes(b"OggS-fake-opus")
    result = transcribe_file(str(source))
    assert result == {
        "text": "part 1",
        "language": "en",
        "backend": "openai_compatible",
        "provider": "openai_compatible",
        "model": "fake-speech-model",
    }
    sent = provider["calls"][0]
    assert sent["path"] == "/v1/audio/transcriptions"
    assert sent["auth"] == "Bearer " + KEY
    assert sent["fields"] == {
        "file": b"OggS-fake-opus",
        "model": b"fake-speech-model",
        "language": b"en",
        "response_format": b"json",
    }
    status = transcribe.describe_status()
    assert status["provider"] == "openai_compatible" and status["endpoint_host"] == "127.0.0.1"
    assert status["reachable"] is True
    assert provider["heads"] == 1 and provider["head_auth"] == "Bearer " + KEY
    assert KEY not in json.dumps(status) + caplog.text


def test_auto_language_omitted(provider, tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_TRANSCRIPTION_LANGUAGE", "auto")
    source = tmp_path / "note.ogg"
    source.write_bytes(b"OggS")
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
    source.write_bytes(b"OggS")
    with pytest.raises(TranscriptionError) as exc:
        transcribe_file(str(source))
    assert not isinstance(exc.value, BackendUnavailableError)
    assert KEY not in str(exc.value)


def test_bounded_response(provider, tmp_path):
    provider["body"] = b" " * (transcribe.HTTP_RESPONSE_LIMIT + 1)
    source = tmp_path / "note.ogg"
    source.write_bytes(b"OggS")
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
