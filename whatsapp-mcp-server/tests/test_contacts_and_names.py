"""search_contacts and the sender-name / LID resolution layer over both databases."""

import sqlite3
from datetime import datetime

import whatsapp
from tests.conftest import ALICE, BOB, BOB_LID, BOB_PN, CARLA, DECOY, FAMILY

# A LID nobody mapped: too long to be an E.164 number, so its shape gives it away.
UNKNOWN_LID = "1171581346817350"


def _jids(rows):
    return [r["jid"] for r in rows]


def test_search_contacts_merges_both_stores_and_dedupes(paired_dbs):
    rows = whatsapp.search_contacts("bob")
    assert _jids(rows) == [BOB]  # in both DBs, listed once
    assert rows[0]["name"] == "Bob"  # messages.db wins
    assert rows[0]["phone_number"] == BOB_PN


def test_search_contacts_by_jid_digits_excludes_groups(paired_dbs):
    rows = whatsapp.search_contacts("5511999999999")
    assert set(_jids(rows)) == {ALICE, DECOY}  # substring match on JID is intended here
    assert FAMILY not in _jids(whatsapp.search_contacts("Family"))


def test_search_contacts_is_case_and_unicode_aware(paired_dbs):
    with paired_dbs.messages() as c:
        c.execute("INSERT INTO chats (jid, name) VALUES ('5511666666666@s.whatsapp.net', 'José Ção')")
    assert _jids(whatsapp.search_contacts("josé")) == ["5511666666666@s.whatsapp.net"]
    assert _jids(whatsapp.search_contacts("Ção")) == ["5511666666666@s.whatsapp.net"]
    assert set(_jids(whatsapp.search_contacts("ALICE"))) == {ALICE, DECOY}  # "Not Alice" matches too


def test_search_contacts_keeps_lids_out_of_phone_number(paired_dbs):
    """The same namespace split get_contact and message rows report (#281)."""
    with paired_dbs.messages() as c:
        c.execute("INSERT INTO chats (jid, name) VALUES (?, 'Vicky')", (f"{BOB_LID}@lid",))
        c.execute("INSERT INTO chats (jid, name) VALUES (?, 'Vicky Two')", (f"{UNKNOWN_LID}@lid",))
    mapped, unmapped = whatsapp.search_contacts("Vicky")
    assert (mapped["jid"], mapped["lid"], mapped["phone_number"]) == (f"{BOB_LID}@lid", BOB_LID, BOB_PN)
    assert (unmapped["jid"], unmapped["lid"], unmapped["phone_number"]) == (
        f"{UNKNOWN_LID}@lid",
        UNKNOWN_LID,
        None,
    )


def test_search_contacts_whatsmeow_only_contact_uses_name_fallback_chain(paired_dbs):
    rows = whatsapp.search_contacts("consultoria")
    assert _jids(rows) == [CARLA]
    assert rows[0]["name"] == "Carla Consultoria"  # business_name when the others are NULL


def test_search_contacts_without_whatsmeow_db(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(paired_dbs.whatsmeow_db.parent / "missing.db"))
    assert _jids(whatsapp.search_contacts("bob")) == [BOB]
    assert whatsapp.search_contacts("consultoria") == []


def test_search_contacts_survives_broken_databases(paired_dbs, caplog):
    with paired_dbs.messages() as c:
        c.execute("DROP TABLE chats")
    with paired_dbs.whatsmeow() as c:
        c.execute("DROP TABLE whatsmeow_contacts")
    assert whatsapp.search_contacts("bob") == []
    assert "Database error" in caplog.text


def test_sender_aliases_from_phone_lid_and_unknown(paired_dbs):
    assert whatsapp._sender_aliases(BOB_PN) == [BOB_PN, BOB, BOB_LID, f"{BOB_LID}@lid"]
    assert whatsapp._sender_aliases(f"{BOB_LID}@lid") == [BOB_PN, BOB, BOB_LID, f"{BOB_LID}@lid"]
    assert whatsapp._sender_aliases("5599999999999@s.whatsapp.net") == [
        "5599999999999",
        "5599999999999@s.whatsapp.net",
        "5599999999999@lid",
    ]


def test_sender_aliases_cached_and_copied(paired_dbs):
    first = whatsapp._sender_aliases(BOB_PN)
    first.append("mutated")
    assert "mutated" not in whatsapp._sender_aliases(BOB_PN)
    with paired_dbs.whatsmeow() as c:
        c.execute("DELETE FROM whatsmeow_lid_map")
    assert BOB_LID in whatsapp._sender_aliases(BOB_PN)  # served from cache
    whatsapp._reset_name_cache()
    assert BOB_LID not in whatsapp._sender_aliases(BOB_PN)


def test_sender_aliases_without_whatsmeow_db(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
    assert whatsapp._sender_aliases(BOB_PN) == [BOB_PN, BOB, f"{BOB_PN}@lid"]


def test_sender_identity_keeps_the_two_namespaces_apart(paired_dbs):
    """A phone number is never a LID and a LID is never a phone number (#281)."""
    assert whatsapp.sender_identity(BOB_LID) == (BOB_PN, BOB_LID)  # bare LID, resolved
    assert whatsapp.sender_identity(f"{BOB_LID}@lid") == (BOB_PN, BOB_LID)
    assert whatsapp.sender_identity(BOB_PN) == (BOB_PN, None)  # bare phone stays a phone
    assert whatsapp.sender_identity(BOB) == (BOB_PN, None)
    assert whatsapp.sender_identity(UNKNOWN_LID) == (None, UNKNOWN_LID)  # over 15 digits
    assert whatsapp.sender_identity(f"{UNKNOWN_LID}@lid") == (None, UNKNOWN_LID)
    assert whatsapp.sender_identity("") == ("", None)  # no identifier at all


def test_sender_identity_without_the_lid_map(paired_dbs, monkeypatch):
    """No map, no resolution — but a LID is still not reported as a phone number."""
    with paired_dbs.whatsmeow() as c:
        c.execute("DROP TABLE whatsmeow_lid_map")
    whatsapp._reset_name_cache()
    assert whatsapp.sender_identity(BOB_LID) == (BOB_LID, None)  # indistinguishable from a number
    assert whatsapp.sender_identity(f"{BOB_LID}@lid") == (None, BOB_LID)  # the JID says it
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
    whatsapp._reset_name_cache()
    assert whatsapp.sender_identity(UNKNOWN_LID) == (None, UNKNOWN_LID)


def test_sender_identities_batch_one_query_and_cache(paired_dbs, monkeypatch):
    counts = {"n": 0}
    real = whatsapp._connect_whatsmeow_db

    def counted():
        counts["n"] += 1
        return real()

    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", counted)
    values = [BOB_LID, f"{BOB_LID}@lid", UNKNOWN_LID, BOB_PN, BOB] * 40
    identities = whatsapp._sender_identities(values)
    assert counts["n"] == 1, counts  # one connection for the whole page
    assert identities[BOB_LID] == (BOB_PN, BOB_LID)
    whatsapp._sender_identities(values)
    assert counts["n"] == 1, counts  # second page served from the cache


def _lid_message(sender: str) -> whatsapp.Message:
    return whatsapp.Message(
        timestamp=datetime(2026, 9, 4, 10, 0, 0),
        sender=sender,
        content="hi",
        is_from_me=False,
        chat_jid=FAMILY,
        id="M1",
    )


def test_msg_to_dict_resolves_a_lid_sender_to_its_phone(paired_dbs):
    row = whatsapp.msg_to_dict(_lid_message(BOB_LID))
    assert row["sender_jid"] == BOB_LID  # unchanged: the value the bridge stored
    assert row["sender_phone"] == BOB_PN
    assert row["sender_lid"] == BOB_LID
    assert row["sender_name"] == "Bob Silva"
    assert row["sender_display"] == f"Bob Silva ({BOB_PN})"


def test_msg_to_dict_never_reports_an_unresolved_lid_as_a_phone_or_a_name(paired_dbs):
    row = whatsapp.msg_to_dict(_lid_message(UNKNOWN_LID))
    assert row["sender_phone"] is None  # grouping by sender_phone gets no ghost contact
    assert row["sender_lid"] == UNKNOWN_LID
    assert row["sender_name"] is None  # nobody is called "1171581346817350"
    assert row["sender_display"] == f"{UNKNOWN_LID}@lid"


def test_msg_to_dict_page_shares_one_sender_lookup(paired_dbs, monkeypatch):
    counts = {"n": 0}
    real = whatsapp._connect_whatsmeow_db

    def counted():
        counts["n"] += 1
        return real()

    messages = [_lid_message(BOB_LID), _lid_message(UNKNOWN_LID)] * 25
    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", counted)
    rows = whatsapp.msgs_to_dicts(messages, include_sender_name=False)
    assert {row["sender_lid"] for row in rows} == {BOB_LID, UNKNOWN_LID}
    assert counts["n"] == 1, counts


def test_contact_names_handle_every_jid_form(paired_dbs, monkeypatch):
    """The one phone-book lookup: senders and chat rows both go through it (#257)."""
    assert whatsapp._contact_names([BOB, f"{BOB_LID}@lid", BOB_LID, CARLA, "999@lid", FAMILY]) == {
        BOB: "Bob Silva",
        f"{BOB_LID}@lid": "Bob Silva",  # via the LID map
        BOB_LID: "Bob Silva",  # bare LID
        CARLA: "Carla Consultoria",
        # "999@lid" is a LID that is not in the map, FAMILY is a group: neither
        # has a phone-book entry, and both are reported as simply absent.
    }
    assert whatsapp._contact_names([BOB_PN]) == {BOB_PN: "Bob Silva"}  # bare phone, tried as a phone JID
    with paired_dbs.whatsmeow() as c:
        c.execute("DROP TABLE whatsmeow_contacts")
    whatsapp._reset_name_cache()
    assert whatsapp._contact_names([BOB]) == {}
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
    whatsapp._reset_name_cache()
    assert whatsapp._contact_names([BOB]) == {}


def test_contact_names_prefer_full_then_push_then_first(paired_dbs):
    with paired_dbs.whatsmeow() as c:
        c.execute("UPDATE whatsmeow_contacts SET full_name = NULL WHERE their_jid = ?", (BOB,))
    whatsapp._reset_name_cache()
    assert whatsapp._contact_names([BOB]) == {BOB: "bobby"}
    with paired_dbs.whatsmeow() as c:
        c.execute("UPDATE whatsmeow_contacts SET push_name = '' WHERE their_jid = ?", (BOB,))
    whatsapp._reset_name_cache()
    assert whatsapp._contact_names([BOB]) == {BOB: "Bob"}


def test_a_name_that_is_just_the_number_identifies_nobody(paired_dbs):
    """get_sender_name and the chat-row fallback now agree on that (#257)."""
    with paired_dbs.whatsmeow() as c:
        c.execute("UPDATE whatsmeow_contacts SET full_name = ? WHERE their_jid = ?", ("+55 11 8888-8888", BOB))
    whatsapp._reset_name_cache()
    assert whatsapp._contact_names([f"{BOB_LID}@lid"]) == {}
    assert whatsapp.get_sender_name(f"{BOB_LID}@lid") == f"{BOB_LID}@lid"
    # Same rule on the chats.name side of the chain.
    with paired_dbs.messages() as c:
        c.execute("UPDATE chats SET name = '5511888888888' WHERE jid = ?", (BOB,))
    whatsapp._reset_name_cache()
    assert whatsapp.get_sender_name(BOB) == BOB


def test_get_sender_name_fallback_chain(paired_dbs):
    assert whatsapp.get_sender_name(ALICE) == "Alice"  # chats.name
    assert whatsapp.get_sender_name("5511999999999") == "Alice"  # bare number matches the JID spelling
    assert (
        whatsapp.get_sender_name(f"{BOB_LID}@lid") == "Bob Silva"
    )  # not in chats under that spelling: whatsmeow via LID
    assert whatsapp.get_sender_name("5521777777777") == "Carla Consultoria"  # bare number + @s.whatsapp.net retry
    assert whatsapp.get_sender_name("5500000000000@s.whatsapp.net") == "5500000000000@s.whatsapp.net"


def test_get_sender_name_ignores_numeric_chat_names(paired_dbs):
    # The bridge stores the phone as the chat name until a push name arrives.
    with paired_dbs.messages() as c:
        c.execute("UPDATE chats SET name = '+5511888888888' WHERE jid = ?", (BOB,))
    assert whatsapp.get_sender_name(BOB) == "Bob Silva"


def test_get_sender_name_survives_database_errors(paired_dbs, monkeypatch, caplog):
    def boom():
        raise sqlite3.OperationalError("locked")

    monkeypatch.setattr(whatsapp, "_connect_messages_db", boom)
    assert whatsapp.get_sender_name(ALICE) == ALICE
    assert "Database error" in caplog.text


def test_get_contact_chats_matches_every_spelling_of_the_sender(paired_dbs):
    # Bob's messages were stored under three spellings; asking with any one of
    # them finds the chats they appear in.
    with paired_dbs.messages() as c:
        c.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES ('120363000000000002@g.us', 'Work', '2026-09-04 10:00:00')"
        )
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
            [
                ("m1", FAMILY, BOB_PN, "hi", "2026-09-04 10:00:00"),
                ("m2", "120363000000000002@g.us", f"{BOB_LID}@lid", "hi", "2026-09-04 10:00:00"),
                ("m3", ALICE, BOB, "hi", "2026-09-04 10:00:00"),
            ],
        )
    for spelling in (BOB_PN, BOB, BOB_LID, f"{BOB_LID}@lid"):
        found = {row["jid"] for row in whatsapp.get_contact_chats(spelling)}
        assert found == {FAMILY, "120363000000000002@g.us", ALICE, BOB}, spelling  # + the DM with Bob
