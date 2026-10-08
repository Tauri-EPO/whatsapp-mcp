"""Manual tools and default ingest agree on dated, visible, clearable refusals."""

import sqlite3
from types import SimpleNamespace

import pytest

import chat_policy
import main
import media_inventory
import media_notes
import transcribe_worker
import whatsapp
from tests.conftest import ALICE, BOB
from tests.test_bridge_request import _Failed
from tests.test_transcribe_ingest import _add_audio

SHA = "aa" * 32


@pytest.fixture
def refused_archive(paired_dbs, monkeypatch):
    with paired_dbs.messages() as conn:
        for message_id, chat in [("BAD/ID", ALICE), ("SAFE1", ALICE), ("BAD/ID", BOB)]:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
                "file_length, file_sha256) VALUES (?, ?, 'x', '', '2026-09-04 10:00:00', 0, 'audio', 9, ?)",
                (message_id, chat, bytes.fromhex(SHA)),
            )
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http, "post", lambda *args, **kwargs: _Failed(500, "media_refused", "unsafe message identity")
    )
    return paired_dbs


@pytest.mark.parametrize("tool", ["download_media", "read_media", "transcribe_audio", "forward_message"])
def test_manual_refusal_leaves_default_ingest_backlog_and_can_be_cleared(refused_archive, tool):
    kwargs = {"chat_jid": ALICE, "message_id": "BAD/ID"}
    if tool == "forward_message":
        kwargs["to_chat_jid"] = BOB
    result = getattr(main, tool)(**kwargs)
    if tool == "read_media":
        result = result.structured_content
    assert result["error"]["code"] == "media_refused"
    assert transcribe_worker.run_once(10, fetch=False).transcribed == 0
    audio = whatsapp.coverage()["audio"]
    assert audio["refused"] == 1 and audio["backlog"] == 2
    items = {(item["chat_jid"], item["message_id"]): item for item in main.list_media()["items"]}
    refusal = items[ALICE, "BAD/ID"]["media_refusal"]
    assert refusal["reason"] == "unsafe message identity" and refusal["updated_at"].endswith("+00:00")
    assert "media_refusal" not in items[ALICE, "SAFE1"]
    assert "media_refusal" not in items[BOB, "BAD/ID"]
    notes = main.get_media_notes(SHA)
    assert notes["notes"] == {}
    assert sum("media_refusal" in message for message in notes["messages"]) == 1
    assert main.clear_media_refusal(ALICE, "BAD/ID")["deleted"]
    assert whatsapp.coverage()["audio"]["backlog"] == 3
    assert not main.clear_media_refusal(ALICE, "BAD/ID")["deleted"]


def test_clear_and_listing_respect_chat_policy(refused_archive, monkeypatch):
    media_notes.record_media_refusal("BAD/ID", BOB, "unsafe message identity")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([ALICE]))
    monkeypatch.setattr(media_notes, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([ALICE]))
    monkeypatch.setattr(media_inventory, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([ALICE]))
    assert main.clear_media_refusal(BOB, "BAD/ID")["error"]["code"] == "denied"
    assert main.clear_media_refusal(ALICE, "MISSING")["error"]["code"] == "not_found"
    assert all("media_refusal" not in item for item in main.list_media()["items"])
    assert all("media_refusal" not in item for item in main.get_media_notes(SHA)["messages"])
    assert (BOB, "BAD/ID") in media_notes.fetch_media_refusals([(BOB, "BAD/ID")])


def test_clear_validates_and_normalizes_its_arguments(refused_archive):
    for chat, message in [("", ""), (ALICE, " "), (" ", "SAFE1")]:
        assert main.clear_media_refusal(chat, message)["error"]["code"] == "invalid_argument"
    media_notes.record_media_refusal("SAFE1", ALICE, "unsafe message identity")
    assert main.clear_media_refusal(" " + ALICE + " ", " SAFE1 ")["deleted"]


def test_clear_prefers_the_exact_stored_identity(refused_archive):
    message_id = "\nBAD/ID\n"
    with refused_archive.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES (?, ?, 'x', '', '2026-09-04 10:00:00', 0)",
            (message_id, ALICE),
        )
    assert main.download_media(ALICE, message_id)["error"]["code"] == "media_refused"
    media_notes.record_media_refusal("BAD/ID", ALICE, "unsafe message identity")
    result = main.clear_media_refusal(ALICE, message_id)
    assert result["deleted"] and result["message_id"] == message_id
    assert set(media_notes.fetch_media_refusals([(ALICE, message_id), (ALICE, "BAD/ID")])) == {(ALICE, "BAD/ID")}


def test_successful_fetch_clears_only_its_refusal_and_cached_audio_is_transcribed(refused_archive, monkeypatch):
    assert main.download_media(ALICE, "SAFE1")["error"]["code"] == "media_refused"
    media_notes.record_media_refusal("BAD/ID", ALICE, "unsafe message identity")
    directory = refused_archive.messages_db.parent / ALICE
    directory.mkdir()
    cached = directory / "audio_20260904_100000_SAFE1.ogg"
    cached.write_bytes(b"opus")
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *args, **kwargs: SimpleNamespace(
            status_code=200, text="", json=lambda: {"success": True, "path": str(cached)}
        ),
    )
    assert main.download_media(ALICE, "SAFE1")["file_path"] == str(cached)
    seen = []

    def transcribe(path):
        seen.append(path)
        return {"text": "spoken words", "backend": "server"}

    assert transcribe_worker.run_once(10, fetch=False, transcribe=transcribe).transcribed == 1
    assert seen == [str(cached)]
    assert main.get_media_notes(SHA)["notes"]["transcript"]["value"] == "spoken words"
    items = {(item["chat_jid"], item["message_id"]): item for item in main.list_media()["items"]}
    assert items[ALICE, "SAFE1"]["cached"] and "media_refusal" not in items[ALICE, "SAFE1"]
    assert "media_refusal" in items[ALICE, "BAD/ID"]
    assert whatsapp.coverage()["audio"]["backlog"] == 0


@pytest.mark.parametrize("unwritable", [False, True])
def test_default_ingest_records_once_and_keeps_write_failures_bounded(paired_dbs, monkeypatch, unwritable):
    for message in ("AUD1", "AUD2", "AUD3", "AUD4"):
        _add_audio(paired_dbs, message, ALICE, cached=False)
    records, requests = [], []
    original = media_notes.record_media_refusal

    def record(message_id, chat_jid, reason):
        records.append((message_id, chat_jid))
        if unwritable or records.count((message_id, chat_jid)) > 1:
            raise sqlite3.OperationalError("database is locked")
        original(message_id, chat_jid, reason)

    def response(*args, **kwargs):
        requests.append(kwargs["json"]["message_id"])
        return _Failed(500, "media_refused", "unsafe message identity")

    monkeypatch.setattr(media_notes, "record_media_refusal", record)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(whatsapp.bridge_http, "post", response)
    transcribe_worker.find_pending(10, fetch=True)
    expected = transcribe_worker.MAX_FETCH_FAILURES if unwritable else 4
    assert len(records) == len(requests) == expected
    assert len(set(records)) == expected


def test_notes_enrichment_opens_one_connection_per_page_and_note_write(refused_archive, monkeypatch):
    media_notes.record_media_refusal("BAD/ID", ALICE, "unsafe message identity")
    original = media_notes._connect
    connections = []

    def connect(create):
        connections.append(create)
        return original(create)

    monkeypatch.setattr(media_notes, "_connect", connect)
    for operation in [
        lambda: main.annotate_media(SHA, "summary", "voice note"),
        main.list_media,
        lambda: main.get_media_notes(SHA),
    ]:
        connections.clear()
        assert "error" not in operation()
        assert len(connections) == 1
