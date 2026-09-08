"""list_messages direction / media / group filters (issue #224) and the
timestamp bounds every messages query shares (issues #253, #270)."""

import json
import sqlite3
from datetime import UTC, datetime, timedelta, timezone

import pytest

import whatsapp

A = "111@s.whatsapp.net"
B = "222@s.whatsapp.net"
G = "120363000000000009@g.us"

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT,
    quoted_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
-- The bridge creates these; the bound test below asserts one of them is used.
CREATE INDEX idx_messages_timestamp ON messages(timestamp);
CREATE INDEX idx_messages_chat_timestamp ON messages(chat_jid, timestamp);
"""

# (id, chat, from_me, media_type)
ROWS = [
    ("a1", A, 0, None),
    ("a2", A, 1, None),
    ("a3", A, 0, "image"),
    ("a4", A, 1, "document"),
    ("a5", A, 0, "reaction"),
    ("b1", B, 0, "audio"),
    ("g1", G, 0, None),
    ("g2", G, 1, "image"),
]


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, ?, ?)",
        [(A, "A", "2024-01-09T10:00:00", "2024-01-02T00:00:00"), (B, "B", None, None), (G, "Group", None, None)],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        [
            (mid, chat, "s", f"msg {mid}", f"2024-01-0{i + 1}T10:00:00", from_me, media)
            for i, (mid, chat, from_me, media) in enumerate(ROWS)
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    yield path
    whatsapp._reset_schema_cache()


def ids(**kwargs) -> list[str]:
    kwargs.setdefault("include_context", False)
    kwargs.setdefault("sort_by", "oldest")
    return [m["id"] for m in whatsapp.list_messages(limit=100, **kwargs)]


def test_no_filters_returns_everything(db):
    assert ids() == [row[0] for row in ROWS]


def test_from_me_true_and_false(db):
    assert ids(from_me=True) == ["a2", "a4", "g2"]
    assert ids(from_me=False) == ["a1", "a3", "a5", "b1", "g1"]


def test_has_media_ignores_pointer_rows(db):
    # a5 is a reaction: media_type is set but there is no file.
    assert ids(has_media=True) == ["a3", "a4", "b1", "g2"]
    assert ids(has_media=False) == ["a1", "a2", "a5", "g1"]


def test_media_type(db):
    assert ids(media_type="document") == ["a4"]
    assert ids(media_type="image") == ["a3", "g2"]


def test_exclude_groups(db):
    assert ids(exclude_groups=True) == ["a1", "a2", "a3", "a4", "a5", "b1"]


def test_filters_combine(db):
    assert ids(from_me=False, has_media=True, exclude_groups=True) == ["a3", "b1"]
    assert ids(from_me=True, media_type="image", chat_jid=G) == ["g2"]


def test_unread_only_implies_inbound_and_combines(db):
    # A's read marker is 2024-01-02; B and G were never read.
    assert ids(unread_only=True) == ["a3", "a5", "b1", "g1"]
    assert ids(unread_only=True, has_media=True) == ["a3", "b1"]
    assert ids(unread_only=True, from_me=False) == ["a3", "a5", "b1", "g1"]


def test_unread_only_with_from_me_is_a_validation_error(db):
    with pytest.raises(whatsapp.ToolError) as exc:
        ids(unread_only=True, from_me=True)
    assert exc.value.code == "invalid_argument"


def test_unknown_media_type_is_rejected(db):
    with pytest.raises(whatsapp.ToolError) as exc:
        ids(media_type="spreadsheet")
    assert exc.value.code == "invalid_argument"


def test_media_type_contradicting_has_media_is_rejected(db):
    with pytest.raises(whatsapp.ToolError) as exc:
        ids(media_type="image", has_media=False)
    assert exc.value.code == "invalid_argument"


def test_tool_reports_the_validation_error_in_the_envelope(db):
    import main

    body = main.list_messages(unread_only=True, from_me=True)
    assert body["error"]["code"] == "invalid_argument"


def test_tool_forwards_the_new_filters(monkeypatch):
    import main

    seen = {}
    monkeypatch.setattr(main, "whatsapp_list_messages", lambda **kw: seen.update(kw) or [])
    main.list_messages(from_me=True, has_media=True, media_type="image", exclude_groups=True)
    assert seen["from_me"] is True
    assert seen["has_media"] is True
    assert seen["media_type"] == "image"
    assert seen["exclude_groups"] is True


# --- timestamp bounds against the canonical stored spelling (#253, #270) ---


def stored(canonical: str) -> str:
    """A "YYYY-MM-DD HH:MM:SS" instant as messages.timestamp holds it.

    One spelling: UTC with an explicit offset, written by the bridge and, for
    rows older releases wrote differently, by its startup migration.
    """
    return f"{canonical}+00:00"


# One inbound message per chat, all at 10:00 on consecutive days, so every tool
# answers a bound with the same five-element set: message ids for list_messages
# and export, bucket keys for message_stats, chat jids for the unread and
# unanswered lists.
BOUND_IDS = ["m1", "m2", "m3", "m4", "m5"]
BOUND_CHATS = [f"{i}00@s.whatsapp.net" for i in range(1, 6)]
BOUND_DAYS = [f"2024-01-0{i}" for i in range(1, 6)]


@pytest.fixture
def bounds_db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, NULL, NULL)",
        [(jid, f"Chat {mid}") for jid, mid in zip(BOUND_CHATS, BOUND_IDS, strict=True)],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
        [
            (mid, jid, jid.split("@")[0], f"msg {mid}", stored(f"{day} 10:00:00"))
            for mid, jid, day in zip(BOUND_IDS, BOUND_CHATS, BOUND_DAYS, strict=True)
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.delenv("WHATSAPP_EXPORT_DIR", raising=False)
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    yield path
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()


# (after, before, the ids between them). Both bounds are strict, so one naming
# the exact instant of a message excludes that message.
BOUND_CASES = [
    ("2024-01-03", None, ["m3", "m4", "m5"]),
    ("2024-01-03T10:00:00", None, ["m4", "m5"]),
    (None, "2024-01-03", ["m1", "m2"]),
    (None, "2024-01-03T10:00:00", ["m1", "m2"]),
    ("2024-01-02", "2024-01-05", ["m2", "m3", "m4"]),
]
LOWER_BOUND_CASES = [(after, expected) for after, before, expected in BOUND_CASES if before is None]


def chats_of(message_ids: list[str]) -> list[str]:
    return sorted(BOUND_CHATS[BOUND_IDS.index(mid)] for mid in message_ids)


@pytest.mark.parametrize(("after", "before", "expected"), BOUND_CASES)
def test_list_messages_bounds(bounds_db, after, before, expected):
    assert ids(after=after, before=before) == expected


def test_a_lower_bound_seeks_the_timestamp_index(bounds_db):
    """The bound binds against the raw column, so SQLite can seek (#270).

    An expression over the column (the substring normalisation this replaced)
    turns every range into a scan of the whole table.
    """
    conn = sqlite3.connect(bounds_db)
    try:
        clauses, params = whatsapp.MessageFilters(after="2024-01-03").build(conn.cursor())
        plan = conn.execute(
            f"EXPLAIN QUERY PLAN SELECT messages.id FROM messages WHERE {' AND '.join(clauses)}",  # noqa: S608
            params,
        ).fetchall()
    finally:
        conn.close()
    detail = " ".join(row[3] for row in plan)
    assert "SEARCH" in detail, detail
    assert "SCAN" not in detail, detail


@pytest.mark.parametrize(("after", "before", "expected"), BOUND_CASES)
def test_message_stats_agrees_with_list_messages(bounds_db, after, before, expected):
    stats = whatsapp.message_stats(group_by="chat", after=after, before=before)
    assert stats["total"]["messages"] == len(expected)
    assert sorted(bucket["key"] for bucket in stats["buckets"]) == chats_of(expected)


@pytest.mark.parametrize(("after", "before", "expected"), BOUND_CASES)
def test_export_messages_agrees_with_list_messages(bounds_db, after, before, expected):
    import export

    result = export.export_messages(after=after, before=before, out_path="bounds.ndjson")
    assert result["count"] == len(expected)
    with open(result["path"], encoding="utf-8") as handle:
        assert [json.loads(line)["id"] for line in handle] == expected


@pytest.mark.parametrize(("since", "expected"), LOWER_BOUND_CASES)
def test_list_unread_since_agrees_with_list_messages(bounds_db, since, expected):
    unread = whatsapp.list_unread(since=since)
    assert sorted(chat["chat_jid"] for chat in unread["chats"]) == chats_of(expected)
    assert whatsapp.list_unread(since=since, count_only=True)["count"] == len(expected)


@pytest.mark.parametrize(("since", "expected"), LOWER_BOUND_CASES)
def test_list_unanswered_since_agrees_with_list_messages(bounds_db, since, expected):
    assert sorted(chat["jid"] for chat in whatsapp.list_unanswered(since=since)) == chats_of(expected)


class FrozenDatetime(datetime):
    """datetime.now() pinned to 2024-01-05 10:00:01 UTC, one second after m5."""

    @classmethod
    def now(cls, tz=None):
        return datetime(2024, 1, 5, 10, 0, 1, tzinfo=tz)


def test_max_age_days_is_the_relative_spelling_of_since(bounds_db, monkeypatch):
    # Now is one second after m5, so a one-day window starts just after m4.
    monkeypatch.setattr(whatsapp, "datetime", FrozenDatetime)
    assert whatsapp.list_unread(max_age_days=1, count_only=True)["count"] == 1


def test_min_age_hours_measures_the_stored_instant(tmp_path, monkeypatch):
    # Two inbound messages, one 3 h old and one 30 min old, spelled the way the
    # bridge writes them: min_age_hours=2 leaves only the older.
    now = datetime.now(UTC).replace(microsecond=0)

    def written(delta):
        return whatsapp.timestamp_bound(now - delta)

    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, NULL, NULL)",
        [(A, "Old"), (B, "Fresh")],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
        [
            ("old", A, "111", "old", written(timedelta(hours=3))),
            ("fresh", B, "222", "fresh", written(timedelta(minutes=30))),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    try:
        assert [chat["jid"] for chat in whatsapp.list_unanswered(min_age_hours=2)] == [A]
    finally:
        whatsapp._reset_schema_cache()
        whatsapp._reset_name_cache()


def test_a_bound_is_rendered_in_the_stored_spelling():
    """Any zone, one bound: UTC with an explicit offset, to the second (#270)."""
    utc = datetime(2024, 1, 3, 12, 0, 0, tzinfo=UTC)
    assert whatsapp.timestamp_bound(utc) == "2024-01-03 12:00:00+00:00"
    assert whatsapp.timestamp_bound(utc.astimezone(timezone(timedelta(hours=-3)))) == "2024-01-03 12:00:00+00:00"
    assert whatsapp.timestamp_bound(utc.astimezone(timezone(timedelta(hours=5, minutes=30)))) == (
        "2024-01-03 12:00:00+00:00"
    )
    # A naive bound is read as UTC, which is what the archive holds.
    assert whatsapp.timestamp_bound(datetime(2024, 1, 3, 12, 0, 0)) == "2024-01-03 12:00:00+00:00"


@pytest.mark.parametrize(
    ("spelled", "expected"),
    [
        ("2024-01-03 12:00:00+00:00", datetime(2024, 1, 3, 12, 0, tzinfo=UTC)),  # canonical
        ("2024-01-03T09:00:00.123456-03:00", datetime(2024, 1, 3, 12, 0, 0, 123456, tzinfo=UTC)),
        ("2024-01-03 09:00:00 -0300 -03", datetime(2024, 1, 3, 12, 0, tzinfo=UTC)),  # Go String()
        ("2024-01-03 12:00:00 +0000 UTC m=+0.001", datetime(2024, 1, 3, 12, 0, tzinfo=UTC)),
        ("2024-01-03T12:00:00Z", datetime(2024, 1, 3, 12, 0, tzinfo=UTC)),
        ("2024-01-03 12:00:00", datetime(2024, 1, 3, 12, 0, tzinfo=UTC)),  # no offset: UTC
    ],
)
def test_parse_db_time_reads_every_spelling_a_release_wrote(spelled, expected):
    assert whatsapp.parse_db_time(spelled) == expected


@pytest.mark.parametrize("bad", ["", "   ", "yesterday", "1704283200"])
def test_parse_db_time_rejects_a_non_timestamp(bad):
    with pytest.raises(ValueError):
        whatsapp.parse_db_time(bad)
