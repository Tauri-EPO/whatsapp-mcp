"""list_media / get_media_stats over a real store: sizes, copies, cache state, allow-list."""

import os
import sqlite3
from datetime import datetime

import pytest

import chat_policy
import main
import media_inventory
import whatsapp
from tests.conftest import ALICE, BOB, FAMILY, MESSAGES_SCHEMA

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
        _insert(c, "IMG1", ALICE, "image", "2026-09-01 10:00:00+00:00", 200_000, SHA_A)
        _insert(c, "IMG1F", FAMILY, "image", "2026-09-02 10:00:00+00:00", 200_000, SHA_A)
        _insert(c, "IMG1G", FAMILY, "image", "2026-09-02 11:00:00+00:00", 200_000, SHA_A)
        _insert(c, "VID1", BOB, "video", "2026-09-03 10:00:00+00:00", 5_000_000, SHA_B)
        _insert(c, "DOC1", ALICE, "document", "2026-09-04 10:00:00+00:00", 50_000, SHA_C, filename="Report Q3.pdf")
        _insert(
            c, "GONE", ALICE, "audio", "2026-09-04 11:00:00+00:00", 9_000, None, deleted="2026-09-04 12:00:00+00:00"
        )
        # never in the inventory: text, pointer rows
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('T1', ?, 'x', 'hi', '2026-09-04 09:00:00+00:00', 0)",
            (ALICE,),
        )
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, target_message_id) VALUES ('R1', ?, 'x', '👍', '2026-09-04 09:30:00+00:00', 0, 'reaction', 'IMG1')",
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
    assert gone["sha256"] is None and gone["deleted_at"] == "2026-09-04T12:00:00+00:00" and gone["copies"] == 1


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


def test_time_bounds_ignore_the_stored_format(media_store):
    """Rows rewritten in the RFC 3339 "T" spelling answer the same bound (#253).

    Membership only: ORDER BY still reads the raw column, so two spellings of
    the same day sort as "T" > " " rather than by instant.
    """
    with media_store.messages() as c:
        c.execute("UPDATE messages SET timestamp = replace(timestamp, ' ', 'T') WHERE id IN ('VID1', 'DOC1')")
    assert sorted(_ids(main.list_media(after="2026-09-03T00:00:00"))) == ["DOC1", "GONE", "VID1"]
    assert _ids(main.list_media(before="2026-09-01T23:59:59")) == ["IMG1"]
    # An offset-aware bound names the same instant as the local one it came from.
    local = datetime(2026, 9, 3).astimezone().isoformat()
    assert sorted(_ids(main.list_media(after=local))) == ["DOC1", "GONE", "VID1"]


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


# --- copy counts are computed for the page, not for the archive (issue #317) ---


def _counts(page):
    return {i["message_id"]: (i["copies"], i["copies_in"]) for i in page["items"]}


@pytest.mark.parametrize(
    "kwargs",
    [
        {},
        {"chat_jid": FAMILY},
        {"chat_jid": [ALICE, FAMILY], "exclude_chat_jid": FAMILY},
        {"exclude_chat_jid": [FAMILY, BOB]},
        {"media_type": "image"},
        {"min_bytes": 100_000},
        {"after": "2026-09-02T00:00:00"},
        {"before": "2026-09-02T23:59:59"},
    ],
)
def test_page_first_counts_match_the_archive_wide_aggregate(media_store, kwargs):
    """The two paths must answer the same numbers.

    Sorting by copies still groups the whole visible archive in SQL; the date
    and size sorts select the page first and look its hashes up afterwards. The
    same request under the three sorts returns the same rows, so their counts
    are directly comparable.
    """
    reference = _counts(main.list_media(sort="copies", **kwargs))
    assert _counts(main.list_media(sort="date", **kwargs)) == reference
    assert _counts(main.list_media(sort="size", **kwargs)) == reference


def test_counts_stay_global_on_a_narrow_page(media_store):
    # One row of one chat, and both copies of that photo live in another chat.
    assert _counts(main.list_media(chat_jid=ALICE, media_type="image", sort="date")) == {"IMG1": (3, 2)}
    # A page of one, walked with the cursor: a row without a hash is its own copy.
    first = main.list_media(limit=1, sort="date")
    assert _counts(first) == {"GONE": (1, 1)} and first["has_more"]
    second = main.list_media(limit=1, sort="date", cursor=first["next_cursor"])
    assert _counts(second) == {"DOC1": (1, 1)}
    third = main.list_media(limit=1, sort="size")
    assert _counts(third) == {"VID1": (1, 1)}


def test_allow_list_hides_copies_in_denied_chats_on_every_sort(media_store, monkeypatch):
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(media_inventory, "CHAT_POLICY", policy)
    for sort in media_inventory.SORTS:
        assert _counts(main.list_media(sort=sort))["IMG1"] == (1, 1), sort


def _statements(monkeypatch, **kwargs) -> list[str]:
    """The SELECTs one list_media_page call sends to messages.db."""
    executed: list[str] = []
    real = whatsapp._connect_messages_db

    def connect() -> sqlite3.Connection:
        conn = real()
        conn.set_trace_callback(executed.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    try:
        media_inventory.list_media_page(**kwargs)
    finally:
        monkeypatch.setattr(whatsapp, "_connect_messages_db", real)
    return [q for q in executed if q.lstrip().upper().startswith(("SELECT", "WITH"))]


@pytest.mark.parametrize(("sort", "materialises"), [("size", False), ("date", False), ("copies", True)])
def test_only_the_copies_sort_materialises_the_archive_wide_aggregate(media_store, monkeypatch, sort, materialises):
    """What the call actually runs, planned: the CTE is gone from the other two sorts."""
    with media_store.messages() as c:  # the hash index the bridge creates (store.go)
        c.execute("CREATE INDEX idx_messages_file_sha256 ON messages(file_sha256) WHERE file_sha256 IS NOT NULL")
    statements = _statements(monkeypatch, chat_jid=ALICE, sort=sort)
    assert len(statements) == (1 if materialises else 2)  # page (+ the counts for its hashes)
    with media_store.messages() as c:
        # EXPLAIN with the parameters left unbound: the plan does not depend on them.
        plans = "\n".join("\n".join(row[3] for row in c.execute("EXPLAIN QUERY PLAN " + q)) for q in statements)
    assert ("MATERIALIZE" in plans.upper()) is materialises, plans
    if not materialises:
        assert "idx_messages_file_sha256" in plans  # the counts are seeked, not grouped


# --- work bound (issue #317) --------------------------------------------------
#
# The page used to join a CTE grouping every visible media row in the store, so
# one row of one chat paid for the whole archive. The guard: with the page fixed,
# unrelated hashes elsewhere must not move the VM instructions the call spends.

TARGET = "5511555555555@s.whatsapp.net"
NOISE = "5511444444444@s.whatsapp.net"


def _archive_db(path, noise_rows: int) -> None:
    """Ten media rows in the target chat, `noise_rows` unique hashes in another one."""
    conn = sqlite3.connect(path)
    conn.executescript(MESSAGES_SCHEMA)
    # The two indexes the bridge creates (store.go) and that this query needs:
    # one to seek the page, one to count the copies of its hashes.
    conn.execute("CREATE INDEX idx_messages_chat_timestamp ON messages(chat_jid, timestamp)")
    conn.execute("CREATE INDEX idx_messages_file_sha256 ON messages(file_sha256) WHERE file_sha256 IS NOT NULL")
    conn.executemany("INSERT INTO chats (jid, name) VALUES (?, ?)", [(TARGET, "Target"), (NOISE, "Noise")])
    insert = (
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_length, "
        "file_sha256) VALUES (?, ?, 's', '', ?, 0, 'image', 1000, ?)"
    )
    conn.executemany(
        insert,
        [(f"t{i}", TARGET, f"2026-09-01 10:{i:02d}:00+00:00", i.to_bytes(32, "big")) for i in range(10)],
    )
    conn.executemany(
        insert,
        [(f"n{i}", NOISE, "2026-08-01 10:00:00+00:00", (1000 + i).to_bytes(32, "big")) for i in range(noise_rows)],
    )
    # No ANALYZE: the bridge never runs it, so a real store has no sqlite_stat1
    # either and the planner works from the same defaults as here.
    conn.commit()
    conn.close()


def _page_instructions(path, monkeypatch, sort: str) -> tuple[int, list[dict]]:
    """VM instructions one page of the target chat costs, and the page."""
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    counted = 0

    def tick() -> int:
        nonlocal counted
        counted += 1
        return 0

    real = whatsapp._connect_messages_db

    def connect() -> sqlite3.Connection:
        conn = real()
        # The handler fires every 1000 VM instructions; counting the calls is
        # the portable stand-in for sqlite3_stmt_status().
        conn.set_progress_handler(tick, 1000)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    try:
        page = media_inventory.list_media_page(chat_jid=TARGET, limit=1, sort=sort)
    finally:
        monkeypatch.setattr(whatsapp, "_connect_messages_db", real)
    return counted * 1000, page.items


@pytest.mark.parametrize("sort", ["date", "size"])
def test_a_small_page_does_not_aggregate_the_whole_archive(tmp_path, monkeypatch, sort):
    small, large = tmp_path / "small.db", tmp_path / "large.db"
    _archive_db(small, 1_000)
    _archive_db(large, 50_000)
    small_work, small_items = _page_instructions(small, monkeypatch, sort)
    large_work, large_items = _page_instructions(large, monkeypatch, sort)
    # Same page, same counts, whatever the rest of the store holds.
    assert [i["message_id"] for i in small_items] == [i["message_id"] for i in large_items] == ["t9"]
    assert [(i["copies"], i["copies_in"]) for i in large_items] == [(1, 1)]
    assert large_work <= max(small_work, 20_000) * 2, f"{small_work} -> {large_work} VM instructions"


# --- one message, one lookup; one page, one directory read (issue #318) -------
#
# The cache lookup used to build the chat's whole map — a stat per file the chat
# ever received — for every single message and again after every fetched file.
# What a listing needs is the names (one directory read, reused across pages)
# plus one stat per row of the page; what a single message needs is the name.


class _CountingScanner:
    """Stands in for os.scandir over fixed names, counting what the caller stats."""

    def __init__(self, names) -> None:
        self.names = list(names)
        self.stats: list[str] = []

    def __call__(self, path):
        scanner = self

        class Entry:
            def __init__(self, name: str) -> None:
                self.name = name

            def is_file(self) -> bool:
                return True

            def stat(self):
                scanner.stats.append(self.name)
                return os.stat_result((0o100644, 0, 0, 1, 0, 0, len(self.name), 0, 0, 0))

        class Listing:
            def __enter__(self):
                return iter([Entry(name) for name in scanner.names])

            def __exit__(self, *exc) -> bool:
                return False

        return Listing()


def _record_stats(monkeypatch) -> list[str]:
    """Every path os.stat is asked about while the test runs."""
    seen: list[str] = []
    real = os.stat

    def recording(path, *args, **kwargs):
        seen.append(str(path))
        return real(path, *args, **kwargs)

    monkeypatch.setattr(media_inventory.os, "stat", recording)
    return seen


def test_lookup_cached_name_answers_like_the_whole_scan(tmp_path, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    d = media_inventory.chat_media_dir(ALICE)
    os.makedirs(d)
    for name in (
        "image_20260904_150405_ABC.jpg",
        "document_20260904_150405_DEF",  # legacy: no extension
        "audio_20260904_150405_GHI.ogg",
        "notes.txt",
        "image_20260904_150405_PART.jpg.part",
    ):
        open(os.path.join(d, name), "wb").write(b"12345")
    os.makedirs(os.path.join(d, "image_20260904_150405_DIR.jpg"))
    scan = media_inventory.scan_chat_cache(ALICE)
    for message_id in ("ABC", "DEF", "GHI", "PART", "DIR", "notes", "nope"):
        found = media_inventory.lookup_cached_name(ALICE, message_id)
        assert found == (scan[message_id].name if message_id in scan else None), message_id
    assert media_inventory.lookup_cached_name("nobody@s.whatsapp.net", "ABC") is None


def test_one_message_is_looked_up_without_stat_ing_the_chat(media_store, monkeypatch):
    scanner = _CountingScanner(f"audio_20260905_090000_M{i}.ogg" for i in range(1_000))
    monkeypatch.setattr(media_inventory.os, "scandir", scanner)
    assert media_inventory.lookup_cached_name(ALICE, "M998") == "audio_20260905_090000_M998.ogg"
    assert media_inventory.lookup_cached_name(ALICE, "M0") == "audio_20260905_090000_M0.ogg"
    assert scanner.stats == []  # two lookups, no file stat-ed
    assert len(media_inventory.scan_chat_cache(ALICE)) == 1_000
    assert len(scanner.stats) == 1_000  # what the map costs, and why a lookup does not build one


def test_a_page_stats_its_own_rows_and_nothing_else(media_store, monkeypatch):
    cached_name = "image_20260901_100000_IMG1.jpg"
    scanner = _CountingScanner([cached_name, *(f"image_20260901_100000_M{i}.jpg" for i in range(1_000))])
    monkeypatch.setattr(media_inventory.os, "scandir", scanner)
    stats = _record_stats(monkeypatch)
    page = main.list_media(chat_jid=ALICE, media_type="image", sort="date")
    assert [i["message_id"] for i in page["items"]] == ["IMG1"]
    assert page["items"][0]["cached"] and page["items"][0]["cached_bytes"] == 1234
    chat_dir = media_inventory.chat_media_dir(ALICE)
    assert [p for p in stats if p.startswith(chat_dir + os.sep)] == [os.path.join(chat_dir, cached_name)]
    assert scanner.stats == []  # the listing never stats a directory entry


def _count_listings(monkeypatch) -> list[str]:
    listed: list[str] = []
    real = media_inventory.list_chat_names

    def counting(chat_jid: str) -> dict[str, str]:
        listed.append(chat_jid)
        return real(chat_jid)

    monkeypatch.setattr(media_inventory, "list_chat_names", counting)
    return listed


def test_repeated_pages_reuse_one_directory_read_per_chat(media_store, monkeypatch):
    listed = _count_listings(monkeypatch)
    for _ in range(3):
        assert _ids(main.list_media(chat_jid=ALICE)) == ["IMG1", "DOC1", "GONE"]
    assert listed == [ALICE]


def test_the_memo_never_claims_a_deleted_file_is_readable(media_store, monkeypatch):
    # Pinned mtime: the memo is reused even though the directory changed, which
    # is the worst case the TTL allows. The stat per row is what answers.
    monkeypatch.setattr(media_inventory, "_dir_mtime_ns", lambda chat_jid: 1)
    assert main.list_media(chat_jid=ALICE)["items"][-1]["message_id"] == "GONE"
    by_id = {i["message_id"]: i for i in main.list_media(chat_jid=ALICE)["items"]}
    assert by_id["IMG1"]["cached"] and by_id["IMG1"]["cached_bytes"] == 1234
    os.unlink(os.path.join(media_inventory.chat_media_dir(ALICE), "image_20260901_100000_IMG1.jpg"))
    by_id = {i["message_id"]: i for i in main.list_media(chat_jid=ALICE)["items"]}
    assert not by_id["IMG1"]["cached"] and by_id["IMG1"]["cached_bytes"] is None


def test_an_arrival_is_seen_at_once_and_the_ttl_bounds_the_rest(media_store, monkeypatch):
    listed = _count_listings(monkeypatch)
    assert not main.list_media(chat_jid=ALICE)["items"][1]["cached"]  # DOC1, nothing on disk
    doc = os.path.join(media_inventory.chat_media_dir(ALICE), "document_20260904_100000_DOC1.pdf")
    open(doc, "wb").write(b"pdf")
    # Writing the file moved the directory's mtime, so the listing is read again.
    assert main.list_media(chat_jid=ALICE)["items"][1]["cached"]
    assert listed == [ALICE, ALICE]

    # The one case the mtime cannot catch (a change the filesystem timestamps
    # cannot separate from the read): pinned mtime, frozen clock. The file is
    # missed for at most CACHE_TTL_S, and never the other way round.
    monkeypatch.setattr(media_inventory, "_dir_mtime_ns", lambda chat_jid: 1)
    now = [1000.0]
    monkeypatch.setattr(media_inventory.time, "monotonic", lambda: now[0])
    media_inventory.forget_cached_names()
    os.unlink(doc)
    assert not main.list_media(chat_jid=ALICE)["items"][1]["cached"]
    open(doc, "wb").write(b"pdf")
    assert not main.list_media(chat_jid=ALICE)["items"][1]["cached"]  # missed, not wrongly claimed
    now[0] += media_inventory.CACHE_TTL_S
    assert main.list_media(chat_jid=ALICE)["items"][1]["cached"]


def test_the_memo_is_bounded(media_store, monkeypatch):
    monkeypatch.setattr(media_inventory, "list_chat_names", lambda chat_jid: {})
    for i in range(media_inventory.CACHE_MAX_CHATS * 2):
        media_inventory.cached_names(f"{i}@s.whatsapp.net")
    assert len(media_inventory._names) == media_inventory.CACHE_MAX_CHATS
    assert f"{media_inventory.CACHE_MAX_CHATS * 2 - 1}@s.whatsapp.net" in media_inventory._names  # newest kept


def test_two_files_for_one_message_are_read_the_same_way_everywhere(media_store):
    """A legacy name and its re-download coexist: the listing and a read agree."""
    import media_read

    alice = media_inventory.chat_media_dir(ALICE)
    legacy = os.path.join(alice, "document_20260904_100000_DOC1")
    open(legacy, "wb").write(b"old")
    fresh = os.path.join(alice, "document_20260904_100000_DOC1.pdf")
    open(fresh, "wb").write(b"new bytes")
    names = media_inventory.list_chat_names(ALICE)
    assert media_inventory.lookup_cached_name(ALICE, "DOC1") == names["DOC1"]
    assert media_read.cached_path(ALICE, "DOC1") == os.path.realpath(os.path.join(alice, names["DOC1"]))
    item = next(i for i in main.list_media(chat_jid=ALICE)["items"] if i["message_id"] == "DOC1")
    assert item["cached_file"] == names["DOC1"]
    assert item["cached_bytes"] == os.path.getsize(os.path.join(alice, names["DOC1"]))


def test_a_download_this_process_asked_for_drops_the_memo(media_store, monkeypatch):
    """The mtime cannot always separate our own write from the read; this can."""

    class Resp:
        status_code = 200

        @staticmethod
        def json():
            return {"success": True, "path": doc}

    monkeypatch.setattr(media_inventory, "_dir_mtime_ns", lambda chat_jid: 1)  # frozen: only the drop can help
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kwargs: Resp())
    doc = os.path.join(media_inventory.chat_media_dir(ALICE), "document_20260904_100000_DOC1.pdf")
    assert not main.list_media(chat_jid=ALICE)["items"][1]["cached"]  # DOC1, memo now holds the listing
    open(doc, "wb").write(b"pdf")
    whatsapp.download_media("DOC1", ALICE)
    assert main.list_media(chat_jid=ALICE)["items"][1]["cached"]
