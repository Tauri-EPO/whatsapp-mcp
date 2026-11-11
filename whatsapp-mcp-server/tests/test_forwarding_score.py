"""Forward evidence survives SQLite -> shared projection -> MCP output."""

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
    assert [r["frequently_forwarded"] for r in rows] == [False, False, False, False, True, True]
    context = main.get_message_context(chat_jid=chat, message_id="F4")
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
