"""send_file / send_audio_message with media_base64: the bytes come in the call,
land under the outbox the bridge may read, go out as a media_path, and are
removed afterwards (issue #423)."""

from __future__ import annotations

import base64
import os
import subprocess

import pytest

import audio
import media_upload
import whatsapp
from errors import ToolError

ALICE = "12025551234@s.whatsapp.net"
PDF = b"%PDF-1.4 inline"
OGG = b"OggS" + bytes(64)


class DummyResponse:
    def __init__(self, payload=None):
        self.status_code = 200
        self._payload = payload or {"success": True, "message": "sent"}
        self.text = "OK"

    def json(self):
        return self._payload


@pytest.fixture
def outbox(monkeypatch, tmp_path):
    root = tmp_path / "outbox"
    root.mkdir()
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(root))
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")
    return root


@pytest.fixture
def bridge(monkeypatch):
    """Records each /send and what the file looked like while the bridge read it."""
    calls = []

    def fake_post(url, json, headers=None, timeout=None):
        path = json.get("media_path")
        content = open(path, "rb").read() if path and os.path.isfile(path) else None
        calls.append({"url": url, "json": json, "content": content})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    return calls


def _b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


# --- send_file ---------------------------------------------------------------


def test_send_file_inline_writes_under_the_outbox_sends_and_removes(outbox, bridge):
    success, _, _ = whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="report.pdf", caption="Q3")

    assert success is True
    assert len(bridge) == 1
    sent = bridge[0]["json"]
    assert sent["message"] == "Q3"
    assert os.path.basename(sent["media_path"]) == "report.pdf"  # the recipient sees this name
    assert os.path.dirname(os.path.dirname(sent["media_path"])) == str(outbox / ".uploads")
    assert bridge[0]["content"] == PDF  # complete while the bridge read it
    assert not os.path.exists(os.path.dirname(sent["media_path"]))  # gone afterwards
    assert os.listdir(outbox / ".uploads") == []


def test_send_file_inline_keeps_the_filename_but_not_its_path(outbox, bridge):
    whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="../../etc/passwd")
    whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="C:\\Users\\x\\rel: a?.pdf")

    assert os.path.basename(bridge[0]["json"]["media_path"]) == "passwd"
    assert os.path.basename(bridge[1]["json"]["media_path"]) == "rel_ a_.pdf"
    for call in bridge:
        assert call["json"]["media_path"].startswith(str(outbox / ".uploads"))


def test_send_file_inline_needs_a_filename(outbox, bridge):
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_file(ALICE, media_base64=_b64(PDF))
    assert excinfo.value.code == "invalid_argument"
    assert "filename" in str(excinfo.value)
    assert bridge == []


@pytest.mark.parametrize(
    "kwargs",
    [
        {},
        {"media_path": "/tmp/x.pdf", "media_base64": "QUJD", "filename": "x.pdf"},
    ],
)
def test_send_file_takes_exactly_one_source(outbox, bridge, kwargs):
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_file(ALICE, **kwargs)
    assert excinfo.value.code == "invalid_argument"
    assert bridge == []


@pytest.mark.parametrize("bad", ["", "   ", "not base64!!", "QUJ"])
def test_send_file_rejects_bad_base64_without_writing(outbox, bridge, bad):
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_file(ALICE, media_base64=bad, filename="x.pdf")
    assert excinfo.value.code == "invalid_argument"
    assert bridge == []
    assert not (outbox / ".uploads").exists()


def test_send_file_rejects_oversize_before_decoding(outbox, bridge, monkeypatch):
    monkeypatch.setattr(media_upload, "MAX_INLINE_BYTES", 16)
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_file(ALICE, media_base64=_b64(b"x" * 64), filename="x.bin")
    assert excinfo.value.code == "invalid_argument"
    assert "limit is 16" in str(excinfo.value)
    assert bridge == []


def test_send_file_inline_accepts_a_data_url(outbox, bridge):
    whatsapp.send_file(ALICE, media_base64="data:application/pdf;base64," + _b64(PDF), filename="a.pdf")
    assert bridge[0]["content"] == PDF


def test_send_file_inline_dry_run_reports_size_and_mime_and_writes_nothing(outbox, bridge):
    success, message, info = whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="report.pdf", dry_run=True)

    assert success is True
    assert info["dry_run"] is True
    assert info["media"] == {
        "filename": "report.pdf",
        "bytes": len(PDF),
        "mime": "application/pdf",
        "inline": True,
        "upload_dir": str(outbox / ".uploads"),
    }
    assert info["payload"]["recipient"] == ALICE
    assert info["payload"]["media_path"].endswith("report.pdf")
    assert bridge == []
    assert not (outbox / ".uploads").exists()


def test_send_file_inline_is_refused_for_a_chat_outside_the_allow_list(outbox, bridge, monkeypatch):
    from chat_policy import ChatPolicy

    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999@s.whatsapp.net"]))
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="x.pdf")
    assert excinfo.value.code == "denied"
    assert not (outbox / ".uploads").exists()


def test_send_file_inline_removes_the_upload_when_the_bridge_fails(outbox, monkeypatch):
    def failing_post(url, json, headers=None, timeout=None):
        return DummyResponse({"success": False, "message": "upload failed"})

    monkeypatch.setattr(whatsapp.bridge_http, "post", failing_post)
    with pytest.raises(ToolError):
        whatsapp.send_file(ALICE, media_base64=_b64(PDF), filename="x.pdf")
    assert os.listdir(outbox / ".uploads") == []


def test_send_file_by_path_is_unchanged(outbox, bridge, tmp_path):
    media = tmp_path / "report.pdf"
    media.write_bytes(PDF)

    whatsapp.send_file(ALICE, str(media), "Q3")

    assert bridge[0]["json"] == {"recipient": ALICE, "media_path": str(media), "message": "Q3"}
    assert media.is_file()  # a caller's file is never removed
    assert not (outbox / ".uploads").exists()


# --- send_audio_message ------------------------------------------------------


def test_send_audio_inline_ogg_goes_out_as_is(outbox, bridge):
    success, _, _ = whatsapp.send_audio_message(ALICE, media_base64=_b64(OGG))

    assert success is True
    sent = bridge[0]["json"]["media_path"]
    assert os.path.basename(sent) == "voice.ogg"
    assert bridge[0]["content"] == OGG
    assert os.listdir(outbox / ".uploads") == []


def test_send_audio_inline_other_formats_are_converted_in_the_outbox(outbox, bridge, monkeypatch):
    seen = {}

    def fake_convert(input_file, bitrate="32k", sample_rate=24000, directory=None):
        seen["input"] = input_file
        seen["directory"] = directory
        out = os.path.join(directory, "converted.ogg")
        with open(out, "wb") as handle:
            handle.write(OGG)
        return out

    monkeypatch.setattr(audio, "convert_to_opus_ogg_temp", fake_convert)

    whatsapp.send_audio_message(ALICE, media_base64=_b64(b"RIFF...."), filename="note.wav")

    assert os.path.basename(seen["input"]) == "note.wav"
    assert seen["directory"] == str(outbox / ".uploads")
    assert os.path.basename(bridge[0]["json"]["media_path"]) == "converted.ogg"
    assert bridge[0]["content"] == OGG
    assert os.listdir(outbox / ".uploads") == []  # the upload and the conversion are both gone


def test_send_audio_by_path_converts_into_the_outbox_and_keeps_the_source(outbox, bridge, monkeypatch, tmp_path):
    src = tmp_path / "in.wav"
    src.write_bytes(b"RIFF")
    seen = {}

    def fake_convert(input_file, bitrate="32k", sample_rate=24000, directory=None):
        seen["directory"] = directory
        os.makedirs(directory, exist_ok=True)
        out = os.path.join(directory, "converted.ogg")
        with open(out, "wb") as handle:
            handle.write(OGG)
        return out

    monkeypatch.setattr(audio, "convert_to_opus_ogg_temp", fake_convert)

    whatsapp.send_audio_message(ALICE, str(src))

    # The bridge only reads inside WHATSAPP_MEDIA_ROOTS, so the conversion must
    # land there rather than in the system temp directory.
    assert seen["directory"] == str(outbox / ".uploads")
    assert src.is_file()
    assert os.listdir(outbox / ".uploads") == []


def test_send_audio_takes_exactly_one_source(outbox, bridge):
    with pytest.raises(ToolError) as excinfo:
        whatsapp.send_audio_message(ALICE)
    assert excinfo.value.code == "invalid_argument"
    assert bridge == []


# --- media_upload helpers ----------------------------------------------------


def test_upload_dir_follows_the_first_media_root(monkeypatch, tmp_path):
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", os.pathsep.join([str(tmp_path / "a"), str(tmp_path / "b")]))
    assert media_upload.upload_dir() == str(tmp_path / "a" / ".uploads")


def test_upload_dir_defaults_to_the_bridge_outbox(monkeypatch):
    monkeypatch.delenv("WHATSAPP_MEDIA_ROOTS", raising=False)
    expected = os.path.join(os.path.expanduser("~"), ".local", "share", "whatsapp-mcp", "outbox", ".uploads")
    assert media_upload.upload_dir() == os.path.abspath(expected)


def test_discard_leaves_files_outside_the_upload_dir_alone(outbox, tmp_path):
    foreign = tmp_path / "keep.txt"
    foreign.write_text("x")
    media_upload.discard(str(foreign))
    assert foreign.is_file()


def test_audio_convert_temp_honours_the_directory(monkeypatch, tmp_path):
    calls = []

    def fake_run(cmd, **kwargs):
        calls.append(cmd)
        with open(cmd[-1], "wb") as handle:
            handle.write(OGG)
        return subprocess.CompletedProcess(cmd, 0, "", "")

    monkeypatch.setattr(audio.subprocess, "run", fake_run)
    src = tmp_path / "in.wav"
    src.write_bytes(b"x")
    target = tmp_path / "uploads"

    out = audio.convert_to_opus_ogg_temp(str(src), directory=str(target))

    assert os.path.dirname(out) == str(target)
    assert out.endswith(".ogg") and os.path.isfile(out)
