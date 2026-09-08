"""chat_jid / exclude_chat_jid as lists, and message_stats(query=...) — issue #289.

A personal assistant reads a *set* of conversations ("family plus clinic",
"everything except the noise"), which used to cost one call per chat; a
comma-joined string answered an empty page, indistinguishable from "nothing
happened there". These tests pin the list form, the refusal of the joined
string, and the aggregate counting the same search list_messages returns.
"""

from __future__ import annotations

import json
import sqlite3

import pytest

import chat_policy
import export
import main
import media_inventory
import media_notes
import whatsapp
from errors import ToolError
from tests.conftest import ALICE, BOB, BOB_LID, BOB_PN, DECOY, FAMILY
from tests.test_search import FTS_SCHEMA

BOB_CHAT = f"{BOB_LID}@lid"  # Bob's conversation is stored under his LID
SHA_IMAGE = "aa" * 32
SHA_VOICE = "cc" * 32

# (id, chat, content, timestamp) — every row inbound, oldest first.
TEXT_ROWS = [
    ("a1", ALICE, "Segue o orcamento da obra", "2026-09-01 10:00:00+00:00"),
    ("a2", ALICE, "combinado (obrigado)", "2026-09-02 10:00:00+00:00"),
    ("b1", BOB_CHAT, "o orcamento do Bob", "2026-09-03 10:00:00+00:00"),
    ("d1", DECOY, "promocao imperdivel", "2026-09-05 10:00:00+00:00"),
    ("f1", FAMILY, "orcamento da festa", "2026-10-04 10:00:00+00:00"),
]
# (id, chat, media_type, sha, bytes, timestamp)
MEDIA_ROWS = [
    ("IMG_A", ALICE, "image", SHA_IMAGE, 200_000, "2026-09-06 10:00:00+00:00"),
    ("IMG_F", FAMILY, "image", SHA_IMAGE, 200_000, "2026-09-07 10:00:00+00:00"),
    ("VOICE", ALICE, "audio", SHA_VOICE, 9_000, "2026-09-08 10:00:00+00:00"),
]
EVERYTHING = ["a1", "a2", "b1", "d1", "IMG_A", "IMG_F", "VOICE", "f1"]


@pytest.fixture
def archive(paired_dbs, monkeypatch):
    """The paired store with an FTS index, four conversations and some media."""
    monkeypatch.delenv("WHATSAPP_EXPORT_DIR", raising=False)
    with paired_dbs.messages() as c:
        c.executescript(FTS_SCHEMA)
        # The bridge indexes (chat_jid, timestamp); the plan test below needs it.
        c.execute("CREATE INDEX idx_messages_chat_timestamp ON messages(chat_jid, timestamp)")
        c.execute("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Bob', NULL)", (BOB_CHAT,))
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, 'x', ?, ?, 0)",
            TEXT_ROWS,
        )
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_sha256, "
            "file_length) VALUES (?, ?, 'x', '', ?, 0, ?, ?, ?)",
            [(mid, chat, ts, kind, bytes.fromhex(sha), size) for mid, chat, kind, sha, size, ts in MEDIA_ROWS],
        )
    whatsapp._reset_schema_cache()
    yield paired_dbs
    whatsapp._reset_schema_cache()


def ids(**kwargs) -> list[str]:
    kwargs.setdefault("include_context", False)
    kwargs.setdefault("sort_by", "oldest")
    return [m["id"] for m in whatsapp.list_messages(limit=100, **kwargs)]


# --- the filter itself ---------------------------------------------------------


def test_a_single_jid_still_filters(archive):
    assert ids(chat_jid=ALICE) == ["a1", "a2", "IMG_A", "VOICE"]


def test_a_list_covers_every_chat_in_it(archive):
    assert ids(chat_jid=[ALICE, FAMILY]) == ["a1", "a2", "IMG_A", "IMG_F", "VOICE", "f1"]
    assert ids(chat_jid=[FAMILY]) == ["IMG_F", "f1"]


def test_both_spellings_of_one_conversation_match(archive):
    # b1 is stored under Bob's LID; the phone JID and the bare number find it.
    assert ids(chat_jid=BOB_CHAT) == ["b1"]
    assert ids(chat_jid=BOB) == ["b1"]
    assert ids(chat_jid=BOB_PN) == ["b1"]


@pytest.mark.parametrize("separator", [",", ", ", " ", ";"])
def test_several_jids_in_one_string_are_refused(archive, separator):
    with pytest.raises(ToolError) as exc:
        ids(chat_jid=f"{ALICE}{separator}{FAMILY}")
    assert exc.value.code == "invalid_argument"
    assert "list of JIDs" in exc.value.message


def test_an_empty_list_is_refused(archive):
    with pytest.raises(ToolError) as exc:
        ids(chat_jid=[])
    assert exc.value.code == "invalid_argument"
    # A blank string keeps meaning "no filter", as every other argument does.
    assert ids(chat_jid="") == EVERYTHING


def test_exclude_drops_chats(archive):
    assert ids(exclude_chat_jid=DECOY) == [i for i in EVERYTHING if i != "d1"]
    assert ids(exclude_chat_jid=[DECOY, FAMILY]) == ["a1", "a2", "b1", "IMG_A", "VOICE"]
    # The exclusion knows the same spellings as the include.
    assert ids(exclude_chat_jid=BOB) == [i for i in EVERYTHING if i != "b1"]


def test_include_and_exclude_combine(archive):
    assert ids(chat_jid=[ALICE, FAMILY], exclude_chat_jid=FAMILY) == ["a1", "a2", "IMG_A", "VOICE"]


def test_the_count_agrees_with_the_page(archive):
    assert whatsapp.count_messages(chat_jid=[ALICE, FAMILY]) == 6
    assert whatsapp.count_messages(exclude_chat_jid=[DECOY, FAMILY]) == 5


def test_one_chat_still_seeks_the_chat_index(archive):
    """A list is bound as IN (...), which SQLite still serves from the index."""
    conn = sqlite3.connect(archive.messages_db)
    try:
        clauses, params = whatsapp.MessageFilters(chat_jid=ALICE).build(conn.cursor())
        plan = conn.execute(
            f"EXPLAIN QUERY PLAN SELECT messages.id FROM messages WHERE {' AND '.join(clauses)}",  # noqa: S608
            params,
        ).fetchall()
    finally:
        conn.close()
    detail = " ".join(row[3] for row in plan)
    assert "SEARCH" in detail, detail
    assert "SCAN" not in detail, detail


# --- message_stats(query=...) --------------------------------------------------


def test_message_stats_counts_a_query_per_chat(archive):
    stats = whatsapp.message_stats(group_by="chat", query="orcamento")
    assert stats["total"]["messages"] == 3
    # Bob's LID row is bucketed under his phone spelling, the one row the pair
    # gets everywhere else (issue #366).
    assert {b["key"]: b["messages"] for b in stats["buckets"]} == {ALICE: 1, BOB: 1, FAMILY: 1}


def test_message_stats_buckets_a_query_over_time(archive):
    stats = whatsapp.message_stats(group_by="month", query="orcamento")
    assert {b["key"]: b["messages"] for b in stats["buckets"]} == {"2026-09": 2, "2026-10": 1}


@pytest.mark.parametrize("query", ["orcamento", "obra", "combinado (obrigado)", "jacare"])
def test_message_stats_counts_exactly_what_list_messages_returns(archive, query):
    # "combinado (obrigado)" is not valid FTS5 syntax: the aggregate has to make
    # the same quoted retry the page does.
    assert whatsapp.message_stats(query=query)["total"]["messages"] == len(ids(query=query))


def test_message_stats_counts_spoken_words_too(archive):
    media_notes.annotate_media(SHA_VOICE, "transcript", "passei o orcamento por audio")
    stats = whatsapp.message_stats(group_by="chat", query="orcamento")
    assert stats["total"]["messages"] == 4
    assert {b["key"]: b["messages"] for b in stats["buckets"]}[ALICE] == 2
    assert ids(query="orcamento") == ["a1", "b1", "VOICE", "f1"]


def test_message_stats_takes_the_same_chat_lists(archive):
    stats = whatsapp.message_stats(group_by="chat", chat_jid=[ALICE, FAMILY], exclude_chat_jid=FAMILY)
    assert {b["key"] for b in stats["buckets"]} == {ALICE}
    assert stats["total"]["messages"] == 4


# --- the other tools -----------------------------------------------------------


def test_export_messages_takes_a_list(archive):
    result = export.export_messages(chat_jid=[ALICE, FAMILY], out_path="picked.ndjson")
    with open(result["path"], encoding="utf-8") as handle:
        assert [json.loads(line)["id"] for line in handle] == ["a1", "a2", "IMG_A", "IMG_F", "VOICE", "f1"]
    assert result["count"] == 6
    # A list has no single chat to name the file after.
    assert export.export_messages(chat_jid=[ALICE, FAMILY])["path"].split("messages-")[1].startswith("all-")
    assert export.export_messages(chat_jid=ALICE)["path"].split("messages-")[1].startswith(f"{ALICE.split('@')[0]}-")


def test_export_messages_excludes(archive):
    assert export.export_messages(exclude_chat_jid=[DECOY, FAMILY], out_path="rest.ndjson")["count"] == 5


def test_an_unfiltered_export_is_still_named_all(archive):
    # "" means "no chat filter" here as everywhere else, not a chat with no name.
    assert export.export_messages(chat_jid="")["path"].split("messages-")[1].startswith("all-")


def test_list_media_takes_a_list(archive):
    page = media_inventory.list_media_page(chat_jid=[ALICE, FAMILY])
    assert sorted(item["message_id"] for item in page.items) == ["IMG_A", "IMG_F", "VOICE"]
    page = media_inventory.list_media_page(chat_jid=[ALICE, FAMILY], exclude_chat_jid=FAMILY)
    assert sorted(item["message_id"] for item in page.items) == ["IMG_A", "VOICE"]


def test_media_stats_takes_a_list(archive):
    stats = media_inventory.media_stats(chat_jid=[ALICE, FAMILY])
    assert {row["chat_jid"] for row in stats["by_chat"]} == {ALICE, FAMILY}
    assert media_inventory.media_stats(chat_jid=[ALICE, FAMILY], exclude_chat_jid=FAMILY)["total"]["files"] == 2


def test_list_unread_takes_a_list(archive):
    unread = whatsapp.list_unread(chat_jid=[ALICE, FAMILY])
    assert {chat["chat_jid"] for chat in unread["chats"]} == {ALICE, FAMILY}
    assert whatsapp.list_unread(chat_jid=[ALICE, FAMILY], count_only=True)["chats_with_unread"] == 2
    assert whatsapp.list_unread(exclude_chat_jid=DECOY, count_only=True)["chats_with_unread"] == 3
    # The LID chat is reachable by the phone spelling here too, and reported
    # under it: Bob's two rows are one conversation (issue #366).
    bob = whatsapp.list_unread(chat_jid=BOB)["chats"]
    assert [chat["chat_jid"] for chat in bob] == [BOB]
    assert bob[0]["aliases"] == [BOB, BOB_CHAT]


def test_list_unanswered_takes_a_list(archive):
    assert {chat["jid"] for chat in whatsapp.list_unanswered(chat_jid=[ALICE, FAMILY])} == {ALICE, FAMILY}
    assert whatsapp.count_unanswered(chat_jid=[ALICE, FAMILY]) == 2
    assert whatsapp.count_unanswered(exclude_chat_jid=[DECOY, FAMILY]) == 2


# --- allow-list ----------------------------------------------------------------


@pytest.fixture
def only_alice(archive, monkeypatch):
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    for module in (whatsapp, media_inventory, media_notes):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)
    return archive


def test_a_denied_jid_anywhere_in_the_list_is_named(only_alice):
    for call in (
        lambda: whatsapp.list_messages(chat_jid=[ALICE, FAMILY]),
        lambda: whatsapp.message_stats(chat_jid=[ALICE, FAMILY]),
        lambda: media_inventory.list_media_page(chat_jid=[ALICE, FAMILY]),
        lambda: media_inventory.media_stats(chat_jid=[ALICE, FAMILY]),
        lambda: whatsapp.list_unread(chat_jid=[ALICE, FAMILY]),
        lambda: whatsapp.count_unanswered(chat_jid=[ALICE, FAMILY]),
        lambda: export.export_messages(chat_jid=[ALICE, FAMILY], out_path="denied.ndjson"),
    ):
        with pytest.raises(ToolError) as exc:
            call()
        assert exc.value.code == "denied"
        assert FAMILY in exc.value.message


def test_an_allowed_list_still_works_and_exclusions_are_free(only_alice):
    assert ids(chat_jid=[ALICE]) == ["a1", "a2", "IMG_A", "VOICE"]
    # Excluding a chat this server cannot read is a no-op, not a refusal.
    assert ids(exclude_chat_jid=[FAMILY]) == ["a1", "a2", "IMG_A", "VOICE"]


# --- through the tools ---------------------------------------------------------


def test_the_tools_pass_the_lists_through(archive):
    page = main.list_messages(chat_jid=[ALICE, FAMILY], include_context=False, exclude_chat_jid=FAMILY)
    assert {row["id"] for row in page["items"]} == {"a1", "a2", "IMG_A", "VOICE"}
    assert main.list_messages(chat_jid=[ALICE, FAMILY], count_only=True)["count"] == 6
    assert main.message_stats(query="orcamento")["total"]["messages"] == 3
    assert {chat["chat_jid"] for chat in main.list_unread(chat_jid=[ALICE, FAMILY])["chats"]} == {ALICE, FAMILY}
    assert {row["jid"] for row in main.list_unanswered(chat_jid=[ALICE, FAMILY])["items"]} == {ALICE, FAMILY}
    assert {row["message_id"] for row in main.list_media(chat_jid=[ALICE, FAMILY])["items"]} == {
        "IMG_A",
        "IMG_F",
        "VOICE",
    }
    assert {row["chat_jid"] for row in main.get_media_stats(chat_jid=[ALICE, FAMILY])["by_chat"]} == {ALICE, FAMILY}


def test_an_empty_list_is_refused_by_every_tool(archive):
    # The media tools default to "" rather than None: an empty list must not be
    # coerced into "every allowed chat" on the way through.
    for body in (
        main.list_messages(chat_jid=[]),
        main.list_media(chat_jid=[]),
        main.list_media(exclude_chat_jid=[]),
        main.get_media_stats(chat_jid=[]),
        main.get_media_stats(exclude_chat_jid=[]),
        main.message_stats(chat_jid=[]),
        main.list_unread(chat_jid=[]),
    ):
        assert body["error"]["code"] == "invalid_argument"
    assert main.list_media(chat_jid="")["items"]  # a blank string is still "no filter"


def test_the_joined_string_reports_an_error_instead_of_an_empty_page(archive):
    # The bug as reported: two JIDs in one string used to answer {"items": []}.
    body = main.list_messages(chat_jid=f"{ALICE},{FAMILY}")
    assert body["error"]["code"] == "invalid_argument"
    assert "list of JIDs" in body["error"]["message"]
