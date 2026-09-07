"""transcribe_audio caches into notes.db; list_messages reads the transcripts back."""

import os

import pytest

import main
import media_inventory
import media_notes
from tests.conftest import ALICE

SHA_AUDIO = "cc" * 32


@pytest.fixture
def audio_store(paired_dbs):
    """One inbound voice note in Alice's chat, with the hash the bridge would store."""
    with paired_dbs.messages() as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256, filename) "
            "VALUES ('AUD1', ?, ?, '', '2026-09-05 09:00:00', 0, 'audio', 30000, ?, ?)",
            (ALICE, ALICE, bytes.fromhex(SHA_AUDIO), "audio_20260905_090000_AUD1.ogg"),
        )
    return paired_dbs


def _fake_whisper(monkeypatch) -> dict[str, list]:
    """Count every download and whisper run so a cache hit can be shown to do neither."""
    seen: dict[str, list] = {"downloads": [], "runs": []}
    monkeypatch.setattr(main, "load_whisper_config", lambda: "cfg")

    def download(message_id, chat_jid):
        seen["downloads"].append((message_id, chat_jid))
        return f"/store/{chat_jid}/{message_id}.ogg"

    def transcribe(path, language=None, config=None):
        seen["runs"].append(path)
        return {"text": f"take {len(seen['runs'])}", "language": "pt", "backend": "server"}

    monkeypatch.setattr(main, "whatsapp_download_media", download)
    monkeypatch.setattr(main, "transcribe_file", transcribe)
    return seen


def test_transcript_is_stored_then_served_from_the_cache(audio_store, monkeypatch):
    seen = _fake_whisper(monkeypatch)

    first = main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")
    assert first["text"] == "take 1" and first["cached"] is False and first["stored"] is True
    assert first["sha256"] == SHA_AUDIO and first["file_path"].endswith("AUD1.ogg")
    assert media_notes.fetch_notes([SHA_AUDIO])[SHA_AUDIO] == {
        "transcript": "take 1",
        "transcript_lang": "pt",
        "transcript_backend": "server",
    }

    second = main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")
    assert second["cached"] is True and second["stored"] is False
    assert (second["text"], second["language"], second["backend"]) == ("take 1", "pt", "server")
    assert seen["runs"] == [f"/store/{ALICE}/AUD1.ogg"]  # whisper ran once
    assert len(seen["downloads"]) == 1  # and the file was fetched once

    forced = main.transcribe_audio(chat_jid=ALICE, message_id="AUD1", force=True)
    assert forced["cached"] is False and forced["text"] == "take 2" and len(seen["runs"]) == 2
    assert media_notes.fetch_notes([SHA_AUDIO])[SHA_AUDIO]["transcript"] == "take 2"


def test_transcript_of_a_bare_file_path_is_not_cached(audio_store, monkeypatch):
    _fake_whisper(monkeypatch)
    out = main.transcribe_audio(file_path="/tmp/voice.ogg")
    assert out["sha256"] is None and out["stored"] is False and out["cached"] is False
    assert media_notes.fetch_notes([SHA_AUDIO]) == {}


def test_list_messages_include_transcripts_never_transcribes(audio_store, monkeypatch):
    _fake_whisper(monkeypatch)
    main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")

    def explode(*args, **kwargs):
        raise AssertionError("list_messages must never transcribe")

    monkeypatch.setattr(main, "transcribe_file", explode)
    monkeypatch.setattr(main, "whatsapp_download_media", explode)

    rows = {r["id"]: r for r in main.list_messages(include_context=False, include_transcripts=True)["items"]}
    assert rows["AUD1"]["transcript"] == "take 1"

    off = {r["id"]: r for r in main.list_messages(include_context=False)["items"]}
    assert "transcript" not in off["AUD1"]  # opt-in only
    assert off["AUD1"]["notes"]["transcript"] == "take 1"  # still visible as a note


def test_transcript_survives_the_cached_bytes_being_purged(audio_store, monkeypatch):
    _fake_whisper(monkeypatch)
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory)
    cached = os.path.join(directory, "audio_20260905_090000_AUD1.ogg")
    with open(cached, "wb") as fh:
        fh.write(b"o" * 10)
    main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")

    os.unlink(cached)  # what purge_media does: bytes gone, message row and notes stay

    item = main.list_media(chat_jid=ALICE)["items"][0]
    assert item["cached"] is False and item["notes"]["transcript"] == "take 1"
    assert main.get_media_notes(SHA_AUDIO)["notes"]["transcript"]["value"] == "take 1"
    assert main.transcribe_audio(chat_jid=ALICE, message_id="AUD1")["cached"] is True
