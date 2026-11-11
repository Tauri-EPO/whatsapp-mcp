"""Edit metadata is readable on current and pre-migration archives."""

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from tests.conftest import ALICE, BOB
from untrusted import clean_untrusted


@pytest.mark.parametrize("schema", ["legacy", "timestamp-only", "current"])
def test_message_edit_rows(paired_dbs, schema, monkeypatch):
    with paired_dbs.messages() as conn:
        if schema != "legacy":
            conn.execute("ALTER TABLE messages ADD COLUMN message_edit_timestamp INTEGER NOT NULL DEFAULT 0")
        if schema == "current":
            conn.execute("ALTER TABLE messages ADD COLUMN original_content TEXT")
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me) "
            "VALUES ('EDITED',?,'5511999999999','newword','2026-10-08 10:00:00+00:00',0)",
            (ALICE,),
        )
        if schema != "legacy":
            conn.execute("UPDATE messages SET message_edit_timestamp=1791453660123")
        if schema == "current":
            conn.execute("UPDATE messages SET original_content=''")
    whatsapp._reset_schema_cache()
    expected = {
        "edited_at": "2026-10-08T10:01:00.123000+00:00" if schema != "legacy" else None,
        "original_content": "" if schema == "current" else None,
    }
    rows = main.list_messages(chat_jid=ALICE, include_context=False)["items"]
    context = main.get_message_context(message_id="EDITED", chat_jid=ALICE, before=0, after=0)
    assert {key: rows[0][key] for key in expected} == expected
    assert {key: context["message"][key] for key in expected} == expected
    compact = main.list_messages(chat_jid=ALICE, include_context=False, fields=list(expected))["items"]
    assert compact == [expected]
    # The MCP reader must never add columns to the bridge-owned archive.
    with paired_dbs.messages() as conn:
        columns = {row[1] for row in conn.execute("PRAGMA table_info(messages)")}
    assert ("original_content" in columns) == (schema == "current")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB]))
    assert main.list_messages(chat_jid=ALICE)["error"]["code"] == "denied"
    assert main.get_message_context(chat_jid=ALICE, message_id="EDITED")["error"]["code"] == "denied"


def test_edit_metadata_in_context_windows(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN message_edit_timestamp INTEGER NOT NULL DEFAULT 0")
        conn.execute("ALTER TABLE messages ADD COLUMN original_content TEXT")
        conn.executemany(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,message_edit_timestamp,original_content) "
            "VALUES (?,?,'5511999999999',?,?,0,?,?)",
            [
                ("BEFORE", ALICE, "newword", "2026-10-08 09:59:00+00:00", 1791453660123, "oldword"),
                ("TARGET", ALICE, "anchorword", "2026-10-08 10:00:00+00:00", 0, None),
                ("AFTER", ALICE, "plainword", "2026-10-08 10:01:00+00:00", 0, None),
            ],
        )
    whatsapp._reset_schema_cache()
    context = main.get_message_context(chat_jid=ALICE, message_id="TARGET", before=1, after=1)
    rows = main.list_messages(chat_jid=ALICE, query="anchorword", context_before=1, context_after=1)["items"]
    assert [row["id"] for row in rows] == ["BEFORE", "TARGET", "AFTER"]
    assert rows == [*context["before"], context["message"], *context["after"]]
    assert rows[0]["original_content"] == "oldword"
    assert rows[0]["edited_at"] == "2026-10-08T10:01:00.123000+00:00"
    assert all(row["edited_at"] is None and row["original_content"] is None for row in rows[1:])


def test_original_content_is_untrusted():
    assert clean_untrusted({"original_content": "oldword"}, wrap=True) == {
        "original_content": "<untrusted>oldword</untrusted>"
    }


@pytest.mark.parametrize("stamp", [0, -(2**63), 2**63 - 1])
def test_invalid_edit_time_does_not_hide_message(paired_dbs, stamp):
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN message_edit_timestamp INTEGER NOT NULL DEFAULT 0")
        conn.execute("ALTER TABLE messages ADD COLUMN original_content TEXT")
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,message_edit_timestamp,original_content) "
            "VALUES ('EDITED',?,'5511999999999','newword','2026-10-08 10:00:00+00:00',0,?,'oldword')",
            (ALICE, stamp),
        )
    whatsapp._reset_schema_cache()
    row = main.list_messages(chat_jid=ALICE, include_context=False)["items"][0]
    assert row["content"] == "newword" and row["original_content"] == "oldword"
    assert row["edited_at"] is None
    context = main.get_message_context(chat_jid=ALICE, message_id="EDITED", before=0, after=0)
    assert context["message"]["edited_at"] is None
