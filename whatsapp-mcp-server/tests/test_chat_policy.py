"""WHATSAPP_ALLOWED_CHATS: read tools filter, write tools refuse."""

import sqlite3

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy, load_chat_policy, normalize_chat_entry
from errors import ToolError
from tests.conftest import ALICE, BOB, BOB_LID, BOB_PN

DM_A = "5511999999999@s.whatsapp.net"
DM_B = "5511888888888@s.whatsapp.net"
GROUP = "120363000000000001@g.us"

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT, quoted_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""


class TestPolicyParsing:
    def test_unset_is_unrestricted(self):
        policy = load_chat_policy({})
        assert not policy.restricted
        assert policy.allows("anything@g.us")
        assert policy.sql_clause("c.jid") == ("1=1", [])

    @pytest.mark.parametrize(
        ("raw", "expected"),
        [
            ("5511999999999", DM_A),
            (" 5511999999999@s.whatsapp.net ", DM_A),
            ("5511999999999:12@s.whatsapp.net", DM_A),
            ("120363000000000001@G.US", GROUP),
            ("", ""),
        ],
    )
    def test_normalize(self, raw, expected):
        assert normalize_chat_entry(raw) == expected

    def test_exact_and_wildcard_entries(self):
        policy = load_chat_policy({"WHATSAPP_ALLOWED_CHATS": "5511999999999, *@g.us ,, "})
        assert policy.restricted
        assert policy.allows(DM_A)
        assert policy.allows("5511999999999")  # bare number form used by send tools
        assert policy.allows(GROUP)
        assert not policy.allows(DM_B)
        assert not policy.allows("")
        assert not policy.allows(None)

    def test_sql_clause_matches_allows(self):
        policy = ChatPolicy.from_entries([DM_A, "*@g.us"])
        clause, params = policy.sql_clause("chats.jid")
        assert (
            clause
            == "((length(chats.jid) - length(replace(chats.jid, '@', '')) <= 1) AND (chats.jid IN (?) OR chats.jid LIKE ?))"
        )
        assert params == [DM_A, "%@g.us"]
        conn = sqlite3.connect(":memory:")
        conn.execute("CREATE TABLE chats (jid TEXT)")
        conn.executemany("INSERT INTO chats VALUES (?)", [(DM_A,), (DM_B,), (GROUP,)])
        rows = conn.execute(f"SELECT jid FROM chats WHERE {clause} ORDER BY jid", params).fetchall()
        assert [r[0] for r in rows] == sorted([DM_A, GROUP])


@pytest.fixture
def restricted_db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
        [
            (DM_A, "Allowed", "2024-01-03T10:00:00"),
            (DM_B, "Blocked", "2024-01-02T10:00:00"),
            (GROUP, "Grp", "2024-01-01T10:00:00"),
        ],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
        [
            ("a1", DM_A, "5511999999999", "hello from allowed", "2024-01-03T10:00:00"),
            ("b1", DM_B, "5511888888888", "hello from blocked", "2024-01-02T10:00:00"),
            ("g1", GROUP, "5511888888888", "group hello", "2024-01-01T10:00:00"),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([DM_A, "*@g.us"]))
    return path


class TestReadsAreFiltered:
    def test_list_chats(self, restricted_db):
        jids = {c["jid"] for c in whatsapp.list_chats(limit=10)}
        assert jids == {DM_A, GROUP}
        assert {c["jid"] for c in whatsapp.list_chats(query="Blocked")} == set()

    def test_list_messages(self, restricted_db):
        ids = {m["id"] for m in whatsapp.list_messages(query="hello", include_context=False)}
        assert ids == {"a1", "g1"}
        # Asking for a blocked chat by name is refused rather than answered with
        # an empty page, which would read as "nothing happened there" (#289).
        with pytest.raises(ToolError) as exc:
            whatsapp.list_messages(chat_jid=DM_B, include_context=False)
        assert exc.value.code == "denied"
        assert DM_B in exc.value.message
        with pytest.raises(ToolError) as exc:
            whatsapp.list_messages(chat_jid=[DM_A, DM_B], include_context=False)
        assert exc.value.code == "denied"
        # Excluding a blocked chat is a no-op, not an error: it can only narrow.
        ids = {m["id"] for m in whatsapp.list_messages(exclude_chat_jid=DM_B, include_context=False)}
        assert ids == {"a1", "g1"}

    def test_get_chat_and_direct_chat(self, restricted_db):
        assert whatsapp.get_chat(DM_A) is not None
        assert whatsapp.get_direct_chat_by_contact("5511999999999") is not None
        # A blocked chat is "denied", distinguishable from "not found".
        with pytest.raises(ToolError) as exc:
            whatsapp.get_chat(DM_B)
        assert exc.value.code == "denied"
        with pytest.raises(ToolError) as exc:
            whatsapp.get_direct_chat_by_contact("5511888888888")
        assert exc.value.code == "denied"
        with pytest.raises(ToolError) as exc:
            whatsapp.get_direct_chat_by_contact("5511777777777")
        assert exc.value.code == "denied"  # unknown number, still outside the allow-list

    def test_contact_chats_and_last_interaction(self, restricted_db):
        # The blocked contact also posted in the allowed group: only that shows.
        chats = whatsapp.get_contact_chats("5511888888888@s.whatsapp.net")
        assert {c["jid"] for c in chats} == {GROUP}
        last = whatsapp.get_last_interaction("5511888888888@s.whatsapp.net")
        assert last is not None and last["chat_jid"] == GROUP

    def test_message_context_refuses_blocked_chat(self, restricted_db):
        with pytest.raises(ToolError, match="not in WHATSAPP_ALLOWED_CHATS") as exc:
            whatsapp.get_message_context("b1", chat_jid=DM_B)
        assert exc.value.code == "denied"
        assert whatsapp.get_message_context("a1", chat_jid=DM_A).message.id == "a1"


class TestWritesAreRefused:
    @pytest.fixture(autouse=True)
    def _policy(self, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([DM_A]))
        # Any bridge call would be a test failure: the refusal must happen before it.
        monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **k: pytest.fail("bridge was called"))

    def test_send_message(self):
        with pytest.raises(ToolError, match="WHATSAPP_ALLOWED_CHATS") as exc:
            whatsapp.send_message(DM_B, "hi")
        assert exc.value.code == "denied"
        with pytest.raises(ToolError):
            whatsapp.send_message("5511888888888", "hi")

    def test_send_file_and_audio(self, tmp_path):
        media = tmp_path / "f.pdf"
        media.write_bytes(b"%PDF")
        with pytest.raises(ToolError) as exc:
            whatsapp.send_file(DM_B, str(media))
        assert exc.value.code == "denied"
        with pytest.raises(ToolError) as exc:
            whatsapp.send_audio_message(DM_B, str(media))
        assert exc.value.code == "denied"

    def test_reaction_mark_read_download(self):
        with pytest.raises(ToolError) as exc:
            whatsapp.send_reaction(DM_B, "m1", "👍")
        assert exc.value.code == "denied"
        with pytest.raises(ToolError) as exc:
            whatsapp.mark_messages_read(["m1"], DM_B)
        assert exc.value.code == "denied"
        with pytest.raises(ToolError) as exc:
            whatsapp.download_media("m1", DM_B)
        assert exc.value.code == "denied"


# One Brazilian mobile, both spellings (fake), a number and a LID nobody has a chat with.
SHORT, LONG = "558877776666", "5588977776666"
SHORT_JID, LONG_JID = f"{SHORT}@s.whatsapp.net", f"{LONG}@s.whatsapp.net"
NO_CHAT = "5511777777777"
NO_CHAT_JID = f"{NO_CHAT}@s.whatsapp.net"
SOME_LID = "290000000000001"
SOME_LID_JID = f"{SOME_LID}@lid"


def _allow(monkeypatch, *entries):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(list(entries)))


def _denial(call):
    with pytest.raises(ToolError) as exc:
        call()
    assert exc.value.code == "denied"
    return exc.value.message


def _add_chat(store, jid, name="Hidden row"):
    with store.messages() as conn:
        conn.execute("INSERT INTO chats (jid, name) VALUES (?, ?)", (jid, name))
    whatsapp._reset_name_cache()


class TestNoChatIsNotDenied:
    """ "Not allowed" and "allowed, no chat stored" are different answers (issue #466).

    get_direct_chat_by_contact tries a bare number as a phone and as a LID. It
    used to refuse unless the list named every one of those, so a number on
    the list answered `denied` until it had a chat. The store is the paired
    one: Alice and Bob have chats, the LID map pairs Bob with his LID.
    """

    @pytest.mark.parametrize("asked", [NO_CHAT, NO_CHAT_JID, f"+{NO_CHAT}"])
    def test_an_allowed_number_without_a_chat_is_not_found(self, paired_dbs, monkeypatch, asked):
        _allow(monkeypatch, NO_CHAT)
        assert whatsapp.get_direct_chat_by_contact(asked) is None
        assert main.get_direct_chat_by_contact(asked)["error"]["code"] == "not_found"

    @pytest.mark.parametrize("asked", [SOME_LID_JID, SOME_LID])
    def test_an_allowed_lid_without_a_chat_is_not_found(self, paired_dbs, monkeypatch, asked):
        _allow(monkeypatch, SOME_LID_JID)
        assert whatsapp.get_direct_chat_by_contact(asked) is None

    @pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID])
    @pytest.mark.parametrize("listed", [SHORT, LONG])
    def test_either_spelling_of_an_allowed_mobile_is_not_found(self, paired_dbs, monkeypatch, asked, listed):
        _allow(monkeypatch, listed)
        assert whatsapp.get_direct_chat_by_contact(asked) is None

    def test_a_contact_listed_by_its_lid_is_not_found_by_its_number(self, paired_dbs, monkeypatch):
        # The list names the LID, as it must for a chat stored under one; the
        # map pairs that LID with the number asked. No chat yet, then one.
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (SOME_LID, SHORT))
        _allow(monkeypatch, SOME_LID_JID)
        for asked in (SHORT, LONG):
            assert whatsapp.get_direct_chat_by_contact(asked) is None
        _add_chat(paired_dbs, SOME_LID_JID, "Acme Clinic")
        assert whatsapp.get_direct_chat_by_contact(SHORT)["jid"] == SOME_LID_JID

    @pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID])
    def test_a_blocked_number_is_denied_with_or_without_a_chat(self, paired_dbs, monkeypatch, asked):
        _allow(monkeypatch, ALICE)
        without_chat = _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))
        _add_chat(paired_dbs, SHORT_JID)
        with_chat = _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))
        # The refusal says nothing about whether the number has a chat.
        assert with_chat == without_chat
        assert "Hidden row" not in with_chat and asked in with_chat

    def test_a_blocked_number_with_a_lid_in_the_map_is_denied_too(self, paired_dbs, monkeypatch):
        _allow(monkeypatch, ALICE)
        _denial(lambda: whatsapp.get_direct_chat_by_contact(BOB_PN))  # has a chat and a LID
        _denial(lambda: whatsapp.get_direct_chat_by_contact("5511666666666"))  # has neither

    @pytest.mark.parametrize("asked", [SHORT, LONG, NO_CHAT, BOB_PN])
    def test_a_wildcard_for_groups_still_denies_a_direct_chat(self, paired_dbs, monkeypatch, asked):
        _allow(monkeypatch, "*@g.us")
        _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))

    def test_a_wildcard_for_lids_does_not_admit_a_phone_number(self, paired_dbs, monkeypatch):
        # `*@lid` names every LID chat. A phone number is not one of them
        # because its digits could be written before `@lid`.
        _allow(monkeypatch, "*@lid")
        without_chat = _denial(lambda: whatsapp.get_direct_chat_by_contact(NO_CHAT))
        _add_chat(paired_dbs, NO_CHAT_JID)
        assert _denial(lambda: whatsapp.get_direct_chat_by_contact(NO_CHAT)) == without_chat
        # A LID is admitted when it is named as one, or when the map knows it as one.
        assert whatsapp.get_direct_chat_by_contact(SOME_LID_JID) is None
        assert whatsapp.get_direct_chat_by_contact(BOB_LID) is None
        # Bare digits nothing knows are a phone number until shown otherwise.
        _denial(lambda: whatsapp.get_direct_chat_by_contact(SOME_LID))

    def test_a_wildcard_for_lids_says_nothing_about_the_lid_map(self, paired_dbs, monkeypatch):
        # Bob's number is in the LID map and the other one is not: under `*@lid`
        # neither is named, and the refusal is the same for both.
        with paired_dbs.messages() as conn:
            conn.execute("DELETE FROM chats WHERE jid = ?", (BOB,))
        _allow(monkeypatch, "*@lid")
        mapped = _denial(lambda: whatsapp.get_direct_chat_by_contact(BOB_PN))
        unmapped = _denial(lambda: whatsapp.get_direct_chat_by_contact(NO_CHAT))
        assert mapped.replace(BOB_PN, "<number>") == unmapped.replace(NO_CHAT, "<number>")

    def test_a_short_lid_the_map_knows_is_read_as_a_lid(self, paired_dbs, monkeypatch):
        short_lid = "1234567890123"
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (short_lid, NO_CHAT))
        whatsapp._reset_name_cache()
        _allow(monkeypatch, f"{short_lid}@lid")
        assert whatsapp.get_direct_chat_by_contact(short_lid) is None

    def test_a_wildcard_answer_does_not_depend_on_a_hidden_row(self, paired_dbs, monkeypatch):
        # Fifteen digits nothing has seen read as a LID (#375), and
        # `*@s.whatsapp.net` does not name a LID. A row under that LID changes
        # nothing in the answer.
        _allow(monkeypatch, "*@s.whatsapp.net")
        without_row = _denial(lambda: whatsapp.get_direct_chat_by_contact(SOME_LID))
        _add_chat(paired_dbs, SOME_LID_JID)
        assert _denial(lambda: whatsapp.get_direct_chat_by_contact(SOME_LID)) == without_row
        # Named as a phone JID, it is the phone number the wildcard admits.
        assert whatsapp.get_direct_chat_by_contact(f"{SOME_LID}@s.whatsapp.net") is None

    def test_a_jid_is_checked_in_the_namespace_it_names(self, paired_dbs, monkeypatch):
        # The list names the phone number; the same digits as a LID are somebody else.
        _allow(monkeypatch, NO_CHAT)
        _denial(lambda: whatsapp.get_direct_chat_by_contact(f"{NO_CHAT}@lid"))
        _allow(monkeypatch, SOME_LID_JID)
        _denial(lambda: whatsapp.get_direct_chat_by_contact(f"{SOME_LID}@s.whatsapp.net"))

    def test_a_chat_the_list_does_not_name_stays_hidden_behind_not_found(self, paired_dbs, monkeypatch):
        # Bob is allowed as a phone number; the list does not name his LID.
        with paired_dbs.messages() as conn:
            conn.execute("DELETE FROM chats WHERE jid = ?", (BOB,))
        _allow(monkeypatch, BOB_PN)
        assert whatsapp.get_direct_chat_by_contact(BOB_PN) is None
        _add_chat(paired_dbs, f"{BOB_LID}@lid")
        assert whatsapp.get_direct_chat_by_contact(BOB_PN) is None


class TestGetContactWithoutAChat:
    """get_contact tries a bare number under two spellings as well (issue #466).

    It classifies the identifier first, as it does without an allow-list, and
    the list then has to name the JID that came out.
    """

    def test_an_allowed_number_without_a_chat_is_answered(self, paired_dbs, monkeypatch):
        _allow(monkeypatch, NO_CHAT)
        contact = main.get_contact(NO_CHAT)
        assert "error" not in contact
        assert (contact["jid"], contact["resolved"], contact["is_lid"]) == (NO_CHAT_JID, False, False)

    @pytest.mark.parametrize("asked", [BOB_PN, "5511666666666", SHORT, LONG])
    def test_a_blocked_number_is_denied(self, paired_dbs, monkeypatch, asked):
        _allow(monkeypatch, ALICE)
        assert main.get_contact(asked)["error"]["code"] == "denied"

    def test_an_allowed_lid_nothing_has_seen_is_answered_as_a_lid(self, paired_dbs, monkeypatch):
        _allow(monkeypatch, SOME_LID_JID)
        contact = main.get_contact(SOME_LID)
        assert "error" not in contact
        assert (contact["jid"], contact["is_lid"], contact["phone_number"]) == (SOME_LID_JID, True, None)

    def test_a_phone_wildcard_does_not_turn_a_lid_into_a_phone_number(self, paired_dbs, monkeypatch):
        # Nothing has seen these fifteen digits: they read as a LID (#375), and
        # `*@s.whatsapp.net` does not name a LID.
        _allow(monkeypatch, "*@s.whatsapp.net")
        assert main.get_contact(SOME_LID)["error"]["code"] == "denied"
        _add_chat(paired_dbs, SOME_LID_JID)
        assert main.get_contact(SOME_LID)["error"]["code"] == "denied"

    def test_a_lid_wildcard_does_not_turn_a_phone_number_into_a_lid(self, paired_dbs, monkeypatch):
        _allow(monkeypatch, "*@lid")
        assert main.get_contact(BOB_PN)["error"]["code"] == "denied"  # a phone with a chat
        assert main.get_contact(NO_CHAT)["error"]["code"] == "denied"  # a phone without one

    def test_a_long_number_listed_as_a_phone_number_is_answered_as_one(self, paired_dbs, monkeypatch):
        # Fourteen unknown digits read as a LID (#375), which the list does not
        # name. It names them exactly as a phone number, and that is an answer:
        # the refusal used to name `<digits>@lid`, a JID nobody wrote.
        digits = "12345678901234"
        _allow(monkeypatch, digits)
        contact = main.get_contact(digits)
        assert "error" not in contact
        assert (contact["jid"], contact["is_lid"]) == (f"{digits}@s.whatsapp.net", False)
        assert whatsapp.get_direct_chat_by_contact(digits) is None

    def test_a_long_phone_number_with_a_hidden_chat_is_not_made_a_lid(self, paired_dbs, monkeypatch):
        # Under `*@lid` the phone spelling cannot be looked up, so "nothing has
        # seen this number" is not known and the #375 rule does not apply.
        digits = "12345678901234"
        _add_chat(paired_dbs, f"{digits}@s.whatsapp.net")
        _allow(monkeypatch, "*@lid")
        assert main.get_contact(digits)["error"]["code"] == "denied"
        _denial(lambda: whatsapp.get_direct_chat_by_contact(digits))

    def test_a_contact_listed_by_its_lid_is_answered_by_its_number(self, paired_dbs, monkeypatch):
        # No chat yet. get_direct_chat_by_contact says not_found for this
        # (test above); get_contact answers about the listed LID.
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (SOME_LID, SHORT))
        whatsapp._reset_name_cache()
        _allow(monkeypatch, SOME_LID_JID)
        for asked in (SHORT, LONG):
            contact = main.get_contact(asked)
            assert "error" not in contact, asked
            assert (contact["jid"], contact["phone_number"], contact["lid"]) == (SOME_LID_JID, SHORT, SOME_LID)

    @pytest.mark.parametrize("asked", [NO_CHAT, NO_CHAT_JID])
    def test_the_name_of_an_unlisted_row_does_not_come_back(self, paired_dbs, monkeypatch, asked):
        # The same digits under the other server: a row the list does not name.
        _add_chat(paired_dbs, f"{NO_CHAT}@lid")
        _allow(monkeypatch, NO_CHAT)
        contact = main.get_contact(asked)
        assert "Hidden row" not in str(contact)
        assert (contact["jid"], contact["resolved"]) == (NO_CHAT_JID, False)

    def test_under_a_list_a_jid_names_its_own_row_only(self, paired_dbs, monkeypatch):
        _add_chat(paired_dbs, f"{NO_CHAT}@lid", "Same digits")
        # Without a list the lookup is what it was: the digits under both servers.
        assert whatsapp.get_sender_name(NO_CHAT_JID) == "Same digits"
        _allow(monkeypatch, NO_CHAT)
        whatsapp._reset_name_cache()
        assert whatsapp.get_sender_name(NO_CHAT_JID) == NO_CHAT_JID
        # A bare sender, which is how a message row stores one, does not say
        # which namespace it is in: both are still tried.
        assert whatsapp.get_sender_name(NO_CHAT) == "Same digits"

    def test_a_chat_under_the_contacts_lid_is_found_by_the_number(self, paired_dbs, monkeypatch):
        # As get_direct_chat_by_contact does (#465): the map pairs the LID with the number.
        with paired_dbs.whatsmeow() as conn:
            conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (SOME_LID, SHORT))
        _add_chat(paired_dbs, SOME_LID_JID, "Acme Clinic")
        for asked in (SHORT, LONG, SHORT_JID):
            contact = main.get_contact(asked)
            assert (contact["jid"], contact["phone_number"], contact["lid"]) == (SOME_LID_JID, SHORT, SOME_LID)
            assert (contact["name"], contact["resolved"]) == ("Acme Clinic", True)
        # The list has to name that LID; the number alone does not reach it.
        _allow(monkeypatch, SOME_LID_JID)
        assert main.get_contact(SOME_LID_JID)["name"] == "Acme Clinic"
        _allow(monkeypatch, SHORT)
        contact = main.get_contact(SHORT)
        assert "Acme Clinic" not in str(contact) and contact["jid"] == SHORT_JID
