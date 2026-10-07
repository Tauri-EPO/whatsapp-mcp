"""A contact is found whichever way its number is written (issue #444).

WhatsApp registers a Brazilian mobile with or without the ninth digit after
the area code and the store holds that spelling alone; the lookups have to
answer for the other one too, and for a number typed with its separators.
"""

import pytest

import phone
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

# One mobile, both spellings: area code 88, without and with the ninth digit.
SHORT, LONG = "558877776666", "5588977776666"
SHORT_JID, LONG_JID = f"{SHORT}@s.whatsapp.net", f"{LONG}@s.whatsapp.net"
# The same digits under another country code: nothing Brazilian about them.
FOREIGN_SHORT, FOREIGN_LONG = "548877776666", "5488977776666"
# A landline and the mobile that differs from it by a ninth digit: two subscribers.
LANDLINE, LANDLINE_PLUS_NINE = "551133334444", "5511933334444"


def _add_chat(store, jid, name="Paroquia"):
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", (jid, name, "2026-10-07 17:14:00")
        )


def _jids(rows):
    return [row["jid"] for row in rows]


class TestSpellings:
    @pytest.mark.parametrize(
        "typed",
        [
            "5588977776666",
            "+5588977776666",
            "+55 (88) 97777-6666",
            "55 88 9 7777 6666",
            "55.88.97777.6666",
            " 55\u00a088 97777-6666 ",
            # As copied out of WhatsApp: directional marks and a non-breaking hyphen.
            "\u202a+55 88 97777\u20116666\u202c",
            "55 88 97777\u20136666",
        ],
    )
    def test_separators_are_dropped(self, typed):
        assert phone.phone_digits(typed) == LONG

    @pytest.mark.parametrize(
        "typed",
        [
            "",
            "  ",
            "+",
            "1.5",
            "(11)",
            "555-12",
            "bob",
            "bob 55",
            "5588977776666@s.whatsapp.net",
            "55%",
            "55_8",
            "٥٥٨٨",
        ],
    )
    def test_anything_else_is_not_a_number(self, typed):
        assert phone.phone_digits(typed) is None

    def test_a_brazilian_mobile_has_two_spellings(self):
        assert phone.br_mobile_alternate(LONG) == SHORT
        assert phone.br_mobile_alternate(SHORT) == LONG
        # An eight-digit number that itself begins with 9 still gains one.
        assert phone.br_mobile_alternate("558899998888") == "5588999998888"
        assert phone.br_mobile_alternate("5588999998888") == "558899998888"

    @pytest.mark.parametrize(
        "digits",
        [
            FOREIGN_SHORT,
            FOREIGN_LONG,
            "12025551234",  # US
            "351912345678",  # Portugal, twelve digits
            LANDLINE,  # subscriber number begins 3: no ninth digit to add
            LANDLINE_PLUS_NINE,  # dropping the 9 would name the landline
            "5588877776666",  # thirteen digits, but the fifth is not the ninth digit
            "551077776666",  # no area code has a zero
            "8877776666",  # no country code
            "55889777766",  # partial
            "55889777766661",  # too long
            "",
        ],
    )
    def test_every_other_number_has_one(self, digits):
        assert phone.br_mobile_alternate(digits) is None


class TestSearchContacts:
    @pytest.mark.parametrize("query", [LONG, "+55 (88) 97777-6666", "55 88 97777-6666"])
    def test_stored_without_the_ninth_digit_found_with_it(self, paired_dbs, query):
        _add_chat(paired_dbs, SHORT_JID)
        (hit,) = whatsapp.search_contacts(query)
        assert hit["jid"] == SHORT_JID
        assert hit["phone_number"] == SHORT
        assert hit["matched"] == "jid"

    @pytest.mark.parametrize("query", [SHORT, "+55 (88) 7777-6666"])
    def test_stored_with_the_ninth_digit_found_without_it(self, paired_dbs, query):
        _add_chat(paired_dbs, LONG_JID)
        (hit,) = whatsapp.search_contacts(query)
        assert hit["jid"] == LONG_JID
        assert hit["matched"] == "jid"

    def test_the_phone_book_is_searched_the_same_way(self, paired_dbs):
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_contacts VALUES ('me', ?, NULL, 'Paroquia', NULL, NULL)", (SHORT_JID,))
        (hit,) = whatsapp.search_contacts(LONG)
        assert (hit["jid"], hit["name"], hit["matched"]) == (SHORT_JID, "Paroquia", "jid")

    def test_a_typed_number_matches_as_its_digits(self, paired_dbs):
        (hit,) = whatsapp.search_contacts("+55 11 88888-8888")
        assert (hit["name"], hit["matched"]) == ("Bob", "jid")
        (partial,) = whatsapp.search_contacts("8888-8888")
        assert partial["name"] == "Bob"

    def test_the_phone_jid_is_a_number_too(self, paired_dbs):
        _add_chat(paired_dbs, SHORT_JID)
        (hit,) = whatsapp.search_contacts(LONG_JID)
        assert (hit["jid"], hit["matched"]) == (SHORT_JID, "jid")
        # The other spelling is still a whole number, and a LID has none.
        _add_chat(paired_dbs, f"1{SHORT}@s.whatsapp.net", "Longer number")
        assert _jids(whatsapp.search_contacts(LONG_JID)) == [SHORT_JID]
        assert whatsapp.search_contacts(f"{LONG}@lid") == []

    @pytest.mark.parametrize("query", ["1.5", "(11)", "55-11", "99 99"])
    def test_a_short_numeric_query_is_not_a_phone_number(self, paired_dbs, query):
        # Every JID in the store contains these digits; none contains the query as typed.
        assert whatsapp.search_contacts(query) == []

    def test_the_other_spelling_is_a_whole_number_not_a_fragment(self, paired_dbs):
        _add_chat(paired_dbs, f"1{SHORT}@s.whatsapp.net", "Longer number")
        _add_chat(paired_dbs, f"{SHORT}1@lid", "A LID")
        assert whatsapp.search_contacts(LONG) == []

    def test_a_foreign_number_is_searched_as_typed(self, paired_dbs):
        _add_chat(paired_dbs, f"{FOREIGN_SHORT}@s.whatsapp.net")
        assert whatsapp.search_contacts(FOREIGN_LONG) == []
        _add_chat(paired_dbs, f"{FOREIGN_LONG}@s.whatsapp.net")
        assert _jids(whatsapp.search_contacts(FOREIGN_SHORT)) == [f"{FOREIGN_SHORT}@s.whatsapp.net"]
        assert _jids(whatsapp.search_contacts(FOREIGN_LONG)) == [f"{FOREIGN_LONG}@s.whatsapp.net"]

    def test_a_landline_does_not_answer_for_a_mobile(self, paired_dbs):
        _add_chat(paired_dbs, f"{LANDLINE}@s.whatsapp.net")
        assert whatsapp.search_contacts(LANDLINE_PLUS_NINE) == []
        _add_chat(paired_dbs, f"{LANDLINE_PLUS_NINE}@s.whatsapp.net")
        assert _jids(whatsapp.search_contacts(LANDLINE)) == [f"{LANDLINE}@s.whatsapp.net"]

    def test_name_queries_and_wildcards_are_untouched(self, paired_dbs):
        _add_chat(paired_dbs, SHORT_JID, "Paroquia 97777")
        (by_name,) = whatsapp.search_contacts("paroquia 9")
        assert by_name["matched"] == "name"
        # A wildcard still reaches only the JID pattern, and still says so with null.
        (by_wildcard,) = whatsapp.search_contacts("5588_7776666")
        assert (by_wildcard["jid"], by_wildcard["matched"]) == (SHORT_JID, None)


class TestDirectChat:
    @pytest.mark.parametrize("asked", [LONG, "+55 (88) 97777-6666", LONG_JID])
    def test_stored_without_the_ninth_digit_found_with_it(self, paired_dbs, asked):
        _add_chat(paired_dbs, SHORT_JID)
        assert whatsapp.get_direct_chat_by_contact(asked)["jid"] == SHORT_JID

    @pytest.mark.parametrize("asked", [SHORT, "55 88 7777-6666", SHORT_JID])
    def test_stored_with_the_ninth_digit_found_without_it(self, paired_dbs, asked):
        _add_chat(paired_dbs, LONG_JID)
        assert whatsapp.get_direct_chat_by_contact(asked)["jid"] == LONG_JID

    def test_the_spelling_asked_for_wins_when_both_have_a_chat(self, paired_dbs):
        _add_chat(paired_dbs, SHORT_JID)
        _add_chat(paired_dbs, LONG_JID)
        assert whatsapp.get_direct_chat_by_contact(LONG)["jid"] == LONG_JID
        assert whatsapp.get_direct_chat_by_contact(SHORT)["jid"] == SHORT_JID

    def test_a_foreign_number_is_looked_up_as_typed(self, paired_dbs):
        _add_chat(paired_dbs, f"{FOREIGN_SHORT}@s.whatsapp.net")
        assert whatsapp.get_direct_chat_by_contact(FOREIGN_LONG) is None
        assert whatsapp.get_direct_chat_by_contact(FOREIGN_SHORT)["jid"] == f"{FOREIGN_SHORT}@s.whatsapp.net"

    def test_a_landline_does_not_answer_for_a_mobile(self, paired_dbs):
        _add_chat(paired_dbs, f"{LANDLINE}@s.whatsapp.net")
        assert whatsapp.get_direct_chat_by_contact(LANDLINE_PLUS_NINE) is None

    def test_a_lid_has_no_other_spelling(self, paired_dbs):
        _add_chat(paired_dbs, SHORT_JID)
        assert whatsapp.get_direct_chat_by_contact(f"{LONG}@lid") is None

    def test_still_a_whole_number(self, paired_dbs):
        _add_chat(paired_dbs, f"1{SHORT}@s.whatsapp.net")
        assert whatsapp.get_direct_chat_by_contact(LONG) is None

    def test_under_an_allow_list_the_admitted_spelling_answers(self, paired_dbs, monkeypatch):
        _add_chat(paired_dbs, SHORT_JID)
        _add_chat(paired_dbs, LONG_JID)
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([SHORT_JID]))
        assert whatsapp.get_direct_chat_by_contact(LONG)["jid"] == SHORT_JID

    def test_the_allow_list_decides_on_the_stored_spelling(self, paired_dbs, monkeypatch):
        _add_chat(paired_dbs, SHORT_JID)
        # Allowed under the spelling the store holds: either one reaches it.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([SHORT_JID]))
        assert whatsapp.get_direct_chat_by_contact(LONG)["jid"] == SHORT_JID
        # Not allowed at all: the other spelling is no way around the list.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
        for asked in (LONG, SHORT):
            with pytest.raises(ToolError) as exc:
                whatsapp.get_direct_chat_by_contact(asked)
            assert exc.value.code == "denied"
        # The list names only the spelling the store does not hold (as a phone
        # and as a LID, every form the lookup tries): the chat stays hidden.
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([LONG, f"{LONG}@lid"]))
        assert whatsapp.get_direct_chat_by_contact(LONG) is None
