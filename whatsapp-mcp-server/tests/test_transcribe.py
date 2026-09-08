"""Tests for the whisper.cpp transcription helpers (fork feature)."""

import subprocess

import httpx
import pytest

import transcribe
from transcribe import BackendUnavailableError, TranscriptionError, WhisperConfig, load_config, transcribe_file


class TestLoadConfig:
    def test_defaults(self):
        cfg = load_config({})
        assert cfg.backend is None
        assert cfg.language == "pt"
        assert cfg.timeout_s == 300

    def test_server_wins_over_cli(self):
        cfg = load_config({"WHISPER_URL": "http://127.0.0.1:8178/inference", "WHISPER_BIN": "/usr/bin/whisper-cli"})
        assert cfg.backend == "server"

    def test_cli_backend(self):
        cfg = load_config({"WHISPER_BIN": " /opt/whisper-cli ", "WHISPER_MODEL": "/m.bin", "WHISPER_LANGUAGE": "en"})
        assert cfg.backend == "cli"
        assert cfg.binary == "/opt/whisper-cli"
        assert cfg.language == "en"

    @pytest.mark.parametrize("value", ["abc", "0", "-5"])
    def test_invalid_timeout(self, value):
        with pytest.raises(TranscriptionError, match="WHISPER_TIMEOUT_S"):
            load_config({"WHISPER_TIMEOUT_S": value})


def _fake_ffmpeg(monkeypatch):
    """Replace ffmpeg with a stub that just creates the output WAV."""

    def fake_run(cmd, **kwargs):
        if cmd[0] == "ffmpeg":
            with open(cmd[-1], "wb") as fh:
                fh.write(b"RIFF....WAVEfmt ")
            return subprocess.CompletedProcess(cmd, 0, "", "")
        raise AssertionError(f"unexpected command {cmd}")

    monkeypatch.setattr(transcribe.subprocess, "run", fake_run)


def test_no_backend_configured_is_a_clear_error(tmp_path):
    audio = tmp_path / "note.ogg"
    audio.write_bytes(b"OggS")
    # BackendUnavailableError, not a plain failure: nothing about this file is wrong.
    with pytest.raises(BackendUnavailableError, match="No whisper backend configured"):
        transcribe_file(str(audio), config=load_config({}))


def test_missing_input_file(tmp_path):
    cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
    with pytest.raises(FileNotFoundError):
        transcribe_file(str(tmp_path / "missing.ogg"), config=cfg)


class TestServerBackend:
    def test_posts_wav_and_returns_text(self, monkeypatch, tmp_path):
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")
        calls = []

        class Resp:
            status_code = 200
            text = '{"text": " olá, tudo bem? "}'

            def json(self):
                return {"text": " olá, tudo bem? "}

        def fake_post(url, files=None, data=None, timeout=None):
            calls.append({"url": url, "files": files, "data": data, "timeout": timeout})
            return Resp()

        monkeypatch.setattr(transcribe.httpx, "post", fake_post)
        cfg = WhisperConfig(url="http://127.0.0.1:8178/inference", binary=None, model=None, language="pt", timeout_s=42)

        result = transcribe_file(str(audio), config=cfg)

        assert result == {"text": "olá, tudo bem?", "language": "pt", "backend": "server"}
        assert calls[0]["url"] == "http://127.0.0.1:8178/inference"
        assert calls[0]["data"]["language"] == "pt"
        assert calls[0]["data"]["response_format"] == "json"
        assert calls[0]["timeout"] == 42
        assert calls[0]["files"]["file"][0] == "audio.wav"

    def test_language_override_and_auto(self, monkeypatch, tmp_path):
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")
        seen = []

        class Resp:
            status_code = 200
            text = "{}"

            def json(self):
                return {"text": "hi"}

        monkeypatch.setattr(transcribe.httpx, "post", lambda url, **kw: (seen.append(kw["data"]), Resp())[1])
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)

        assert transcribe_file(str(audio), language="en", config=cfg)["language"] == "en"
        assert seen[-1]["language"] == "en"
        transcribe_file(str(audio), language="auto", config=cfg)
        assert "language" not in seen[-1]

    def test_http_error_is_reported(self, monkeypatch, tmp_path):
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        class Resp:
            status_code = 500
            text = "model not loaded"

            def json(self):
                return {}

        monkeypatch.setattr(transcribe.httpx, "post", lambda *a, **kw: Resp())
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
        # A plain 500 is how whisper.cpp reports an inference it could not
        # finish, so it stays a verdict on this file (#377).
        with pytest.raises(TranscriptionError, match="HTTP 500") as raised:
            transcribe_file(str(audio), config=cfg)
        assert not isinstance(raised.value, BackendUnavailableError)

    def test_a_read_timeout_is_about_this_file(self, monkeypatch, tmp_path):
        """The server had the request: a note too long for WHISPER_TIMEOUT_S is parked, not retried."""
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        def slow(*args, **kwargs):
            raise httpx.ReadTimeout("timed out")

        monkeypatch.setattr(transcribe.httpx, "post", slow)
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(TranscriptionError, match="timed out on this file after 10s") as raised:
            transcribe_file(str(audio), config=cfg)
        assert not isinstance(raised.value, BackendUnavailableError)

    def test_a_connect_timeout_is_an_outage(self, monkeypatch, tmp_path):
        """Nothing accepted the connection, so nothing was said about the audio."""
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        def unreachable(*args, **kwargs):
            raise httpx.ConnectTimeout("timed out")

        monkeypatch.setattr(transcribe.httpx, "post", unreachable)
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(BackendUnavailableError, match="request failed"):
            transcribe_file(str(audio), config=cfg)

    def test_a_refused_connection_is_an_outage_not_a_bad_file(self, monkeypatch, tmp_path):
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        def refuse(*args, **kwargs):
            raise httpx.ConnectError("[Errno 111] Connection refused")

        monkeypatch.setattr(transcribe.httpx, "post", refuse)
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(BackendUnavailableError, match="Connection refused"):
            transcribe_file(str(audio), config=cfg)

    def test_a_url_httpx_cannot_even_build_is_an_outage(self, monkeypatch, tmp_path):
        """InvalidURL is not an HTTPError, so it needs naming: a typo must not park the archive."""
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        def bad_url(*args, **kwargs):
            raise httpx.InvalidURL("Invalid port: 'port'")

        monkeypatch.setattr(transcribe.httpx, "post", bad_url)
        cfg = WhisperConfig(url="http://whisper:port/inference", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(BackendUnavailableError, match="Invalid port"):
            transcribe_file(str(audio), config=cfg)

    @pytest.mark.parametrize("status", sorted(transcribe.OUTAGE_STATUSES))
    def test_a_deployment_status_is_an_outage_not_a_bad_file(self, monkeypatch, tmp_path, status):
        """A misrouted WHISPER_URL, or a server still loading, answers these for every file alike."""
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        class Resp:
            status_code = status
            text = "File Not Found"

            def json(self):
                return {}

        monkeypatch.setattr(transcribe.httpx, "post", lambda *a, **kw: Resp())
        cfg = WhisperConfig(url="http://x/", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(BackendUnavailableError, match=f"HTTP {status}"):
            transcribe_file(str(audio), config=cfg)

    def test_a_4xx_is_about_this_file(self, monkeypatch, tmp_path):
        """The server answered: whatever it says, it is this audio it refused."""
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")

        class Resp:
            status_code = 400
            text = "unsupported audio"

            def json(self):
                return {}

        monkeypatch.setattr(transcribe.httpx, "post", lambda *a, **kw: Resp())
        cfg = WhisperConfig(url="http://x/inference", binary=None, model=None, language="pt", timeout_s=10)
        with pytest.raises(TranscriptionError, match="HTTP 400") as raised:
            transcribe_file(str(audio), config=cfg)
        assert not isinstance(raised.value, BackendUnavailableError)


class TestCliBackend:
    def test_runs_whisper_cli_and_reads_txt(self, monkeypatch, tmp_path):
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")
        model = tmp_path / "ggml-small.bin"
        model.write_bytes(b"ggml")
        binary = tmp_path / "whisper-cli"
        binary.write_bytes(b"#!/bin/sh\n")
        commands = []

        def fake_run(cmd, **kwargs):
            commands.append(cmd)
            if cmd[0] == "ffmpeg":
                with open(cmd[-1], "wb") as fh:
                    fh.write(b"RIFF")
            else:
                prefix = cmd[cmd.index("-of") + 1]
                with open(prefix + ".txt", "w", encoding="utf-8") as fh:
                    fh.write(" Bom dia.\n")
            return subprocess.CompletedProcess(cmd, 0, "", "")

        monkeypatch.setattr(transcribe.subprocess, "run", fake_run)
        cfg = WhisperConfig(url=None, binary=str(binary), model=str(model), language="pt", timeout_s=10)

        result = transcribe_file(str(audio), config=cfg)

        assert result == {"text": "Bom dia.", "language": "pt", "backend": "cli"}
        cli = commands[1]
        assert cli[0] == str(binary)
        assert cli[cli.index("-m") + 1] == str(model)
        assert cli[cli.index("-l") + 1] == "pt"
        assert cli[cli.index("-f") + 1].endswith("audio.wav")

    def test_missing_model_is_clear(self, monkeypatch, tmp_path):
        _fake_ffmpeg(monkeypatch)
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")
        cfg = WhisperConfig(url=None, binary="/usr/bin/whisper-cli", model=None, language="pt", timeout_s=10)
        # Half a backend is an outage: the audio is fine, the deployment is not.
        with pytest.raises(BackendUnavailableError, match="WHISPER_MODEL"):
            transcribe_file(str(audio), config=cfg)

    def test_whisper_exiting_non_zero_is_about_this_file(self, monkeypatch, tmp_path):
        audio = tmp_path / "note.ogg"
        audio.write_bytes(b"OggS")
        model = tmp_path / "ggml-small.bin"
        model.write_bytes(b"ggml")
        binary = tmp_path / "whisper-cli"
        binary.write_bytes(b"#!/bin/sh\n")

        def fake_run(cmd, **kwargs):
            if cmd[0] == "ffmpeg":
                with open(cmd[-1], "wb") as fh:
                    fh.write(b"RIFF")
                return subprocess.CompletedProcess(cmd, 0, "", "")
            raise subprocess.CalledProcessError(1, cmd, stderr="failed to decode audio")

        monkeypatch.setattr(transcribe.subprocess, "run", fake_run)
        cfg = WhisperConfig(url=None, binary=str(binary), model=str(model), language="pt", timeout_s=10)
        with pytest.raises(TranscriptionError, match="failed to decode audio") as raised:
            transcribe_file(str(audio), config=cfg)
        assert not isinstance(raised.value, BackendUnavailableError)
