"""audio.convert_to_opus_ogg / convert_to_opus_ogg_temp with ffmpeg replaced by a fake."""

import os
import subprocess

import pytest

import audio


def _fake_ffmpeg(calls):
    """A subprocess.run stand-in that records the command and writes the output file."""

    def run(cmd, **kwargs):
        calls.append((cmd, kwargs))
        assert cmd[0] == "ffmpeg" and cmd[-2] == "-y"
        with open(cmd[-1], "wb") as f:
            f.write(b"OggS")
        return subprocess.CompletedProcess(cmd, 0, "", "")

    return run


def test_convert_defaults_to_ogg_next_to_input(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(audio.subprocess, "run", _fake_ffmpeg(calls))
    src = tmp_path / "note.m4a"
    src.write_bytes(b"x")

    out = audio.convert_to_opus_ogg(str(src))

    assert out == str(tmp_path / "note.ogg") and os.path.isfile(out)
    cmd, kwargs = calls[0]
    assert cmd[cmd.index("-i") + 1] == str(src)
    assert cmd[cmd.index("-c:a") + 1] == "libopus"
    assert cmd[cmd.index("-b:a") + 1] == "32k"
    assert cmd[cmd.index("-ar") + 1] == "24000"
    assert cmd[cmd.index("-application") + 1] == "voip"
    assert kwargs["check"] is True and kwargs["capture_output"] is True and kwargs["timeout"] > 0


def test_convert_creates_output_directory_and_honours_options(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(audio.subprocess, "run", _fake_ffmpeg(calls))
    src = tmp_path / "in.wav"
    src.write_bytes(b"x")
    out = tmp_path / "nested" / "dir" / "voice.ogg"

    assert audio.convert_to_opus_ogg(str(src), str(out), bitrate="64k", sample_rate=48000) == str(out)
    assert out.is_file()
    cmd, _ = calls[0]
    assert cmd[cmd.index("-b:a") + 1] == "64k" and cmd[cmd.index("-ar") + 1] == "48000"


def test_convert_missing_input_is_file_not_found(monkeypatch, tmp_path):
    monkeypatch.setattr(audio.subprocess, "run", _fake_ffmpeg([]))
    with pytest.raises(FileNotFoundError, match="Input file not found"):
        audio.convert_to_opus_ogg(str(tmp_path / "nope.mp3"))


def test_convert_ffmpeg_failure_surfaces_stderr(monkeypatch, tmp_path):
    def failing(cmd, **kwargs):
        raise subprocess.CalledProcessError(1, cmd, stderr="Unknown encoder 'libopus'")

    monkeypatch.setattr(audio.subprocess, "run", failing)
    src = tmp_path / "in.wav"
    src.write_bytes(b"x")
    with pytest.raises(RuntimeError, match="Unknown encoder 'libopus'"):
        audio.convert_to_opus_ogg(str(src))


def test_convert_temp_returns_ogg_temp_file(monkeypatch, tmp_path):
    monkeypatch.setattr(audio.subprocess, "run", _fake_ffmpeg([]))
    src = tmp_path / "in.wav"
    src.write_bytes(b"x")

    out = audio.convert_to_opus_ogg_temp(str(src), bitrate="24k")
    try:
        assert out.endswith(".ogg") and os.path.isfile(out)
        assert open(out, "rb").read() == b"OggS"
    finally:
        os.unlink(out)


def test_convert_temp_cleans_up_on_failure(monkeypatch, tmp_path):
    created = []
    real_named = audio.tempfile.NamedTemporaryFile

    def tracking_named(*args, **kwargs):
        f = real_named(*args, **kwargs)
        created.append(f.name)
        return f

    def failing(cmd, **kwargs):
        raise subprocess.CalledProcessError(1, cmd, stderr="boom")

    monkeypatch.setattr(audio.tempfile, "NamedTemporaryFile", tracking_named)
    monkeypatch.setattr(audio.subprocess, "run", failing)
    src = tmp_path / "in.wav"
    src.write_bytes(b"x")

    with pytest.raises(RuntimeError, match="boom"):
        audio.convert_to_opus_ogg_temp(str(src))
    assert created and not os.path.exists(created[0])

    with pytest.raises(FileNotFoundError):
        audio.convert_to_opus_ogg_temp(str(tmp_path / "missing.wav"))
    assert not os.path.exists(created[1])
