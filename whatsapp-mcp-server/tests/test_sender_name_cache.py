"""get_sender_name: bounded connections per result set, exact-match lookup, TTL cache."""

import sqlite3
from datetime import datetime

import pytest

import whatsapp

MESSAGES_SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, quoted_message_id TEXT, deleted_at TIMESTAMP, view_once INTEGER DEFAULT 0,
    target_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
"""
WHATSMEOW_SCHEMA = """
CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
"""


@pytest.fixture
def dbs(tmp_path, monkeypatch):
    mdb = tmp_path / "messages.db"
    wdb = tmp_path / "whatsapp.db"
    with sqlite3.connect(mdb) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("INSERT INTO chats VALUES ('5511999999999@s.whatsapp.net', 'Alice', '2026-09-04 10:00:00')")
        c.execute("INSERT INTO chats VALUES ('120363000000000001@g.us', 'Family', '2026-09-04 10:00:00')")
        # a chat whose JID *contains* another number's digits: the old LIKE fallback matched it
        c.execute("INSERT INTO chats VALUES ('15511999999999@s.whatsapp.net', 'Not Alice', '2026-09-04 10:00:00')")
    with sqlite3.connect(wdb) as c:
        c.executescript(WHATSMEOW_SCHEMA)
        c.execute("INSERT INTO whatsmeow_lid_map VALUES ('231241139937355', '5511888888888')")
        c.execute(
            "INSERT INTO whatsmeow_contacts VALUES ('me', '5511888888888@s.whatsapp.net', 'Bob', 'Bob Silva', 'bobby', NULL)"
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(mdb))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(wdb))
    whatsapp._reset_name_cache()
    yield mdb, wdb
    whatsapp._reset_name_cache()


def _count_connections(monkeypatch):
    counts = {"messages": 0, "whatsmeow": 0}
    real_m, real_w = whatsapp._connect_messages_db, whatsapp._connect_whatsmeow_db

    def cm():
        counts["messages"] += 1
        return real_m()

    def cw():
        counts["whatsmeow"] += 1
        return real_w()

    monkeypatch.setattr(whatsapp, "_connect_messages_db", cm)
    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", cw)
    return counts


def test_exact_match_does_not_pick_a_jid_that_merely_contains_the_digits(dbs):
    assert whatsapp.get_sender_name("5511999999999@s.whatsapp.net") == "Alice"
    assert whatsapp.get_sender_name("5511999999999") == "Alice"
    # 511999999999 is a substring of both JIDs above; before, LIKE could return "Not Alice"/"Alice" arbitrarily
    assert whatsapp.get_sender_name("511999999999") == "511999999999"


def test_lid_resolves_through_one_whatsmeow_connection(dbs, monkeypatch):
    counts = _count_connections(monkeypatch)
    assert whatsapp.get_sender_name("231241139937355@lid") == "Bob Silva"
    assert counts["whatsmeow"] == 1, counts


def test_repeated_senders_hit_the_cache(dbs, monkeypatch):
    counts = _count_connections(monkeypatch)
    for _ in range(50):
        whatsapp.get_sender_name("5511999999999@s.whatsapp.net")
        whatsapp.get_sender_name("231241139937355@lid")
        whatsapp._sender_aliases("5511888888888")
    assert counts["messages"] == 2, counts  # one per distinct sender
    assert counts["whatsmeow"] <= 3, counts  # LID name once, aliases once (+ at most one miss path)


def test_cache_expires(dbs, monkeypatch):
    whatsapp.get_sender_name("5511999999999@s.whatsapp.net")
    with sqlite3.connect(dbs[0]) as c:
        c.execute("UPDATE chats SET name='Alice Renamed' WHERE jid='5511999999999@s.whatsapp.net'")
    assert whatsapp.get_sender_name("5511999999999@s.whatsapp.net") == "Alice"  # still cached
    monkeypatch.setattr(whatsapp, "NAME_CACHE_TTL_S", 0.0)
    whatsapp._reset_name_cache()
    assert whatsapp.get_sender_name("5511999999999@s.whatsapp.net") == "Alice Renamed"


def test_msg_to_dict_list_resolves_each_sender_once(dbs, monkeypatch):
    counts = _count_connections(monkeypatch)
    msgs = [
        whatsapp.Message(
            timestamp=datetime(2026, 9, 4, 10, 0, 0),
            sender="5511999999999@s.whatsapp.net",
            content=f"m{i}",
            is_from_me=False,
            chat_jid="5511999999999@s.whatsapp.net",
            id=f"M{i}",
        )
        for i in range(200)
    ]
    out = [whatsapp.msg_to_dict(m) for m in msgs]
    assert all(d["sender_name"] == "Alice" for d in out)
    assert counts["messages"] == 1, counts
