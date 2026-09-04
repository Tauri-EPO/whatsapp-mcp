"""notes.db: agent notes keyed by content hash, visibility through the allow-list."""

import os
import sqlite3

import pytest

import chat_policy
import main
import media_inventory
import media_notes
import whatsapp
from tests.conftest import ALICE, BOB, FAMILY

SHA_A = "aa" * 32  # photo in Alice's chat and in Family
SHA_B = "bb" * 32  # video with Bob only
SHA_X = "ee" * 32  # nobody has this file


@pytest.fixture
def notes_store(paired_dbs):
    with paired_dbs.messages() as c:
        rows = [
            ("IMG1", ALICE, "image", "2026-09-01 10:00:00", 200_000, bytes.fromhex(SHA_A), None),
            ("IMG1F", FAMILY, "image", "2026-09-02 10:00:00", 200_000, bytes.fromhex(SHA_A), None),
            ("VID1", BOB, "video", "2026-09-03 10:00:00", 5_000_000, bytes.fromhex(SHA_B), None),
        ]
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256, filename) VALUES (?, ?, 'x', '', ?, 0, ?, ?, ?, ?)",
            [(i, ch, ts, mt, ln, sha, fn) for i, ch, mt, ts, ln, sha, fn in rows],
        )
    return paired_dbs


def test_notes_db_is_created_lazily_in_wal_mode(notes_store):
    path = media_notes.notes_db_path()
    assert os.path.dirname(path) == media_inventory.media_root()
    assert main.get_media_notes(SHA_A) == {
        "sha256": SHA_A,
        "notes": {},
        "messages": main.get_media_notes(SHA_A)["messages"],
    }
    assert not os.path.exists(path)  # reads never create the file
    assert main.search_media_notes("anything") == []

    out = main.annotate_media(SHA_A, "summary", "Family photo from the beach")
    assert (
        out["success"] and out["sha256"] == SHA_A and out["key"] == "summary" and out["updated_at"].endswith("+00:00")
    )
    assert os.path.exists(path)
    with sqlite3.connect(path) as c:
        assert c.execute("PRAGMA journal_mode").fetchone()[0] == "wal"
        assert c.execute("SELECT COUNT(*) FROM media_notes").fetchone()[0] == 1


def test_write_read_overwrite_delete(notes_store):
    main.annotate_media(SHA_A.upper(), " tags ", '["family","beach"]')
    main.annotate_media(SHA_A, "keep", "yes")
    got = main.get_media_notes(SHA_A)
    assert set(got["notes"]) == {"tags", "keep"}
    assert got["notes"]["tags"]["value"] == '["family","beach"]'
    assert [m["message_id"] for m in got["messages"]] == ["IMG1F", "IMG1"]  # newest first, both chats
    assert got["messages"][0]["chat_name"] == "Family" and got["messages"][0]["bytes"] == 200_000

    main.annotate_media(SHA_A, "keep", "no")
    assert main.get_media_notes(SHA_A)["notes"]["keep"]["value"] == "no"

    assert main.annotate_media(SHA_A, "keep", "")["deleted"] is True
    assert main.annotate_media(SHA_A, "keep", "   ")["deleted"] is False
    assert set(main.get_media_notes(SHA_A)["notes"]) == {"tags"}


def test_notes_show_inline_in_list_media(notes_store):
    main.annotate_media(SHA_B, "summary", "Birthday video")
    items = {i["message_id"]: i for i in main.list_media()["items"]}
    assert items["VID1"]["notes"] == {"summary": "Birthday video"}
    assert items["IMG1"]["notes"] == {} and items["IMG1F"]["notes"] == {}
    main.annotate_media(SHA_A, "keep", "yes")
    items = {i["message_id"]: i for i in main.list_media()["items"]}
    assert items["IMG1"]["notes"] == items["IMG1F"]["notes"] == {"keep": "yes"}  # one note per hash


def test_search_media_notes(notes_store):
    main.annotate_media(SHA_A, "summary", "Contrato de aluguel assinado")
    main.annotate_media(SHA_A, "tags", "contrato,imóvel")
    main.annotate_media(SHA_B, "summary", "Vídeo do aniversário")
    hits = main.search_media_notes("CONTRATO")
    assert {(h["sha256"], h["key"]) for h in hits} == {(SHA_A, "summary"), (SHA_A, "tags")}
    assert [h["key"] for h in main.search_media_notes("contrato", key="tags")] == ["tags"]
    assert main.search_media_notes("imóvel")[0]["value"] == "contrato,imóvel"
    assert main.search_media_notes("aniversário", limit=1)[0]["sha256"] == SHA_B
    assert main.search_media_notes("   ")["error"]["code"] == "invalid_argument"


def test_unknown_hash_and_bad_arguments(notes_store):
    assert main.annotate_media(SHA_X, "summary", "x")["error"]["code"] == "not_found"
    assert main.get_media_notes(SHA_X)["error"]["code"] == "not_found"
    assert main.annotate_media("not-a-hash", "summary", "x")["error"]["code"] == "invalid_argument"
    assert main.annotate_media(SHA_A, "", "x")["error"]["code"] == "invalid_argument"
    assert main.annotate_media(SHA_A, "k" * 65, "x")["error"]["code"] == "invalid_argument"
    too_big = "x" * (media_notes.MAX_VALUE_BYTES + 1)
    assert main.annotate_media(SHA_A, "transcript", too_big)["error"]["code"] == "invalid_argument"
    assert main.search_media_notes("x", key="") == []


def test_allow_list_hides_hashes_and_notes(notes_store, monkeypatch):
    # Notes written while everything was visible...
    main.annotate_media(SHA_A, "summary", "shared photo")
    main.annotate_media(SHA_B, "summary", "bob's video")

    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    for module in (whatsapp, media_inventory, media_notes):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)

    # ...are unreachable for hashes that only exist in denied chats: reported
    # as not_found, never as denied, so existence does not leak.
    assert main.get_media_notes(SHA_B)["error"]["code"] == "not_found"
    assert main.annotate_media(SHA_B, "keep", "no")["error"]["code"] == "not_found"
    assert [h["sha256"] for h in main.search_media_notes("video")] == []
    # The shared photo stays visible, but only its message in the allowed chat is listed.
    got = main.get_media_notes(SHA_A)
    assert got["notes"]["summary"]["value"] == "shared photo"
    assert [m["chat_jid"] for m in got["messages"]] == [ALICE]
    assert [h["sha256"] for h in main.search_media_notes("photo")] == [SHA_A]


def test_note_survives_purge_of_the_cached_file(notes_store):
    d = media_inventory.chat_media_dir(BOB)
    os.makedirs(d)
    cached = os.path.join(d, "video_20260903_100000_VID1.mp4")
    open(cached, "wb").write(b"v" * 10)
    main.annotate_media(SHA_B, "keep", "yes")
    assert main.list_media(chat_jid=BOB)["items"][0]["cached"] is True

    os.unlink(cached)  # what purge_media (#98) will do: bytes gone, row stays
    item = main.list_media(chat_jid=BOB)["items"][0]
    assert item["cached"] is False and item["sha256"] == SHA_B and item["notes"] == {"keep": "yes"}
    assert main.get_media_notes(SHA_B)["notes"]["keep"]["value"] == "yes"
