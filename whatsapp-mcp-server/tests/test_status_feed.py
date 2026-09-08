"""`status@broadcast` is its own family (issue #379).

WhatsApp files everyone's status posts under a single JID, stored with the
number of whoever posted last. The triage listings drop it unconditionally —
nobody is waiting for a reply to a status — while the chat listings keep it and
say what it is: "Status updates", `name_source: "system"`, `is_status: true`.
"""

import sqlite3
from datetime import datetime, timedelta

import pytest

import main
import whatsapp
from tests.conftest import MESSAGES_SCHEMA

ALICE = "5511999999999@s.whatsapp.net"
POSTER = "5585879144551"  # the contact whose status arrived last
STATUS = "status@broadcast"


def _stamp(**delta) -> str:
    return (datetime.now() - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S")


@pytest.fixture
def db(tmp_path, monkeypatch):
    """One waiting DM and one status post, the feed newer so it would sort first."""
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        rows = [(ALICE, "Alice", _stamp(hours=2)), (STATUS, POSTER, _stamp(minutes=15))]
        for jid, name, stamp in rows:
            c.execute("INSERT INTO chats VALUES (?, ?, ?, NULL)", (jid, name, stamp))
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?,?,?,?,?,0)",
                (f"m-{jid}", jid, POSTER if jid == STATUS else jid.split("@")[0], "hi", stamp),
            )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    yield path
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()


def test_list_unanswered_skips_the_feed(db):
    """It used to sort first, being the newest inbound message in the store."""
    assert [item["jid"] for item in main.list_unanswered()["items"]] == [ALICE]
    assert main.list_unanswered(count_only=True)["count"] == 1


def test_list_unread_skips_the_feed(db):
    out = main.list_unread()
    assert [chat["chat_jid"] for chat in out["chats"]] == [ALICE]
    assert out["total_unread"] == 1 and out["chats_with_unread"] == 1
    assert main.list_unread(count_only=True) == {"count": 1, "chats_with_unread": 1}


def test_naming_the_feed_does_not_bring_it_back(db):
    """Unconditional: `chat_jid` narrows the triage lists, it does not widen them."""
    assert main.list_unanswered(chat_jid=STATUS)["items"] == []
    assert main.list_unread(chat_jid=STATUS)["chats"] == []
    assert main.list_unread(chat_jid=STATUS, count_only=True)["count"] == 0


def test_list_chats_labels_the_feed(db):
    row = {chat["jid"]: chat for chat in main.list_chats(limit=10)["items"]}[STATUS]
    assert row["name"] == "Status updates"
    assert row["name_source"] == "system"
    assert row["is_status"] is True
    assert row["is_group"] is False
    assert row["push_name"] is None
    # The pushed number is a fact about one post, not the name of the chat.
    assert POSTER not in str(row["name"])
    # Twins collapse phone/LID pairs only; the feed is neither spelling.
    assert "aliases" not in row


def test_get_chat_labels_the_feed(db):
    assert main.get_chat(STATUS)["name"] == "Status updates"
    assert main.get_chat(STATUS)["is_status"] is True


def test_is_status_marks_only_the_feed(db):
    assert "is_status" not in main.get_chat(ALICE)
    assert all("is_status" not in item for item in main.list_unanswered()["items"])


def test_the_posts_stay_readable(db):
    """The feed leaves triage, not the archive."""
    posts = main.list_messages(chat_jid=STATUS, include_context=False)["items"]
    assert [post["chat_jid"] for post in posts] == [STATUS]
    # Named as the chat listings name it, not after the poster of one post.
    assert [post["chat_name"] for post in posts] == ["Status updates"]


def test_the_feed_is_not_a_contact(db):
    """It was answering as one, with `phone_number: "status"` (issue #379)."""
    assert whatsapp.search_contacts(POSTER) == []
    assert STATUS not in [hit["jid"] for hit in whatsapp.search_contacts("status")]
    # Someone else with the same digits is still found.
    assert whatsapp.search_contacts("5511999999999")[0]["jid"] == ALICE


def test_the_label_is_what_sorting_and_query_read(db):
    """A name shown in a listing has to be a name the same listing can find."""
    by_name = [chat["name"] for chat in main.list_chats(sort_by="name", limit=10)["items"]]
    assert by_name == ["Alice", "Status updates"]
    assert [chat["jid"] for chat in main.list_chats(query="Status updates")["items"]] == [STATUS]
    # And the poster's number is no longer the name of a chat.
    assert main.list_chats(query=POSTER)["items"] == []


def test_the_archive_reports_the_feed_under_the_same_name(db):
    """One chat, one label: a stats bucket and a chat row must not disagree."""
    buckets = {bucket["key"]: bucket["label"] for bucket in main.message_stats(group_by="chat")["buckets"]}
    assert buckets[STATUS] == "Status updates"
    by_chat = {row["chat_jid"]: row["name"] for row in whatsapp.coverage(by_chat=True)["items"]}
    assert by_chat[STATUS] == "Status updates"


def test_is_status_is_a_chat_listing_field(db):
    """Valid where the rows can carry it, refused where it would project nothing."""
    assert main.list_chats(fields=["jid", "is_status"], omit_nulls=True)["items"] == [
        {"jid": STATUS, "is_status": True},
        {"jid": ALICE},
    ]
    error = main.list_unanswered(fields=["is_status"])["error"]
    assert error["code"] == "invalid_argument" and "is_status" in error["message"]
