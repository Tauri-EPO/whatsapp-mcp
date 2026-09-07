"""message_stats: counts by chat, day, month and sender (issue #228)."""

import sqlite3

import pytest

import whatsapp
from chat_policy import load_chat_policy

A = "111@s.whatsapp.net"
B = "222@s.whatsapp.net"
G = "120363000000000009@g.us"

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT,
    quoted_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
"""

# (id, chat, sender, timestamp, is_from_me, media_type)
ROWS = [
    ("a1", A, "111", "2024-01-01T10:00:00", 0, None),
    ("a2", A, "me", "2024-01-01T11:00:00", 1, None),
    ("a3", A, "111", "2024-01-02T10:00:00", 0, "image"),
    ("a4", A, "111", "2024-02-03T10:00:00", 0, "reaction"),
    ("b1", B, "222", "2024-01-02T12:00:00", 0, "document"),
    ("g1", G, "333", "2024-02-05T10:00:00", 0, None),
    ("g2", G, "me", "2024-02-05T11:00:00", 1, "image"),
]


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, ?, ?)",
        [(A, "Alice", None, None), (B, "Bob", None, None), (G, "Group", None, None)],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, timestamp, is_from_me, media_type, content) "
        "VALUES (?, ?, ?, ?, ?, ?, 'x')",
        ROWS,
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    yield path
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()


def keys(result) -> list:
    return [b["key"] for b in result["buckets"]]


def test_group_by_chat(db):
    result = whatsapp.message_stats(group_by="chat")
    assert keys(result) == [A, G, B]
    alice = result["buckets"][0]
    assert alice["label"] == "Alice"
    assert (alice["messages"], alice["from_me"], alice["inbound"], alice["media"]) == (4, 1, 3, 1)
    assert alice["first_timestamp"] == "2024-01-01T10:00:00"
    assert alice["last_timestamp"] == "2024-02-03T10:00:00"


def test_totals_cover_every_matching_message(db):
    total = whatsapp.message_stats(group_by="chat")["total"]
    assert total == {
        "buckets": 3,
        "messages": 7,
        "from_me": 2,
        "inbound": 5,
        "media": 3,  # a3, b1, g2 — the reaction pointer row is not media
        "first_timestamp": "2024-01-01T10:00:00",
        "last_timestamp": "2024-02-05T11:00:00",
    }


def test_group_by_day_and_month(db):
    by_day = whatsapp.message_stats(group_by="day")
    assert dict((b["key"], b["messages"]) for b in by_day["buckets"]) == {
        "2024-01-01": 2,
        "2024-01-02": 2,
        "2024-02-05": 2,
        "2024-02-03": 1,
    }
    by_month = whatsapp.message_stats(group_by="month")
    assert [(b["key"], b["messages"]) for b in by_month["buckets"]] == [("2024-01", 4), ("2024-02", 3)]


def test_group_by_sender(db):
    result = whatsapp.message_stats(group_by="sender")
    assert dict((b["key"], b["messages"]) for b in result["buckets"]) == {"111": 3, "me": 2, "222": 1, "333": 1}


def test_ordered_by_count_desc_and_capped(db):
    result = whatsapp.message_stats(group_by="chat", limit=1)
    counts = [b["messages"] for b in result["buckets"]]
    assert counts == [4]
    assert result["truncated"] is True
    assert result["total"]["buckets"] == 3
    assert result["total"]["messages"] == 7  # totals ignore the cap


@pytest.mark.parametrize(
    "filters",
    [
        {},
        {"from_me": True},
        {"has_media": True},
        {"media_type": "image"},
        {"exclude_groups": True},
        {"chat_jid": A},
        {"after": "2024-01-15"},
        {"before": "2024-01-15", "from_me": False},
    ],
)
def test_counts_match_list_messages_for_the_same_filters(db, filters):
    listed = whatsapp.list_messages(limit=100, include_context=False, **filters)
    stats = whatsapp.message_stats(group_by="chat", **filters)
    assert stats["total"]["messages"] == len(listed)
    assert stats["total"]["from_me"] == sum(1 for m in listed if m["is_from_me"])
    assert stats["total"]["inbound"] == sum(1 for m in listed if not m["is_from_me"])
    assert sum(b["messages"] for b in stats["buckets"]) == len(listed)


def test_allow_list_filters_buckets_and_refuses_a_blocked_chat(db, monkeypatch):
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", A)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", load_chat_policy())
    assert keys(whatsapp.message_stats(group_by="chat")) == [A]
    with pytest.raises(whatsapp.ToolError) as exc:
        whatsapp.message_stats(group_by="chat", chat_jid=B)
    assert exc.value.code == "denied"


def test_unknown_grouping_is_rejected(db):
    with pytest.raises(whatsapp.ToolError) as exc:
        whatsapp.message_stats(group_by="week")
    assert exc.value.code == "invalid_argument"


def test_tool_returns_the_envelope_for_a_bad_grouping(db):
    import main

    assert main.message_stats(group_by="week")["error"]["code"] == "invalid_argument"


def test_tool_forwards_arguments(monkeypatch):
    import main

    seen = {}
    monkeypatch.setattr(main, "whatsapp_message_stats", lambda **kw: seen.update(kw) or {})
    main.message_stats(group_by="sender", sender_jid="111", limit=5, exclude_groups=True)
    assert seen["group_by"] == "sender"
    assert seen["sender_phone_number"] == "111"
    assert seen["limit"] == 5
    assert seen["exclude_groups"] is True
