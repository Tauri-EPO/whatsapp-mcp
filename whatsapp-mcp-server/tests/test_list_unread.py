"""list_unread: chats with unread inbound rows, newest first, honouring read markers and the allow-list."""

import sqlite3

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
    assert main.list_unread(since="yesterday")["error"]["code"] == "invalid_argument"


def test_respects_allow_list(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511111111111"]))
    out = whatsapp.list_unread()
    assert [c["chat_jid"] for c in out["chats"]] == [A]


def test_db_error_is_internal(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(db.parent / "missing" / "x.db"))
    whatsapp._reset_schema_cache()
    with pytest.raises(ToolError) as exc:
        whatsapp.list_unread()
    assert exc.value.code == "internal"
