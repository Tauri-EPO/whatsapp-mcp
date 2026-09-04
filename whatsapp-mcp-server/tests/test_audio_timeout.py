"""ffmpeg conversions must not hang: both call sites pass a timeout and surface TimeoutExpired."""

import subprocess

import pytest

import audio
import transcribe


def test_ffmpeg_timeout_env(monkeypatch):
    monkeypatch.delenv("FFMPEG_TIMEOUT_S", raising=False)
    assert audio.ffmpeg_timeout_s() == audio.DEFAULT_FFMPEG_TIMEOUT_S
    monkeypatch.setenv("FFMPEG_TIMEOUT_S", "15")
    assert audio.ffmpeg_timeout_s() == 15
    monkeypatch.setenv("FFMPEG_TIMEOUT_S", "0")
    assert audio.ffmpeg_timeout_s() == audio.DEFAULT_FFMPEG_TIMEOUT_S
    monkeypatch.setenv("FFMPEG_TIMEOUT_S", "abc")
    assert audio.ffmpeg_timeout_s() == audio.DEFAULT_FFMPEG_TIMEOUT_S


def _stalled_run(cmd, **kwargs):
    assert kwargs.get("timeout"), "ffmpeg must run with a timeout"
    raise subprocess.TimeoutExpired(cmd, kwargs["timeout"])


def test_convert_to_opus_ogg_times_out(monkeypatch, tmp_path):
    monkeypatch.setenv("FFMPEG_TIMEOUT_S", "3")
    monkeypatch.setattr(audio.subprocess, "run", _stalled_run)
    src = tmp_path / "in.m4a"
    src.write_bytes(b"x")
    with pytest.raises(RuntimeError, match="timed out after 3s"):
        audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))


def test_convert_to_wav16k_times_out(monkeypatch, tmp_path):
    monkeypatch.setenv("FFMPEG_TIMEOUT_S", "3")
    monkeypatch.setattr(transcribe.subprocess, "run", _stalled_run)
    src = tmp_path / "in.ogg"
    src.write_bytes(b"x")
    with pytest.raises(transcribe.TranscriptionError, match="timed out after 3s"):
        transcribe.convert_to_wav16k(str(src), str(tmp_path / "out.wav"))


def test_ffmpeg_calls_pass_timeout(monkeypatch, tmp_path):
    seen = []

    def ok(cmd, **kwargs):
        seen.append(kwargs.get("timeout"))
        return subprocess.CompletedProcess(cmd, 0, "", "")

    monkeypatch.setattr(audio.subprocess, "run", ok)
    monkeypatch.setattr(transcribe.subprocess, "run", ok)
    src = tmp_path / "a.ogg"
    src.write_bytes(b"x")
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "b.ogg"))
    transcribe.convert_to_wav16k(str(src), str(tmp_path / "c.wav"))
    assert len(seen) == 2 and all(t and t > 0 for t in seen)
