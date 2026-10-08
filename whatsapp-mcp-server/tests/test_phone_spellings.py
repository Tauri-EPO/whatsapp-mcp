"""Every tool that takes a number finds a Brazilian mobile by either spelling (issue #475).

`search_contacts` and `get_direct_chat_by_contact` got there first (#444,
tests/test_phone_lookup.py). The rest resolve a contact through
`_sender_aliases`, so that is where the second spelling lives, and the
allow-list decides once, in `_require_readable`. Fake numbers throughout.
"""

import pytest

import main
import notes
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import ALICE, FAMILY

# One mobile, both spellings: area code 88, without and with the ninth digit.
SHORT, LONG = "558877776666", "5588977776666"
SHORT_JID, LONG_JID = f"{SHORT}@s.whatsapp.net", f"{LONG}@s.whatsapp.net"
LID = "290000000000001"
# The same digits under another country code, and a landline next to a mobile.
FOREIGN_SHORT, FOREIGN_LONG = "548877776666", "5488977776666"
LANDLINE, LANDLINE_PLUS_NINE = "551133334444", "5511933334444"
ME = "5511000000000"


def _add_chat(store, jid, name="Acme Clinic"):
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", (jid, name, "2026-10-07 17:14:00")
        )


def _add_message(store, message_id, chat_jid, sender, stamp, from_me=False):
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me)"
            " VALUES (?, ?, ?, 's.whatsapp.net', ?, ?, ?)",
            (message_id, chat_jid, sender, f"text of {message_id}", f"2026-10-07 {stamp}+00:00", int(from_me)),
        )


@pytest.fixture
def clinic(paired_dbs):
    """The contact as WhatsApp registered it, without the ninth digit: a chat, a group post, a reply."""
    _add_chat(paired_dbs, SHORT_JID)
    _add_message(paired_dbs, "in1", SHORT_JID, SHORT, "10:00:00")
    _add_message(paired_dbs, "grp1", FAMILY, SHORT, "11:00:00")
    _add_message(paired_dbs, "out1", SHORT_JID, ME, "12:00:00", from_me=True)
    return paired_dbs


def _ids(rows):
    return sorted(row["id"] for row in rows)


def _jids(rows):
    return sorted(row["jid"] for row in rows)


BOTH = pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID])
BOTH_JIDS = pytest.mark.parametrize("asked", [SHORT_JID, LONG_JID])


class TestAliases:
    def test_a_brazilian_mobile_resolves_to_both_spellings(self, paired_dbs):
        assert whatsapp._sender_aliases(LONG) == [LONG, LONG_JID, f"{LONG}@lid", SHORT, SHORT_JID]
        assert set(whatsapp._sender_aliases(SHORT_JID)) >= {SHORT, SHORT_JID, LONG, LONG_JID}

    def test_the_lid_map_is_asked_for_both(self, paired_dbs):
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
        # The map knows the registered spelling; the one typed still comes along.
        assert whatsapp._sender_aliases(LONG) == [SHORT, SHORT_JID, LID, f"{LID}@lid", LONG, LONG_JID]
        assert whatsapp._sender_aliases(SHORT) == [SHORT, SHORT_JID, LID, f"{LID}@lid", LONG, LONG_JID]

    def test_a_lid_names_the_same_set_as_its_number(self, paired_dbs):
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
        # Whichever identifier names the contact, a filter reads the same rows.
        assert whatsapp._sender_aliases(f"{LID}@lid") == whatsapp._sender_aliases(SHORT)
        assert whatsapp._sender_aliases(LID) == whatsapp._sender_aliases(SHORT)

    def test_the_set_does_not_depend_on_the_spelling_typed(self, paired_dbs):
        # The map knows each spelling under a LID of its own (the two-row case).
        other_lid = "290000000000002"
        with paired_dbs.whatsmeow() as conn:
            conn.executemany("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", [(LID, SHORT), (other_lid, LONG)])
        everything = {SHORT, SHORT_JID, LONG, LONG_JID, LID, f"{LID}@lid", other_lid, f"{other_lid}@lid"}
        for asked in (SHORT, LONG, SHORT_JID, LONG_JID, f"{LID}@lid", f"{other_lid}@lid"):
            assert set(whatsapp._sender_aliases(asked)) == everything, asked

    def test_a_lid_itself_has_one_spelling(self, paired_dbs):
        # Digits shaped like a Brazilian mobile, named as a LID: not a phone number.
        assert whatsapp._sender_aliases(f"{LONG}@lid") == [LONG, LONG_JID, f"{LONG}@lid"]

    def test_the_answer_from_before_is_still_there_for_notes(self, paired_dbs):
        assert whatsapp._sender_aliases(LONG, both_spellings=False) == [LONG, LONG_JID, f"{LONG}@lid"]
        assert whatsapp._sender_aliases(LONG) != whatsapp._sender_aliases(LONG, both_spellings=False)

    @pytest.mark.parametrize("number", [FOREIGN_SHORT, FOREIGN_LONG, LANDLINE, LANDLINE_PLUS_NINE, "12025550100"])
    def test_every_other_number_resolves_as_before(self, paired_dbs, number):
        assert whatsapp._sender_aliases(number) == [number, f"{number}@s.whatsapp.net", f"{number}@lid"]

    def test_separators_are_not_dropped_here(self, paired_dbs):
        # The search tools tolerate a typed number (phone.phone_digits); what
        # decides a filter and the allow-list takes the digits as given.
        typed = "+55(88)97777-6666"
        assert whatsapp._sender_aliases(typed) == [typed, f"{typed}@s.whatsapp.net", f"{typed}@lid"]
        assert whatsapp.other_phone_spelling(typed) is None
        assert whatsapp.other_phone_spelling(f"{LONG}:7@s.whatsapp.net") == SHORT_JID


class TestEveryTool:
    @BOTH
    def test_get_contact_chats(self, clinic, asked):
        assert _jids(whatsapp.get_contact_chats(asked)) == sorted([SHORT_JID, FAMILY])

    @BOTH
    def test_get_last_interaction_reaches_what_this_account_sent(self, clinic, asked):
        assert whatsapp.get_last_interaction(asked)["id"] == "out1"

    @pytest.mark.parametrize("asked", [SHORT, LONG])
    def test_list_messages_by_sender(self, clinic, asked):
        rows = whatsapp.list_messages(sender_phone_number=asked, include_context=False)
        assert _ids(rows) == ["grp1", "in1"]

    @BOTH
    def test_list_messages_by_chat(self, clinic, asked):
        assert _ids(whatsapp.list_messages(chat_jid=asked, include_context=False)) == ["in1", "out1"]

    @BOTH
    def test_the_shared_chat_filter(self, clinic, asked):
        # message_stats, list_unread, list_unanswered, list_media and
        # export_messages all bind what this returns.
        assert set(whatsapp.chat_jid_filter(asked)) >= {SHORT_JID, LONG_JID}

    @pytest.mark.parametrize("query", [SHORT, LONG, "+55 (88) 97777-6666", "55 88 7777-6666", LONG_JID])
    def test_list_chats_by_number(self, clinic, query):
        assert _jids(whatsapp.list_chats(query=query)) == [SHORT_JID]
        assert whatsapp.count_chats(query=query) == 1

    @BOTH_JIDS
    def test_get_chat(self, clinic, asked):
        assert whatsapp.get_chat(asked)["jid"] == SHORT_JID

    @pytest.mark.parametrize("asked", [SHORT, LONG])
    def test_get_chat_still_takes_a_jid_not_a_bare_number(self, clinic, asked):
        assert whatsapp.get_chat(asked) is None

    def test_get_chat_merges_both_allowed_archive_spellings(self, clinic):
        _add_chat(clinic, LONG_JID, "Second row")
        assert whatsapp.get_chat(LONG_JID) == whatsapp.get_chat(SHORT_JID)
        assert whatsapp.get_chat(SHORT_JID)["jid"] == SHORT_JID
        assert whatsapp.get_chat(LONG_JID)["aliases"] == [SHORT_JID, LONG_JID]

    @BOTH
    def test_get_contact_reports_the_stored_spelling(self, clinic, asked):
        contact = main.get_contact(asked)
        assert (contact["jid"], contact["phone_number"], contact["name"]) == (SHORT_JID, SHORT, "Acme Clinic")
        assert contact["identifier"] == asked

    @pytest.mark.parametrize("asked", [SHORT, LONG])
    def test_get_contact_without_a_chat_of_its_own(self, paired_dbs, asked):
        with paired_dbs.whatsmeow() as conn:
            conn.execute(
                "INSERT INTO whatsmeow_contacts VALUES ('me', ?, NULL, 'Acme Clinic', NULL, NULL)", (SHORT_JID,)
            )
        contact = main.get_contact(asked)
        assert (contact["jid"], contact["name"], contact["resolved"]) == (SHORT_JID, "Acme Clinic", True)

    def test_a_merged_phone_and_lid_pair_is_listed_by_either_spelling(self, clinic):
        # The LID row holds the newest message, so it lists and the phone row hides.
        _add_chat(clinic, f"{LID}@lid", "Acme Clinic")
        _add_message(clinic, "lid1", f"{LID}@lid", LID, "14:00:00")
        with clinic.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (LID, SHORT))
        whatsapp._reset_name_cache()
        for query in (SHORT, LONG, LONG_JID, "+55 (88) 7777-6666", "55 88 97777-6666"):
            assert len(whatsapp.list_chats(query=query)) == 1, query
            assert whatsapp.count_chats(query=query) == 1, query

    def test_a_note_uses_one_archive_key_whichever_spelling_wrote_it(self, clinic):
        notes.annotate("contact", LONG_JID, "role", "typed spelling")
        notes.annotate("contact", SHORT_JID, "role", "stored spelling")
        assert notes.get_notes("contact", LONG_JID)["notes"]["role"]["value"] == "stored spelling"
        assert notes.get_notes("contact", SHORT_JID)["notes"]["role"]["value"] == "stored spelling"


class TestNothingElseWidens:
    def test_a_foreign_number_is_matched_as_typed(self, paired_dbs):
        jid = f"{FOREIGN_SHORT}@s.whatsapp.net"
        _add_chat(paired_dbs, jid)
        _add_message(paired_dbs, "f1", jid, FOREIGN_SHORT, "10:00:00")
        assert whatsapp.get_contact_chats(FOREIGN_LONG) == []
        assert whatsapp.get_last_interaction(FOREIGN_LONG) is None
        assert whatsapp.list_messages(sender_phone_number=FOREIGN_LONG, include_context=False) == []
        assert whatsapp.list_chats(query=FOREIGN_LONG) == []
        assert whatsapp.get_chat(f"{FOREIGN_LONG}@s.whatsapp.net") is None
        assert _ids(whatsapp.list_messages(sender_phone_number=FOREIGN_SHORT, include_context=False)) == ["f1"]

    def test_a_landline_does_not_answer_for_a_mobile(self, paired_dbs):
        jid = f"{LANDLINE}@s.whatsapp.net"
        _add_chat(paired_dbs, jid)
        _add_message(paired_dbs, "l1", jid, LANDLINE, "10:00:00")
        assert whatsapp.get_last_interaction(LANDLINE_PLUS_NINE) is None
        assert whatsapp.list_chats(query=LANDLINE_PLUS_NINE) == []
        assert whatsapp.get_chat(f"{LANDLINE_PLUS_NINE}@s.whatsapp.net") is None

    def test_a_group_jid_has_one_spelling(self, clinic):
        assert whatsapp.chat_jid_filter(FAMILY) == [FAMILY]
        # A direct chat whose user part is the group's id is somebody else.
        group_id = FAMILY.split("@")[0]
        _add_chat(clinic, f"{group_id}@s.whatsapp.net", "Not the group")
        _add_message(clinic, "later", f"{group_id}@s.whatsapp.net", ME, "15:00:00", from_me=True)
        assert whatsapp.get_last_interaction(FAMILY)["id"] == "grp1"


def _allow(monkeypatch, *entries):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(list(entries)))
    monkeypatch.setattr(notes, "CHAT_POLICY", whatsapp.CHAT_POLICY)


def _denied(call):
    with pytest.raises(ToolError) as exc:
        call()
    assert exc.value.code == "denied"


class TestAllowList:
    """A read is refused only when the list names neither spelling; rows stay those the list names."""

    @BOTH
    def test_a_number_blocked_in_both_spellings_is_denied_in_both(self, clinic, monkeypatch, asked):
        _allow(monkeypatch, ALICE)
        _denied(lambda: whatsapp.list_messages(chat_jid=asked, include_context=False))
        _denied(lambda: whatsapp.get_chat(asked))
        _denied(lambda: whatsapp.get_last_interaction(asked))
        _denied(lambda: notes.get_notes("contact", asked))
        # A tool answers with the envelope instead of raising.
        assert main.get_contact(asked)["error"]["code"] == "denied"
        # The sender-style lookups never said "denied": they answer with nothing.
        assert whatsapp.get_contact_chats(asked) == []
        assert whatsapp.list_messages(sender_phone_number=asked, include_context=False) == []
        assert whatsapp.list_chats(query=asked) == []

    @BOTH
    def test_a_wildcard_for_groups_still_denies_a_direct_chat(self, clinic, monkeypatch, asked):
        _allow(monkeypatch, "*@g.us")
        _denied(lambda: whatsapp.list_messages(chat_jid=asked, include_context=False))
        _denied(lambda: whatsapp.get_chat(asked))
        # Their post in an allowed group is that group's to show.
        assert _ids(whatsapp.list_messages(sender_phone_number=asked, include_context=False)) == ["grp1"]

    def test_a_group_is_unaffected(self, clinic, monkeypatch):
        _allow(monkeypatch, SHORT_JID)
        _denied(lambda: whatsapp.list_messages(chat_jid=FAMILY, include_context=False))
        _allow(monkeypatch, "*@g.us")
        assert _ids(whatsapp.list_messages(chat_jid=FAMILY, include_context=False)) == ["grp1"]

    @BOTH_JIDS
    def test_allowed_under_the_stored_spelling_answers_for_either(self, clinic, monkeypatch, asked):
        _allow(monkeypatch, SHORT_JID)
        assert _ids(whatsapp.list_messages(chat_jid=asked, include_context=False)) == ["in1", "out1"]
        assert whatsapp.get_chat(asked)["jid"] == SHORT_JID
        assert main.get_contact(asked)["jid"] == SHORT_JID
        assert whatsapp.get_last_interaction(asked)["id"] == "out1"
        # The group post stays out: the list does not name the group.
        assert _ids(whatsapp.list_messages(sender_phone_number=asked, include_context=False)) == ["in1"]
        assert _jids(whatsapp.get_contact_chats(asked)) == [SHORT_JID]

    def test_only_the_rows_the_list_names_come_back(self, clinic, monkeypatch):
        # A second row under the other spelling, which the list does not name.
        _add_chat(clinic, LONG_JID, "Second row")
        _add_message(clinic, "hidden1", LONG_JID, LONG, "13:00:00")
        _allow(monkeypatch, SHORT_JID)
        for asked in (SHORT_JID, LONG_JID):
            assert _ids(whatsapp.list_messages(chat_jid=asked, include_context=False)) == ["in1", "out1"]
            assert whatsapp.get_chat(asked)["jid"] == SHORT_JID
            assert whatsapp.get_last_interaction(asked)["id"] == "out1"
            assert _jids(whatsapp.list_chats(query=asked)) == [SHORT_JID]

    def test_a_list_naming_the_spelling_the_store_lacks_shows_nothing(self, clinic, monkeypatch):
        _allow(monkeypatch, LONG_JID)
        for asked in (SHORT_JID, LONG_JID):
            assert whatsapp.list_messages(chat_jid=asked, include_context=False) == []
            assert whatsapp.get_chat(asked) is None
            assert whatsapp.get_last_interaction(asked) is None

    def test_the_unlisted_spelling_is_not_named_through_get_contact(self, clinic, monkeypatch):
        with clinic.whatsmeow() as conn:
            conn.execute(
                "INSERT INTO whatsmeow_contacts VALUES ('me', ?, NULL, 'Acme Clinic', NULL, NULL)", (SHORT_JID,)
            )
        _allow(monkeypatch, LONG_JID)
        # Whichever spelling is asked, the answer is about the one the list
        # names, and that one has no chat and no phone-book entry here.
        for asked in (LONG_JID, SHORT_JID):
            contact = main.get_contact(asked)
            assert (contact["jid"], contact["resolved"]) == (LONG_JID, False)
            assert "Acme Clinic" not in str(contact)
        # The bare number is refused outright, through the `@lid` guess a bare
        # number is also tried under (issue #466): no name there either.
        assert "Acme Clinic" not in str(main.get_contact(LONG))

    @pytest.mark.parametrize("listed", [SHORT_JID, LONG_JID])
    def test_a_formatted_number_is_refused_whichever_spelling_is_listed(self, clinic, monkeypatch, listed):
        _allow(monkeypatch, listed)
        _denied(lambda: whatsapp.list_messages(chat_jid="+5588977776666", include_context=False))
        _denied(lambda: whatsapp.list_messages(chat_jid="+558877776666", include_context=False))

    def test_notes_require_typed_spelling_and_write_the_ninth_digit_key(self, clinic, monkeypatch):
        _allow(monkeypatch, SHORT_JID)
        _denied(lambda: notes.get_notes("contact", LONG_JID))
        _denied(lambda: notes.annotate("chat", LONG_JID, "role", "x"))
        assert notes.annotate("chat", SHORT_JID, "role", "x")["target_id"] == LONG_JID
        assert notes.get_notes("chat", SHORT_JID)["notes"]["role"]["value"] == "x"
        assert notes.get_notes("contact", SHORT_JID)["notes"] == {}

    def test_a_send_preview_names_the_recipient_as_given(self, clinic):
        # The bridge decides which number a send reaches: the preview must not
        # borrow the name of the chat stored under the other spelling.
        assert main.send_message(SHORT, "hello", dry_run=True)["recipient_name"] == "Acme Clinic"
        preview = main.send_message(LONG, "hello", dry_run=True)
        assert (preview["recipient_jid"], preview["recipient_name"]) == (LONG_JID, None)

    def test_writes_still_compare_the_recipient_as_given(self, clinic, monkeypatch):
        _allow(monkeypatch, SHORT_JID)
        _denied(lambda: whatsapp.send_message(LONG, "hello", dry_run=True))
        assert whatsapp.send_message(SHORT, "hello", dry_run=True)[0] is True
