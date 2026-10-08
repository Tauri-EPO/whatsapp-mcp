"""Real SQLite archive keys, Brazilian search-only aliases and read-side twins."""

import sqlite3

import pytest

import main
import media_notes
import notes
import triage
import whatsapp
from chat_policy import ChatPolicy
from tests.conftest import ALICE, BOB_LID, BOB_PN, FAMILY
from tests.test_phone_spellings import (
    FOREIGN_LONG,
    FOREIGN_SHORT,
    LANDLINE,
    LANDLINE_PLUS_NINE,
    LID,
    LONG,
    LONG_JID,
    SHORT,
    SHORT_JID,
)
from tests.test_structured_errors import _sdk_client


@pytest.mark.parametrize("target_type", ["chat", "contact", "message"])
@pytest.mark.parametrize("listed", [SHORT_JID, LONG_JID, f"{LID}@lid"])
def test_notes_drop_denied_legacy_origins_and_recheck_canonical_writes(paired_dbs, monkeypatch, target_type, listed):
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
    suffix = "/synthetic-message" if target_type == "message" else ""
    denied = next(jid for jid in [SHORT_JID, LONG_JID, f"{LID}@lid"] if jid != listed)
    conn = notes._connect(create=True)
    assert conn is not None
    conn.execute(
        "INSERT INTO notes VALUES (?, ?, 'role', 'hidden legacy', '2026-10-08', 'legacy', 1)",
        (target_type, denied + suffix),
    )
    conn.close()
    _allow(monkeypatch, listed)
    assert main.get_notes(target_type, listed + suffix, include_history=True)["notes"] == {}
    assert main.get_notes(target_type, listed + suffix, include_history=True)["history"] == []
    assert notes.fetch_notes_for(target_type, [listed + suffix, denied + suffix]) == {}
    assert main.search_notes("hidden legacy", target_type=target_type) == []
    assert main.annotate(target_type, denied + suffix, "role", "refused")["error"]["code"] == "denied"
    assert main.annotate(target_type, listed + suffix, "role", "visible canonical")["target_id"] == LONG_JID + suffix
    assert main.get_notes(target_type, listed + suffix)["notes"]["role"]["value"] == "visible canonical"
    assert notes.fetch_notes_for(target_type, [listed + suffix]) == {listed + suffix: {"role": "visible canonical"}}
    assert main.search_notes("visible canonical", target_type=target_type)[0]["target_id"] == listed + suffix
    _allow(monkeypatch, ALICE)
    assert main.search_notes("visible canonical", target_type=target_type) == []


def test_triage_drops_denied_legacy_and_keeps_admitted_write_origin(paired_dbs, monkeypatch):
    conn = notes._connect(create=True)
    assert conn is not None
    conn.execute("INSERT INTO notes VALUES ('chat', ?, 'mute', 'yes', '2026-10-08', 'legacy', 1)", (LONG_JID,))
    conn.close()
    _allow(monkeypatch, SHORT_JID)
    assert triage._state_by_jid((triage.MUTE_KEY,)) == {}
    assert main.annotate("chat", LONG_JID, "mute", "yes")["error"]["code"] == "denied"
    main.annotate("chat", SHORT_JID, "mute", "yes")
    assert triage._state_by_jid((triage.MUTE_KEY,)) == {SHORT_JID: {"muted": True}}
    _allow(monkeypatch, LONG_JID)
    assert triage._state_by_jid((triage.MUTE_KEY,)) == {}


def test_literal_chat_keeps_confirmed_phone_lid_name_pair(paired_dbs):
    _chat(paired_dbs, SHORT_JID, SHORT)
    _chat(paired_dbs, f"{LID}@lid", "Clinic")
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
    chat = whatsapp.get_chat(SHORT_JID, both_spellings=False)
    assert chat["jid"] == SHORT_JID and chat["name"] == "Clinic"
    assert set(chat["aliases"]) == {SHORT_JID, f"{LID}@lid"}


def test_allowed_legacy_message_search_keeps_one_message_suffix(paired_dbs, monkeypatch):
    conn = notes._connect(create=True)
    assert conn is not None
    target = SHORT_JID + "/synthetic-message"
    conn.execute(
        "INSERT INTO notes VALUES ('message', ?, 'role', 'visible legacy', '2026-10-08', 'legacy', 1)", (target,)
    )
    conn.close()
    _allow(monkeypatch, SHORT_JID)
    assert main.search_notes("visible legacy", target_type="message")[0]["target_id"] == target


def _chat(store, jid, name="", stamp="2026-10-08 10:00:00"):
    with store.messages() as conn:
        conn.execute("INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", (jid, name, stamp))


def _message(store, jid, name, stamp, from_me=False):
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id,chat_jid,sender,content,timestamp,is_from_me) VALUES (?, ?, ?, ?, ?, ?)",
            (name, jid, jid.partition("@")[0], name, stamp, from_me),
        )


def _allow(monkeypatch, *jids):
    policy = ChatPolicy.from_entries(list(jids))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(notes, "CHAT_POLICY", policy)


@pytest.fixture
def phone_pair(paired_dbs):
    _chat(paired_dbs, SHORT_JID, "Acme Clinic")
    _chat(paired_dbs, LONG_JID)
    _message(paired_dbs, SHORT_JID, "older-history", "2026-10-08 09:00:00")
    _message(paired_dbs, LONG_JID, "newer-inbound", "2026-10-08 10:00:00")
    return paired_dbs


@pytest.mark.parametrize("stored,query", [(SHORT_JID, "(88) 97777-6666"), (LONG_JID, "(88) 7777-6666")])
def test_national_mobile_search_adds_whole_brazilian_alternate_only(paired_dbs, stored, query):
    _chat(paired_dbs, stored, "Clinic")
    assert stored in {row["jid"] for row in main.search_contacts(query)}
    extra = whatsapp._jid_search_patterns(query, national_mobile=True)[0]
    assert stored in extra and f"%{stored}%" not in extra
    assert stored not in whatsapp._jid_search_patterns(query)[0]
    assert whatsapp.get_direct_chat_by_contact(query) is None


@pytest.mark.parametrize("digits", [LANDLINE[2:], LANDLINE_PLUS_NINE[2:], FOREIGN_SHORT, "12025550100"])
def test_national_search_keeps_non_mobile_and_foreign_literal_patterns(paired_dbs, digits):
    assert whatsapp._jid_search_patterns(digits, national_mobile=True) == whatsapp._jid_search_patterns(digits)


@pytest.mark.parametrize("target_type", ["chat", "contact", "message"])
@pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])
def test_notes_write_one_archive_key_and_read_it_on_both_spellings(paired_dbs, target_type, asked):
    _chat(paired_dbs, SHORT_JID, "Clinic")
    suffix = "/synthetic-message" if target_type == "message" else ""
    written = main.annotate(target_type, asked + suffix, "role", "synthetic note")
    assert written["target_id"] == LONG_JID + suffix
    for jid in [SHORT_JID, LONG_JID]:
        assert main.get_notes(target_type, jid + suffix)["notes"]["role"]["value"] == "synthetic note"
    with sqlite3.connect(media_notes.notes_db_path()) as conn:
        assert conn.execute("SELECT DISTINCT target_id FROM notes").fetchall() == [(LONG_JID + suffix,)]
    assert notes.fetch_notes_for(target_type, [SHORT_JID + suffix, LONG_JID + suffix]) == {
        SHORT_JID + suffix: {"role": "synthetic note"},
        LONG_JID + suffix: {"role": "synthetic note"},
    }


@pytest.mark.parametrize("stored", [SHORT_JID, LONG_JID])
def test_note_key_is_independent_of_chat_contact_and_registered_spelling(paired_dbs, stored):
    other = LONG_JID if stored == SHORT_JID else SHORT_JID
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, NULL, 'Clinic', NULL, NULL)", (stored,))
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, other.partition("@")[0]))
    assert set(whatsapp.canonical_note_jids([stored, other, f"{LID}@lid"]).values()) == {LONG_JID}
    _chat(paired_dbs, other)
    assert set(whatsapp.canonical_note_jids([stored, other, f"{LID}@lid"]).values()) == {LONG_JID}


@pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])
def test_legacy_notes_canonical_wins_and_delete_covers_both(paired_dbs, asked):
    _chat(paired_dbs, SHORT_JID)
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('contact', ?, 'role', ?, ?, 'legacy', ?)",
            [
                (LONG_JID, "canonical", "2026-10-07T10:00:00+00:00", 1),
                (SHORT_JID, "newer legacy", "2026-10-08T10:00:00+00:00", 7),
            ],
        )
    finally:
        conn.close()
    assert notes.get_notes("contact", asked)["notes"]["role"]["value"] == "canonical"
    assert notes.fetch_notes_for("contact", [LONG_JID])[LONG_JID] == {"role": "canonical"}
    deleted = notes.annotate("contact", asked, "role", "")
    assert deleted["deleted"] and deleted["version"] == 8
    assert notes.get_notes("contact", SHORT_JID)["notes"] == {}
    assert notes.get_notes("contact", LONG_JID)["notes"] == {}
    assert notes.fetch_notes_for("contact", [LONG_JID, SHORT_JID]) == {}


@pytest.mark.parametrize("listed", [SHORT_JID, LONG_JID])
@pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])
def test_note_policy_requires_the_typed_spelling(paired_dbs, monkeypatch, listed, asked):
    _chat(paired_dbs, SHORT_JID)
    _allow(monkeypatch, listed)
    if listed == asked:
        assert main.annotate("contact", asked, "role", "allowed")["target_id"] == LONG_JID
        assert main.get_notes("contact", asked)["notes"]["role"]["value"] == "allowed"
    else:
        assert main.annotate("contact", asked, "role", "blocked")["error"]["code"] == "denied"
        assert main.get_notes("contact", asked)["error"]["code"] == "denied"
    assert main.annotate("contact", ALICE, "role", "blocked")["error"]["code"] == "denied"
    _allow(monkeypatch, ALICE)
    for jid in [SHORT_JID, LONG_JID]:
        assert main.annotate("contact", jid, "role", "blocked")["error"]["code"] == "denied"
        assert main.mark_handled(jid)["error"]["code"] == "denied"
        assert main.snooze(jid, "2099-01-01")["error"]["code"] == "denied"


@pytest.mark.parametrize(
    "jid",
    [
        f"{FOREIGN_SHORT}@s.whatsapp.net",
        f"{FOREIGN_LONG}@s.whatsapp.net",
        f"{LANDLINE}@s.whatsapp.net",
        f"{LANDLINE_PLUS_NINE}@s.whatsapp.net",
        f"{LID}@lid",
    ],
)
def test_foreign_landline_and_unmapped_lid_keep_one_key(paired_dbs, jid):
    assert main.annotate("contact", jid, "role", "one")["target_id"] == jid
    assert notes._jid_spellings(jid) == [jid]


@pytest.mark.parametrize("action", ["handled", "snooze", "mute"])
@pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])
def test_triage_on_either_spelling_hides_the_stored_chat(paired_dbs, asked, action):
    _chat(paired_dbs, SHORT_JID, "Clinic")
    _message(paired_dbs, SHORT_JID, "pending", "2026-10-08 09:00:00")
    assert SHORT_JID in {row["jid"] for row in main.list_unanswered()["items"]}
    if action == "handled":
        assert main.mark_handled(asked)["chat_jid"] == LONG_JID
    elif action == "snooze":
        assert main.snooze(asked, "2099-01-01")["chat_jid"] == LONG_JID
    else:
        main.annotate("chat", asked, "mute", "yes")
    assert SHORT_JID not in {row["jid"] for row in main.list_unanswered()["items"]}
    assert main.list_unanswered(count_only=True)["count"] == 0


def test_stored_pair_measured_and_merged_across_read_tools(phone_pair):
    with phone_pair.messages() as conn:
        stored = {row[0] for row in conn.execute("SELECT jid FROM chats")}
        pairs = [
            (jid, whatsapp.other_phone_spelling(jid))
            for jid in stored
            if len(jid.partition("@")[0]) == 13 and whatsapp.other_phone_spelling(jid) in stored
        ]
    assert len(pairs) == 1
    rows = [row for row in main.list_chats()["items"] if row["jid"] in {SHORT_JID, LONG_JID}]
    assert len(rows) == 1 and rows[0]["aliases"] == [SHORT_JID, LONG_JID]
    assert rows[0]["last_message"] == "newer-inbound"
    assert whatsapp.get_chat(SHORT_JID) == whatsapp.get_chat(LONG_JID)
    assert whatsapp.get_direct_chat_by_contact(SHORT) == whatsapp.get_direct_chat_by_contact(LONG)
    assert len([row for row in main.search_contacts(SHORT) if row["jid"] in {SHORT_JID, LONG_JID}]) == 1


@pytest.mark.parametrize("listed", [SHORT_JID, LONG_JID])
def test_phone_pair_never_merges_a_hidden_spelling(phone_pair, monkeypatch, listed):
    _allow(monkeypatch, listed)
    rows = main.list_chats()["items"]
    assert len(rows) == 1 and rows[0]["jid"] == listed
    assert "aliases" not in rows[0]
    content = "older-history" if listed == SHORT_JID else "newer-inbound"
    assert rows[0]["last_message"] == content
    assert whatsapp.message_stats(group_by="chat")["total"]["messages"] == 1


def test_three_member_chat_counts_reads_and_answers_without_losing_single_chats(phone_pair):
    lid_jid = f"{LID}@lid"
    with phone_pair.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
    _chat(phone_pair, lid_jid)
    _message(phone_pair, lid_jid, "newest-lid", "2026-10-08 11:00:00")
    _message(phone_pair, ALICE, "single-chat", "2026-10-08 12:00:00")
    whatsapp._reset_name_cache()
    expected = [SHORT_JID, LONG_JID, lid_jid]
    merged = whatsapp.get_chat(LONG_JID)
    assert merged["aliases"] == expected and merged["last_message"] == "newest-lid"
    assert merged == whatsapp.get_chat(SHORT_JID) == whatsapp.get_chat(lid_jid)
    stats = whatsapp.message_stats(group_by="chat")
    assert {row["key"]: row["messages"] for row in stats["buckets"]} == {SHORT_JID: 3, ALICE: 1}
    unread = whatsapp.list_unread(limit_chats=50)
    assert {row["chat_jid"]: row["unread_count"] for row in unread["chats"]} == {SHORT_JID: 3, ALICE: 1}
    with phone_pair.messages() as conn:
        conn.execute("UPDATE chats SET last_read_time = '2026-10-08 13:00:00' WHERE jid = ?", (LONG_JID,))
    assert {row["chat_jid"] for row in whatsapp.list_unread()["chats"]} == {ALICE}
    _message(phone_pair, LONG_JID, "answered-other-spelling", "2026-10-08 14:00:00", True)
    assert SHORT_JID not in {row["jid"] for row in whatsapp.list_unanswered()}
    assert ALICE in {row["jid"] for row in whatsapp.list_unanswered()}


@pytest.mark.parametrize("source", ["full_name", "push_name", "first_name", "business_name"])
def test_placeholder_chat_is_searchable_by_its_phonebook_display_name(paired_dbs, source):
    with paired_dbs.messages() as conn:
        conn.execute("UPDATE chats SET name = ? WHERE jid = ?", (ALICE.partition("@")[0], ALICE))
    # Prime an empty profile before the store learns the name. The named query
    # resolves current local metadata rather than trusting that cached absence.
    assert whatsapp.list_chats(query="Alice Contact") == []
    with paired_dbs.whatsmeow() as conn:
        conn.execute(
            f"INSERT INTO whatsmeow_contacts (our_jid, their_jid, {source}) VALUES ('me', ?, ?)",
            (ALICE, "Alice Contact"),
        )
    rows = main.list_chats(query="Alice Contact")["items"]
    assert [(row["jid"], row["name"]) for row in rows] == [(ALICE, "Alice Contact")]
    assert whatsapp.count_chats(query="Alice Contact") == 1
    with paired_dbs.messages() as conn:
        assert conn.execute("SELECT name FROM chats WHERE jid = ?", (ALICE,)).fetchone()[0] == ALICE.partition("@")[0]


def test_phonebook_name_query_keeps_real_names_and_groups(paired_dbs):
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, 'Book Name', NULL, NULL, NULL)", (ALICE,))
    assert whatsapp.list_chats(query="Book Name") == []
    assert {row["jid"]: row["name"] for row in whatsapp.list_chats(query="Alice")}[ALICE] == "Alice"
    with paired_dbs.messages() as conn:
        conn.execute("UPDATE chats SET name = ? WHERE jid = ?", (FAMILY.partition("@")[0], FAMILY))
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, 'Group Book Name', NULL, NULL, NULL)", (FAMILY,))
    assert whatsapp.list_chats(query="Group Book Name") == []


def test_placeholder_name_alias_policy_is_rechecked_after_warm_search(phone_pair, monkeypatch):
    with phone_pair.messages() as conn:
        conn.execute("UPDATE chats SET name = '' WHERE jid IN (?, ?)", (SHORT_JID, LONG_JID))
    with phone_pair.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, 'Clinic Name', NULL, NULL, NULL)", (LONG_JID,))
    assert whatsapp.list_chats(query="Clinic Name")[0]["name"] == "Clinic Name"
    _allow(monkeypatch, SHORT_JID)
    assert whatsapp.list_chats(query="Clinic Name") == []
    assert whatsapp.count_chats(query="Clinic Name") == 0
    allowed = whatsapp.list_chats()[0]
    assert "Clinic Name" not in str(allowed)
    assert allowed["jid"] == SHORT_JID


@pytest.mark.parametrize("restricted", [False, True])
def test_listing_canonical_keys_are_batched_for_every_target(phone_pair, monkeypatch, restricted):
    notes.annotate("contact", LONG_JID, "role", "shared")
    notes.annotate("contact", ALICE, "role", "single")
    statements = []
    for method in ("_connect_messages_db", "_connect_whatsmeow_db"):
        original = getattr(whatsapp, method)

        def connect(original=original):
            conn = original()
            conn.set_trace_callback(statements.append)
            return conn

        monkeypatch.setattr(whatsapp, method, connect)
    alternate_alice = whatsapp.other_phone_spelling(ALICE)
    ids = [SHORT_JID, LONG_JID, ALICE, alternate_alice]
    if restricted:
        _allow(monkeypatch, *ids)
    out = notes.fetch_notes_for("contact", ids)
    assert out[SHORT_JID] == out[LONG_JID] == {"role": "shared"}
    assert out[ALICE] == out[alternate_alice] == {"role": "single"}
    assert not [
        sql for sql in statements if "SELECT jid FROM chats" in sql or "SELECT their_jid FROM whatsmeow_contacts" in sql
    ]


@pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])
async def test_sdk_notes_and_triage_flow_uses_the_archive_key(paired_dbs, asked):
    _chat(paired_dbs, SHORT_JID, "Clinic")
    _message(paired_dbs, SHORT_JID, "pending", "2026-10-08 09:00:00")
    async with _sdk_client() as client:
        written = await client.call_tool(
            "annotate", {"target_type": "contact", "target_id": asked, "key": "role", "value": "note"}
        )
        assert not written.is_error and written.structured_content["target_id"] == LONG_JID
        for jid in [SHORT_JID, LONG_JID]:
            result = await client.call_tool("get_notes", {"target_type": "contact", "target_id": jid})
            assert not result.is_error and result.structured_content["notes"]["role"]["value"] == "note"
        before = await client.call_tool("list_unanswered", {})
        assert SHORT_JID in {row["jid"] for row in before.structured_content["items"]}
        handled = await client.call_tool("mark_handled", {"chat_jid": asked})
        assert not handled.is_error and handled.structured_content["chat_jid"] == LONG_JID
        after = await client.call_tool("list_unanswered", {})
        assert SHORT_JID not in {row["jid"] for row in after.structured_content["items"]}


@pytest.mark.parametrize("target_type", ["chat", "contact", "message"])
def test_new_lid_mapping_during_warm_cache_keeps_reads_and_versions(paired_dbs, target_type):
    jid = f"{LID}@lid"
    suffix = "/synthetic-message" if target_type == "message" else ""
    whatsapp._sender_aliases(jid)  # Prime the still-unmapped five-minute cache.
    assert main.get_notes(target_type, jid + suffix)["notes"] == {}
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
    first = main.annotate(target_type, jid + suffix, "role", "first")
    assert first["target_id"] == LONG_JID + suffix and first["version"] == 1
    assert main.get_notes(target_type, jid + suffix)["notes"]["role"]["value"] == "first"
    second = main.annotate(target_type, jid + suffix, "role", "second")
    assert second["version"] == 2 and second["replaced"] == "first"
    assert main.get_notes(target_type, SHORT_JID + suffix)["notes"]["role"]["value"] == "second"


@pytest.mark.parametrize("replacement", ["replacement", ""])
def test_note_search_resolves_winning_legacy_alias_before_matching(paired_dbs, replacement):
    _chat(paired_dbs, SHORT_JID)
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, LONG))
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('contact', ?, 'role', ?, ?, 'legacy', ?)",
            [
                (SHORT_JID, "superseded", "2026-10-07T10:00:00+00:00", 1),
                (f"{LID}@lid", replacement, "2026-10-08T10:00:00+00:00", 2),
            ],
        )
    finally:
        conn.close()
    current = main.get_notes("contact", LONG_JID)["notes"]
    assert current == {} if not replacement else current["role"]["value"] == replacement
    assert main.search_notes("superseded", target_type="contact") == []
    if replacement:
        assert main.search_notes(replacement, target_type="contact")[0]["value"] == replacement


@pytest.mark.parametrize("recipient,name", [(SHORT, "Short name"), (LONG, "Long name")])
def test_literal_send_preview_retains_its_own_name_and_jid(phone_pair, recipient, name):
    with phone_pair.messages() as conn:
        conn.executemany(
            "UPDATE chats SET name = ? WHERE jid = ?", [("Short name", SHORT_JID), ("Long name", LONG_JID)]
        )
    literal = whatsapp.get_chat(f"{recipient}@s.whatsapp.net", both_spellings=False)
    assert literal["jid"] == f"{recipient}@s.whatsapp.net" and literal["name"] == name
    assert "aliases" not in literal
    preview = whatsapp.send_message(recipient, "synthetic", dry_run=True)[2]
    assert preview["recipient_name"] == name and preview["payload"]["recipient"] == recipient


@pytest.mark.parametrize("value", ["latest", ""])
def test_inline_legacy_notes_match_direct_read_update_precedence(paired_dbs, value):
    _chat(paired_dbs, LONG_JID)
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, LONG))
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('contact', ?, 'role', ?, ?, 'legacy', ?)",
            [
                (SHORT_JID, "stale", "2026-10-07T10:00:00+00:00", 1),
                (f"{LID}@lid", value, "2026-10-08T10:00:00+00:00", 2),
            ],
        )
    finally:
        conn.close()
    direct = main.get_notes("contact", f"{LID}@lid")["notes"]
    expected = {key: note["value"] for key, note in direct.items()}
    assert notes.fetch_notes_for("contact", [f"{LID}@lid"]) == ({f"{LID}@lid": expected} if expected else {})


def test_legacy_lid_triage_tombstone_beats_older_noncanonical_phone(phone_pair):
    with phone_pair.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, LONG))
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('chat', ?, 'mute', ?, ?, 'legacy', ?)",
            [(SHORT_JID, "yes", "2026-10-07T10:00:00+00:00", 1), (f"{LID}@lid", "", "2026-10-08T10:00:00+00:00", 2)],
        )
    finally:
        conn.close()
    assert main.get_notes("chat", LONG_JID)["notes"] == {}
    assert SHORT_JID in {row["jid"] for row in main.list_unanswered()["items"]}
    assert main.list_unanswered(count_only=True)["count"] == 1


@pytest.mark.parametrize("target_type", ["chat", "contact", "message"])
def test_note_delete_does_not_resurrect_when_archive_rows_change(paired_dbs, target_type):
    suffix = "/synthetic-message" if target_type == "message" else ""
    assert main.annotate(target_type, SHORT_JID + suffix, "role", "old")["target_id"] == LONG_JID + suffix
    _chat(paired_dbs, LONG_JID)
    assert main.annotate(target_type, LONG_JID + suffix, "role", "")["deleted"]
    _chat(paired_dbs, SHORT_JID)
    for jid in (SHORT_JID, LONG_JID):
        assert main.get_notes(target_type, jid + suffix)["notes"] == {}
    assert notes.fetch_notes_for(target_type, [SHORT_JID + suffix, LONG_JID + suffix]) == {}
    assert main.search_notes("old", target_type=target_type) == []


@pytest.mark.parametrize("fail_alias", [False, True])
def test_legacy_alias_rewrite_is_atomic_and_preserves_history(paired_dbs, fail_alias):
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('contact', ?, 'log', ?, ?, 'legacy', ?)",
            [
                (SHORT_JID, "first", "2026-10-07T10:00:00+00:00", 1),
                (SHORT_JID, "second", "2026-10-08T10:00:00+00:00", 2),
            ],
        )
        if fail_alias:
            conn.execute(
                "CREATE TRIGGER fail_alias BEFORE INSERT ON notes WHEN NEW.source = 'alias' BEGIN SELECT RAISE(ABORT, 'alias refused'); END"
            )
    finally:
        conn.close()
    if fail_alias:
        with pytest.raises(sqlite3.IntegrityError, match="alias refused"):
            notes.annotate("contact", SHORT_JID, "log", "third", mode="append")
        with sqlite3.connect(media_notes.notes_db_path()) as conn:
            assert conn.execute("SELECT COUNT(*) FROM notes WHERE target_id = ?", (LONG_JID,)).fetchone()[0] == 0
        assert notes.get_notes("contact", LONG_JID)["notes"]["log"]["value"] == "second"
    else:
        written = notes.annotate("contact", SHORT_JID, "log", "third", mode="append")
        assert written["target_id"] == LONG_JID and written["value"] == "second\nthird" and written["version"] == 3
        with sqlite3.connect(media_notes.notes_db_path()) as conn:
            assert (
                conn.execute(
                    "SELECT value FROM notes WHERE target_id = ? AND key = 'log' ORDER BY version DESC LIMIT 1",
                    (SHORT_JID,),
                ).fetchone()[0]
                == ""
            )
        history = notes.get_notes("contact", LONG_JID, include_history=True)["history"]
        assert {"first", "second", "second\nthird"} <= {row["value"] for row in history}


def test_literal_preview_does_not_borrow_the_other_phonebook_name(phone_pair):
    with phone_pair.messages() as conn:
        conn.execute("UPDATE chats SET name = '' WHERE jid = ?", (SHORT_JID,))
    with phone_pair.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, 'Other spelling', NULL, NULL, NULL)", (LONG_JID,))
    preview = whatsapp.send_message(SHORT, "synthetic", dry_run=True)[2]
    assert preview["recipient_name"] != "Other spelling"
    assert preview["payload"]["recipient"] == SHORT


@pytest.mark.parametrize("stored,query", [(SHORT, "(88) 97777-6666"), (LONG, "(88) 7777-6666")])
def test_national_extra_identity_follows_its_confirmed_lid_only(paired_dbs, stored, query):
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, stored))
    _chat(paired_dbs, f"{LID}@lid", "Clinic")
    rows = main.search_contacts(query)
    assert [(row["jid"], row["matched"]) for row in rows] == [(f"{LID}@lid", "phone_number")]
    patterns = whatsapp._jid_search_patterns(query, national_mobile=True)[0]
    assert f"{LID}@lid" in patterns and f"%{LID}@lid%" not in patterns
    assert f"{LID}@lid" not in whatsapp._jid_search_patterns(query)[0]


@pytest.mark.parametrize("replacement", ["latest", ""])
def test_phone_listings_include_all_confirmed_lid_note_aliases_in_one_batch(phone_pair, monkeypatch, replacement):
    other_lid = "100000000000001@lid"
    with phone_pair.whatsmeow() as conn:
        conn.executemany(
            "INSERT INTO whatsmeow_lid_map VALUES (?, ?)", [(LID, SHORT), (other_lid.partition("@")[0], LONG)]
        )
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('chat', ?, 'role', ?, ?, 'legacy', ?)",
            [
                (SHORT_JID, "stale", "2026-10-07T10:00:00+00:00", 1),
                (f"{LID}@lid", "older lid", "2026-10-07T11:00:00+00:00", 1),
                (other_lid, replacement, "2026-10-08T10:00:00+00:00", 2),
            ],
        )
    finally:
        conn.close()
    calls = []
    original = whatsapp._connect_whatsmeow_db

    def connect():
        conn = original()
        conn.set_trace_callback(calls.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", connect)
    expected = {jid: {"role": replacement} for jid in (SHORT_JID, LONG_JID)} if replacement else {}
    assert notes.fetch_notes_for("chat", [SHORT_JID, LONG_JID]) == expected
    assert len([sql for sql in calls if "SELECT lid, pn FROM whatsmeow_lid_map WHERE pn IN" in sql]) == 1
    assert main.get_notes("chat", SHORT_JID)["notes"] == main.get_notes("chat", LONG_JID)["notes"]
    row = main.get_chat(SHORT_JID)
    assert row["notes"] == ({"role": replacement} if replacement else {})


@pytest.mark.parametrize("target_type", ["chat", "contact", "message"])
def test_unmapped_lid_matching_a_phone_user_stays_in_its_namespace(paired_dbs, monkeypatch, target_type):
    jid = f"{BOB_PN}@lid"  # Only pn=BOB_PN is present; it is not this LID.
    unrelated = f"{BOB_LID}@s.whatsapp.net"
    suffix = "/synthetic-message" if target_type == "message" else ""
    assert whatsapp.canonical_note_jids([jid])[jid] == jid
    assert notes._jid_spellings(jid) == [jid]
    written = main.annotate(target_type, jid + suffix, "role", "anonymous note")
    assert written["target_id"] == jid + suffix
    assert main.get_notes(target_type, unrelated + suffix)["notes"] == {}
    assert notes.fetch_notes_for(target_type, [unrelated + suffix]) == {}
    _allow(monkeypatch, unrelated)
    assert main.annotate(target_type, jid + suffix, "role", "blocked")["error"]["code"] == "denied"
    assert main.get_notes(target_type, jid + suffix)["error"]["code"] == "denied"


@pytest.mark.parametrize("placeholder", [SHORT, "", None])
def test_contact_search_collapse_keeps_a_known_real_name(phone_pair, placeholder):
    with phone_pair.messages() as conn:
        conn.executemany("UPDATE chats SET name = ? WHERE jid = ?", [(placeholder, SHORT_JID), ("Clinic", LONG_JID)])
    hits = [row for row in main.search_contacts("5588") if row["jid"] == SHORT_JID]
    assert len(hits) == 1 and hits[0]["name"] == "Clinic"
    assert hits[0]["aliases"] == [SHORT_JID, LONG_JID]


@pytest.mark.parametrize("placeholder", [LONG, f"+{LONG[:4]} {LONG[4:8]}-{LONG[8:]}", "", None])
def test_phone_pair_name_order_and_cursor_use_the_displayed_real_name(phone_pair, placeholder):
    with phone_pair.messages() as conn:
        conn.execute("DELETE FROM chats WHERE jid NOT IN (?, ?)", (SHORT_JID, LONG_JID))
        conn.executemany("UPDATE chats SET name = ? WHERE jid = ?", [("Zulu", SHORT_JID), (placeholder, LONG_JID)])
    _chat(phone_pair, ALICE, "Aaron", "2026-10-08 11:00:00")
    first = whatsapp.list_chats_page(sort_by="name", limit=1)
    assert [(row["jid"], row["name"]) for row in first.items] == [(ALICE, "Aaron")]
    second = whatsapp.list_chats_page(sort_by="name", limit=1, cursor=first.next_cursor)
    assert [(row["jid"], row["name"]) for row in second.items] == [(SHORT_JID, "Zulu")]
    assert not second.has_more


@pytest.mark.parametrize("triples", [1, 100])
@pytest.mark.parametrize("members", ["triple", "phone_pair", "lid_pair"])
def test_triples_preserve_indexed_unread_joins_for_unrelated_group_history(paired_dbs, monkeypatch, triples, members):
    groups = [f"120363{index:012d}@g.us" for index in range(200)]
    with paired_dbs.messages() as conn:
        conn.execute("DELETE FROM messages")
        conn.execute("DELETE FROM chats")
        conn.execute("CREATE INDEX idx_messages_chat_timestamp ON messages(chat_jid, timestamp)")
        conn.executemany("INSERT INTO chats (jid,name) VALUES (?, 'Group')", [(jid,) for jid in groups])
        conn.executemany(
            "INSERT INTO messages (id,chat_jid,content,timestamp,is_from_me) VALUES (?, ?, 'synthetic', '2026-10-08 10:00:00', 0)",
            [(str(index), jid) for jid in groups for index in range(200)],
        )
    expected = {"count": 40000, "chats_with_unread": 200}
    assert whatsapp.list_unread(count_only=True) == expected
    for index in range(triples):
        short = f"5511{60000000 + index:08d}"
        long = short[:4] + "9" + short[4:]
        lid = str(100000000000100 + index)
        jids = [f"{short}@s.whatsapp.net"]
        if members != "lid_pair":
            jids.append(f"{long}@s.whatsapp.net")
        if members != "phone_pair":
            jids.append(f"{lid}@lid")
        for jid in jids:
            _chat(paired_dbs, jid)
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (lid, short))
    whatsapp._reset_name_cache()
    queries = []
    original = whatsapp._connect_messages_db

    def connect():
        conn = original()
        conn.set_trace_callback(queries.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    assert whatsapp.list_unread(count_only=True) == expected
    counted = next(sql for sql in queries if "SELECT COUNT(*), COUNT(DISTINCT chats.jid)" in sql)
    with paired_dbs.messages() as conn:
        plans = [row[3] for row in conn.execute("EXPLAIN QUERY PLAN " + counted)]
    assert any("SEARCH messages USING" in plan and "chat_jid=?" in plan for plan in plans), "\n".join(plans)
    assert not any("CORRELATED LIST SUBQUERY" in plan for plan in plans)


def test_name_matches_above_sqlite_budget_keep_complete_counts_and_cursor_pages(paired_dbs, monkeypatch):
    jids = [f"1202555{index:07d}@s.whatsapp.net" for index in range(1100)]
    with paired_dbs.messages() as conn:
        conn.execute("DELETE FROM chats")
        conn.executemany("INSERT INTO chats (jid,name) VALUES (?, '')", [(jid,) for jid in jids])
    with paired_dbs.whatsmeow() as conn:
        conn.executemany(
            "INSERT INTO whatsmeow_contacts (our_jid,their_jid,full_name) VALUES ('me', ?, 'Synthetic Clinic')",
            [(jid,) for jid in jids],
        )
    original = whatsapp._connect_messages_db

    def connect():
        conn = original()
        conn.setlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    assert whatsapp.count_chats(query="Synthetic Clinic") == len(jids)
    found, cursor = [], None
    while True:
        page = whatsapp.list_chats_page(query="Synthetic Clinic", limit=200, cursor=cursor)
        found.extend(row["jid"] for row in page.items)
        if not page.has_more:
            break
        cursor = page.next_cursor
        assert cursor is not None
    assert found == jids
    # Each read gets a fresh match table and current authorization, including
    # after a broad query has warmed contact-name and pair caches.
    _allow(monkeypatch, ALICE)
    assert whatsapp.count_chats(query="Synthetic Clinic") == 0
    assert whatsapp.list_chats_page(query="Synthetic Clinic").items == []
    with paired_dbs.messages() as conn:
        assert conn.execute("SELECT name FROM sqlite_master WHERE name = 'chat_name_matches'").fetchall() == []


def test_name_candidate_chunks_reserve_large_policy_budget(paired_dbs, monkeypatch):
    jids = [f"1202555{index:07d}@s.whatsapp.net" for index in range(600)]
    with paired_dbs.messages() as conn:
        conn.execute("DELETE FROM chats")
        conn.executemany("INSERT INTO chats (jid,name) VALUES (?, '')", [(jid,) for jid in [*jids, ALICE]])
    with paired_dbs.whatsmeow() as conn:
        conn.executemany(
            "INSERT INTO whatsmeow_contacts (our_jid,their_jid,full_name) VALUES ('me', ?, 'Synthetic Clinic')",
            [(jid,) for jid in [*jids, ALICE]],
        )
    _allow(monkeypatch, *jids)
    original = whatsapp._connect_messages_db

    def connect():
        conn = original()
        conn.setlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    assert whatsapp.count_chats() == 600
    assert whatsapp.count_chats(query="Synthetic Clinic") == 600
    found, cursor = [], None
    while True:
        page = whatsapp.list_chats_page(query="Synthetic Clinic", limit=200, cursor=cursor)
        found.extend(row["jid"] for row in page.items)
        if not page.has_more:
            break
        cursor = page.next_cursor
    assert found == jids and ALICE not in found
    _allow(monkeypatch, jids[-1])
    assert whatsapp.count_chats(query="Synthetic Clinic") == 1
    assert [row["jid"] for row in whatsapp.list_chats(query="Synthetic Clinic")] == [jids[-1]]


def test_maximum_message_page_keeps_canonical_notes_and_tombstones_at_999_bind_limit(phone_pair, monkeypatch):
    with phone_pair.messages() as conn:
        conn.execute("DELETE FROM messages")
        conn.executemany(
            "INSERT INTO messages (id,chat_jid,sender,content,timestamp,is_from_me) VALUES (?, ?, ?, 'synthetic', '2026-10-08 10:00:00', 0)",
            [(f"m{index:04d}", SHORT_JID, SHORT) for index in range(500)],
        )
    assert main.annotate("message", f"{SHORT_JID}/m0000", "role", "canonical")["target_id"] == f"{LONG_JID}/m0000"
    conn = notes._connect(create=True)
    try:
        conn.executemany(
            "INSERT INTO notes VALUES ('message', ?, 'role', 'legacy', '2099-01-01T00:00:00+00:00', 'legacy', 99)",
            [(f"{SHORT_JID}/m0000",), (f"{SHORT_JID}/m0499",)],
        )
    finally:
        conn.close()
    main.annotate("message", f"{LONG_JID}/m0499", "role", "")
    original = notes._connect

    def connect(create):
        conn = original(create)
        if conn is not None:
            conn.setlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
        return conn

    monkeypatch.setattr(notes, "_connect", connect)
    for limit in (499, 500):
        result = main.list_messages(chat_jid=SHORT_JID, limit=limit, include_context=False)
        assert "error" not in result and len(result["items"]) == limit
    rows = {row["id"]: row for row in result["items"]}
    assert rows["m0000"]["message_notes"] == {"role": "canonical"}
    assert "message_notes" not in rows["m0499"]


@pytest.mark.parametrize("with_lid", [False, True])
def test_identity_cap_accounts_for_large_policy_without_losing_quiet_rows(paired_dbs, monkeypatch, with_lid):
    with paired_dbs.messages() as conn:
        conn.execute("DELETE FROM messages")
        conn.execute("DELETE FROM chats")
    jids = []
    for index in range(60):
        short = f"5511{60000000 + index:08d}"
        long = short[:4] + "9" + short[4:]
        members = [f"{short}@s.whatsapp.net", f"{long}@s.whatsapp.net"]
        if with_lid:
            lid = str(100000000000100 + index)
            members.append(f"{lid}@lid")
            with paired_dbs.whatsmeow() as conn:
                conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (lid, short))
        for jid in members:
            _chat(paired_dbs, jid, "Clinic")
            _message(paired_dbs, jid, "own", f"2026-10-08 10:{index:02d}:00", from_me=True)
        jids.extend(members)
    for index in range(600 - len(jids)):
        jid = f"1202555{index:07d}@s.whatsapp.net"
        _chat(paired_dbs, jid, "Quiet")
        jids.append(jid)
    _allow(monkeypatch, *jids)
    original = whatsapp._connect_messages_db

    def connect():
        conn = original()
        conn.setlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_messages_db", connect)
    found, cursor = [], None
    while True:
        page = whatsapp.list_chats_page(sort_by="name", limit=200, cursor=cursor)
        found.extend(page.items)
        if not page.has_more:
            break
        cursor = page.next_cursor
    represented = [jid for row in found for jid in row.get("aliases", [row["jid"]])]
    assert sorted(represented) == sorted(jids)
    assert len({row["jid"] for row in found}) == len(found)
    assert whatsapp.count_chats() == len(found) < len(jids)
    assert whatsapp.list_unread(count_only=True) == {"count": 0, "chats_with_unread": 0}
    assert whatsapp.list_unanswered() == []
