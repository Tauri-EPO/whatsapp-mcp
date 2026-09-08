"""notes.db: agent notes keyed by content hash, visibility through the allow-list."""

import os
import sqlite3

import pytest

import chat_policy
import main
import media_inventory
import media_notes
import whatsapp
from errors import ToolError
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


def test_hash_lookups_use_the_bridge_index(notes_store, monkeypatch):
    """The bridge creates idx_messages_file_sha256; the note/inventory queries must be able to use it."""
    import re
    import sqlite3

    with notes_store.messages() as c:
        c.execute("CREATE INDEX idx_messages_file_sha256 ON messages(file_sha256) WHERE file_sha256 IS NOT NULL")
    executed = []
    real_connect = whatsapp._connect_messages_db

    def recording_connect():
        conn = real_connect()
        conn.set_trace_callback(executed.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", recording_connect)
    main.get_media_notes(SHA_A)
    media_notes.visible_hashes([SHA_A, SHA_B])
    main.list_media()
    # Only the queries that seek by hash: since issue #317 the page select
    # carries file_sha256 as a column and reads its rows by chat and time.
    hash_queries = [
        q
        for q in executed
        if q.lstrip().upper().startswith(("SELECT", "WITH")) and re.search(r"file_sha256 (=|IN \(|IS NOT NULL)", q)
    ]
    assert len(hash_queries) >= 3, executed
    with sqlite3.connect(notes_store.messages_db) as c:
        for q in hash_queries:
            plan = "\n".join(row[3] for row in c.execute("EXPLAIN QUERY PLAN " + q))
            assert "idx_messages_file_sha256" in plan, (q, plan)


# --- notes wherever media is surfaced (issue #222) -----------------------------


def _count_note_lookups(monkeypatch) -> list[list[str]]:
    """Record every fetch_notes call so a page can be shown to cost exactly one."""
    calls: list[list[str]] = []
    real = media_notes.fetch_notes

    def counting(hashes):
        calls.append(list(hashes))
        return real(hashes)

    monkeypatch.setattr(media_notes, "fetch_notes", counting)
    return calls


def _add_text_message(store, msg_id: str = "TXT1") -> None:
    with store.messages() as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES (?, ?, 'x', 'hello', '2026-09-04 10:00:00', 0)",
            (msg_id, ALICE),
        )


def test_list_messages_returns_notes_for_media_rows_in_one_query(notes_store, monkeypatch):
    _add_text_message(notes_store)
    main.annotate_media(SHA_A, "summary", "Family photo from the beach")

    calls = _count_note_lookups(monkeypatch)
    rows = {r["id"]: r for r in main.list_messages(include_context=False, limit=50)["items"]}

    assert len(calls) == 1  # one batched lookup per page, never one per row
    assert set(calls[0]) == {SHA_A, SHA_B}
    assert rows["IMG1"]["notes"] == rows["IMG1F"]["notes"] == {"summary": "Family photo from the beach"}
    assert rows["VID1"]["notes"] == {}  # media nobody has interpreted yet
    assert "notes" not in rows["TXT1"]  # text rows stay untouched


def test_message_context_returns_notes_in_one_query(notes_store, monkeypatch):
    main.annotate_media(SHA_A, "summary", "Family photo")
    calls = _count_note_lookups(monkeypatch)

    context = main.get_message_context(ALICE, "IMG1", before=1, after=1)

    assert len(calls) == 1
    assert context["message"]["notes"] == {"summary": "Family photo"}


def test_list_unread_returns_notes_in_one_query(notes_store, monkeypatch):
    main.annotate_media(SHA_B, "summary", "Birthday video")
    calls = _count_note_lookups(monkeypatch)

    rows = {m["id"]: m for chat in main.list_unread()["chats"] for m in chat["messages"]}

    assert len(calls) == 1
    assert rows["VID1"]["notes"] == {"summary": "Birthday video"}
    assert rows["IMG1"]["notes"] == {}


def test_download_media_returns_the_hash_and_its_notes(notes_store, monkeypatch):
    _add_text_message(notes_store)
    main.annotate_media(SHA_A, "summary", "Family photo")
    monkeypatch.setattr(main, "whatsapp_download_media", lambda mid, chat: f"/store/{chat}/{mid}.jpg")

    out = main.download_media(ALICE, "IMG1")
    assert out["success"] and out["file_path"].endswith("IMG1.jpg")
    assert out["sha256"] == SHA_A and out["notes"] == {"summary": "Family photo"}

    # A row without a hash still answers, with nothing to remember.
    assert main.download_media(ALICE, "TXT1")["sha256"] is None
    assert main.download_media(ALICE, "TXT1")["notes"] == {}


def test_list_media_has_notes_flag_and_filter(notes_store):
    # No notes.db yet: nothing is annotated, everything is backlog.
    assert all(item["has_notes"] is False for item in main.list_media()["items"])
    assert main.list_media(has_notes=True)["items"] == []
    assert len(main.list_media(has_notes=False)["items"]) == 3

    main.annotate_media(SHA_A, "summary", "Family photo")

    annotated = main.list_media(has_notes=True)["items"]
    assert {item["message_id"] for item in annotated} == {"IMG1", "IMG1F"}  # one note, both copies
    assert all(item["has_notes"] is True for item in annotated)
    backlog = main.list_media(has_notes=False)["items"]
    assert [item["message_id"] for item in backlog] == ["VID1"]
    assert backlog[0]["has_notes"] is False


def _bulk_notes(store, count, visible):
    """``count`` matching notes on distinct hashes, oldest first; ``visible`` of them carry a message.

    Written straight into the two databases: the point of these tests is what
    one search does with a large archive, not how it got there.
    """
    hashes = [f"{i:064x}" for i in range(count)]
    with store.messages() as c:
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256) VALUES (?, ?, 'x', '', '2026-09-01 10:00:00', 0, 'image', 10, ?)",
            [(f"M{i}", ALICE, bytes.fromhex(sha)) for i, sha in enumerate(hashes) if i in visible],
        )
    conn = media_notes._connect(create=True)
    assert conn is not None
    try:
        conn.executemany(
            "INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, 'summary', 'common note', ?)",
            # Zero-padded so the string order the query sorts by is the index
            # order: hash 0 is the oldest note, hence the last one examined.
            [(sha, f"2026-09-08T00:00:00.{i:06d}+00:00") for i, sha in enumerate(hashes)],
        )
        conn.commit()
    finally:
        conn.close()
    return hashes


def _visibility_spy(monkeypatch):
    """Every ``visible_hashes`` call, as the number of hashes it was asked about."""
    sizes: list[int] = []
    original = media_notes.visible_hashes

    def spy(hashes):
        sizes.append(len(hashes))
        return original(hashes)

    monkeypatch.setattr(media_notes, "visible_hashes", spy)
    return sizes


def test_search_asks_about_hashes_in_bounded_batches(notes_store, monkeypatch):
    """More distinct matching hashes than messages.db will bind at once, still one answer."""
    hashes = _bulk_notes(notes_store, 1200, visible={0})
    # The bundled SQLite binds 32,766 variables, so reproducing the production
    # failure faithfully would take 32,767 notes. Lowering that ceiling makes
    # 1,200 hashes in one IN clause the same overflow, at 1/27 of the fixture.
    connect = whatsapp._connect_messages_db

    def tight_connect():
        conn = connect()
        conn.setlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, media_notes.SEARCH_BATCH + 10)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", tight_connect)
    # What the old single-IN-clause search did with every matching hash at once.
    with pytest.raises(ToolError, match="too many SQL variables"):
        media_notes.visible_hashes(hashes)

    sizes = _visibility_spy(monkeypatch)
    # The only visible hash is the oldest note, so the walk exhausts the matches.
    assert [hit["sha256"] for hit in main.search_media_notes("common note", limit=1)] == [hashes[0]]
    assert sum(sizes) == 1200  # every match examined...
    assert max(sizes) <= media_notes.SEARCH_BATCH  # ...never in one IN clause
    assert len(sizes) == 1200 // media_notes.SEARCH_BATCH


def test_a_small_limit_stops_after_one_batch(notes_store, monkeypatch):
    """1,000 notes, all visible, limit=1: one batch examined, one hash question."""
    hashes = _bulk_notes(notes_store, 1000, visible=set(range(1000)))
    sizes = _visibility_spy(monkeypatch)

    hits = main.search_media_notes("common note", limit=1)
    assert [hit["sha256"] for hit in hits] == [hashes[-1]]  # newest note
    assert sizes == [media_notes.SEARCH_BATCH]
    assert len(main.search_media_notes("common note", limit=200)) == 200


def test_the_batch_size_only_changes_how_many_rounds_it_takes(notes_store, monkeypatch):
    """A hidden run ahead of the match is walked through, whatever the batch size."""
    monkeypatch.setattr(media_notes, "SEARCH_BATCH", 2)
    hashes = _bulk_notes(notes_store, 5, visible={0})
    sizes = _visibility_spy(monkeypatch)

    assert [hit["sha256"] for hit in main.search_media_notes("common note", limit=1)] == [hashes[0]]
    assert sizes == [2, 2, 1]
