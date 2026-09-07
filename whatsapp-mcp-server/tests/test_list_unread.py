"""list_unread: chats with unread inbound rows, newest first, honouring read markers and the allow-list."""

import sqlite3
from datetime import datetime, timedelta

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import MESSAGES_SCHEMA

A, B, G = "5511111111111@s.whatsapp.net", "5511222222222@s.whatsapp.net", "120363000000000001@g.us"


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.execute("INSERT INTO chats VALUES (?, 'Alice', '2026-09-04 10:05:00', NULL)", (A,))  # never read
        c.execute("INSERT INTO chats VALUES (?, 'Bob', '2026-09-04 11:00:00', '2026-09-04 10:30:00')", (B,))
        c.execute("INSERT INTO chats VALUES (?, 'Group', '2026-09-04 09:00:00', '2026-09-04 09:30:00')", (G,))
        rows = [
            ("a1", A, "5511111111111", "hi", "2026-09-04 10:00:00", 0, None),
            ("a2", A, "5511111111111", "there", "2026-09-04 10:05:00", 0, None),
            ("a3", A, "me", "my reply", "2026-09-04 10:06:00", 1, None),  # outbound never counts
            ("b1", B, "5511222222222", "old", "2026-09-04 10:00:00", 0, None),  # before marker
            ("b2", B, "5511222222222", "new", "2026-09-04 11:00:00", 0, None),
            ("b3", B, "5511222222222", "reaction", "2026-09-04 11:01:00", 0, "reaction"),  # pointer row
            ("g1", G, "5511333333333", "seen", "2026-09-04 09:00:00", 0, None),  # read
        ]
        for mid, chat, sender, content, ts, from_me, media in rows:
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) VALUES (?,?,?,?,?,?,?)",
                (mid, chat, sender, content, ts, from_me, media),
            )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def test_unread_summary(db):
    out = main.list_unread()
    assert out["chats_with_unread"] == 2 and out["total_unread"] == 3
    by = {c["chat_jid"]: c for c in out["chats"]}
    assert list(by) == [B, A]  # most recent unread first
    assert by[A]["unread_count"] == 2 and [m["id"] for m in by[A]["messages"]] == ["a1", "a2"]
    assert by[B]["unread_count"] == 1 and [m["id"] for m in by[B]["messages"]] == ["b2"]
    assert by[A]["last_read_time"] is None and by[B]["last_read_time"] == "2026-09-04 10:30:00"


def test_limits_and_since(db):
    out = main.list_unread(limit_chats=1, limit_per_chat=1)
    assert out["chats_with_unread"] == 1 and out["chats"][0]["chat_jid"] == B
    out = main.list_unread(since="2026-09-04T10:03:00")
    assert {c["chat_jid"]: c["unread_count"] for c in out["chats"]} == {B: 1, A: 1}
    # the listed rows obey the same bound as the counts
    assert {c["chat_jid"]: [m["id"] for m in c["messages"]] for c in out["chats"]} == {B: ["b2"], A: ["a2"]}
    assert main.list_unread(since="yesterday")["error"]["code"] == "invalid_argument"


def test_respects_allow_list(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511111111111"]))
    out = whatsapp.list_unread()
    assert [c["chat_jid"] for c in out["chats"]] == [A]


@pytest.fixture
def mixed(tmp_path, monkeypatch):
    """One direct chat and one group, each with a fresh and a week-old unread row."""
    path = tmp_path / "mixed.db"

    def stamp(**delta):
        return (datetime.now() - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S")

    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.execute("INSERT INTO chats VALUES (?, 'Alice', ?, NULL)", (A, stamp(hours=1)))
        c.execute("INSERT INTO chats VALUES (?, 'Neighbourhood', ?, NULL)", (G, stamp(hours=2)))
        rows = [
            ("a_old", A, stamp(days=10)),
            ("a_new", A, stamp(hours=1)),
            ("g_old", G, stamp(days=10)),
            ("g_new", G, stamp(hours=2)),
        ]
        for mid, chat, ts in rows:
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?,?,?,?,?,0)",
                (mid, chat, chat.split("@")[0], mid, ts),
            )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def test_exclude_groups_keeps_direct_chats_only(mixed):
    everything = main.list_unread()
    assert {c["chat_jid"] for c in everything["chats"]} == {A, G} and everything["total_unread"] == 4

    out = main.list_unread(exclude_groups=True)
    assert [c["chat_jid"] for c in out["chats"]] == [A]
    assert all(c["chat_jid"].endswith("@s.whatsapp.net") and not c["is_group"] for c in out["chats"])
    assert out["total_unread"] == 2 and out["chats_with_unread"] == 1


def test_max_age_days_bounds_counts_and_rows(mixed):
    out = main.list_unread(max_age_days=3)
    assert out["total_unread"] == 2 and out["chats_with_unread"] == 2
    assert {c["chat_jid"]: [m["id"] for m in c["messages"]] for c in out["chats"]} == {A: ["a_new"], G: ["g_new"]}

    both = main.list_unread(exclude_groups=True, max_age_days=3)
    assert [c["chat_jid"] for c in both["chats"]] == [A] and both["total_unread"] == 1

    wide = main.list_unread(max_age_days=30)
    assert wide["total_unread"] == 4


def test_since_and_max_age_days_are_mutually_exclusive(mixed):
    err = main.list_unread(since="2026-09-04T10:00:00", max_age_days=3)["error"]
    assert err["code"] == "invalid_argument" and "not both" in err["message"]


@pytest.mark.parametrize("days", [0, -1])
def test_max_age_days_must_be_positive(mixed, days):
    assert main.list_unread(max_age_days=days)["error"]["code"] == "invalid_argument"


def test_db_error_is_internal(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(db.parent / "missing" / "x.db"))
    whatsapp._reset_schema_cache()
    with pytest.raises(ToolError) as exc:
        whatsapp.list_unread()
    assert exc.value.code == "internal"
