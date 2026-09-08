"""The namespace the bridge records for each sender (messages.sender_server, #375).

The length heuristic of #281 cannot tell a 15-digit LID from a 15-digit phone
number. The bridge knows which one it stored, so the column decides and the
heuristic is left for the rows written before it existed.
"""

import sqlite3
from datetime import datetime

import pytest

import export
import whatsapp
from tests.conftest import BOB_LID, BOB_PN, FAMILY

# The sender from the report: 15 digits, in status@broadcast, mapped nowhere.
STATUS_LID = "191134718546018"

# A messages.db written by a bridge from before the column existed.
LEGACY_SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT,
    quoted_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
"""


def _message(sender: str, server: str | None = None) -> whatsapp.Message:
    return whatsapp.Message(
        timestamp=datetime(2026, 9, 8, 10, 0, 0),
        sender=sender,
        sender_server=server,
        content="hi",
        is_from_me=False,
        chat_jid=FAMILY,
        id="M1",
    )


class TestTheRecordedNamespaceDecides:
    def test_a_15_digit_lid_is_no_longer_a_phone_number(self, paired_dbs):
        row = whatsapp.msg_to_dict(_message(STATUS_LID, "lid"))
        assert row["sender_phone"] is None  # it was reported as the number itself
        assert row["sender_lid"] == STATUS_LID
        assert row["sender_name"] is None  # nobody is called "191134718546018"
        assert row["sender_display"] == f"{STATUS_LID}@lid"

    def test_a_recorded_lid_is_still_resolved_through_the_map(self, paired_dbs):
        row = whatsapp.msg_to_dict(_message(BOB_LID, "lid"))
        assert row["sender_phone"] == BOB_PN
        assert row["sender_lid"] == BOB_LID
        assert row["sender_name"] == "Bob Silva"

    def test_a_recorded_phone_is_not_second_guessed_by_length(self, paired_dbs):
        """16 digits is a LID on sight — unless the bridge stored it as a number."""
        row = whatsapp.msg_to_dict(_message("1171581346817350", "s.whatsapp.net"))
        assert row["sender_phone"] == "1171581346817350"
        assert row["sender_lid"] is None

    def test_a_legacy_row_still_uses_the_heuristic(self, paired_dbs):
        phone = whatsapp.msg_to_dict(_message(BOB_PN))
        assert (phone["sender_phone"], phone["sender_lid"]) == (BOB_PN, None)

        mapped_lid = whatsapp.msg_to_dict(_message(BOB_LID))
        assert (mapped_lid["sender_phone"], mapped_lid["sender_lid"]) == (BOB_PN, BOB_LID)

        # The case the column exists for: no evidence anywhere, so a legacy row
        # keeps reading as a phone number until the bridge backfill reaches it.
        legacy = whatsapp.msg_to_dict(_message(STATUS_LID))
        assert (legacy["sender_phone"], legacy["sender_lid"]) == (STATUS_LID, None)

    def test_a_page_classifies_every_row_from_its_own_column(self, paired_dbs):
        messages = [_message(STATUS_LID, "lid"), _message(BOB_PN, "s.whatsapp.net"), _message(BOB_LID)]
        identities = whatsapp.fetch_sender_identities(messages)
        assert identities[STATUS_LID] == (None, STATUS_LID)
        assert identities[BOB_PN] == (BOB_PN, None)
        assert identities[BOB_LID] == (BOB_PN, BOB_LID)  # legacy row, resolved by the map

    def test_the_same_digits_cannot_serve_a_recorded_and_a_guessed_answer(self, paired_dbs):
        """The 5-minute identity cache is keyed by namespace as well as value."""
        assert whatsapp.sender_identity(STATUS_LID) == (STATUS_LID, None)  # guessed: a number
        assert whatsapp.sender_identity(STATUS_LID, "lid") == (None, STATUS_LID)  # recorded


class TestAStoreWithoutTheColumn:
    """A messages.db an older bridge wrote must stay readable (it selects NULL)."""

    @pytest.fixture
    def legacy_db(self, tmp_path, monkeypatch):
        path = tmp_path / "store" / "messages.db"
        path.parent.mkdir()
        monkeypatch.delenv("WHATSAPP_EXPORT_DIR", raising=False)
        conn = sqlite3.connect(path)
        conn.executescript(LEGACY_SCHEMA)
        conn.execute("INSERT INTO chats (jid, name) VALUES (?, ?)", (FAMILY, "Family"))
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
            ("M1", FAMILY, STATUS_LID, "hi", "2026-09-08 10:00:00+00:00"),
        )
        conn.commit()
        conn.close()
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
        whatsapp._reset_schema_cache()
        whatsapp._reset_name_cache()
        yield path
        whatsapp._reset_schema_cache()
        whatsapp._reset_name_cache()

    def test_reads_keep_working(self, legacy_db):
        rows = whatsapp.list_messages(limit=10, include_context=False)
        assert [row["id"] for row in rows] == ["M1"]
        # Nothing recorded the namespace, so the heuristic answers as before.
        assert rows[0]["sender_phone"] == STATUS_LID

    def test_exports_keep_working(self, legacy_db):
        """Every reader degrades, not just the paged ones (export.py has its own query)."""
        result = export.export_messages(out_path="legacy.ndjson")
        assert result["count"] == 1

    def test_the_namespace_lookup_finds_nothing(self, legacy_db):
        assert whatsapp.stored_sender_namespace(STATUS_LID) is None


class TestStoredSenderNamespace:
    def test_reads_the_namespace_off_the_archive(self, paired_dbs):
        with paired_dbs.messages() as conn:
            conn.executemany(
                "INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me) "
                "VALUES (?, ?, ?, ?, 'hi', '2026-09-08 10:00:00+00:00', 0)",
                [
                    ("M1", FAMILY, STATUS_LID, "lid"),
                    ("M2", FAMILY, BOB_PN, "s.whatsapp.net"),
                    ("M3", FAMILY, "5511666666666", None),  # legacy row
                ],
            )
        whatsapp._reset_name_cache()

        assert whatsapp.stored_sender_namespace(STATUS_LID) == "lid"
        assert whatsapp.stored_sender_namespace(BOB_PN) == "s.whatsapp.net"
        assert whatsapp.stored_sender_namespace("5511666666666") is None
        assert whatsapp.stored_sender_namespace("") is None
        assert whatsapp.stored_sender_namespace(f"{STATUS_LID}@lid") is None  # bare users only

    def test_a_missing_archive_is_not_created(self, tmp_path, monkeypatch):
        missing = tmp_path / "messages.db"
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(missing))
        whatsapp._reset_name_cache()
        assert whatsapp.stored_sender_namespace(STATUS_LID) is None
        assert not missing.exists()


class TestUnknownLidDigits:
    def test_only_the_ambiguous_lengths_qualify(self, paired_dbs):
        assert whatsapp.unknown_lid_digits(STATUS_LID) is True  # 15 digits, unknown
        assert whatsapp.unknown_lid_digits("3504706738598") is False  # 13: a plausible number
        assert whatsapp.unknown_lid_digits("1171581346817350") is False  # 16: already a LID
        assert whatsapp.unknown_lid_digits("35047067385985x") is False  # not digits

    def test_a_number_the_archive_or_the_phone_book_knows_is_not_a_lid(self, paired_dbs):
        with paired_dbs.messages() as conn:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me) "
                "VALUES ('M1', ?, ?, 's.whatsapp.net', 'hi', '2026-09-08 10:00:00+00:00', 0)",
                (FAMILY, STATUS_LID),
            )
        with paired_dbs.whatsmeow() as conn:
            conn.execute(
                "INSERT INTO whatsmeow_contacts VALUES ('me', ?, 'Long', 'Long Number', NULL, NULL)",
                ("356789012345678@s.whatsapp.net",),
            )
        whatsapp._reset_name_cache()

        assert whatsapp.unknown_lid_digits(STATUS_LID) is False  # stored as a phone number
        assert whatsapp.unknown_lid_digits("356789012345678") is False  # in the phone book

    def test_a_phone_book_nobody_can_read_is_not_evidence(self, paired_dbs, monkeypatch):
        """No entry and could not look are different answers (#281)."""
        with paired_dbs.whatsmeow() as conn:
            conn.execute("DROP TABLE whatsmeow_contacts")
        whatsapp._reset_name_cache()
        assert whatsapp.unknown_lid_digits(STATUS_LID) is False  # unreadable, so not a LID

        monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", "/nonexistent/whatsapp.db")
        whatsapp._reset_name_cache()
        assert whatsapp.unknown_lid_digits(STATUS_LID) is False
