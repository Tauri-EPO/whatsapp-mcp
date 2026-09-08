"""One person, one row: phone/LID twins collapse in every chat listing (issue #337).

A contact WhatsApp knows under both a phone JID and a `@lid` owns two rows in
`chats`, and the listings used to return them as two people with their messages
split. These tests cover the collapse in list_chats, coverage, get_contact_chats
and the by-JID lookups, the LID chats that must keep listing on their own, and
what WHATSAPP_ALLOWED_CHATS does to a pair.
"""

from __future__ import annotations

import pytest

import triage
import whatsapp
from chat_policy import ChatPolicy
from tests.conftest import ALICE, BOB, BOB_LID, BOB_PN, DECOY, FAMILY

BOB_LID_JID = f"{BOB_LID}@lid"
# A LID whatsmeow_lid_map has never paired: nothing to collapse it into.
LONE_LID = "999888777666555@lid"

MESSAGES = [
    # Bob under the number, then the same conversation under his LID.
    ("p1", BOB, BOB_PN, "under the number", "2026-09-04 09:00:00"),
    ("l1", BOB_LID_JID, BOB_LID, "under the lid", "2026-09-05 11:00:00"),
    ("s1", LONE_LID, "999888777666555", "who is this", "2026-09-04 08:00:00"),
]


@pytest.fixture
def twinned(paired_dbs):
    """The conftest store plus Bob's LID chat and one unpaired LID chat."""
    with paired_dbs.messages() as conn:
        conn.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
            [
                (BOB_LID_JID, "", "2026-09-05 11:00:00"),
                (LONE_LID, "Nobody", "2026-09-04 08:00:00"),
            ],
        )
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
            MESSAGES,
        )
    whatsapp._reset_name_cache()
    return paired_dbs


def _by_jid(chats):
    return {chat["jid"]: chat for chat in chats}


def test_list_chats_returns_one_row_for_both_spellings(twinned):
    chats = _by_jid(whatsapp.list_chats(limit=50))

    assert BOB_LID_JID not in chats
    bob = chats[BOB]
    assert bob["aliases"] == [BOB, BOB_LID_JID]
    # The row that lists is the one holding the newest message, so last_message
    # and last_message_time describe the conversation, not half of it.
    assert bob["last_message"] == "under the lid"
    assert bob["last_message_time"].startswith("2026-09-05T11:00:00")
    assert set(chats) == {ALICE, BOB, FAMILY, DECOY, LONE_LID}


def test_an_unpaired_lid_chat_still_lists_on_its_own(twinned):
    lone = _by_jid(whatsapp.list_chats(limit=50))[LONE_LID]
    assert lone["name"] == "Nobody"
    assert "aliases" not in lone


def test_the_merged_row_takes_the_newer_read_marker(twinned):
    """A conversation read under one spelling is read under both."""
    assert _by_jid(whatsapp.list_chats(limit=50))[BOB]["unread"] is True
    with twinned.messages() as conn:
        # The marker sits on the row that does not list; it still counts.
        conn.execute("UPDATE chats SET last_read_time = ? WHERE jid = ?", ("2026-09-05 12:00:00", BOB))
    merged = _by_jid(whatsapp.list_chats(limit=50))[BOB]
    assert merged["last_read_time"].startswith("2026-09-05T12:00:00")
    assert merged["unread"] is False


def test_the_merged_row_keeps_the_timestamp_it_is_sorted_by(twinned):
    """A marker the hidden row advanced without storing a message does not move the row."""
    with twinned.messages() as conn:
        conn.execute("UPDATE chats SET last_message_time = ? WHERE jid = ?", ("2026-09-09 23:00:00", BOB))
    chats = whatsapp.list_chats(limit=50)
    assert chats[0]["jid"] == BOB  # newest stored message, and first
    assert chats[0]["last_message_time"].startswith("2026-09-05T11:00:00")
    assert chats[0]["last_message"] == "under the lid"


def test_a_merged_row_is_found_by_the_name_and_spelling_it_absorbed(twinned):
    """The name shown comes from the hidden row; searching for it must work."""
    assert _by_jid(whatsapp.list_chats(limit=50))[BOB]["name"] == "Bob"
    assert [chat["jid"] for chat in whatsapp.list_chats(query="Bob", limit=50)] == [BOB]
    assert [chat["jid"] for chat in whatsapp.list_chats(query=BOB_PN, limit=50)] == [BOB]
    assert whatsapp.count_chats(query="Bob") == 1


def test_sorting_by_name_uses_the_name_the_merged_row_shows(twinned):
    """The `@lid` row that lists carries no name; the pair still files under "Bob"."""
    names = [(chat["jid"], chat["name"]) for chat in whatsapp.list_chats(sort_by="name", limit=50)]
    assert [name for _jid, name in names] == sorted(name or "" for _jid, name in names)
    assert names[1] == (BOB, "Bob")  # after Alice, not first under ""

    paged: list[str] = []
    cursor = None
    for _ in range(10):
        page = whatsapp.list_chats_page(limit=2, sort_by="name", cursor=cursor)
        paged.extend(chat["jid"] for chat in page.items)
        cursor = page.next_cursor
        if cursor is None:
            break
    assert paged == [jid for jid, _name in names]


def test_paging_over_the_merge_yields_every_chat_once(twinned):
    seen: list[str] = []
    cursor = None
    for _ in range(10):
        page = whatsapp.list_chats_page(limit=1, cursor=cursor)
        seen.extend(chat["jid"] for chat in page.items)
        cursor = page.next_cursor
        if cursor is None:
            break
    assert seen == [BOB, FAMILY, DECOY, ALICE, LONE_LID]  # newest first, JID as tie-break
    assert whatsapp.count_chats() == len(seen)


def test_coverage_by_chat_sums_the_two_rows(twinned):
    page = whatsapp.coverage(by_chat=True, limit=50)
    entries = {entry["chat_jid"]: entry for entry in page["items"]}

    assert BOB_LID_JID not in entries
    assert entries[BOB]["messages"] == 2
    assert entries[BOB]["aliases"] == [BOB, BOB_LID_JID]
    assert entries[BOB]["first_message_time"].startswith("2026-09-04 09:00:00")
    assert entries[BOB]["last_message_time"].startswith("2026-09-05 11:00:00")
    assert entries[BOB]["stub_only"] is False  # two rows across the pair, not a lone stub
    assert page["chats_total"] == 5


def test_coverage_window_sees_the_messages_of_the_absorbed_row(twinned):
    """A window can name a period only the hidden row of a pair has."""
    result = whatsapp.coverage(after="2026-09-03", before="2026-09-05")
    assert result["total_messages"] == 2  # Bob under his number, and the lone LID
    assert result["chats_with_messages"] == 2
    assert result["chats_without_messages"] == 3


def test_two_lids_for_one_number_stay_two_rows(twinned):
    """whatsmeow_lid_map keys on the LID only: one number can carry several."""
    other_lid = "555444333222111@lid"
    with twinned.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (other_lid.split("@")[0], BOB_PN))
    with twinned.messages() as conn:
        conn.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Bob again', ?)",
            (other_lid, "2026-09-03 08:00:00"),
        )
    whatsapp._reset_name_cache()

    jids = [chat["jid"] for chat in whatsapp.list_chats(limit=50)]
    assert len(jids) == len(set(jids))  # never the same JID on two rows
    # The busiest pair merges; the quieter LID keeps listing under its own JID.
    assert _by_jid(whatsapp.list_chats(limit=50))[BOB]["aliases"] == [BOB, BOB_LID_JID]
    assert other_lid in jids


def test_coverage_totals_count_the_pair_once(twinned):
    result = whatsapp.coverage()
    assert result["chats_total"] == 5
    assert result["chats_with_messages"] == 2  # Bob (merged) and the lone LID
    assert result["total_messages"] == 3


def test_get_contact_chats_reports_one_conversation(twinned):
    rows = whatsapp.get_contact_chats(BOB_PN, limit=20)
    assert [row["jid"] for row in rows] == [BOB]
    assert rows[0]["aliases"] == [BOB, BOB_LID_JID]
    assert rows[0]["membership"] == "spoke"
    # The LID spelling of the same contact answers the same thing.
    assert [row["jid"] for row in whatsapp.get_contact_chats(BOB_LID, limit=20)] == [BOB]


def test_get_chat_answers_the_merged_row_for_either_spelling(twinned):
    by_phone = whatsapp.get_chat(BOB)
    by_lid = whatsapp.get_chat(BOB_LID_JID)
    assert by_phone == by_lid
    assert by_phone["jid"] == BOB
    assert by_phone["aliases"] == [BOB, BOB_LID_JID]
    assert by_phone["last_message"] == "under the lid"


def test_get_direct_chat_by_contact_answers_the_merged_row(twinned):
    chat = whatsapp.get_direct_chat_by_contact(BOB_PN)
    assert (chat["jid"], chat["last_message"]) == (BOB, "under the lid")


def test_get_direct_chat_by_contact_binds_the_allow_list_clause(twinned, monkeypatch):
    """The policy parameters belong to the policy clause, not to the ORDER BY."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB]))
    assert whatsapp.get_direct_chat_by_contact(BOB_PN)["jid"] == BOB


def test_an_allow_list_naming_one_spelling_merges_nothing(twinned, monkeypatch):
    """Half a pair stays half a pair: the allow-list keeps hiding what it hid."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB]))
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert set(chats) == {BOB}
    assert "aliases" not in chats[BOB]
    assert chats[BOB]["last_message"] == "under the number"

    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB, BOB_LID_JID]))
    whatsapp._reset_name_cache()
    merged = _by_jid(whatsapp.list_chats(limit=50))[BOB]
    assert merged["aliases"] == [BOB, BOB_LID_JID]


def test_a_lid_chat_whose_phone_twin_has_no_row_is_left_alone(twinned):
    with twinned.messages() as conn:
        conn.execute("DELETE FROM chats WHERE jid = ?", (BOB,))
    whatsapp._reset_name_cache()
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert BOB not in chats
    assert "aliases" not in chats[BOB_LID_JID]


def test_aliases_is_a_projectable_field(twinned):
    rows = whatsapp.shape_rows(whatsapp.list_chats(limit=50), fields=["jid", "aliases"], known=whatsapp.CHAT_FIELDS)
    assert {"jid": BOB, "aliases": [BOB, BOB_LID_JID]} in rows
    assert {"jid": LONE_LID} in rows  # no key at all on a chat with one spelling


# --- the triage listings (issue #366) -------------------------------------------


def test_list_unread_counts_the_pair_as_one_conversation(twinned):
    """Two rows, one backlog: the counts are summed and the messages listed together."""
    out = whatsapp.list_unread(limit_chats=50, limit_per_chat=10)
    rows = {chat["chat_jid"]: chat for chat in out["chats"]}

    assert BOB_LID_JID not in rows
    bob = rows[BOB]
    assert bob["aliases"] == [BOB, BOB_LID_JID]
    assert bob["unread_count"] == 2
    assert [message["content"] for message in bob["messages"]] == ["under the number", "under the lid"]
    assert bob["latest_unread"].startswith("2026-09-05 11:00:00")
    assert bob["chat_name"] == "Bob"  # the LID row that lists carries none
    # count_only answers over the same rows.
    counted = whatsapp.list_unread(count_only=True)
    assert (counted["count"], counted["chats_with_unread"]) == (out["total_unread"], len(out["chats"]))
    assert counted["chats_with_unread"] == 2  # Bob and the unpaired LID


def test_a_read_marker_on_either_spelling_reads_the_pair(twinned):
    """The marker sits on the row that does not list; it still covers both."""
    with twinned.messages() as conn:
        conn.execute("UPDATE chats SET last_read_time = ? WHERE jid = ?", ("2026-09-05 12:00:00", BOB))
    out = whatsapp.list_unread(limit_chats=50)
    assert BOB not in {chat["chat_jid"] for chat in out["chats"]}
    assert whatsapp.list_unread(count_only=True)["chats_with_unread"] == 1


def test_list_unanswered_waits_once_for_the_pair(twinned):
    items = whatsapp.list_unanswered(limit=50)
    rows = {item["jid"]: item for item in items}

    assert BOB_LID_JID not in rows
    assert rows[BOB]["aliases"] == [BOB, BOB_LID_JID]
    assert rows[BOB]["last_inbound_time"].startswith("2026-09-05 11:00:00")
    assert whatsapp.count_unanswered() == len(items)


def test_an_answer_under_one_spelling_answers_the_pair(twinned):
    """Who spoke last is asked of the conversation, not of the spelling."""
    with twinned.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)"
            " VALUES ('p2', ?, ?, 'respondi', '2026-09-06 08:00:00', 1)",
            (BOB, BOB_PN),
        )
    whatsapp._reset_name_cache()
    assert BOB not in [item["jid"] for item in whatsapp.list_unanswered(limit=50)]
    assert whatsapp.count_unanswered() == 1  # the unpaired LID alone


def test_paging_the_unanswered_queue_holds_the_pair_once(twinned):
    seen: list[str] = []
    cursor = None
    for _ in range(10):
        page = whatsapp.list_unanswered_page(limit=1, cursor=cursor)
        seen.extend(item["jid"] for item in page.items)
        cursor = page.next_cursor
        if cursor is None:
            break
    assert seen == [BOB, LONE_LID]  # newest inbound first, and the pair once
    assert whatsapp.count_unanswered() == len(seen)


def test_a_triage_note_decides_for_the_pair(twinned):
    """mark_handled writes the phone spelling; the row listing under the LID obeys it."""
    out = triage.mark_handled(BOB_LID_JID)
    assert out["chat_jid"] == BOB

    assert BOB not in [item["jid"] for item in whatsapp.list_unanswered(limit=50)]
    assert whatsapp.count_unanswered() == 1
    unread = whatsapp.list_unread(limit_chats=50, hide_handled=True)
    assert BOB not in {chat["chat_jid"] for chat in unread["chats"]}
    assert whatsapp.list_unread(count_only=True, hide_handled=True)["chats_with_unread"] == len(unread["chats"])


def test_message_stats_sums_the_pair_into_one_bucket(twinned):
    stats = whatsapp.message_stats(group_by="chat")
    buckets = {bucket["key"]: bucket for bucket in stats["buckets"]}

    assert BOB_LID_JID not in buckets
    assert buckets[BOB]["messages"] == 2
    assert buckets[BOB]["label"] == "Bob"
    assert stats["total"]["buckets"] == len(buckets) == 2
    assert stats["total"]["messages"] == 3


def test_the_unanswered_row_carries_aliases_as_a_field(twinned):
    rows = whatsapp.shape_rows(
        whatsapp.list_unanswered(limit=50), fields=["jid", "aliases"], known=whatsapp.UNANSWERED_FIELDS
    )
    assert {"jid": BOB, "aliases": [BOB, BOB_LID_JID]} in rows


def test_a_read_receipt_goes_to_the_row_that_stores_each_message(twinned, monkeypatch):
    """The merged row lists both spellings; the bridge validates one chat at a time."""
    sent: list[dict] = []

    def fake_request(method, path, json=None, **kwargs):
        sent.append(json)
        return {"success": True, "messages": len(json.get("message_ids") or []), "message": "ok"}

    monkeypatch.setattr(whatsapp, "_bridge_request", fake_request)
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda payload: payload)

    out = whatsapp.mark_messages_read(["p1", "l1"], BOB)
    assert {call["chat_jid"]: call["message_ids"] for call in sent} == {BOB: ["p1"], BOB_LID_JID: ["l1"]}
    assert out["messages"] == 2

    # The whole-chat form has no ids to route, so both rows are marked.
    sent.clear()
    whatsapp.mark_messages_read(None, BOB)
    assert sorted(call["chat_jid"] for call in sent) == sorted([BOB, BOB_LID_JID])

    # A chat WhatsApp knows one way is one call, as before.
    sent.clear()
    whatsapp.mark_messages_read(["s1"], LONE_LID)
    assert sent == [{"chat_jid": LONE_LID, "message_ids": ["s1"]}]


def test_the_stats_label_is_the_name_the_merged_row_shows(twinned):
    """MIN() over the pair's two names would file a person under a bare number."""
    with twinned.messages() as conn:
        conn.execute("UPDATE chats SET name = ? WHERE jid = ?", (BOB_PN, BOB))
        conn.execute("UPDATE chats SET name = 'Bob Silva' WHERE jid = ?", (BOB_LID_JID,))
    whatsapp._reset_name_cache()

    labels = {bucket["key"]: bucket["label"] for bucket in whatsapp.message_stats(group_by="chat")["buckets"]}
    assert labels[BOB] == "Bob Silva"  # the row that lists, not the smaller string
    assert labels[BOB] == _by_jid(whatsapp.list_chats(limit=50))[BOB]["name"]
