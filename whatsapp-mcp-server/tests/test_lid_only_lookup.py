"""A chat stored only under its LID is found by the contact's phone number (issue #465).

WhatsApp keeps some direct chats under `<lid>@lid` and never under the phone
JID. `whatsmeow_lid_map` knows the number behind the LID, so a lookup by that
number, in either spelling of a Brazilian mobile, has to reach the row. Fake
identifiers throughout.
"""

import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

# One Brazilian mobile, both spellings; the map knows it without the ninth digit.
SHORT, LONG = "558877776666", "5588977776666"
SHORT_JID, LONG_JID = f"{SHORT}@s.whatsapp.net", f"{LONG}@s.whatsapp.net"
LID = "290000000000001"
LID_JID = f"{LID}@lid"
# A foreign number with a LID of its own.
FOREIGN, FOREIGN_LID = "548877776666", "290000000000009"


@pytest.fixture
def lid_only(paired_dbs):
    """The chat exists under the LID alone; the map pairs the LID with the number."""
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Acme Clinic', '2026-10-07 17:14:00')",
            (LID_JID,),
        )
    with paired_dbs.whatsmeow() as conn:
        conn.executemany("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", [(LID, SHORT), (FOREIGN_LID, FOREIGN)])
    whatsapp._reset_name_cache()
    return paired_dbs


ASKED = pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID, "+55 (88) 7777-6666", "55 88 97777-6666"])


class TestDirectChat:
    @ASKED
    def test_found_by_the_phone_number(self, lid_only, asked):
        assert whatsapp.get_direct_chat_by_contact(asked)["jid"] == LID_JID

    def test_the_phone_row_wins_when_there_is_one(self, lid_only):
        # No message on either side, so nothing merges the two rows.
        with lid_only.messages() as conn:
            conn.execute(
                "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Phone row', '2026-10-07 18:00:00')",
                (SHORT_JID,),
            )
        assert whatsapp.get_direct_chat_by_contact(SHORT)["aliases"] == [SHORT_JID, LID_JID]

    def test_a_number_the_map_does_not_know_finds_nothing(self, lid_only):
        assert whatsapp.get_direct_chat_by_contact("5588966665555") is None
        assert whatsapp.get_direct_chat_by_contact("548877776667") is None

    def test_the_lid_must_be_on_the_allow_list_itself(self, lid_only, monkeypatch):
        # The list names the LID: the number, in either spelling, reaches it.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([LID_JID, SHORT_JID, f"{SHORT}@lid"]))
        assert whatsapp.get_direct_chat_by_contact(SHORT)["jid"] == LID_JID
        # The list names the phone number only (in every form the lookup tries):
        # it does not expand to the LID, so the row stays hidden.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([SHORT_JID, f"{SHORT}@lid"]))
        assert whatsapp.get_direct_chat_by_contact(SHORT) is None
        # The list names neither: denied, as for any number outside it.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
        for asked in (SHORT, LONG):
            with pytest.raises(ToolError) as exc:
                whatsapp.get_direct_chat_by_contact(asked)
            assert exc.value.code == "denied"

    def test_no_whatsmeow_database(self, lid_only, monkeypatch):
        monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
        whatsapp._reset_name_cache()
        assert whatsapp.get_direct_chat_by_contact(SHORT) is None


class TestSearchContacts:
    @pytest.mark.parametrize("query", [SHORT, LONG, SHORT_JID, LONG_JID, "+55 (88) 7777-6666", "7777-6666"])
    def test_found_by_the_phone_number(self, lid_only, query):
        (hit,) = whatsapp.search_contacts(query)
        assert (hit["jid"], hit["phone_number"], hit["lid"]) == (LID_JID, SHORT, LID)
        assert (hit["name"], hit["matched"]) == ("Acme Clinic", "phone_number")

    def test_a_name_search_still_says_name(self, lid_only):
        (hit,) = whatsapp.search_contacts("acme")
        assert (hit["jid"], hit["matched"]) == (LID_JID, "name")

    def test_the_phone_book_entry_under_the_lid_is_found_too(self, paired_dbs):
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
            conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, NULL, 'Acme Clinic', NULL, NULL)", (LID_JID,))
        (hit,) = whatsapp.search_contacts(LONG)
        assert (hit["jid"], hit["phone_number"], hit["matched"]) == (LID_JID, SHORT, "phone_number")

    def test_a_whole_jid_is_not_a_fragment(self, lid_only):
        # The digits of a JID query name that number, not every number holding them.
        with lid_only.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES ('290000000000002', ?)", (f"1{SHORT}",))
        with lid_only.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES ('290000000000002@lid', 'Longer number')")
        assert [hit["jid"] for hit in whatsapp.search_contacts(SHORT_JID)] == [LID_JID]
        # As bare digits it is a substring search, as it is against a phone JID.
        assert len(whatsapp.search_contacts(SHORT)) == 2

    def test_a_foreign_number_has_one_spelling(self, lid_only):
        with lid_only.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Abroad')", (f"{FOREIGN_LID}@lid",))
        assert [hit["jid"] for hit in whatsapp.search_contacts(FOREIGN)] == [f"{FOREIGN_LID}@lid"]
        assert whatsapp.search_contacts("5488977776666") == []

    def test_no_whatsmeow_database(self, lid_only, monkeypatch):
        monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
        whatsapp._reset_name_cache()
        assert whatsapp.search_contacts(SHORT) == []


class TestListChats:
    @pytest.mark.parametrize("query", [SHORT, LONG, "+55 (88) 7777-6666"])
    def test_the_chat_is_listed_by_its_number(self, lid_only, query):
        assert [chat["jid"] for chat in whatsapp.list_chats(query=query)] == [LID_JID]
        assert whatsapp.count_chats(query=query) == 1

    def test_the_allow_list_still_filters_the_listing(self, lid_only, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([SHORT_JID]))
        assert whatsapp.list_chats(query=SHORT) == []
