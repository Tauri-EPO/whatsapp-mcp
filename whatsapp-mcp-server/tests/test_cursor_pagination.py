"""Keyset pagination: walking cursors yields every row exactly once, has_more ends the walk."""

import sqlite3

import pytest

import main
import whatsapp
from errors import ToolError
from tests.conftest import MESSAGES_SCHEMA

CHAT = "5511999999999@s.whatsapp.net"


@pytest.fixture
def mdb(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        for n in range(37):
            jid = f"55119{n:08d}@s.whatsapp.net"
            # two chats share a timestamp to exercise the tie-breaker
            ts = f"2026-09-04 10:{n // 2:02d}:00"
            c.execute("INSERT INTO chats VALUES (?, ?, ?)", (jid, f"Chat {n:02d}", ts))
        c.execute("INSERT INTO chats VALUES ('nullts@s.whatsapp.net', 'No time', NULL)")
        c.execute("INSERT INTO chats VALUES (?, 'Main', '2026-09-04 12:00:00')", (CHAT,))
        for n in range(53):
            # several messages per second so (timestamp, id) ties happen
            ts = f"2026-09-04 11:{n // 5:02d}:00"
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
                (f"M{n:03d}", CHAT, "5511999999999", f"hello {n}", ts),
            )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def walk(fn, *args, **kwargs):
    seen, cursor, pages = [], None, 0
    while True:
        page = fn(*args, cursor=cursor, **kwargs)
        pages += 1
        seen.extend(page.items)
        if not page.has_more:
            assert page.next_cursor is None
            return seen, pages
        assert page.next_cursor
        cursor = page.next_cursor
        assert pages < 100, "cursor walk did not terminate"


@pytest.mark.parametrize("sort_by", ["newest", "oldest"])
def test_messages_walk_is_complete_and_ordered(mdb, sort_by):
    items, pages = walk(whatsapp.list_messages_page, limit=7, include_context=False, chat_jid=CHAT, sort_by=sort_by)
    ids = [m["id"] for m in items]
    assert len(ids) == 53 and len(set(ids)) == 53
    assert pages == 8
    stamps = [m["timestamp"] for m in items]
    assert stamps == sorted(stamps, reverse=(sort_by == "newest"))


def test_messages_cursor_survives_new_arrivals(mdb):
    first = whatsapp.list_messages_page(limit=10, include_context=False, chat_jid=CHAT)
    with sqlite3.connect(mdb) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('NEW', ?, 'x', 'late', '2026-09-04 23:59:00', 0)",
            (CHAT,),
        )
    second = whatsapp.list_messages_page(limit=10, include_context=False, chat_jid=CHAT, cursor=first.next_cursor)
    first_ids = {m["id"] for m in first.items}
    assert not first_ids & {m["id"] for m in second.items}, "offset paging would have repeated a row here"


def test_chats_walk_both_sorts(mdb):
    for sort_by in ("last_active", "name"):
        items, _ = walk(whatsapp.list_chats_page, limit=8, sort_by=sort_by, include_last_message=False)
        jids = [c["jid"] for c in items]
        assert len(jids) == 39 and len(set(jids)) == 39, sort_by
        assert jids[-1] == "nullts@s.whatsapp.net" if sort_by == "last_active" else True


def test_contact_chats_walk(mdb):
    with sqlite3.connect(mdb) as c:
        for n in range(12):
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, '5511888888888', 'x', '2026-09-04 09:00:00', 0)",
                (f"C{n}", f"55119{n:08d}@s.whatsapp.net"),
            )
    items, pages = walk(whatsapp.get_contact_chats_page, "5511888888888@s.whatsapp.net", limit=5)
    assert len({c["jid"] for c in items}) == 12 and pages == 3


def test_bad_cursor_is_invalid_argument(mdb):
    with pytest.raises(ToolError) as exc:
        whatsapp.list_messages_page(cursor="not-a-cursor", chat_jid=CHAT)
    assert exc.value.code == "invalid_argument"
    chats_cursor = whatsapp.list_chats_page(limit=1).next_cursor
    with pytest.raises(ToolError):
        whatsapp.list_messages_page(cursor=chats_cursor, chat_jid=CHAT)  # wrong kind
    msg_cursor = whatsapp.list_messages_page(limit=1, include_context=False, chat_jid=CHAT).next_cursor
    with pytest.raises(ToolError):
        whatsapp.list_messages_page(cursor=msg_cursor, chat_jid=CHAT, sort_by="oldest")  # sort mismatch


def test_tools_return_page_envelope(mdb):
    page = main.list_messages(chat_jid=CHAT, limit=5, include_context=False)
    assert set(page) == {"items", "next_cursor", "has_more"} and page["has_more"] is True
    page2 = main.list_messages(chat_jid=CHAT, limit=5, include_context=False, cursor=page["next_cursor"])
    assert {m["id"] for m in page["items"]}.isdisjoint({m["id"] for m in page2["items"]})
    chats = main.list_chats(limit=100)
    assert chats["has_more"] is False and chats["next_cursor"] is None and len(chats["items"]) == 39


# A forward keeps its message ID in every chat it lands in, so (timestamp, id)
# is not a total order: the store key is (id, chat_jid). See issue #313.
FWD_ID = "same-forwarded-id"
FWD_TS = "2026-09-01 12:00:00"
LATER_TS = "2026-09-01 12:00:01"
FWD_CHATS = ("aaa@s.whatsapp.net", "bbb@g.us", "ccc@s.whatsapp.net")
REVOKED_CHAT = "ddd@s.whatsapp.net"

# Mirrors whatsapp-bridge/fts.go, enough of it to rank a query.
FWD_FTS_SCHEMA = """
CREATE VIRTUAL TABLE messages_fts USING fts5(
    content, content='messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
"""


@pytest.fixture
def forwarded(tmp_path, monkeypatch):
    """The same forwarded ID and timestamp in three chats, plus a revoked copy."""
    path = tmp_path / "messages.db"
    insert = (
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at) "
        "VALUES (?, ?, '5511777777777', ?, ?, 0, ?)"
    )
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.executescript(FWD_FTS_SCHEMA)
        for jid in (*FWD_CHATS, REVOKED_CHAT):
            c.execute("INSERT INTO chats VALUES (?, ?, ?)", (jid, f"Chat {jid[:3]}", LATER_TS))
        for jid in FWD_CHATS:
            c.execute(insert, (FWD_ID, jid, "orcamento forwarded", FWD_TS, None))
            c.execute(insert, ("second-forward", jid, "outro assunto", LATER_TS, None))
        # Same ID and timestamp as the tie group, but revoked.
        c.execute(insert, (FWD_ID, REVOKED_CHAT, "orcamento forwarded", FWD_TS, "2026-09-01 13:00:00"))
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def keys(items):
    return [(m["id"], m["chat_jid"]) for m in items]


@pytest.mark.parametrize("sort_by", ["newest", "oldest"])
def test_forwarded_copies_are_paged_exactly_once(forwarded, sort_by):
    items, pages = walk(whatsapp.list_messages_page, limit=1, include_context=False, sort_by=sort_by)
    assert len(keys(items)) == 7 and len(set(keys(items))) == 7
    assert pages == 7
    stamps = [m["timestamp"] for m in items]
    assert stamps == sorted(stamps, reverse=(sort_by == "newest"))
    assert (FWD_ID, REVOKED_CHAT) in set(keys(items))


@pytest.mark.parametrize("sort_by", ["newest", "oldest"])
def test_forwarded_copies_with_chat_filter_and_deleted_hidden(forwarded, sort_by):
    picked = [FWD_CHATS[0], FWD_CHATS[2]]
    items, _ = walk(
        whatsapp.list_messages_page,
        limit=1,
        include_context=False,
        sort_by=sort_by,
        chat_jid=picked,
        include_deleted=False,
    )
    assert sorted(keys(items)) == sorted([(mid, jid) for jid in picked for mid in (FWD_ID, "second-forward")])


def test_forwarded_copies_walk_without_deleted(forwarded):
    items, _ = walk(whatsapp.list_messages_page, limit=2, include_context=False, include_deleted=False)
    assert len(set(keys(items))) == 6
    assert (FWD_ID, REVOKED_CHAT) not in set(keys(items))


def test_relevance_tie_break_is_deterministic(forwarded):
    items, pages = walk(
        whatsapp.list_messages_page,
        limit=1,
        include_context=False,
        sort_by="relevance",
        query="orcamento",
        include_deleted=False,
    )
    assert keys(items) == sorted(keys(items), reverse=True) and len(set(keys(items))) == 3
    assert pages == 3


@pytest.mark.parametrize("sort_by", ["newest", "oldest"])
def test_legacy_cursor_without_chat_jid_resumes_on_the_old_seek(forwarded, sort_by):
    # Cursor as this server wrote it before the chat_jid became part of the key:
    # accepted, never repeats a row, still skips the copies tied on (t, id).
    legacy = whatsapp.encode_cursor({"k": "messages", "s": sort_by, "t": FWD_TS, "i": FWD_ID})
    page = whatsapp.list_messages_page(limit=10, include_context=False, sort_by=sort_by, cursor=legacy)
    expected = set() if sort_by == "newest" else {("second-forward", jid) for jid in FWD_CHATS}
    assert set(keys(page.items)) == expected and page.has_more is False


@pytest.mark.parametrize("sort_by", ["newest", "oldest"])
def test_legacy_cursor_does_not_repeat_its_boundary_row(mdb, sort_by):
    """An ordinary walk (unique IDs) that started before the upgrade stays exact."""
    first = whatsapp.list_messages_page(limit=2, include_context=False, chat_jid=CHAT, sort_by=sort_by)
    state = whatsapp.decode_cursor(first.next_cursor, "messages")
    assert state is not None and state.pop("c") == CHAT
    legacy = whatsapp.encode_cursor(state)
    second = whatsapp.list_messages_page(limit=2, include_context=False, chat_jid=CHAT, sort_by=sort_by, cursor=legacy)
    assert {m["id"] for m in first.items}.isdisjoint({m["id"] for m in second.items})
