import inspect
import sqlite3

import pytest

import main
import media_notes
import media_read
import notes
import whatsapp
from chat_policy import ChatPolicy, load_chat_policy
from errors import ToolError

DM = "5511888888888@s.whatsapp.net"
GROUP = "120363000000000002@g.us"
AMBIGUOUS = [DM + "@g.us", GROUP + "@s.whatsapp.net", GROUP + "@g.us", DM + "@s.whatsapp.net"]


def test_invalid_entry_warns_once_without_its_value(caplog):
    raw = "5511888888888:1@lid@s.whatsapp.net"
    policy = load_chat_policy({"WHATSAPP_ALLOWED_CHATS": "," + raw + "," + raw})
    assert policy.restricted
    assert len(caplog.records) == 1
    assert "[2, 3]" in caplog.text
    assert raw not in caplog.text


def test_restricted_sql_predicate_composes_under_not_and_or():
    policy = ChatPolicy.from_entries([DM])
    clause, params = policy.sql_clause("jid")
    with sqlite3.connect(":memory:") as db:
        db.execute("CREATE TABLE chats (jid TEXT)")
        db.executemany("INSERT INTO chats VALUES (?)", [(jid,) for jid in [DM, GROUP, *AMBIGUOUS]])
        denied = {row[0] for row in db.execute(f"SELECT jid FROM chats WHERE NOT {clause}", params)}
        allowed = {row[0] for row in db.execute(f"SELECT jid FROM chats WHERE 0 OR {clause}", params)}
    assert denied == {GROUP, *AMBIGUOUS}
    assert allowed == {DM}


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
    assert actual == ({jid for jid in (DM, GROUP) if policy.allows(jid)} if entries else {DM, GROUP, *AMBIGUOUS})


def test_invalid_exact_entry_cannot_authorize_a_reading(monkeypatch):
    target = DM + "@g.us"
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([target]))
    assert not whatsapp._named_exactly(target)


TOOL_CALLS = [
    ("send_message", lambda target: (target, "hello")),
    ("send_file", lambda target: (target, "/fake/sample.pdf")),
    ("send_audio_message", lambda target: (target, "/fake/sample.ogg")),
    ("send_reaction", lambda target: (target, "MSG1", "")),
    ("send_typing", lambda target: (target, True)),
    ("archive_chat", lambda target: (target, True)),
    ("update_group", lambda target: (target, "Example")),
    ("mark_messages_read", lambda target: (["MSG1"], target)),
    ("edit_message", lambda target: (target, "MSG1", "edited")),
    ("forward_message", lambda target: (target, "MSG1", GROUP)),
    ("forward_message", lambda target: (GROUP, "MSG1", target)),
    ("delete_message", lambda target: (target, "MSG1")),
    ("get_poll_results", lambda target: ("MSG1", target)),
    ("get_group_members", lambda target: (target,)),
    ("manage_group_participants", lambda target: (target, "add", ["5511999999999"])),
    ("get_group_invite_link", lambda target: (target,)),
    ("leave_group", lambda target: (target,)),
    ("request_history", lambda target: (target,)),
    ("download_media", lambda target: ("MSG1", target)),
    ("media_read.read_media", lambda target: (target, "MSG1")),
    ("main.transcribe_audio", lambda target: (target, "MSG1")),
    ("media_notes.clear_media_refusal", lambda target: (target, "MSG1")),
    ("purge_media", lambda target: (None, target)),
    ("get_direct_chat_by_contact", lambda target: (target,)),
    ("get_contact_chats", lambda target: (target,)),
    ("get_message_context", lambda target: ("MSG1", 5, 5, target)),
]


@pytest.mark.parametrize("entries", [[], ["*@g.us"], ["*@s.whatsapp.net"], [DM, GROUP]])
@pytest.mark.parametrize("target", AMBIGUOUS)
@pytest.mark.parametrize("tool,args_for", TOOL_CALLS)
def test_tool_denial_precedes_http(monkeypatch, entries, target, tool, args_for):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(entries))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *args, **kwargs: pytest.fail("HTTP before denial"))
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *args, **kwargs: pytest.fail("HTTP before denial"))
    with pytest.raises(ToolError) as exc:
        module_name, _, name = tool.rpartition(".")
        module = {"": whatsapp, "main": main, "media_read": media_read, "media_notes": media_notes}[module_name]
        args = args_for(target)
        if tool == "forward_message" and args[0] == GROUP and target != GROUP:
            args = (DM if entries == ["*@s.whatsapp.net"] else GROUP, args[1], args[2])
        inspect.unwrap(getattr(module, name))(*args)
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
    expected = {jid for jid in (DM, GROUP) if policy.allows(jid)} if entries else {DM, GROUP, *AMBIGUOUS}
    assert {row["jid"] for row in whatsapp.list_chats()} == expected
    assert {row["chat_jid"] for row in whatsapp.list_messages(include_context=False)} == expected
    assert whatsapp.count_chats() == len(expected)
    for jid in AMBIGUOUS:
        with pytest.raises(ToolError) as exc:
            notes._require_allowed(jid)
        assert exc.value.code == "denied"
