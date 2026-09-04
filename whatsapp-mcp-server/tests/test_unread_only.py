import sqlite3

import pytest

import whatsapp

A = "111@s.whatsapp.net"  # read marker in the middle
B = "222@s.whatsapp.net"  # never read
SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT,
    quoted_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
"""


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, ?, ?)",
        [(A, "A", "2024-01-05T10:00:00", "2024-01-02T12:00:00"), (B, "B", "2024-01-04T10:00:00", None)],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?)",
        [
            ("a1", A, "111", "read already", "2024-01-01T10:00:00", 0),
            ("a2", A, "111", "unread 1", "2024-01-03T10:00:00", 0),
            ("a3", A, "me", "my reply", "2024-01-04T10:00:00", 1),
            ("a4", A, "111", "unread 2", "2024-01-05T10:00:00", 0),
            ("b1", B, "222", "never read chat", "2024-01-04T10:00:00", 0),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


def test_unread_only_oldest_first(db):
    ids = [m["id"] for m in whatsapp.list_messages(unread_only=True, sort_by="oldest", include_context=False)]
    assert ids == ["a2", "b1", "a4"]


def test_unread_only_combines_with_chat_filter(db):
    ids = [m["id"] for m in whatsapp.list_messages(unread_only=True, chat_jid=A, include_context=False)]
    assert ids == ["a4", "a2"]


def test_default_is_unfiltered(db):
    assert len(whatsapp.list_messages(include_context=False)) == 5


def test_tool_passes_flag(monkeypatch):
    import main

    seen = {}
    monkeypatch.setattr(main, "whatsapp_list_messages", lambda **kw: seen.update(kw) or [])
    main.list_messages(unread_only=True)
    assert seen["unread_only"] is True
