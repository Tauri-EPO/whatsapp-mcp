"""The transcribe_audio MCP tool: argument validation, download step, error mapping."""

import main
from transcribe import TranscriptionError


def _config(monkeypatch):
    monkeypatch.setattr(main, "load_whisper_config", lambda: "cfg")


def test_requires_file_path_or_message_reference(monkeypatch):
    _config(monkeypatch)
    out = main.transcribe_audio()
    assert out["error"]["code"] == "invalid_argument"
    assert main.transcribe_audio(chat_jid="c@s.whatsapp.net")["error"]["code"] == "invalid_argument"
    assert main.transcribe_audio(message_id="m1")["error"]["code"] == "invalid_argument"


def test_downloads_via_bridge_then_transcribes(monkeypatch):
    _config(monkeypatch)
    seen = {}
    monkeypatch.setattr(
        main, "whatsapp_download_media", lambda mid, chat: seen.setdefault("dl", (mid, chat)) and "/store/c/audio.ogg"
    )

    def fake_transcribe(path, language=None, config=None):
        seen["tx"] = (path, language, config)
        return {"text": "olá", "language": "pt", "backend": "server"}

    monkeypatch.setattr(main, "transcribe_file", fake_transcribe)

    out = main.transcribe_audio(chat_jid="c@s.whatsapp.net", message_id="m1", language="pt")
    assert out == {
        "success": True,
        "file_path": "/store/c/audio.ogg",
        "text": "olá",
        "language": "pt",
        "backend": "server",
    }
    assert seen["dl"] == ("m1", "c@s.whatsapp.net")
    assert seen["tx"] == ("/store/c/audio.ogg", "pt", "cfg")


def test_file_path_skips_the_bridge_and_defaults_language(monkeypatch):
    _config(monkeypatch)
    monkeypatch.setattr(
        main, "whatsapp_download_media", lambda *a: (_ for _ in ()).throw(AssertionError("must not download"))
    )
    monkeypatch.setattr(
        main, "transcribe_file", lambda path, language=None, config=None: {"text": "x", "language": language}
    )
    out = main.transcribe_audio(file_path="/tmp/a.ogg")
    assert out["success"] and out["file_path"] == "/tmp/a.ogg" and out["language"] is None


def test_download_failure_is_internal_error(monkeypatch):
    _config(monkeypatch)
    monkeypatch.setattr(main, "whatsapp_download_media", lambda mid, chat: None)
    out = main.transcribe_audio(chat_jid="c@s.whatsapp.net", message_id="m1")
    assert out["error"]["code"] == "internal" and "download" in out["error"]["message"].lower()


def test_transcription_errors_map_to_codes(monkeypatch):
    _config(monkeypatch)

    def missing(path, language=None, config=None):
        raise FileNotFoundError(f"Audio file not found: {path}")

    monkeypatch.setattr(main, "transcribe_file", missing)
    out = main.transcribe_audio(file_path="/tmp/gone.ogg")
    assert out["error"]["code"] == "not_found" and out["file_path"] == "/tmp/gone.ogg"

    def broken(path, language=None, config=None):
        raise TranscriptionError("No whisper backend configured")

    monkeypatch.setattr(main, "transcribe_file", broken)
    out = main.transcribe_audio(file_path="/tmp/a.ogg")
    assert out["error"]["code"] == "internal" and "whisper backend" in out["error"]["message"]
