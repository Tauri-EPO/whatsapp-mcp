"""list_messages(query=...) also matches what was *said* in a voice note.

messages_fts is the bridge's index over messages.content and never sees
notes.db, so the union happens on the MCP side; these tests pin both paths
(FTS index present and substring fallback).
"""

from __future__ import annotations

import sqlite3

import pytest

import chat_policy
import media_inventory
import media_notes
import whatsapp
from tests.test_search import FTS_SCHEMA, SCHEMA

CHAT = "111@s.whatsapp.net"
OTHER = "222@s.whatsapp.net"
SHA_VOICE = "aa" * 32
SHA_OTHER = "bb" * 32

# One text message that mentions the budget, two voice notes that only say it.
TEXT_ROWS = [
    ("m1", CHAT, "Segue o orcamento da obra", "2024-01-01T10:00:00"),
    ("m2", CHAT, "combinado, obrigado", "2024-01-02T10:00:00"),
]
AUDIO_ROWS = [
    ("v1", CHAT, SHA_VOICE, "2024-01-03T10:00:00"),
    ("v2", OTHER, SHA_OTHER, "2024-01-04T10:00:00"),
]


def _make_db(tmp_path, monkeypatch, with_fts: bool):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    if with_fts:
        conn.executescript(FTS_SCHEMA)
    conn.executemany("INSERT INTO chats (jid, name) VALUES (?, ?)", [(CHAT, "Alice"), (OTHER, "Bob")])
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, '111', ?, ?, 0)",
        TEXT_ROWS,
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_sha256) "
        "VALUES (?, ?, '111', '', ?, 0, 'audio', ?)",
        [(mid, chat, ts, bytes.fromhex(sha)) for mid, chat, sha, ts in AUDIO_ROWS],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    # Both voice notes were transcribed (by hand or by the ingest worker).
    media_notes.annotate_media(SHA_VOICE, "transcript", "manda o orcamento da reforma (casa) por favor")
    media_notes.annotate_media(SHA_OTHER, "transcript", "chego domingo")
    return path


@pytest.fixture
def fts_db(tmp_path, monkeypatch):
    yield _make_db(tmp_path, monkeypatch, with_fts=True)
    whatsapp._reset_schema_cache()


@pytest.fixture
def plain_db(tmp_path, monkeypatch):
    yield _make_db(tmp_path, monkeypatch, with_fts=False)
    whatsapp._reset_schema_cache()


def ids(query, **kwargs):
    kwargs.setdefault("include_context", False)
    return sorted(m["id"] for m in whatsapp.list_messages(query=query, **kwargs))


def test_a_stored_transcript_is_searchable_through_the_index(fts_db):
    # "orcamento" is in one message body and in one transcript.
    assert ids("orcamento") == ["m1", "v1"]
    # A word only ever spoken still finds its voice note.
    assert ids("domingo") == ["v2"]
    # And a word nobody said or wrote finds nothing.
    assert ids("jacaré") == []


def test_a_stored_transcript_is_searchable_without_the_index(plain_db):
    assert ids("orcamento") == ["m1", "v1"]
    assert ids("domingo") == ["v2"]


def test_the_count_matches_the_page(fts_db):
    assert whatsapp.count_messages(query="orcamento") == 2
    assert whatsapp.count_messages(query="domingo") == 1
    assert whatsapp.count_messages(query="jacaré") == 0


def test_transcript_hits_combine_with_the_other_filters(fts_db):
    assert ids("orcamento", chat_jid=CHAT) == ["m1", "v1"]
    assert ids("orcamento", media_type="audio") == ["v1"]
    assert ids("domingo", chat_jid=CHAT) == []  # v2 is in the other chat


def test_the_allow_list_still_hides_denied_chats(fts_db, monkeypatch):
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": CHAT})
    for module in (whatsapp, media_inventory, media_notes):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)
    # The transcript of the denied chat's voice note matches, but its message row
    # is out of scope, so nothing leaks.
    assert ids("domingo") == []
    assert ids("orcamento") == ["m1", "v1"]


def test_an_invalid_fts_query_still_falls_back_with_transcripts_in_play(fts_db):
    # "(" is FTS5 syntax: the raw MATCH fails and is retried with every token
    # quoted. That retry has to keep working now that MATCH sits in a subquery
    # next to the transcript hashes.
    assert ids("reforma (casa)") == ["v1"]


def test_relevance_sort_degrades_instead_of_failing(fts_db):
    # bm25 needs the FTS join, which the union cannot use; the answer is still
    # the right set, ordered newest-first.
    assert [m["id"] for m in whatsapp.list_messages(query="orcamento", sort_by="relevance", include_context=False)] == [
        "v1",
        "m1",
    ]


def test_transcript_matches_are_bounded(fts_db, monkeypatch):
    monkeypatch.setattr(media_notes, "MAX_TRANSCRIPT_MATCHES", 1)
    assert len(media_notes.transcript_hashes("o", limit=1)) == 1
    # No notes.db at all is simply "no transcript matched", not an error.
    monkeypatch.setattr(media_notes, "notes_db_path", lambda: "/nope/notes.db")
    assert media_notes.transcript_hashes("orcamento") == []
    assert ids("orcamento") == ["m1"]
