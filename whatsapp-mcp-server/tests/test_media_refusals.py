"""Manual tools and default ingest agree on dated, visible, clearable refusals."""

import sqlite3

import pytest

import chat_policy
import main
import media_inventory
import media_notes
import transcribe_worker
import whatsapp
from tests.conftest import ALICE, BOB
from tests.test_bridge_request import _Failed

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


def test_an_undated_refusal_store_migrates_once(refused_archive):
    with sqlite3.connect(media_notes.notes_db_path()) as conn:
        conn.execute(
            "CREATE TABLE media_refusals (chat_jid TEXT, message_id TEXT, reason TEXT, PRIMARY KEY(chat_jid, message_id))"
        )
        conn.execute("INSERT INTO media_refusals VALUES (?, 'BAD/ID', 'unsafe message identity')", (ALICE,))
    first = media_notes.fetch_media_refusals([(ALICE, "BAD/ID")])
    assert first[ALICE, "BAD/ID"]["updated_at"].endswith("+00:00")
    assert media_notes.fetch_media_refusals([(ALICE, "BAD/ID")]) == first
