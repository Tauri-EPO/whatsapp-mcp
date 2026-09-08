"""list_messages(include_context=True): bounded window reads, dedupe on (id, chat_jid)."""

import sqlite3
from datetime import UTC, datetime, timedelta

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
CREATE INDEX idx_messages_chat_timestamp ON messages(chat_jid, timestamp);
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
        rows.append(
            (f"a{i}", CHAT_A, f"A msg {i} {'hit' if i in (3, 7) else ''}", f"2024-01-{i:02d} 10:00:00+00:00", None)
        )
    # Same message ID "a3" also exists in chat B (forwarded): must be kept distinct.
    rows.append(("a3", CHAT_B, "B forwarded hit", "2024-02-01 10:00:00+00:00", None))
    rows.append(("b0", CHAT_B, "B before", "2024-01-31 10:00:00+00:00", None))
    rows.append(("b2", CHAT_B, "B after revoked", "2024-02-02 10:00:00+00:00", "2024-02-02 11:00:00+00:00"))
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


def test_context_uses_one_query_per_direction_regardless_of_hit_count(db):
    result = ids(query="hit", context_before=1, context_after=1, sort_by="oldest")
    # 3 hits (a3, a7 in A; a3 in B) -> 1 search query + 1 "before" + 1 "after".
    assert message_queries() == 3
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


def test_batching_keeps_each_hit_its_own_window(db, monkeypatch):
    # A hit is addressed by its position inside the batch, so the second batch
    # must not read the first batch's positions.
    monkeypatch.setattr(whatsapp, "_CONTEXT_HITS_PER_QUERY", 1)
    result = ids(query="hit", context_before=1, context_after=1, sort_by="oldest")
    assert message_queries() == 1 + 3 * 2  # one search + before/after per one-hit batch
    assert [r[0] for r in result] == ["a2", "a3", "a4", "a6", "a7", "a8", "b0", "a3", "b2"]


def test_a_repeated_hit_gets_one_window(db):
    conn = sqlite3.connect(db)
    try:
        cursor = conn.cursor()
        cursor.execute(
            f"SELECT {whatsapp.MESSAGE_COLUMNS} FROM messages JOIN chats ON chats.jid = messages.chat_jid"
            " WHERE messages.id = 'a3' AND messages.chat_jid = ?",
            (CHAT_A,),
        )
        hit = whatsapp._row_to_message(cursor.fetchone())
        windows = whatsapp._fetch_context_windows(cursor, [hit, hit], before=1, after=1)
    finally:
        conn.close()
    assert {key: ([m.id for m in b], [m.id for m in a]) for key, (b, a) in windows.items()} == {
        ("a3", CHAT_A): (["a2"], ["a4"])
    }


def test_one_sided_window_runs_a_single_context_query(db):
    result = ids(query="hit", chat_jid=CHAT_A, context_before=1, context_after=0, sort_by="oldest")
    assert message_queries() == 2
    assert [r[0] for r in result] == ["a2", "a3", "a6", "a7"]


# --- work bound (issue #312) ------------------------------------------------
#
# The old helper ranked the whole chat with ROW_NUMBER() before cutting the
# window, so a longer archive cost proportionally more even for the same 20
# hits and the same one neighbour each. These two tests are the guard: the
# window is seeked on idx_messages_chat_timestamp, and the VM instructions it
# takes do not follow the history.

CHAT_BIG = "999@s.whatsapp.net"


def _history_db(path, rows: int, chats: int = 1, analyze: bool = False) -> list[str]:
    """`chats` chats of `rows` messages, one per minute, every 50th one a hit.

    The hits sit mid-block (never first or last), so every one of them has a
    neighbour on both sides whatever the history length is.
    """
    jids = [CHAT_BIG] + [f"{900 + i}@s.whatsapp.net" for i in range(1, chats)]
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany("INSERT INTO chats (jid, name) VALUES (?, 'Big')", [(jid,) for jid in jids])
    start = datetime(2024, 1, 1, tzinfo=UTC)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, 's', ?, ?, 0)",
        [
            (
                f"m{i:07d}",
                jid,
                "hit" if i % 50 == 25 else f"msg {i}",
                (start + timedelta(minutes=i)).strftime("%Y-%m-%d %H:%M:%S+00:00"),
            )
            for jid in jids
            for i in range(rows)
        ],
    )
    if analyze:
        conn.execute("ANALYZE")
    conn.commit()
    conn.close()
    return jids


def _window_instructions(tmp_path, rows: int, hits: int = 20) -> tuple[int, dict]:
    """VM instructions _fetch_context_windows spends on `hits` hits, and its result."""
    path = tmp_path / f"history{rows}.db"
    _history_db(path, rows)
    conn = sqlite3.connect(path)
    try:
        cursor = conn.cursor()
        cursor.execute(
            f"SELECT {whatsapp.MESSAGE_COLUMNS} FROM messages JOIN chats ON chats.jid = messages.chat_jid"
            " WHERE messages.content = 'hit' ORDER BY messages.timestamp DESC, messages.id DESC LIMIT ?",
            (hits,),
        )
        anchors = [whatsapp._row_to_message(row) for row in cursor.fetchall()]
        assert len(anchors) == hits
        counted = 0

        def tick() -> int:
            nonlocal counted
            counted += 1
            return 0

        # The progress handler fires every 1000 VM instructions; counting the
        # calls is the portable stand-in for sqlite3_stmt_status().
        conn.set_progress_handler(tick, 1000)
        windows = whatsapp._fetch_context_windows(cursor, anchors, before=1, after=1)
        conn.set_progress_handler(None, 1000)
        return counted * 1000, {key: ([m.id for m in b], [m.id for m in a]) for key, (b, a) in windows.items()}
    finally:
        conn.close()


def test_window_work_does_not_grow_with_the_history(tmp_path):
    small, small_windows = _window_instructions(tmp_path, 1_000)
    large, large_windows = _window_instructions(tmp_path, 50_000)
    # Same shape of request, same amount of work: 50x the archive must not cost
    # 50x the instructions. The old ranking form did exactly that.
    assert large <= max(small, 20_000) * 2, f"{small} -> {large} VM instructions"
    # And the 20 newest hits of a longer archive still get their two neighbours.
    assert all(len(before) == 1 and len(after) == 1 for before, after in large_windows.values())
    assert all(len(before) == 1 and len(after) == 1 for before, after in small_windows.values())


@pytest.mark.parametrize("newest_first", [True, False])
def test_context_side_query_pins_the_indexed_join_order(tmp_path, newest_first):
    # Explained at the size and on the shape production uses: a full batch, more
    # than one chat, and a store where ANALYZE has run. With any of those absent
    # SQLite picks the right order by luck; with all three it used to drive from
    # `chats`, build an automatic index on messages.chat_jid and re-run the
    # neighbour subquery once per (chat message x hit) — 90 s for one batch.
    path = tmp_path / f"plan{newest_first}.db"
    jids = _history_db(path, 400, chats=3, analyze=True)
    batch = whatsapp._CONTEXT_HITS_PER_QUERY
    values = ",".join("(?, ?, ?)" for _ in range(batch))
    params: list = []
    for index in range(batch):
        params.extend([index, jids[index % len(jids)], "2024-01-01 10:00:00+00:00"])
    conn = sqlite3.connect(path)
    try:
        sql = whatsapp._context_side_sql(values, newest_first, include_deleted=True)
        plan = [row[3] for row in conn.execute("EXPLAIN QUERY PLAN " + sql, (*params, 5))]
    finally:
        conn.close()
    assert any("SEARCH neighbour USING INDEX idx_messages_chat_timestamp" in step for step in plan), plan
    # The hit list drives the loop and `messages` is reached by the rowids the
    # subquery returned. An AUTOMATIC index on messages, or `chats` opened before
    # the hits, is the reordering that made the subquery run per row.
    assert any(step.startswith("SEARCH messages USING INTEGER PRIMARY KEY") for step in plan), plan
    assert not any("AUTOMATIC" in step or step.startswith("SCAN messages") for step in plan), plan
    steps = [index for index, step in enumerate(plan) if step == "SCAN hits" or "chats" in step]
    assert plan[steps[0]] == "SCAN hits", plan
