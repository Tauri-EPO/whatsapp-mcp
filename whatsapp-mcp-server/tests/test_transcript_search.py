"""list_messages(query=...) also matches what was *said* in a voice note.

messages_fts is the bridge's index over messages.content and never sees
notes.db, so the union happens on the MCP side; these tests pin both paths
(FTS index present and substring fallback).
"""

from __future__ import annotations

import sqlite3
from datetime import datetime, timedelta

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
SHA_NEW = "cc" * 32

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


def relevance(query, **kwargs):
    kwargs.setdefault("include_context", False)
    return [m["id"] for m in whatsapp.list_messages(query=query, sort_by="relevance", **kwargs)]


def test_relevance_ranks_spoken_and_written_hits_together(fts_db):
    # Both sides are scored with bm25 (messages_fts for the text, transcripts_fts
    # for the audio) and ordered as one set. m1 says the word in five words, v1's
    # transcript in nine, so the text hit wins — which is also how we know the
    # sort is not the newest-first fallback: that would put v1 (Jan 3) first.
    assert relevance("orcamento") == ["m1", "v1"]
    assert ids("orcamento") == ["m1", "v1"]


def test_relevance_puts_the_better_transcript_first(fts_db):
    # Two voice notes say the word; the shorter transcript is the better match.
    media_notes.annotate_media(SHA_OTHER, "transcript", "chego domingo, levo o orcamento")
    assert relevance("orcamento") == ["m1", "v2", "v1"]


def test_the_relevance_cursor_walks_every_hit_once(fts_db):
    seen, cursor = [], None
    for _ in range(5):
        page = whatsapp.list_messages_page(
            query="orcamento", sort_by="relevance", include_context=False, limit=1, cursor=cursor
        )
        seen.extend(m["id"] for m in page.items)
        cursor = page.next_cursor
        if cursor is None:
            break
    assert seen == relevance("orcamento")


def test_the_index_is_rebuilt_from_notes_written_before_it_existed(fts_db):
    # A store from an older version has the transcript notes but no index.
    conn = sqlite3.connect(media_notes.notes_db_path())
    conn.executescript(f"DROP TABLE {media_notes.TRANSCRIPTS_FTS_TABLE}; DELETE FROM notes_meta;")
    conn.commit()
    conn.close()
    # First use notices the missing marker and fills the index from the notes.
    assert [sha for sha, _ in media_notes.transcript_matches("domingo")] == [SHA_OTHER]
    assert ids("orcamento") == ["m1", "v1"]


def _bulk_voice_notes(db_path, count: int, word: str) -> list[str]:
    """`count` voice notes in CHAT, each its own hash, all saying the same word."""
    shas = [f"{i:064x}" for i in range(count)]
    start = datetime(2024, 2, 1, 10, 0, 0)
    conn = sqlite3.connect(db_path)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_sha256) "
        "VALUES (?, ?, '111', '', ?, 0, 'audio', ?)",
        [(f"b{i}", CHAT, (start + timedelta(seconds=i)).isoformat(), bytes.fromhex(sha)) for i, sha in enumerate(shas)],
    )
    conn.commit()
    conn.close()
    whatsapp._reset_schema_cache()
    notes = sqlite3.connect(media_notes.notes_db_path())
    notes.executemany(
        "INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, 'transcript', ?, '2024-02-01T00:00:00')",
        [(sha, f"falamos de {word}") for sha in shas],
    )
    # Written behind media_notes' back, so the index has to be rebuilt.
    notes.execute("DELETE FROM notes_meta")
    notes.commit()
    notes.close()
    return shas


def _move_to_chat(db_path, sha: str, chat: str) -> None:
    conn = sqlite3.connect(db_path)
    conn.execute("UPDATE messages SET chat_jid = ? WHERE file_sha256 = ?", (chat, bytes.fromhex(sha)))
    conn.commit()
    conn.close()


def test_every_matching_transcript_is_counted_and_reachable_after_a_chat_filter(fts_db):
    # 5,001 voice notes say the same word: one more than the candidate cap that
    # used to be applied before the message filters ran. The count was 5,000, and
    # the chat holding only a dropped hash answered zero.
    count = 5001
    _bulk_voice_notes(fts_db, count, "jabuticaba")
    hits = media_notes.transcript_matches("jabuticaba")
    assert len(hits) == count
    assert whatsapp.count_messages(query="jabuticaba") == count
    # The hash the cap dropped is the worst-ranked one; give it a chat of its own.
    _move_to_chat(fts_db, hits[-1][0], OTHER)
    assert whatsapp.count_messages(query="jabuticaba", chat_jid=OTHER) == 1
    assert len(ids("jabuticaba", chat_jid=OTHER)) == 1
    assert whatsapp.count_messages(query="jabuticaba", chat_jid=CHAT) == count - 1
    # And paging through the whole set still reaches every hit exactly once.
    seen, cursor = [], None
    while True:
        page = whatsapp.list_messages_page(query="jabuticaba", include_context=False, limit=1000, cursor=cursor)
        seen.extend(m["id"] for m in page.items)
        cursor = page.next_cursor
        if cursor is None or not page.has_more:
            break
    assert len(seen) == len(set(seen)) == count


def _add_audio(db_path, mid: str, chat: str, sha: str, timestamp: str, content: str = "") -> None:
    conn = sqlite3.connect(db_path)
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_sha256) "
        "VALUES (?, ?, '111', ?, ?, 0, 'audio', ?)",
        (mid, chat, content, timestamp, bytes.fromhex(sha)),
    )
    conn.commit()
    conn.close()
    whatsapp._reset_schema_cache()


def test_the_same_file_in_two_chats_is_one_hit_per_message(fts_db):
    # The voice note was forwarded: one hash, two message rows, two hits — and
    # each chat filter sees only its own.
    _add_audio(fts_db, "v3", OTHER, SHA_VOICE, "2024-01-05T10:00:00")
    assert ids("orcamento") == ["m1", "v1", "v3"]
    assert whatsapp.count_messages(query="orcamento") == 3
    assert ids("orcamento", chat_jid=OTHER) == ["v3"]
    assert ids("orcamento", exclude_chat_jid=OTHER) == ["m1", "v1"]
    assert whatsapp.count_messages(query="orcamento", exclude_chat_jid=OTHER) == 2


def test_a_row_matching_content_and_transcript_is_counted_once(fts_db):
    # A voice note sent with a caption: messages_fts matches the caption and the
    # transcript index matches the same row's hash.
    _add_audio(fts_db, "v3", CHAT, SHA_VOICE, "2024-01-06T10:00:00", content="orcamento em anexo")
    assert ids("orcamento") == ["m1", "v1", "v3"]
    assert whatsapp.count_messages(query="orcamento") == 3


def test_time_bounds_apply_to_transcript_hits(fts_db):
    # m1 was written on Jan 1, v1 was spoken on Jan 3.
    assert ids("orcamento", after="2024-01-02T00:00:00") == ["v1"]
    assert whatsapp.count_messages(query="orcamento", after="2024-01-02T00:00:00") == 1
    assert ids("orcamento", before="2024-01-02T00:00:00") == ["m1"]
    assert whatsapp.count_messages(query="orcamento", before="2024-01-02T00:00:00") == 1


def test_the_substring_fallback_is_scoped_the_same_way(plain_db):
    assert ids("domingo", chat_jid=OTHER) == ["v2"]
    assert ids("domingo", chat_jid=CHAT) == []
    assert whatsapp.count_messages(query="domingo", chat_jid=OTHER) == 1
    assert whatsapp.count_messages(query="domingo", chat_jid=CHAT) == 0
    assert whatsapp.count_messages(query="orcamento", after="2024-01-02T00:00:00") == 1


def test_message_stats_counts_the_same_hits(fts_db):
    stats = whatsapp.message_stats(group_by="chat", query="orcamento")
    assert stats["total"]["messages"] == whatsapp.count_messages(query="orcamento") == 2
    assert {bucket["key"]: bucket["messages"] for bucket in stats["buckets"]} == {CHAT: 2}
    scoped = whatsapp.message_stats(group_by="chat", query="domingo", chat_jid=OTHER)
    assert scoped["total"]["messages"] == 1


def test_a_missing_notes_db_is_not_an_error(fts_db, monkeypatch):
    monkeypatch.setattr(media_notes, "notes_db_path", lambda: "/nope/notes.db")
    assert media_notes.transcript_matches("orcamento") == []
    assert ids("orcamento") == ["m1"]


def test_an_index_write_that_fails_invalidates_the_index(fts_db, monkeypatch):
    # A transient failure (a busy database, a build without fts5) must not leave
    # the note saying one thing and the index another: the marker goes with it,
    # so the next read rebuilds instead of answering from a stale index.
    notes_path = media_notes.notes_db_path()
    with monkeypatch.context() as broken:
        broken.setattr(media_notes, "_ensure_transcripts_fts", lambda conn: False)
        media_notes.annotate_media(SHA_OTHER, "transcript", "falamos de jabuticaba")
    notes = sqlite3.connect(notes_path)
    assert notes.execute("SELECT COUNT(*) FROM notes_meta").fetchone()[0] == 0
    notes.close()
    assert ids("jabuticaba") == ["v2"]
    assert ids("domingo") == []


# --- the index write path (one row per hash, reached by rowid) ---------------

FTS = media_notes.TRANSCRIPTS_FTS_TABLE
MAP = media_notes.TRANSCRIPTS_MAP_TABLE


def _notes_conn():
    return sqlite3.connect(media_notes.notes_db_path())


def _index_rows(sha: str) -> tuple[list[tuple[int, str]], list[int]]:
    """The index entries and the map entries for one hash."""
    conn = _notes_conn()
    try:
        entries = conn.execute(f"SELECT rowid, text FROM {FTS} WHERE sha256 = ?", (sha,)).fetchall()
        mapped = [row[0] for row in conn.execute(f"SELECT fts_rowid FROM {MAP} WHERE sha256 = ?", (sha,))]
    finally:
        conn.close()
    return entries, mapped


def _write_plans(sha: str, value: str) -> list[str]:
    """The query plans SQLite chooses for the statements one transcript write issues."""
    conn = _notes_conn()
    statements: list[str] = []
    conn.set_trace_callback(statements.append)
    media_notes.index_transcript(conn, sha, value)
    conn.commit()
    conn.set_trace_callback(None)
    plans = []
    for statement in statements:
        # fts5 traces its own internal statements with a "--" prefix; the ones
        # this module wrote are the rest.
        if statement.startswith("--") or not statement.lstrip().upper().startswith(("SELECT", "INSERT", "DELETE")):
            continue
        if FTS not in statement and MAP not in statement:
            continue
        plans.extend(row[3] for row in conn.execute("EXPLAIN QUERY PLAN " + statement))
    conn.close()
    return plans


@pytest.mark.skipif(
    sqlite3.sqlite_version_info < (3, 45),
    reason="fts5 only spells the plan it chose into the idxStr EXPLAIN prints from 3.45 on",
)
def test_replacing_a_transcript_never_scans_the_index(fts_db):
    _bulk_voice_notes(fts_db, 200, "jabuticaba")
    media_notes.transcript_matches("jabuticaba")  # the rebuild that fills the map
    plans = _write_plans(f"{7:064x}", "falamos de jabuticaba e tambem de manga")
    assert plans
    # "VIRTUAL TABLE INDEX 0:" is the full scan the hash lookup used to be;
    # "INDEX 0:=" is the same table reached by rowid.
    assert [plan for plan in plans if plan.endswith("VIRTUAL TABLE INDEX 0:")] == []
    assert any(plan.endswith("VIRTUAL TABLE INDEX 0:=") for plan in plans)
    assert any(f"SEARCH {MAP}" in plan for plan in plans)
    assert [plan for plan in plans if f"SCAN {MAP}" in plan] == []


def test_one_index_row_per_hash_through_replace_delete_and_rewrite(fts_db):
    entries, mapped = _index_rows(SHA_VOICE)
    assert len(entries) == 1 and mapped == [entries[0][0]]
    first_rowid = entries[0][0]

    media_notes.annotate_media(SHA_VOICE, "transcript", "agora falamos de jabuticaba")
    entries, mapped = _index_rows(SHA_VOICE)
    assert [row[0] for row in entries] == mapped == [first_rowid]
    assert "jabuticaba" in entries[0][1]
    # The replaced text is really gone, not merely outranked.
    assert ids("orcamento") == ["m1"]
    assert ids("jabuticaba") == ["v1"]

    media_notes.annotate_media(SHA_VOICE, "transcript", "")
    assert _index_rows(SHA_VOICE) == ([], [])
    assert ids("jabuticaba") == []

    media_notes.annotate_media(SHA_VOICE, "transcript", "falamos de novo de jabuticaba")
    entries, mapped = _index_rows(SHA_VOICE)
    assert len(entries) == 1 and mapped == [entries[0][0]]
    assert ids("jabuticaba") == ["v1"]


def test_a_store_whose_index_predates_the_map_is_rebuilt_with_it(fts_db):
    conn = _notes_conn()
    # What version 1 left behind: the index, no map, and its own marker.
    conn.executescript(f"DROP TABLE {MAP};")
    conn.execute("UPDATE notes_meta SET value = '1' WHERE key = ?", (media_notes._FTS_VERSION_KEY,))
    conn.commit()
    conn.close()

    assert ids("domingo") == ["v2"]  # first use notices the version and rebuilds

    conn = _notes_conn()
    try:
        assert conn.execute(
            "SELECT value FROM notes_meta WHERE key = ?", (media_notes._FTS_VERSION_KEY,)
        ).fetchone() == (media_notes._FTS_VERSION,)
        paired = conn.execute(
            f"SELECT f.sha256 FROM {FTS} f JOIN {MAP} m ON m.fts_rowid = f.rowid AND m.sha256 = f.sha256"
        ).fetchall()
        assert sorted(row[0] for row in paired) == sorted([SHA_OTHER, SHA_VOICE])
        assert conn.execute(f"SELECT COUNT(*) FROM {MAP}").fetchone()[0] == 2
    finally:
        conn.close()
    # And the next write still lands on the one row the rebuild mapped.
    media_notes.annotate_media(SHA_OTHER, "transcript", "chego domingo com jabuticaba")
    entries, mapped = _index_rows(SHA_OTHER)
    assert len(entries) == 1 and mapped == [entries[0][0]]
    assert ids("jabuticaba") == ["v2"]


def test_a_rolled_back_write_leaves_neither_an_entry_nor_a_map_row(fts_db):
    conn = _notes_conn()
    try:
        media_notes.index_transcript(conn, SHA_NEW, "falamos de jabuticaba")
        conn.rollback()
        assert conn.execute(f"SELECT COUNT(*) FROM {FTS} WHERE sha256 = ?", (SHA_NEW,)).fetchone()[0] == 0
        assert conn.execute(f"SELECT COUNT(*) FROM {MAP} WHERE sha256 = ?", (SHA_NEW,)).fetchone()[0] == 0
    finally:
        conn.close()
    assert ids("jabuticaba") == []


def test_deleting_a_transcript_whose_map_row_is_gone_still_clears_the_index(fts_db):
    # Nothing in this module can lose a map row (writes and rebuilds touch both
    # tables in one transaction), but a forgotten transcript must never stay
    # searchable, so the delete falls back to the sweep by hash.
    conn = _notes_conn()
    conn.execute(f"DELETE FROM {MAP} WHERE sha256 = ?", (SHA_VOICE,))
    conn.commit()
    conn.close()
    media_notes.annotate_media(SHA_VOICE, "transcript", "")
    assert _index_rows(SHA_VOICE) == ([], [])
    assert ids("orcamento") == ["m1"]
