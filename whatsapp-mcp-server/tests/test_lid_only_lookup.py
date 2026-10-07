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

    def test_a_pair_with_both_rows_is_still_the_merged_row(self, lid_only):
        with lid_only.messages() as conn:
            conn.execute(
                "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Phone row', '2026-10-07 18:00:00')",
                (SHORT_JID,),
            )
        assert whatsapp.get_direct_chat_by_contact(SHORT)["aliases"] == [SHORT_JID, LID_JID]

    @pytest.mark.parametrize("asked", [SHORT, LONG])
    def test_a_phone_row_comes_before_a_lid_row(self, lid_only, monkeypatch, asked):
        # The two rows unmerged (as past the pair cap): the ranking alone decides,
        # and it must not depend on the spelling asked.
        with lid_only.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Phone row')", (SHORT_JID,))
        monkeypatch.setattr(whatsapp, "_chat_twins", lambda cursor, only=None: whatsapp.NO_CHAT_TWINS)
        assert whatsapp.get_direct_chat_by_contact(asked)["jid"] == SHORT_JID

    def test_every_lid_of_the_number_is_tried(self, lid_only):
        # Only `lid` is unique in the map: the chat may be under the second LID.
        second = "290000000000005"
        with lid_only.whatsmeow() as conn:
            conn.execute("DELETE FROM whatsmeow_lid_map WHERE lid = ?", (LID,))
            conn.executemany("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", [("290000000000000", SHORT), (LID, SHORT)])
        assert second not in LID_JID
        assert whatsapp.get_direct_chat_by_contact(SHORT)["jid"] == LID_JID
        assert [hit["jid"] for hit in whatsapp.search_contacts(SHORT)] == [LID_JID]

    def test_a_chat_that_appears_later_is_found_at_once(self, paired_dbs):
        # Nothing is cached on the way: "not found" must not outlive the first message.
        assert whatsapp.get_direct_chat_by_contact(SHORT) is None
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
        with paired_dbs.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Acme Clinic')", (LID_JID,))
        assert whatsapp.get_direct_chat_by_contact(SHORT)["jid"] == LID_JID

    def test_a_number_the_map_does_not_know_finds_nothing(self, lid_only):
        assert whatsapp.get_direct_chat_by_contact("5511977776666") is None
        assert whatsapp.get_direct_chat_by_contact("548877776667") is None

    def test_the_lid_must_be_on_the_allow_list_itself(self, lid_only, monkeypatch):
        # The list names the LID, and nothing else: the number, in either
        # spelling, reaches that allowed chat (its rows state the number anyway).
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([LID_JID]))
        for asked in (SHORT, LONG):
            assert whatsapp.get_direct_chat_by_contact(asked)["jid"] == LID_JID
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
    @pytest.mark.parametrize("query", [SHORT, LONG, SHORT_JID, LONG_JID, "+55 (88) 7777-6666"])
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

    @pytest.mark.parametrize("fragment", ["7777-6666", "88777766", "5588777766"])
    def test_a_fragment_is_not_followed_to_a_lid(self, lid_only, fragment):
        # The map holds everybody ever seen in a group: only a whole number is looked up.
        assert whatsapp.search_contacts(fragment) == []
        assert whatsapp.list_chats(query=fragment) == []

    def test_the_whole_number_not_a_longer_one(self, lid_only):
        other = "290000000000002"
        with lid_only.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (other, f"1{SHORT}"))
        with lid_only.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Longer number')", (f"{other}@lid",))
        for query in (SHORT, SHORT_JID, LONG):
            assert [hit["jid"] for hit in whatsapp.search_contacts(query)] == [LID_JID]

    def test_a_contact_with_both_rows_is_one_hit(self, lid_only):
        with lid_only.messages() as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Acme Clinic')", (SHORT_JID,))
        for query in (SHORT, LONG):
            (hit,) = whatsapp.search_contacts(query)
            assert (hit["jid"], hit["matched"]) == (SHORT_JID, "jid")

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
