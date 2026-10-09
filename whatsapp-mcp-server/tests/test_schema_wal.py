import os
import sqlite3

import whatsapp
from tests.conftest import ALICE, MESSAGES_SCHEMA


def test_location_schema_change_in_wal_becomes_visible(paired_dbs):
    writer = sqlite3.connect(paired_dbs.messages_db)
    try:
        writer.execute("PRAGMA journal_mode=WAL")
        writer.execute("PRAGMA wal_autocheckpoint=0")
        writer.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type) VALUES ('WAL', ?, '5511999999999', 'Location', '2026-10-08 10:00:00+00:00', 0, 'location')",
            (ALICE,),
        )
        writer.commit()
        writer.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        assert whatsapp.list_messages(chat_jid=ALICE, include_context=False)[0]["location"] is None
        before = whatsapp._db_signature(str(paired_dbs.messages_db))
        writer.execute("ALTER TABLE messages ADD COLUMN location TEXT")
        writer.execute(
            "UPDATE messages SET location=? WHERE id='WAL'", ('{"live":false,"latitude":0.25,"longitude":0.5}',)
        )
        writer.commit()
        assert whatsapp._db_signature(str(paired_dbs.messages_db)) == before
        assert os.path.getsize(str(paired_dbs.messages_db) + "-wal") > 0
        assert whatsapp.list_messages(chat_jid=ALICE, include_context=False)[0]["location"] == {
            "live": False,
            "latitude": 0.25,
            "longitude": 0.5,
        }
    finally:
        writer.close()


def test_all_schema_probes_observe_wal_migrations(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    writer = sqlite3.connect(path)
    reader = None
    try:
        writer.executescript(MESSAGES_SCHEMA.replace("sender_server TEXT, ", "").replace(", mentions TEXT", ""))
        writer.execute("PRAGMA journal_mode=WAL")
        writer.execute("PRAGMA wal_autocheckpoint=0")
        writer.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
        whatsapp._reset_schema_cache()
        reader = sqlite3.connect(f"file:{path.as_posix()}?mode=ro", uri=True)
        cursor = reader.cursor()
        before = whatsapp.message_columns(cursor)
        assert "messages.location" not in before and "messages.sender_server" not in before
        assert whatsapp._last_read_time_select(cursor, "chats") == "NULL"
        assert whatsapp._has_mentions_column(cursor) is False
        assert whatsapp._fts_available(reader) is False
        signature = whatsapp._db_signature(str(path))
        writer.executescript(
            "ALTER TABLE messages ADD COLUMN sender_server TEXT; ALTER TABLE messages ADD COLUMN location TEXT; ALTER TABLE messages ADD COLUMN mentions TEXT; ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP; CREATE VIRTUAL TABLE messages_fts USING fts5(content);"
        )
        writer.commit()
        assert whatsapp._db_signature(str(path)) == signature
        after = whatsapp.message_columns(cursor)
        assert "messages.location" in after and "messages.sender_server" in after
        assert whatsapp._last_read_time_select(cursor, "chats") == "chats.last_read_time"
        assert whatsapp._has_mentions_column(cursor) is True
        assert whatsapp._fts_available(reader) is True
    finally:
        if reader is not None:
            reader.close()
        writer.close()
