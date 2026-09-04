"""Revoked messages keep their content; deleted_at is exposed; include_deleted hides them."""

import sqlite3

import pytest

import whatsapp

CHAT = "5511999999999@s.whatsapp.net"
SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, quoted_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.execute("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Alice', '2024-01-03T10:00:00')", (CHAT,))
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, deleted_at)"
        " VALUES (?, ?, '111', ?, ?, 0, ?, ?, ?)",
        [
            ("m1", CHAT, "first", "2024-01-01T10:00:00", None, None, None),
            ("m2", CHAT, "oops sent by mistake", "2024-01-02T10:00:00", "image", "image_x.jpg", "2024-01-02T10:05:00"),
            ("m3", CHAT, "third", "2024-01-03T10:00:00", None, None, None),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


def test_revoked_message_returned_with_content_and_deleted_at(db):
    rows = {m["id"]: m for m in whatsapp.list_messages(chat_jid=CHAT, include_context=False)}
    assert set(rows) == {"m1", "m2", "m3"}
    assert rows["m2"]["content"] == "oops sent by mistake"
    assert rows["m2"]["filename"] == "image_x.jpg"
    assert rows["m2"]["deleted_at"] == "2024-01-02T10:05:00"
    assert rows["m1"]["deleted_at"] is None


def test_include_deleted_false_hides_revoked(db):
    ids = [m["id"] for m in whatsapp.list_messages(chat_jid=CHAT, include_context=False, include_deleted=False)]
    assert ids == ["m3", "m1"]


def test_context_windows_respect_include_deleted(db):
    ctx = whatsapp.get_message_context("m3", before=5, after=0, chat_jid=CHAT)
    assert [m.id for m in ctx.before] == ["m2", "m1"]
    assert ctx.before[0].deleted_at is not None
    ctx = whatsapp.get_message_context("m3", before=5, after=0, chat_jid=CHAT, include_deleted=False)
    assert [m.id for m in ctx.before] == ["m1"]
    # The target itself is always returned, even when revoked.
    assert whatsapp.get_message_context("m2", chat_jid=CHAT, include_deleted=False).message.id == "m2"


def test_tool_passes_flag_through(monkeypatch):
    import main

    seen = {}
    monkeypatch.setattr(main, "whatsapp_list_messages", lambda **kw: seen.update(kw) or [])
    main.list_messages(include_deleted=False)
    assert seen["include_deleted"] is False


def test_view_once_flag_is_exposed(db):
    conn = sqlite3.connect(db)
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, view_once)"
        " VALUES ('vo', ?, '111', '🔒 view-once image', '2024-01-04T10:00:00', 0, 'image', 1)",
        (CHAT,),
    )
    conn.commit()
    conn.close()
    rows = {m["id"]: m for m in whatsapp.list_messages(chat_jid=CHAT, include_context=False)}
    assert rows["vo"]["view_once"] is True
    assert rows["m1"]["view_once"] is False
