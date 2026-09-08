"""exclude_groups keeps direct conversations only (issue #256).

One fixture with every server WhatsApp puts in a chat list — a phone JID, a LID
alias, a group, a broadcast list, a status feed, a newsletter channel and a bot
chat — asserted through all five tools that take the flag, so they cannot drift
apart. `@bot` is a fan-out surface for this purpose: Meta AI answers on its own
and nobody is waiting there for a reply (issue #274).
"""

import json
import sqlite3
from datetime import datetime, timedelta

import pytest

import export
import main
import whatsapp
from tests.conftest import MESSAGES_SCHEMA

PHONE = "5511111111111@s.whatsapp.net"  # direct
LID = "231241139937355@lid"  # direct, anonymous alias
GROUP = "120363000000000001@g.us"
BROADCAST = "120363000000000002@broadcast"
NEWSLETTER = "120363000000000003@newsletter"
STATUS = "status@broadcast"
BOT = "867051314767696@bot"  # Meta AI and friends: not a person, not direct

DIRECT = [PHONE, LID]
FANOUT = [GROUP, BROADCAST, NEWSLETTER, STATUS, BOT]
ALL_CHATS = DIRECT + FANOUT
# The status feed leaves the triage listings whatever exclude_groups says
# (issue #379); every other chat is only ever dropped by the flag.
TRIAGED = [jid for jid in ALL_CHATS if jid != STATUS]


def _stamp(**delta) -> str:
    return (datetime.now() - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S")


@pytest.fixture
def db(tmp_path, monkeypatch):
    """One inbound, never-read message per chat, newest for the direct ones."""
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        for i, jid in enumerate(ALL_CHATS):
            c.execute("INSERT INTO chats VALUES (?, ?, ?, NULL)", (jid, f"Chat {i}", _stamp(hours=i + 1)))
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?,?,?,?,?,0)",
                (f"m{i}", jid, jid.split("@")[0], f"message {i}", _stamp(hours=i + 1)),
            )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(tmp_path / "exports"))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    yield path
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()


def test_list_messages(db):
    unfiltered = whatsapp.list_messages(limit=100, include_context=False)
    assert {m["chat_jid"] for m in unfiltered} == set(ALL_CHATS)

    direct = whatsapp.list_messages(limit=100, include_context=False, exclude_groups=True)
    assert {m["chat_jid"] for m in direct} == set(DIRECT)


def test_message_stats(db):
    stats = whatsapp.message_stats(group_by="chat", exclude_groups=True)
    assert {b["key"] for b in stats["buckets"]} == set(DIRECT)
    assert stats["total"]["messages"] == len(DIRECT)


def test_export_messages(db):
    result = export.export_messages(exclude_groups=True, out_path="direct.ndjson")
    with open(result["path"], encoding="utf-8") as handle:
        rows = [json.loads(line) for line in handle]
    assert {row["chat_jid"] for row in rows} == set(DIRECT)
    assert result["count"] == len(DIRECT)


def test_list_unread(db):
    assert {c["chat_jid"] for c in main.list_unread()["chats"]} == set(TRIAGED)

    out = main.list_unread(exclude_groups=True)
    assert {c["chat_jid"] for c in out["chats"]} == set(DIRECT)
    assert out["total_unread"] == len(DIRECT) and out["chats_with_unread"] == len(DIRECT)


def test_list_unanswered(db):
    assert {item["jid"] for item in main.list_unanswered()["items"]} == set(TRIAGED)
    assert {item["jid"] for item in main.list_unanswered(exclude_groups=True)["items"]} == set(DIRECT)


def test_bot_chats_are_not_direct(db):
    """Decision on #274: a @bot chat is a chat, but never a direct one."""
    assert not any(BOT.endswith(suffix) for suffix in whatsapp.DIRECT_JID_SUFFIXES)
    assert BOT in {c["chat_jid"] for c in main.list_unread()["chats"]}
    assert BOT not in {c["chat_jid"] for c in main.list_unread(exclude_groups=True)["chats"]}


def test_is_group_still_means_g_us_only(db):
    """The flag widened; the field did not. A channel is not a group."""
    by_jid = {c["jid"]: c for c in main.list_chats(limit=100)["items"]}
    assert by_jid[GROUP]["is_group"] is True
    others = (PHONE, LID, BROADCAST, NEWSLETTER, STATUS, BOT)
    assert [by_jid[jid]["is_group"] for jid in others] == [False] * len(others)
