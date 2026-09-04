"""Schema probes are memoised per DB file; direct-chat lookup is exact."""

import sqlite3

import pytest

import whatsapp
from tests.test_sender_name_cache import MESSAGES_SCHEMA


@pytest.fixture
def mdb(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("INSERT INTO chats VALUES ('5511999999999@s.whatsapp.net', 'Alice', '2026-09-04 10:00:00')")
        c.execute("INSERT INTO chats VALUES ('15511999999999@s.whatsapp.net', 'Not Alice', '2026-09-04 11:00:00')")
        c.execute("INSERT INTO chats VALUES ('120363000000000001@g.us', 'Group', '2026-09-04 12:00:00')")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    yield path
    whatsapp._reset_schema_cache()


def test_probes_run_once_per_file(mdb, monkeypatch):
    calls = {"pragma": 0}

    class CountingCursor(sqlite3.Cursor):
        def execute(self, sql, *a, **k):
            if "PRAGMA table_info" in sql:
                calls["pragma"] += 1
            return super().execute(sql, *a, **k)

    class CountingConnection(sqlite3.Connection):
        def cursor(self, factory=CountingCursor):
            return super().cursor(factory)

    monkeypatch.setattr(
        whatsapp,
        "_connect_messages_db",
        lambda: sqlite3.connect(whatsapp.MESSAGES_DB_PATH, factory=CountingConnection),
    )
    for _ in range(5):
        whatsapp.list_chats(limit=5)
    assert calls["pragma"] == 1, calls


def test_probe_invalidates_when_file_changes(mdb, monkeypatch):
    conn = sqlite3.connect(mdb)
    assert whatsapp._fts_available(conn) is False
    with sqlite3.connect(mdb) as c:
        c.execute("CREATE VIRTUAL TABLE messages_fts USING fts5(content)")
    # mtime granularity can be coarse: bump the size too so the signature moves
    with sqlite3.connect(mdb) as c:
        c.execute("INSERT INTO chats VALUES ('x@s.whatsapp.net', 'x', '2026-09-04 12:00:00')")
    assert whatsapp._fts_available(sqlite3.connect(mdb)) is True


def test_direct_chat_lookup_is_exact(mdb):
    assert whatsapp.get_direct_chat_by_contact("5511999999999")["name"] == "Alice"
    assert whatsapp.get_direct_chat_by_contact("+55 11 99999-9999")["name"] == "Alice"
    assert whatsapp.get_direct_chat_by_contact("5511999999999@s.whatsapp.net")["name"] == "Alice"
    # a substring of two different JIDs: the old LIKE lookup returned one of them at random
    assert whatsapp.get_direct_chat_by_contact("511999999999") is None
    assert whatsapp.get_direct_chat_by_contact("120363000000000001") is None
