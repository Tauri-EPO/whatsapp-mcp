"""list_media / get_media_stats over a real store: sizes, copies, cache state, allow-list."""

import os

import pytest

import chat_policy
import main
import media_inventory
import whatsapp
from tests.conftest import ALICE, BOB, FAMILY

SHA_A = bytes.fromhex("aa" * 32)  # a photo forwarded into three chats
SHA_B = bytes.fromhex("bb" * 32)  # a big video, once
SHA_C = bytes.fromhex("cc" * 32)  # a document with the sender's filename


def _insert(c, msg_id, chat, media_type, ts, length, sha, filename=None, sender="5511888888888", deleted=None):
    c.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, "
        "file_length, file_sha256, deleted_at) VALUES (?, ?, ?, '', ?, 0, ?, ?, ?, ?, ?)",
        (msg_id, chat, sender, ts, media_type, filename, length, sha, deleted),
    )


@pytest.fixture
def media_store(paired_dbs):
    with paired_dbs.messages() as c:
        _insert(c, "IMG1", ALICE, "image", "2026-09-01 10:00:00", 200_000, SHA_A)
        _insert(c, "IMG1F", FAMILY, "image", "2026-09-02 10:00:00", 200_000, SHA_A)
        _insert(c, "IMG1G", FAMILY, "image", "2026-09-02 11:00:00", 200_000, SHA_A)
        _insert(c, "VID1", BOB, "video", "2026-09-03 10:00:00", 5_000_000, SHA_B)
        _insert(c, "DOC1", ALICE, "document", "2026-09-04 10:00:00", 50_000, SHA_C, filename="Report Q3.pdf")
        _insert(c, "GONE", ALICE, "audio", "2026-09-04 11:00:00", 9_000, None, deleted="2026-09-04 12:00:00")
        # never in the inventory: text, pointer rows
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('T1', ?, 'x', 'hi', '2026-09-04 09:00:00', 0)",
            (ALICE,),
        )
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, target_message_id) VALUES ('R1', ?, 'x', '👍', '2026-09-04 09:30:00', 0, 'reaction', 'IMG1')",
            (ALICE,),
        )
    # Cache: the photo in Alice's chat and the video are on disk; one stray .part file.
    alice_dir = media_inventory.chat_media_dir(ALICE)
    os.makedirs(alice_dir)
    (open(os.path.join(alice_dir, "image_20260901_100000_IMG1.jpg"), "wb")).write(b"x" * 1234)
    (open(os.path.join(alice_dir, "document_20260904_100000_DOC1.part"), "wb")).write(b"partial")
    bob_dir = media_inventory.chat_media_dir(BOB)
    os.makedirs(bob_dir)
    (open(os.path.join(bob_dir, "video_20260903_100000_VID1.mp4"), "wb")).write(b"v" * 4321)
    return paired_dbs


def _ids(page):
    return [i["message_id"] for i in page["items"]]


def test_size_sort_and_fields(media_store):
    page = main.list_media()
    assert _ids(page) == ["VID1", "IMG1G", "IMG1F", "IMG1", "DOC1", "GONE"]  # ties: newest first
    assert not page["has_more"] and page["next_cursor"] is None
    vid = page["items"][0]
    assert vid["bytes"] == 5_000_000 and vid["sha256"] == "bb" * 32 and vid["media_type"] == "video"
    assert vid["cached"] and vid["cached_bytes"] == 4321 and vid["cached_file"] == "video_20260903_100000_VID1.mp4"
    assert vid["chat_name"] == "Bob" and vid["copies"] == 1 and vid["copies_in"] == 1
    doc = next(i for i in page["items"] if i["message_id"] == "DOC1")
    assert doc["filename"] == "Report Q3.pdf" and not doc["cached"] and doc["cached_bytes"] is None
    gone = page["items"][-1]
    assert gone["sha256"] is None and gone["deleted_at"] == "2026-09-04T12:00:00" and gone["copies"] == 1


def test_copies_are_counted_across_chats(media_store):
    page = main.list_media(sort="copies")
    top = page["items"][0]
    assert top["sha256"] == "aa" * 32 and top["copies"] == 3 and top["copies_in"] == 2
    assert _ids(page)[:3] == ["IMG1G", "IMG1F", "IMG1"]  # same hash and size: newest first
    # Cached in Alice's chat only; the forwarded copies are not on disk.
    by_id = {i["message_id"]: i for i in page["items"]}
    assert by_id["IMG1"]["cached"] and not by_id["IMG1F"]["cached"] and not by_id["IMG1G"]["cached"]


def test_filters(media_store):
    assert _ids(main.list_media(chat_jid=FAMILY)) == ["IMG1G", "IMG1F"]
    assert _ids(main.list_media(media_type="document")) == ["DOC1"]
    assert _ids(main.list_media(min_bytes=100_000)) == ["VID1", "IMG1G", "IMG1F", "IMG1"]
    assert _ids(main.list_media(after="2026-09-03T00:00:00", sort="date")) == ["GONE", "DOC1", "VID1"]
    assert _ids(main.list_media(before="2026-09-01T23:59:59")) == ["IMG1"]
    # copies keep counting the whole archive even when the page is filtered
    assert main.list_media(chat_jid=FAMILY)["items"][0]["copies"] == 3


def test_pagination_cursor(media_store):
    first = main.list_media(limit=4)
    assert _ids(first) == ["VID1", "IMG1G", "IMG1F", "IMG1"] and first["has_more"]
    second = main.list_media(limit=4, cursor=first["next_cursor"])
    assert _ids(second) == ["DOC1", "GONE"] and not second["has_more"]
    assert _ids(main.list_media(limit=4, page=1)) == ["DOC1", "GONE"]
    assert main.list_media(cursor="nope")["error"]["code"] == "invalid_argument"


def test_invalid_arguments(media_store):
    assert main.list_media(sort="weight")["error"]["code"] == "invalid_argument"
    assert main.list_media(media_type="hologram")["error"]["code"] == "invalid_argument"
    assert main.list_media(after="yesterday")["error"]["code"] == "invalid_argument"


def test_allow_list(media_store, monkeypatch):
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(media_inventory, "CHAT_POLICY", policy)
    page = main.list_media(sort="copies")
    assert set(_ids(page)) == {"IMG1", "DOC1", "GONE"}
    assert page["items"][0]["copies"] == 1  # copies in denied chats are not revealed
    assert main.list_media(chat_jid=FAMILY)["error"]["code"] == "denied"
    assert main.get_media_stats(chat_jid=BOB)["error"]["code"] == "denied"
    stats = main.get_media_stats()
    assert [c["chat_jid"] for c in stats["by_chat"]] == [ALICE]


def test_media_stats(media_store):
    stats = main.get_media_stats()
    assert stats["total"] == {
        "files": 6,
        "bytes": 5_659_000,
        "cached_files": 2,
        "cached_bytes": 1234 + 4321,
        "duplicate_groups": 1,
        "duplicate_bytes": 400_000,
    }
    assert [c["chat_jid"] for c in stats["by_chat"]] == [BOB, FAMILY, ALICE]
    alice = stats["by_chat"][2]
    assert alice == {
        "chat_jid": ALICE,
        "chat_name": "Alice",
        "files": 3,
        "distinct_files": 2,
        "bytes": 259_000,
        "cached_files": 1,
        "cached_bytes": 1234,
    }
    assert stats["by_type"][0] == {"media_type": "video", "files": 1, "bytes": 5_000_000}
    assert stats["media_root"] == media_inventory.media_root()

    only_family = main.get_media_stats(chat_jid=FAMILY)
    assert only_family["total"]["files"] == 2 and only_family["by_chat"][0]["cached_files"] == 0


def test_scan_chat_cache_parses_bridge_filenames(tmp_path, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    d = media_inventory.chat_media_dir("5511999999999:12@s.whatsapp.net")
    assert d.endswith("5511999999999_12@s.whatsapp.net")
    os.makedirs(d)
    for name in (
        "image_20260904_150405_ABC.jpg",
        "document_20260904_150405_DEF",
        "document_20260904_150405_PDF.pdf",
        "audio_20260904_150405_GHI.ogg",
        "sticker_20260904_150405_JKL.webp",
        "notes.txt",
        "image_20260904_150405_MNO.jpg.part",
    ):
        open(os.path.join(d, name), "wb").write(b"1")
    os.makedirs(os.path.join(d, "image_20260904_150405_DIR.jpg"))
    found = media_inventory.scan_chat_cache("5511999999999:12@s.whatsapp.net")
    assert sorted(found) == ["ABC", "DEF", "GHI", "JKL", "PDF"]
    assert found["DEF"].name == "document_20260904_150405_DEF"  # legacy: no extension
    assert found["PDF"].name == "document_20260904_150405_PDF.pdf"
    assert media_inventory.scan_chat_cache("nobody@s.whatsapp.net") == {}


def test_list_messages_rows_carry_bytes_and_sha256(media_store):
    rows = whatsapp.list_messages(chat_jid=ALICE, include_context=False)
    by_id = {r["id"]: r for r in rows}
    assert by_id["DOC1"]["bytes"] == 50_000 and by_id["DOC1"]["sha256"] == "cc" * 32
    assert by_id["T1"]["bytes"] is None and by_id["T1"]["sha256"] is None
    assert by_id["R1"]["bytes"] is None and by_id["R1"]["sha256"] is None  # pointer row
    assert by_id["GONE"]["bytes"] == 9_000 and by_id["GONE"]["sha256"] is None
