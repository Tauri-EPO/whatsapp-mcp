"""search_contacts and the sender-name / LID resolution layer over both databases."""

import sqlite3

import whatsapp
from tests.conftest import ALICE, BOB, BOB_LID, BOB_PN, CARLA, DECOY, FAMILY


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


def test_resolve_lid_to_phone(paired_dbs, monkeypatch):
    assert whatsapp._resolve_lid_to_phone(BOB_LID) == BOB_PN
    assert whatsapp._resolve_lid_to_phone(f"{BOB_LID}@lid") == BOB_PN
    assert whatsapp._resolve_lid_to_phone("000") is None
    with paired_dbs.whatsmeow() as c:
        c.execute("DROP TABLE whatsmeow_lid_map")
    assert whatsapp._resolve_lid_to_phone(BOB_LID) is None
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
    assert whatsapp._resolve_lid_to_phone(BOB_LID) is None


def test_resolve_name_from_whatsmeow_handles_every_jid_form(paired_dbs, monkeypatch):
    assert whatsapp._resolve_name_from_whatsmeow(BOB) == "Bob Silva"
    assert whatsapp._resolve_name_from_whatsmeow(f"{BOB_LID}@lid") == "Bob Silva"  # via the LID map
    assert whatsapp._resolve_name_from_whatsmeow(BOB_LID) == "Bob Silva"  # bare LID
    assert whatsapp._resolve_name_from_whatsmeow(CARLA) == "Carla Consultoria"
    assert whatsapp._resolve_name_from_whatsmeow("999@lid") is None  # LID not in the map: stop
    assert whatsapp._resolve_name_from_whatsmeow(BOB_PN) is None  # bare phone without contact row for the bare form
    with paired_dbs.whatsmeow() as c:
        c.execute("DROP TABLE whatsmeow_contacts")
    assert whatsapp._resolve_name_from_whatsmeow(BOB) is None
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
    assert whatsapp._resolve_name_from_whatsmeow(BOB) is None


def test_resolve_name_prefers_full_then_push_then_first(paired_dbs):
    with paired_dbs.whatsmeow() as c:
        c.execute("UPDATE whatsmeow_contacts SET full_name = NULL WHERE their_jid = ?", (BOB,))
    assert whatsapp._resolve_name_from_whatsmeow(BOB) == "bobby"
    with paired_dbs.whatsmeow() as c:
        c.execute("UPDATE whatsmeow_contacts SET push_name = '' WHERE their_jid = ?", (BOB,))
    assert whatsapp._resolve_name_from_whatsmeow(BOB) == "Bob"


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
