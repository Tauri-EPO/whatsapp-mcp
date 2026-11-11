"""Forward evidence survives SQLite -> shared projection -> MCP output."""

import pytest

import main
import whatsapp


def test_forwarded_rows_and_context(paired_dbs):
    chat = "120363000000000001@g.us"
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN forwarding_score INTEGER")
        conn.execute("DELETE FROM messages")
        for index, score in enumerate([None, 0, 1, 3, 4, 5]):
            conn.execute(
                "INSERT INTO messages (id,chat_jid,sender,content,timestamp,is_from_me,forwarding_score) "
                "VALUES (?,?,?,?,?,?,?)",
                (f"F{index}", chat, "5511999999999", "forward", f"2026-11-01 00:00:0{index}+00:00", False, score),
            )
    whatsapp._reset_schema_cache()
    result = main.list_messages(chat_jid=chat, include_context=False, sort_by="oldest")
    rows = result["items"]
    assert [r["forwarding_score"] for r in rows] == [None, 0, 1, 3, 4, 5]
    assert [r["forwarded"] for r in rows] == [False, True, True, True, True, True]
    assert [r["frequently_forwarded"] for r in rows] == [False, False, False, False, False, True]
    context = main.get_message_context(chat_jid=chat, message_id="F5")
    assert context["message"]["frequently_forwarded"] is True
    shaped = main.list_messages(chat_jid=chat, include_context=False, fields=["id", "forwarded"])
    assert all(set(r) == {"id", "forwarded"} for r in shaped["items"])


def test_legacy_archive_has_unknown_forwarding(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id,chat_jid,sender,content,timestamp,is_from_me) VALUES (?,?,?,?,?,?)",
            ("OLD", "5511999999999@s.whatsapp.net", "5511999999999", "original", "2026-11-01 00:00:00+00:00", False),
        )
    rows = whatsapp.list_messages(include_context=False)
    assert rows
    assert all(r["forwarding_score"] is None and not r["forwarded"] and not r["frequently_forwarded"] for r in rows)


@pytest.mark.parametrize("length", [0, 1, 15, 19, 21])
def test_malformed_projection_is_refused(length):
    row = (
        "2026-11-01 00:00:00+00:00",
        "5511999999999",
        "Alice",
        "text",
        False,
        "5511999999999@s.whatsapp.net",
        "BAD",
        "",
        None,
        None,
        None,
        False,
        None,
        None,
        None,
        None,
        None,
        None,
        None,
        None,
    )
    row = (*row, None)  # 20 columns is now the complete edit + forwarding projection.
    with pytest.raises(ValueError):
        whatsapp._row_to_message(row[:length])


@pytest.mark.parametrize("length", [16, 17, 18, 20])
def test_known_projection_versions(length):
    row = (
        "2026-11-01 00:00:00+00:00",
        "5511999999999",
        "Alice",
        "text",
        False,
        "5511999999999@s.whatsapp.net",
        "LEGACY",
        "",
        None,
        None,
        None,
        False,
        None,
        None,
        None,
        None,
        None,
        None,
    )
    message = whatsapp._row_to_message((*row, None, None)[:length])
    assert message.id == "LEGACY"
    assert message.forwarding_score is None
    assert message.edited_at is None and message.original_content is None


@pytest.mark.parametrize(
    "missing",
    [(), ("sender_server",), ("location",), ("forwarding_score",), ("sender_server", "location", "forwarding_score")],
)
def test_projection_schema_is_probed_once(paired_dbs, missing):
    with paired_dbs.messages() as conn:
        for column in ("location", "forwarding_score"):
            conn.execute(f"ALTER TABLE messages ADD COLUMN {column}")
        for column in missing:
            conn.execute(f"ALTER TABLE messages DROP COLUMN {column}")
        conn.commit()
        whatsapp._reset_schema_cache()
        observed = []
        conn.set_trace_callback(observed.append)
        for _ in range(2):
            columns = whatsapp.message_columns(conn.cursor())
            for column in ("sender_server", "location", "forwarding_score"):
                assert (f"messages.{column}" in columns) is (column not in missing)
        assert observed.count("PRAGMA table_info(messages)") == 1
        assert observed.count("PRAGMA main.schema_version") == 2
