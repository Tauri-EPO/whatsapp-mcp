import sqlite3

import pytest

import notes
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

DM = "5511888888888@s.whatsapp.net"
GROUP = "120363000000000002@g.us"
AMBIGUOUS = [DM + "@g.us", GROUP + "@s.whatsapp.net", GROUP + "@g.us", DM + "@s.whatsapp.net"]


@pytest.mark.parametrize(
    "entries", [[], ["*@g.us"], ["*@s.whatsapp.net"], [DM + "@g.us"], ["5511888888888:1@lid@s.whatsapp.net"]]
)
def test_policy_and_sql_refuse_ambiguous_rows(entries):
    policy = ChatPolicy.from_entries(entries)
    assert policy.restricted == bool(entries)
    assert all(not policy.allows(jid) for jid in AMBIGUOUS)
    if entries == ["5511888888888:1@lid@s.whatsapp.net"]:
        assert not policy.allows(DM)
    with sqlite3.connect(":memory:") as db:
        db.execute("CREATE TABLE chats (jid TEXT)")
        db.executemany("INSERT INTO chats VALUES (?)", [(jid,) for jid in [DM, GROUP, *AMBIGUOUS]])
        clause, params = policy.sql_clause("jid")
        actual = {row[0] for row in db.execute(f"SELECT jid FROM chats WHERE {clause}", params)}
    assert actual == {jid for jid in (DM, GROUP) if policy.allows(jid)}


def test_invalid_exact_entry_cannot_authorize_a_reading(monkeypatch):
    target = DM + "@g.us"
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([target]))
    assert not whatsapp._named_exactly(target)


@pytest.mark.parametrize("entries", [[], ["*@g.us"], ["*@s.whatsapp.net"]])
@pytest.mark.parametrize("target", AMBIGUOUS)
@pytest.mark.parametrize("tool", ["send_message", "forward_message", "send_typing", "update_group", "archive_chat"])
def test_tool_denial_precedes_http(monkeypatch, entries, target, tool):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(entries))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *args, **kwargs: pytest.fail("HTTP before denial"))
    args = (target, "hello")
    if tool == "forward_message":
        source = DM if entries == ["*@s.whatsapp.net"] else GROUP
        args = (source, "SOURCE1", target)
    elif tool == "send_typing":
        args = (target, True)
    elif tool == "archive_chat":
        args = (target, True)
    with pytest.raises(ToolError) as exc:
        getattr(whatsapp, tool)(*args)
    assert exc.value.code == "denied"


@pytest.mark.parametrize("entries", [[], ["*@g.us"], ["*@s.whatsapp.net"]])
def test_real_read_tools_omit_ambiguous_archive_rows(paired_dbs, monkeypatch, entries):
    policy = ChatPolicy.from_entries(entries)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(notes, "CHAT_POLICY", policy)
    with paired_dbs.messages() as db:
        db.execute("DELETE FROM chats")
        for index, jid in enumerate([DM, GROUP, *AMBIGUOUS]):
            db.execute("INSERT INTO chats (jid, name) VALUES (?, ?)", (jid, "Alice"))
            db.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
                (str(index), jid, "5511999999999", "hello", "2026-10-01 00:00:00+00:00"),
            )
    expected = {jid for jid in (DM, GROUP) if policy.allows(jid)}
    assert {row["jid"] for row in whatsapp.list_chats()} == expected
    assert {row["chat_jid"] for row in whatsapp.list_messages(include_context=False)} == expected
    assert whatsapp.count_chats() == len(expected)
    for jid in AMBIGUOUS:
        with pytest.raises(ToolError) as exc:
            notes._require_allowed(jid)
        assert exc.value.code == "denied"
