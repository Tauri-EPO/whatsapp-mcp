"""WHATSAPP_ALLOWED_CHATS: read tools filter, write tools refuse."""

import sqlite3

import pytest

import whatsapp
from chat_policy import ChatPolicy, load_chat_policy, normalize_chat_entry
from errors import ToolError

DM_A = "5511999999999@s.whatsapp.net"
DM_B = "5511888888888@s.whatsapp.net"
GROUP = "120363000000000001@g.us"

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
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
        assert whatsapp.list_messages(chat_jid=DM_B, include_context=False) == []

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
