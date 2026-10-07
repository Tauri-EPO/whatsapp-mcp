"""WHATSAPP_ALLOWED_CHATS: read tools filter, write tools refuse."""

import sqlite3

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy, load_chat_policy, normalize_chat_entry
from errors import ToolError

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
        assert clause == "(chats.jid IN (?) OR chats.jid LIKE ?)"
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


# One Brazilian mobile, both spellings (fake), and a number nobody has a chat with.
SHORT, LONG = "558877776666", "5588977776666"
SHORT_JID, LONG_JID = f"{SHORT}@s.whatsapp.net", f"{LONG}@s.whatsapp.net"
NO_CHAT = "5511777777777"
NO_CHAT_LID = "290000000000001"


def _allow(monkeypatch, *entries):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(list(entries)))


def _denial(call):
    with pytest.raises(ToolError) as exc:
        call()
    assert exc.value.code == "denied"
    return exc.value.message


class TestNoChatIsNotDenied:
    """ "Not allowed" and "allowed, no chat stored" are different answers (issue #466).

    get_direct_chat_by_contact tries a bare number as a phone and as a LID. It
    used to refuse unless the list named every one of those, so a number on
    the list answered `denied` until it had a chat.
    """

    @pytest.mark.parametrize("asked", [NO_CHAT, f"{NO_CHAT}@s.whatsapp.net", f"+{NO_CHAT}"])
    def test_an_allowed_number_without_a_chat_is_not_found(self, restricted_db, monkeypatch, asked):
        _allow(monkeypatch, NO_CHAT)
        assert whatsapp.get_direct_chat_by_contact(asked) is None
        assert main.get_direct_chat_by_contact(asked)["error"]["code"] == "not_found"

    @pytest.mark.parametrize("asked", [f"{NO_CHAT_LID}@lid", NO_CHAT_LID])
    def test_an_allowed_lid_without_a_chat_is_not_found(self, restricted_db, monkeypatch, asked):
        _allow(monkeypatch, f"{NO_CHAT_LID}@lid")
        assert whatsapp.get_direct_chat_by_contact(asked) is None

    @pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID])
    @pytest.mark.parametrize("listed", [SHORT, LONG])
    def test_either_spelling_of_an_allowed_mobile_is_not_found(self, restricted_db, monkeypatch, asked, listed):
        _allow(monkeypatch, listed)
        assert whatsapp.get_direct_chat_by_contact(asked) is None

    @pytest.mark.parametrize("asked", [SHORT, LONG, SHORT_JID, LONG_JID])
    def test_a_blocked_number_is_denied_with_or_without_a_chat(self, restricted_db, monkeypatch, asked):
        _allow(monkeypatch, DM_A)
        without_chat = _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))
        with sqlite3.connect(restricted_db) as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Hidden')", (SHORT_JID,))
        with_chat = _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))
        # The refusal says nothing about whether the number has a chat.
        assert with_chat == without_chat
        assert "Hidden" not in with_chat and asked in with_chat

    def test_the_rows_from_before_are_still_denied(self, restricted_db):
        _denial(lambda: whatsapp.get_direct_chat_by_contact("5511888888888"))  # blocked, has a chat
        _denial(lambda: whatsapp.get_direct_chat_by_contact("5511666666666"))  # blocked, no chat

    @pytest.mark.parametrize("asked", [SHORT, LONG, NO_CHAT])
    def test_a_wildcard_for_groups_still_denies_a_direct_chat(self, restricted_db, monkeypatch, asked):
        _allow(monkeypatch, "*@g.us")
        _denial(lambda: whatsapp.get_direct_chat_by_contact(asked))

    def test_a_jid_is_checked_in_the_namespace_it_names(self, restricted_db, monkeypatch):
        # The list names the phone number; the same digits as a LID are somebody else.
        _allow(monkeypatch, NO_CHAT)
        _denial(lambda: whatsapp.get_direct_chat_by_contact(f"{NO_CHAT}@lid"))
        _allow(monkeypatch, f"{NO_CHAT_LID}@lid")
        _denial(lambda: whatsapp.get_direct_chat_by_contact(f"{NO_CHAT_LID}@s.whatsapp.net"))

    def test_a_chat_the_list_does_not_name_stays_hidden_behind_not_found(self, restricted_db, monkeypatch):
        # The number is allowed as a phone; its only row is under a LID the list does not name.
        with sqlite3.connect(restricted_db) as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Hidden')", (f"{NO_CHAT}@lid",))
        _allow(monkeypatch, NO_CHAT)
        assert whatsapp.get_direct_chat_by_contact(NO_CHAT) is None


class TestGetContactWithoutAChat:
    """get_contact tries a bare number under two spellings as well (issue #466)."""

    def test_an_allowed_number_without_a_chat_is_answered(self, restricted_db, monkeypatch):
        _allow(monkeypatch, NO_CHAT)
        contact = main.get_contact(NO_CHAT)
        assert "error" not in contact
        assert (contact["jid"], contact["resolved"]) == (f"{NO_CHAT}@s.whatsapp.net", False)

    @pytest.mark.parametrize("asked", ["5511888888888", "5511666666666", SHORT, LONG])
    def test_a_blocked_number_is_denied(self, restricted_db, asked):
        assert main.get_contact(asked)["error"]["code"] == "denied"

    def test_an_allowed_lid_is_answered_as_a_lid(self, restricted_db, monkeypatch):
        _allow(monkeypatch, f"{NO_CHAT_LID}@lid")
        contact = main.get_contact(NO_CHAT_LID)
        assert "error" not in contact
        assert contact["jid"] == f"{NO_CHAT_LID}@lid"

    def test_the_lid_guess_is_not_taken_when_the_list_does_not_name_it(self, restricted_db, monkeypatch):
        # Fourteen digits nobody has seen read as a LID (#375), but the list
        # names them as a phone number: the answer stays about that spelling.
        digits = "12345678901234"
        _allow(monkeypatch, digits)
        contact = main.get_contact(digits)
        assert "error" not in contact
        assert contact["jid"] == f"{digits}@s.whatsapp.net"

    @pytest.mark.parametrize("asked", [NO_CHAT, f"{NO_CHAT}@s.whatsapp.net"])
    def test_the_name_of_an_unlisted_row_does_not_come_back(self, restricted_db, monkeypatch, asked):
        # The same digits under the other server: a row the list does not name.
        with sqlite3.connect(restricted_db) as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Hidden row')", (f"{NO_CHAT}@lid",))
        _allow(monkeypatch, NO_CHAT)
        contact = main.get_contact(asked)
        assert "Hidden row" not in str(contact)
        assert (contact["jid"], contact["resolved"]) == (f"{NO_CHAT}@s.whatsapp.net", False)

    def test_without_an_allow_list_the_name_lookup_is_what_it_was(self, restricted_db, monkeypatch):
        with sqlite3.connect(restricted_db) as conn:
            conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'Same digits')", (f"{NO_CHAT}@lid",))
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
        assert main.get_contact(f"{NO_CHAT}@s.whatsapp.net")["name"] == "Same digits"
