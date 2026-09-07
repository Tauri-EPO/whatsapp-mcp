"""list_messages direction / media / group filters (issue #224)."""

import sqlite3

import pytest

import whatsapp

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
