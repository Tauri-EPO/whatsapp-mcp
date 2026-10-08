import inspect
import json
import os
import pathlib
import sqlite3
import subprocess
import sys

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
    assert not caplog.records
    assert policy.invalid_positions == (2, 3)
    policy.warn_invalid_entries()
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
def test_tool_denial_precedes_http(paired_dbs, monkeypatch, entries, target, tool, args_for):
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
    assert exc.value.code == "invalid_argument"


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
        assert exc.value.code == "invalid_argument"


def test_malformed_entry_startup_warn_is_json(tmp_path):
    raw = "5511888888888:1@lid@s.whatsapp.net"
    env = dict(
        os.environ,
        WHATSAPP_ALLOWED_CHATS="," + raw + "," + raw,
        WHATSAPP_MCP_LOG_FORMAT="json",
        WHATSAPP_MCP_TRANSPORT="stdio",
        WHATSAPP_PARENT_WATCHDOG_S="0",
        TRANSCRIBE_ON_INGEST="0",
        WHATSAPP_STORE_DIR=str(tmp_path),
        WHATSAPP_API_URL="http://127.0.0.1:1/api",
    )
    result = subprocess.run(
        [sys.executable, "main.py"],
        cwd=pathlib.Path(main.__file__).parent,
        input="",
        text=True,
        capture_output=True,
        env=env,
        timeout=15,
    )
    assert result.returncode == 0, result.stderr
    warnings = [line for line in result.stderr.splitlines() if "malformed entries" in line]
    assert len(warnings) == 1
    record = json.loads(warnings[0])
    assert record["level"] == "WARNING"
    assert "[2, 3]" in record["msg"]
    assert raw not in result.stderr


def test_typing_normalizes_before_policy_and_http(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    calls = []

    def post(_url, **kwargs):
        calls.append(kwargs["json"])
        return type("Response", (), {"status_code": 200, "json": lambda self: {"success": True}})()

    monkeypatch.setattr(whatsapp.bridge_http, "post", post)
    assert whatsapp.send_typing("+55 11 99999-9999")["success"]
    assert calls == [{"recipient": "5511999999999", "is_typing": True}]
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["*@g.us"]))
    with pytest.raises(ToolError) as exc:
        whatsapp.send_typing("+55 11 99999-9999")
    assert exc.value.code == "denied"
    assert len(calls) == 1
    assert whatsapp.send_typing(" " + GROUP + " ")["success"]
    assert calls[-1] == {"recipient": GROUP, "is_typing": True}


READ_CALLS = [
    ("get_chat", "chat_jid"),
    ("get_contact", "identifier"),
    ("list_messages", "chat_jid"),
    ("list_messages", "sender_jid"),
    ("list_messages", "exclude_chat_jid"),
    ("list_media", "chat_jid"),
    ("list_media", "exclude_chat_jid"),
    ("get_media_stats", "chat_jid"),
    ("get_media_stats", "exclude_chat_jid"),
    ("export_messages", "chat_jid"),
    ("export_messages", "sender_jid"),
    ("export_messages", "exclude_chat_jid"),
    ("message_stats", "chat_jid"),
    ("message_stats", "sender_jid"),
    ("message_stats", "exclude_chat_jid"),
    ("coverage", "chat_jid"),
    ("list_unanswered", "chat_jid"),
    ("list_unanswered", "exclude_chat_jid"),
    ("list_unread", "chat_jid"),
    ("list_unread", "exclude_chat_jid"),
    ("mark_handled", "chat_jid"),
    ("snooze", "chat_jid"),
    ("annotate", "chat"),
    ("annotate", "contact"),
    ("get_notes", "chat"),
    ("get_notes", "contact"),
    ("get_last_interaction", "contact_jid"),
]


@pytest.mark.parametrize("entries", [[], ["*@g.us"], ["*@s.whatsapp.net"], [DM, GROUP]])
@pytest.mark.parametrize("target", AMBIGUOUS)
@pytest.mark.parametrize("tool,argument", READ_CALLS)
def test_read_filter_malformed_argument_is_invalid_before_alias_or_effects(
    paired_dbs, monkeypatch, entries, target, tool, argument
):
    policy = ChatPolicy.from_entries(entries)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(notes, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("HTTP before refusal"))
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: pytest.fail("HTTP before refusal"))
    kwargs = {argument: target}
    if tool in {"annotate", "get_notes"}:
        kwargs = {"target_type": argument, "target_id": target}
        if tool == "annotate":
            kwargs.update(key="example", value="example")
    elif tool == "snooze":
        kwargs["until"] = "2999-01-01"
    with pytest.raises(ToolError) as exc:
        inspect.unwrap(getattr(main, tool))(**kwargs)
    assert exc.value.code == "invalid_argument"
    assert "Malformed chat target" in str(exc.value)
