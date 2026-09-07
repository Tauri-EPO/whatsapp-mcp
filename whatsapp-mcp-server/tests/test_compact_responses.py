"""Compact reads: fields, omit_nulls, max_content_chars and count_only (issue #223)."""

import json
import sqlite3
from datetime import datetime, timedelta

import pytest

import main
import whatsapp
from tests.conftest import MESSAGES_SCHEMA

A, G = "5511111111111@s.whatsapp.net", "120363000000000001@g.us"
PAGE = 500


def _dumped(rows) -> int:
    """Bytes the rows cost on the wire, the way an MCP result is serialised."""
    return len(json.dumps(rows, ensure_ascii=False).encode("utf-8"))


@pytest.fixture
def db(tmp_path, monkeypatch):
    """One chat with a 500-message page: odd rows inbound, even rows sent by us."""
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.execute("INSERT INTO chats VALUES (?, 'Alice', '2026-09-04 10:00:00', NULL)", (A,))
        c.execute("INSERT INTO chats VALUES (?, 'Group', '2026-09-03 10:00:00', NULL)", (G,))
        start = datetime(2026, 9, 4, 0, 0, 0)
        for i in range(PAGE):
            inbound = i % 2 == 1
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) "
                "VALUES (?,?,?,?,?,?,?)",
                (
                    f"m{i:04d}",
                    A,
                    A if inbound else "me",
                    f"mensagem numero {i} sobre o orcamento de setembro",
                    (start + timedelta(minutes=i)).strftime("%Y-%m-%d %H:%M:%S"),
                    0 if inbound else 1,
                    None,
                ),
            )
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES "
            "('g1', ?, ?, 'oi', '2026-09-03 10:00:00', 0)",
            (G, G),
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


# --- the size regression the issue is about -----------------------------------


def test_omit_nulls_shrinks_a_500_message_page(db):
    full = main.list_messages(chat_jid=A, limit=PAGE, include_context=False)
    compact = main.list_messages(chat_jid=A, limit=PAGE, include_context=False, omit_nulls=True)
    assert len(full["items"]) == len(compact["items"]) == PAGE

    before, after = _dumped(full["items"]), _dumped(compact["items"])
    reduction = 1 - after / before
    assert reduction >= 0.30, f"omit_nulls only saved {reduction:.1%} ({before} -> {after} bytes)"
    # Same information, fewer keys: nothing that carried a value was dropped.
    assert [row["content"] for row in compact["items"]] == [row["content"] for row in full["items"]]


def test_fields_shrinks_further_than_omit_nulls(db):
    compact = main.list_messages(chat_jid=A, limit=PAGE, include_context=False, omit_nulls=True)
    projected = main.list_messages(
        chat_jid=A, limit=PAGE, include_context=False, fields=["timestamp", "sender_phone", "content"]
    )
    assert _dumped(projected["items"]) < _dumped(compact["items"])


# --- fields -------------------------------------------------------------------


def test_fields_projects_and_keeps_order(db):
    out = main.list_messages(chat_jid=A, limit=3, include_context=False, fields=["id", "content"])
    assert all(set(row) == {"id", "content"} for row in out["items"])
    assert out["has_more"] is True and out["next_cursor"]


def test_unknown_field_lists_the_valid_names(db):
    out = main.list_messages(chat_jid=A, limit=1, include_context=False, fields=["id", "sender_number"])
    assert out["error"]["code"] == "invalid_argument"
    assert "sender_number" in out["error"]["message"]
    assert "sender_phone" in out["error"]["message"] and "content" in out["error"]["message"]


def test_notes_and_transcript_are_valid_field_names(db):
    out = main.list_messages(chat_jid=A, limit=1, include_context=False, fields=["notes", "transcript", "sha256"])
    assert "error" not in out


def test_fields_must_be_a_non_empty_list(db):
    assert main.list_messages(chat_jid=A, fields=[])["error"]["code"] == "invalid_argument"
    assert main.list_messages(chat_jid=A, fields="content")["error"]["code"] == "invalid_argument"


def test_message_fields_are_derived_from_the_converted_row(db):
    row = main.list_messages(chat_jid=A, limit=1, include_context=False)["items"][0]
    assert set(row) <= set(whatsapp.MESSAGE_FIELDS)
    assert {"notes", "transcript", "content_truncated"} <= set(whatsapp.MESSAGE_FIELDS)


# --- max_content_chars --------------------------------------------------------


def test_max_content_chars_truncates_and_flags(db):
    out = main.list_messages(chat_jid=A, limit=5, include_context=False, max_content_chars=10)
    for row in out["items"]:
        assert row["content"] == "mensagem n" and row["content_truncated"] is True


def test_short_content_is_not_flagged(db):
    out = main.list_messages(chat_jid=G, limit=1, include_context=False, max_content_chars=10)
    assert out["items"][0]["content"] == "oi" and "content_truncated" not in out["items"][0]


def test_truncation_flag_survives_a_projection(db):
    out = main.list_messages(chat_jid=A, limit=1, include_context=False, fields=["content"], max_content_chars=8)
    assert out["items"][0] == {"content": "mensagem", "content_truncated": True}


def test_max_content_chars_must_be_positive(db):
    assert main.list_messages(chat_jid=A, max_content_chars=0)["error"]["code"] == "invalid_argument"


# --- omit_nulls ---------------------------------------------------------------


def test_omit_nulls_drops_only_empty_values(db):
    row = main.list_messages(chat_jid=A, limit=1, include_context=False, omit_nulls=True)["items"][0]
    assert "media_type" not in row and "deleted_at" not in row and "view_once" not in row
    assert row["chat_name"] == "Alice" and row["id"] == "m0499"
    # is_from_me=False is an answer, but an absent key reads the same way: the
    # inbound row keeps only what it carries.
    assert "is_from_me" not in row


def test_omit_nulls_keeps_true_flags(db):
    outbound = main.list_messages(chat_jid=A, limit=1, include_context=False, from_me=True, omit_nulls=True)["items"][0]
    assert outbound["is_from_me"] is True


# --- count_only ---------------------------------------------------------------


def test_count_only_matches_the_same_filters(db):
    assert main.list_messages(chat_jid=A, count_only=True) == {"count": PAGE}
    assert main.list_messages(chat_jid=A, from_me=False, count_only=True) == {"count": PAGE // 2}
    assert main.list_messages(query="orcamento", count_only=True)["count"] == PAGE
    assert main.list_messages(chat_jid=A, after="2026-09-04T01:00:00", count_only=True)["count"] == PAGE - 61
    assert main.list_messages(exclude_groups=True, count_only=True) == {"count": PAGE}


def test_count_only_ignores_limit(db):
    assert main.list_messages(chat_jid=A, limit=5, count_only=True) == {"count": PAGE}


def test_count_only_refuses_fields_and_pagination(db):
    for kwargs in ({"fields": ["id"]}, {"cursor": "abc"}, {"page": 2}):
        out = main.list_messages(chat_jid=A, count_only=True, **kwargs)
        assert out["error"]["code"] == "invalid_argument"
        assert next(iter(kwargs)) in out["error"]["message"]


def test_count_only_propagates_filter_errors(db):
    out = main.list_messages(unread_only=True, from_me=True, count_only=True)
    assert out["error"]["code"] == "invalid_argument"


# --- the other bulk reads -----------------------------------------------------


def test_get_message_context_shapes_every_row(db):
    out = main.get_message_context(A, "m0100", before=2, after=2, fields=["id", "content"], max_content_chars=8)
    assert out["message"] == {"id": "m0100", "content": "mensagem", "content_truncated": True}
    assert [row["id"] for row in out["before"]] == ["m0099", "m0098"]
    assert all(set(row) == {"id", "content", "content_truncated"} for row in out["after"])
    assert main.get_message_context(A, "m0100", fields=["nope"])["error"]["code"] == "invalid_argument"


def test_list_unread_shapes_and_counts(db):
    shaped = main.list_unread(limit_per_chat=3, fields=["id", "content"], omit_nulls=True)
    rows = shaped["chats"][0]["messages"]
    assert len(rows) == 3 and all(set(row) == {"id", "content"} for row in rows)

    counted = main.list_unread(count_only=True)
    assert counted == {"count": PAGE // 2 + 1, "chats_with_unread": 2}
    assert "chats" not in counted
    assert main.list_unread(count_only=True, exclude_groups=True) == {"count": PAGE // 2, "chats_with_unread": 1}
    assert main.list_unread(count_only=True, fields=["id"])["error"]["code"] == "invalid_argument"


def test_list_unanswered_shapes_chat_rows(db):
    out = main.list_unanswered(fields=["jid", "last_inbound_time"])
    assert out["items"] == [
        {"jid": A, "last_inbound_time": "2026-09-04 08:19:00"},
        {"jid": G, "last_inbound_time": "2026-09-03 10:00:00"},
    ]
    # Chat rows, so the valid names are the chat ones: a message key is refused.
    error = main.list_unanswered(fields=["content"])["error"]
    assert error["code"] == "invalid_argument" and "last_message" in error["message"]

    assert main.list_unanswered(count_only=True) == {"count": 2}
    assert main.list_unanswered(count_only=True, exclude_groups=True) == {"count": 1}
    assert main.list_unanswered(count_only=True, cursor="abc")["error"]["code"] == "invalid_argument"


def test_list_unanswered_omit_nulls(db):
    row = main.list_unanswered(omit_nulls=True)["items"][0]
    assert "last_read_time" not in row and "unread" in row
    assert row["name"] == "Alice" and row["jid"] == A
