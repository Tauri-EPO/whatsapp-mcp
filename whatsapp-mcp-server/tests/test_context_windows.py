"""list_messages(include_context=True): one query per batch, dedupe on (id, chat_jid)."""

import sqlite3

import pytest

import whatsapp

CHAT_A = "111@s.whatsapp.net"
CHAT_B = "222@g.us"
SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT, quoted_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""


class CountingCursor(sqlite3.Cursor):
    executed: list[str] = []

    def execute(self, sql, *args):
        CountingCursor.executed.append(sql)
        return super().execute(sql, *args)


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany("INSERT INTO chats (jid, name) VALUES (?, ?)", [(CHAT_A, "A"), (CHAT_B, "B")])
    rows = []
    for i in range(1, 11):  # a1..a10 in chat A, one per day
        rows.append((f"a{i}", CHAT_A, f"A msg {i} {'hit' if i in (3, 7) else ''}", f"2024-01-{i:02d}T10:00:00", None))
    # Same message ID "a3" also exists in chat B (forwarded): must be kept distinct.
    rows.append(("a3", CHAT_B, "B forwarded hit", "2024-02-01T10:00:00", None))
    rows.append(("b0", CHAT_B, "B before", "2024-01-31T10:00:00", None))
    rows.append(("b2", CHAT_B, "B after revoked", "2024-02-02T10:00:00", "2024-02-02T11:00:00"))
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at)"
        " VALUES (?, ?, 's', ?, ?, 0, ?)",
        rows,
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))

    real_connect = whatsapp._connect_messages_db

    class ConnProxy:
        def __init__(self, conn):
            self._conn = conn

        def cursor(self):
            return self._conn.cursor(factory=CountingCursor)

        def __getattr__(self, name):
            return getattr(self._conn, name)

    monkeypatch.setattr(whatsapp, "_connect_messages_db", lambda: ConnProxy(real_connect()))
    CountingCursor.executed = []
    return path


def ids(**kwargs):
    return [(m["id"], m["chat_jid"]) for m in whatsapp.list_messages(**kwargs)]


def message_queries() -> int:
    """Statements that read the messages table (name lookups for sender_display are excluded)."""
    return sum(1 for sql in CountingCursor.executed if "FROM messages" in sql or "JOIN messages" in sql)


def test_context_uses_one_query_regardless_of_hit_count(db):
    result = ids(query="hit", context_before=1, context_after=1, sort_by="oldest")
    # 3 hits (a3, a7 in A; a3 in B) -> 1 search query + 1 context query.
    assert message_queries() == 2
    assert result == [
        ("a2", CHAT_A),
        ("a3", CHAT_A),
        ("a4", CHAT_A),
        ("a6", CHAT_A),
        ("a7", CHAT_A),
        ("a8", CHAT_A),
        ("b0", CHAT_B),
        ("a3", CHAT_B),
        ("b2", CHAT_B),
    ]


def test_dedupe_is_per_chat_not_per_id(db):
    result = ids(query="hit", context_before=0, context_after=0)
    assert ("a3", CHAT_A) in result and ("a3", CHAT_B) in result


def test_overlapping_windows_dedupe(db):
    # a3 and a7 with windows of 3 overlap at a4..a6; each row appears once, in reading order.
    result = ids(query="hit", chat_jid=CHAT_A, context_before=3, context_after=3, sort_by="oldest")
    assert [r[0] for r in result] == ["a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"]


def test_include_deleted_false_applies_to_context(db):
    result = ids(query="hit", chat_jid=CHAT_B, context_before=1, context_after=1, include_deleted=False)
    assert [r[0] for r in result] == ["b0", "a3"]


def test_zero_windows_skip_the_context_query(db):
    ids(query="hit", context_before=0, context_after=0)
    assert message_queries() == 1
