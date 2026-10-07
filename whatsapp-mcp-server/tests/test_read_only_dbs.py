"""messages.db and whatsapp.db are opened read-only (issue #458).

The bridge owns both files. Opening them read-write let a wrong path create an
empty database (every tool then answered "no such table" or an empty list) and
let a stray write reach the bridge's archive or whatsmeow's session store.
"""

import sqlite3
import sys

import pytest

import main
import media_notes
import whatsapp
from errors import ToolError
from tests.conftest import ALICE, make_paired_store


def test_a_missing_messages_db_is_a_named_error_and_creates_nothing(tmp_path, monkeypatch):
    missing = tmp_path / "wrong-dir" / "messages.db"
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(missing))
    with pytest.raises(ToolError) as exc:
        whatsapp._connect_messages_db()
    assert exc.value.code == "internal"
    assert str(missing) in exc.value.message
    assert "WHATSAPP_DB_PATH" in exc.value.message and "WHATSAPP_STORE_DIR" in exc.value.message
    assert not missing.exists() and not missing.parent.exists()
    assert list(tmp_path.iterdir()) == []


def test_a_tool_over_a_missing_messages_db_answers_with_the_envelope(tmp_path, monkeypatch):
    missing = tmp_path / "messages.db"
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(missing))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))
    out = main.list_chats()
    assert out["error"]["code"] == "internal" and str(missing) in out["error"]["message"]
    assert not missing.exists()


def test_search_contacts_does_not_turn_a_wrong_path_into_an_empty_list(paired_dbs, tmp_path, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "wrong" / "messages.db"))
    out = main.search_contacts("Bob")
    assert out["error"]["code"] == "internal" and "messages.db not found" in out["error"]["message"]


def test_helpers_that_tolerate_an_unreadable_archive_still_do(tmp_path, monkeypatch):
    """The missing-file error is also a sqlite3.Error, so `except sqlite3.Error` keeps working."""
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    with pytest.raises(sqlite3.Error):
        whatsapp._connect_messages_db()
    assert whatsapp.stored_sender_namespace("5511900000000") is None


def test_a_write_through_either_connection_is_refused(paired_dbs):
    for connect, statement in (
        (whatsapp._connect_messages_db, "INSERT INTO chats (jid, name) VALUES ('x@s.whatsapp.net', 'x')"),
        (whatsapp._connect_messages_db, "CREATE TABLE intruder (a)"),
        (whatsapp._connect_whatsmeow_db, "DELETE FROM whatsmeow_lid_map"),
    ):
        conn = connect()
        try:
            with pytest.raises(sqlite3.OperationalError, match="readonly"):
                conn.execute(statement)
        finally:
            conn.close()
    with paired_dbs.messages() as conn:
        assert conn.execute("SELECT COUNT(*) FROM chats WHERE jid = 'x@s.whatsapp.net'").fetchone()[0] == 0


def test_reads_work_and_temp_tables_are_still_allowed(paired_dbs):
    conn = whatsapp._connect_messages_db()
    try:
        assert conn.execute("SELECT name FROM chats WHERE jid = ?", (ALICE,)).fetchone() == ("Alice",)
        # triage and the transcript search keep scratch tables in the temp schema.
        conn.execute("CREATE TEMP TABLE scratch (a)")
        conn.execute("INSERT INTO scratch VALUES (1)")
        assert conn.execute("SELECT a FROM scratch").fetchone() == (1,)
    finally:
        conn.close()


def test_a_missing_whatsapp_db_is_not_created_and_the_phone_book_lookups_tolerate_it(tmp_path, monkeypatch):
    absent = tmp_path / "whatsapp.db"
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(absent))
    with pytest.raises(sqlite3.OperationalError):
        whatsapp._connect_whatsmeow_db()
    assert not absent.exists()
    assert whatsapp.unknown_lid_digits("231241139937355") is False
    assert whatsapp.owner_identity_local() is None
    assert not absent.exists()


def test_bridge_status_never_fails_without_the_databases(tmp_path, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "messages.db"))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))

    def down(*args, **kwargs):
        raise ToolError("bridge_unavailable", "bridge down")

    monkeypatch.setattr(whatsapp, "_bridge_request", down)
    status = whatsapp.bridge_status()
    assert status["ok"] is False and "reason" in status
    assert list(tmp_path.iterdir()) == []


class TestWal:
    """The bridge keeps messages.db in WAL mode: a read-only reader must see its rows."""

    @staticmethod
    def _wal_db(path):
        writer = sqlite3.connect(path, isolation_level=None)
        writer.execute("PRAGMA journal_mode=WAL")
        writer.execute("PRAGMA wal_autocheckpoint=0")  # keep new rows in the -wal file
        writer.execute("CREATE TABLE messages (id TEXT, content TEXT)")
        writer.execute("INSERT INTO messages VALUES ('1', 'checkpointed')")
        writer.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        writer.execute("INSERT INTO messages VALUES ('2', 'only in the wal')")
        return writer

    def test_a_wal_database_the_bridge_has_open_is_read_through_its_log(self, tmp_path, monkeypatch):
        path = tmp_path / "messages.db"
        writer = self._wal_db(path)
        try:
            assert (tmp_path / "messages.db-wal").stat().st_size > 0
            monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
            conn = whatsapp._connect_messages_db()
            try:
                assert [r[0] for r in conn.execute("SELECT id FROM messages ORDER BY id")] == ["1", "2"]
                writer.execute("INSERT INTO messages VALUES ('3', 'written while the reader is open')")
                assert conn.execute("SELECT COUNT(*) FROM messages").fetchone()[0] == 3
            finally:
                conn.close()
        finally:
            writer.close()

    def test_a_wal_database_whose_bridge_has_stopped_is_still_readable(self, tmp_path, monkeypatch):
        """A bridge killed mid-write leaves -wal (and maybe -shm) behind; a reader recovers from it."""
        path = tmp_path / "messages.db"
        writer = self._wal_db(path)
        copy_dir = tmp_path / "after-a-crash"
        copy_dir.mkdir()
        try:
            # What a crash leaves: the main file and the log, with no live writer.
            for suffix in ("", "-wal"):
                (copy_dir / f"messages.db{suffix}").write_bytes((tmp_path / f"messages.db{suffix}").read_bytes())
        finally:
            writer.close()
        assert not (copy_dir / "messages.db-shm").exists()
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(copy_dir / "messages.db"))
        conn = whatsapp._connect_messages_db()
        try:
            assert [r[0] for r in conn.execute("SELECT id FROM messages ORDER BY id")] == ["1", "2"]
        finally:
            conn.close()

    def test_a_cleanly_closed_wal_database_is_readable(self, tmp_path, monkeypatch):
        path = tmp_path / "messages.db"
        self._wal_db(path).close()
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
        conn = whatsapp._connect_messages_db()
        try:
            assert conn.execute("SELECT COUNT(*) FROM messages").fetchone()[0] == 2
        finally:
            conn.close()


class TestPathsSurviveTheUri:
    @pytest.mark.parametrize(
        "directory",
        ["with space", "hash #1", "percent %41", "amp & plus +", "accents çã"],
    )
    def test_awkward_directory_names(self, tmp_path, monkeypatch, directory):
        store = make_paired_store(_mkdir(tmp_path / directory))
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store.messages_db))
        monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(store.whatsmeow_db))
        for connect, query in (
            (whatsapp._connect_messages_db, "SELECT COUNT(*) FROM chats"),
            (whatsapp._connect_whatsmeow_db, "SELECT COUNT(*) FROM whatsmeow_lid_map"),
        ):
            conn = connect()
            try:
                assert conn.execute(query).fetchone()[0] >= 1
            finally:
                conn.close()

    @pytest.mark.skipif(sys.platform == "win32", reason="? is not allowed in Windows file names")
    def test_a_question_mark_in_the_path(self, tmp_path, monkeypatch):
        store = make_paired_store(_mkdir(tmp_path / "what?"))
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store.messages_db))
        conn = whatsapp._connect_messages_db()
        try:
            assert conn.execute("SELECT COUNT(*) FROM chats").fetchone()[0] >= 1
        finally:
            conn.close()

    def test_a_relative_path_resolves_against_the_working_directory(self, tmp_path, monkeypatch):
        store = make_paired_store(_mkdir(tmp_path / "rel dir"))
        monkeypatch.chdir(tmp_path)
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store.messages_db.relative_to(tmp_path)))
        conn = whatsapp._connect_messages_db()
        try:
            assert conn.execute("SELECT COUNT(*) FROM chats").fetchone()[0] >= 1
        finally:
            conn.close()

    @pytest.mark.skipif(sys.platform != "win32", reason="backslash separators are a Windows spelling")
    def test_backslashes_and_a_drive_letter(self, tmp_path, monkeypatch):
        store = make_paired_store(_mkdir(tmp_path / "back slash"))
        spelled = str(store.messages_db).replace("/", "\\")
        assert "\\" in spelled and ":" in spelled
        monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", spelled)
        conn = whatsapp._connect_messages_db()
        try:
            assert conn.execute("SELECT COUNT(*) FROM chats").fetchone()[0] >= 1
        finally:
            conn.close()


def _mkdir(path):
    path.mkdir()
    return path


def _write_note(sha, key, value):
    """Through the module that owns notes.db: its own read-write connection."""
    conn = media_notes._connect(create=True)
    assert conn is not None
    try:
        conn.execute(
            "INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, ?, ?, '2026-09-04')"
            " ON CONFLICT(sha256, key) DO UPDATE SET value = excluded.value",
            (sha, key, value),
        )
        conn.commit()
    finally:
        conn.close()


class TestNotesDbStaysReadWrite:
    """notes.db is the MCP server's own; only the read-only join through ATTACH is affected."""

    def test_attach_on_the_read_connection_reads_notes(self, paired_dbs):
        _write_note("a" * 64, "kind", "invoice")
        conn = whatsapp._connect_messages_db()
        try:
            whatsapp.attach_notes_read_only(conn, media_notes.notes_db_path())
            assert conn.execute("SELECT value FROM notesdb.media_notes WHERE sha256 = ?", ("a" * 64,)).fetchone() == (
                "invoice",
            )
            # The join is attached with its own mode=ro: it cannot write the notes by accident.
            with pytest.raises(sqlite3.OperationalError, match="readonly"):
                conn.execute("DELETE FROM notesdb.media_notes")
            # ...while its owner keeps writing, and the join sees it.
            _write_note("a" * 64, "kind", "invoice v2")
            assert conn.execute("SELECT value FROM notesdb.media_notes WHERE sha256 = ?", ("a" * 64,)).fetchone() == (
                "invoice v2",
            )
        finally:
            conn.close()

    def test_the_inventory_and_coverage_joins_still_work(self, paired_dbs):
        _write_note("c" * 64, "keep", "yes")
        import media_inventory

        conn = whatsapp._connect_messages_db()
        try:
            clause = media_inventory._notes_exists_clause(conn, True)
            assert clause is not None
            assert conn.execute(f"SELECT COUNT(*) FROM messages m WHERE {clause}").fetchone() == (0,)
        finally:
            conn.close()
