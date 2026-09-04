"""Shared fixtures: a paired messages.db + whatsapp.db the way the bridge writes them.

Test modules that need both databases use ``paired_dbs`` (or ``paired_store``
for the raw paths) instead of re-declaring the schema. Modules that probe a
specific schema variant (missing columns, no FTS) keep their own DDL on purpose.
"""

from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from pathlib import Path

import pytest

import whatsapp

# messages.db as written by a bridge from before the read-state migration
# (chats without last_read_time). Tests that probe the migration path add the
# column themselves; ``make_paired_store`` applies it like the bridge does.
MESSAGES_SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, quoted_message_id TEXT, deleted_at TIMESTAMP, view_once INTEGER DEFAULT 0,
    target_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""

# The two whatsmeow tables the MCP server reads from whatsapp.db.
WHATSMEOW_SCHEMA = """
CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
CREATE TABLE whatsmeow_contacts (
    our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT
);
"""

# One phone with a LID mapping and a contact-store name (Bob), one chat that
# only exists in messages.db (Alice), one group, one decoy whose JID contains
# Alice's digits, and one contact only whatsmeow knows (Carla, business).
ALICE = "5511999999999@s.whatsapp.net"
BOB_PN, BOB_LID = "5511888888888", "231241139937355"
BOB = f"{BOB_PN}@s.whatsapp.net"
FAMILY = "120363000000000001@g.us"
DECOY = "15511999999999@s.whatsapp.net"
CARLA = "5521777777777@s.whatsapp.net"


@dataclass
class PairedStore:
    messages_db: Path
    whatsmeow_db: Path

    def messages(self) -> sqlite3.Connection:
        return sqlite3.connect(self.messages_db)

    def whatsmeow(self) -> sqlite3.Connection:
        return sqlite3.connect(self.whatsmeow_db)


def make_paired_store(tmp_path: Path) -> PairedStore:
    mdb = tmp_path / "messages.db"
    wdb = tmp_path / "whatsapp.db"
    with sqlite3.connect(mdb) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
            [
                (ALICE, "Alice", "2026-09-04 10:00:00"),
                (BOB, "Bob", "2026-09-04 09:00:00"),
                (FAMILY, "Family", "2026-09-04 10:00:00"),
                (DECOY, "Not Alice", "2026-09-04 10:00:00"),
            ],
        )
    with sqlite3.connect(wdb) as c:
        c.executescript(WHATSMEOW_SCHEMA)
        c.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (BOB_LID, BOB_PN))
        c.executemany(
            "INSERT INTO whatsmeow_contacts VALUES ('me', ?, ?, ?, ?, ?)",
            [
                (BOB, "Bob", "Bob Silva", "bobby", None),
                (CARLA, None, None, None, "Carla Consultoria"),
            ],
        )
    return PairedStore(mdb, wdb)


@pytest.fixture
def paired_store(tmp_path) -> PairedStore:
    """Fresh paired store on disk; nothing is pointed at it yet."""
    return make_paired_store(tmp_path)


@pytest.fixture
def paired_dbs(paired_store, monkeypatch) -> PairedStore:
    """The paired store wired into the whatsapp module, caches reset around the test."""
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(paired_store.messages_db))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(paired_store.whatsmeow_db))
    whatsapp._reset_name_cache()
    whatsapp._reset_schema_cache()
    yield paired_store
    whatsapp._reset_name_cache()
    whatsapp._reset_schema_cache()
