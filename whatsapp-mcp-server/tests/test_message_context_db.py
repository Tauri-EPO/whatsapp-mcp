"""get_message_context against a real SQLite file: (id, chat_jid) lookup and WAL reads."""

import sqlite3

import pytest

import whatsapp

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, quoted_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""

CHAT_A = "111@s.whatsapp.net"
CHAT_B = "222@g.us"


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.execute("PRAGMA journal_mode=WAL")  # what the bridge sets; readers must cope
    conn.executemany("INSERT INTO chats (jid, name) VALUES (?, ?)", [(CHAT_A, "Alice"), (CHAT_B, "Group")])
    rows = [
        # Same message ID forwarded into two chats, at different times.
        ("SHARED", CHAT_A, "111", "a-target", "2024-01-01T10:00:00", 0),
        ("SHARED", CHAT_B, "333", "b-target", "2024-01-02T10:00:00", 0),
        ("A1", CHAT_A, "111", "a-before", "2024-01-01T09:00:00", 0),
        ("A2", CHAT_A, "me", "a-after", "2024-01-01T11:00:00", 1),
        ("B1", CHAT_B, "333", "b-before", "2024-01-02T09:00:00", 0),
    ]
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?)", rows
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


def test_lookup_by_id_and_chat_is_unambiguous(db):
    ctx_a = whatsapp.get_message_context("SHARED", before=5, after=5, chat_jid=CHAT_A)
    assert ctx_a.message.content == "a-target"
    assert [m.content for m in ctx_a.before] == ["a-before"]
    assert [m.content for m in ctx_a.after] == ["a-after"]

    ctx_b = whatsapp.get_message_context("SHARED", before=5, after=5, chat_jid=CHAT_B)
    assert ctx_b.message.content == "b-target"
    assert [m.content for m in ctx_b.before] == ["b-before"]
    assert ctx_b.after == []


def test_lookup_without_chat_uses_most_recent_match(db):
    ctx = whatsapp.get_message_context("SHARED", before=1, after=1)
    assert ctx.message.chat_jid == CHAT_B  # newer row wins


def test_missing_message_names_the_chat(db):
    with pytest.raises(ValueError, match="in chat 111@s.whatsapp.net"):
        whatsapp.get_message_context("NOPE", chat_jid=CHAT_A)


def test_connections_use_busy_timeout(db, monkeypatch):
    seen = {}
    real_connect = sqlite3.connect

    def spy(path, *args, **kwargs):
        seen["timeout"] = kwargs.get("timeout")
        return real_connect(path, *args, **kwargs)

    monkeypatch.setattr(whatsapp.sqlite3, "connect", spy)
    whatsapp.get_message_context("A1", chat_jid=CHAT_A)
    assert seen["timeout"] == whatsapp.SQLITE_BUSY_TIMEOUT_S
