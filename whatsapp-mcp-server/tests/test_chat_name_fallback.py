"""Chat names fall back to the phone book (issue #230).

`chats.name` is whatever WhatsApp pushed for the conversation; for a large
share of direct chats it is empty or the bare number even when the contact is
saved. These tests cover the fallback to whatsmeow_contacts, the `name_source`
flag, and the promise that resolution is batched per page, not per chat.
"""

from __future__ import annotations

import pytest

import whatsapp
from tests.conftest import BOB, BOB_LID, BOB_PN, CARLA

STRANGER = "5599000000000@s.whatsapp.net"
UNNAMED_GROUP = "120363000000000009@g.us"


@pytest.fixture
def unnamed_chats(paired_dbs):
    """Bob stored as digits, Carla and Bob's LID stored with no name at all."""
    with paired_dbs.messages() as conn:
        conn.execute("UPDATE chats SET name = ? WHERE jid = ?", (BOB_PN, BOB))
        conn.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
            [
                (CARLA, None, "2026-09-04 08:00:00"),
                (f"{BOB_LID}@lid", "", "2026-09-04 07:00:00"),
                (STRANGER, None, "2026-09-04 06:00:00"),
                (UNNAMED_GROUP, None, "2026-09-04 05:00:00"),
            ],
        )
    whatsapp._reset_name_cache()
    return paired_dbs


def _by_jid(chats):
    return {chat["jid"]: chat for chat in chats}


def test_list_chats_uses_the_phone_book_for_unnamed_chats(unnamed_chats):
    chats = _by_jid(whatsapp.list_chats(limit=50))

    # Digits where the contact store knows a name.
    assert chats[BOB]["name"] == "Bob Silva"
    assert chats[BOB]["name_source"] == "contacts"
    # No name at all; business_name is the last resort of the chain.
    assert chats[CARLA]["name"] == "Carla Consultoria"
    assert chats[CARLA]["name_source"] == "contacts"


def test_lid_chats_resolve_through_the_lid_map(unnamed_chats):
    chat = _by_jid(whatsapp.list_chats(limit=50))[f"{BOB_LID}@lid"]
    assert chat["name"] == "Bob Silva"
    assert chat["name_source"] == "contacts"


def test_stored_names_are_kept_and_flagged(unnamed_chats):
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert chats["5511999999999@s.whatsapp.net"]["name"] == "Alice"
    assert chats["5511999999999@s.whatsapp.net"]["name_source"] == "chat"


def test_unknown_chats_and_groups_report_jid(unnamed_chats):
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert chats[STRANGER]["name"] is None
    assert chats[STRANGER]["name_source"] == "jid"
    # Groups have no phone-book entry; they are never looked up.
    assert chats[UNNAMED_GROUP]["name"] is None
    assert chats[UNNAMED_GROUP]["name_source"] == "jid"


def test_resolution_is_batched_not_one_query_per_chat(unnamed_chats, monkeypatch):
    """A page of N unnamed chats costs the same two queries as a page of one."""
    executed: list[str] = []
    real_connect = whatsapp._connect_whatsmeow_db

    def recording_connect():
        conn = real_connect()
        conn.set_trace_callback(executed.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", recording_connect)

    whatsapp.list_chats(limit=50)
    contact_queries = [q for q in executed if "whatsmeow_contacts" in q]
    lid_queries = [q for q in executed if "whatsmeow_lid_map" in q]
    assert len(contact_queries) == 1, executed
    assert len(lid_queries) == 1, executed

    # Four more unnamed chats, same query count.
    with unnamed_chats.messages() as conn:
        conn.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, NULL, '2026-09-03 10:00:00')",
            [(f"55990000{n:04d}@s.whatsapp.net",) for n in range(4)],
        )
    whatsapp._reset_name_cache()
    executed.clear()
    whatsapp.list_chats(limit=50)
    assert len([q for q in executed if "whatsmeow_contacts" in q]) == 1, executed
    assert len([q for q in executed if "whatsmeow_lid_map" in q]) == 1, executed


def test_cached_names_skip_the_contact_store_entirely(unnamed_chats, monkeypatch):
    whatsapp.list_chats(limit=50)

    executed: list[str] = []
    real_connect = whatsapp._connect_whatsmeow_db

    def recording_connect():
        conn = real_connect()
        conn.set_trace_callback(executed.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", recording_connect)
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert chats[CARLA]["name"] == "Carla Consultoria"
    assert executed == []


def test_get_chat_and_direct_chat_resolve_too(unnamed_chats):
    chat = whatsapp.get_chat(CARLA)
    assert chat["name"] == "Carla Consultoria"
    assert chat["name_source"] == "contacts"

    direct = whatsapp.get_direct_chat_by_contact(BOB_PN)
    assert direct["name"] == "Bob Silva"
    assert direct["name_source"] == "contacts"


def test_get_contact_chats_resolves_too(unnamed_chats):
    chats = _by_jid(whatsapp.get_contact_chats(CARLA))
    assert chats[CARLA]["name"] == "Carla Consultoria"
    assert chats[CARLA]["name_source"] == "contacts"


def test_missing_contact_store_leaves_names_alone(unnamed_chats, monkeypatch, tmp_path):
    """No whatsapp.db (stdio setups that only ship messages.db) must not error."""
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    whatsapp._reset_name_cache()
    chats = _by_jid(whatsapp.list_chats(limit=50))
    assert chats[CARLA]["name"] is None
    assert chats[CARLA]["name_source"] == "jid"
    assert chats[BOB]["name"] == BOB_PN
    assert chats[BOB]["name_source"] == "jid"
