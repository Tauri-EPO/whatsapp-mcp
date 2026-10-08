package main

import (
	"database/sql"
	"strings"
	"testing"
)

func seedMigrationEffect(t *testing.T, ms *MessageStore) {
	t.Helper()
	seedLegacyRow(t, ms.db, "INSERT INTO chats(jid,name) VALUES (?, 'Alice')", mediaTestChat)
	seedLegacyRow(t, ms.db, "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,file_length) VALUES ('ATOMIC1', ?, 'x', '', '2026-09-04 10:00:00+00:00', 0, 'image', 0)", mediaTestChat)
}

func assertMigrationRolledBack(t *testing.T, ms *MessageStore, name string) {
	t.Helper()
	var length sql.NullInt64
	if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='ATOMIC1'").Scan(&length); err != nil || !length.Valid || length.Int64 != 0 {
		t.Fatalf("migration effect survived failure: length=%v err=%v", length, err)
	}
	if applied, err := migrationApplied(ms.db, name); err != nil || applied {
		t.Fatalf("migration marker survived failure: applied=%v err=%v", applied, err)
	}
}

func TestNamedMigrationRollsBackWhenMarkerFails(t *testing.T) {
	ms := newMigrationTestStore(t)
	seedMigrationEffect(t, ms)
	seedLegacyRow(t, ms.db, "DELETE FROM schema_migrations WHERE name=?", undeclaredMediaLengthsMigration)
	// The rewrite succeeds, then the marker fails before COMMIT. A marker
	// recorded outside the transaction would leave the rewrite behind.
	seedLegacyRow(t, ms.db, "CREATE TRIGGER reject_migration_marker BEFORE INSERT ON schema_migrations BEGIN SELECT RAISE(ABORT, 'marker refused'); END")
	if err := migrateUndeclaredMediaLengths(ms.db); err == nil {
		t.Fatal("marker failure was ignored")
	}
	assertMigrationRolledBack(t, ms, undeclaredMediaLengthsMigration)
	seedLegacyRow(t, ms.db, "DROP TRIGGER reject_migration_marker")
	if err := migrateUndeclaredMediaLengths(ms.db); err != nil {
		t.Fatal(err)
	}
	if applied, err := migrationApplied(ms.db, undeclaredMediaLengthsMigration); err != nil || !applied {
		t.Fatalf("successful retry missing marker: applied=%v err=%v", applied, err)
	}
}

func TestNamedMigrationRejectsDuplicateMarker(t *testing.T) {
	ms := newMigrationTestStore(t)
	seedMigrationEffect(t, ms)
	const name = "duplicate_marker_test"
	applied, err := applyNamedMigration(ms.db, name, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE messages SET file_length=NULL WHERE id='ATOMIC1'"); err != nil {
			return err
		}
		// A callback cannot take ownership of the helper's marker. The final
		// INSERT must reject this duplicate instead of silently ignoring it.
		return recordMigration(tx, name)
	})
	if err == nil || applied {
		t.Fatalf("duplicate marker accepted: applied=%v err=%v", applied, err)
	}
	assertMigrationRolledBack(t, ms, name)
}

func TestNamedMigrationSkipsAnAppliedBody(t *testing.T) {
	ms := newMigrationTestStore(t)
	calls := 0
	body := func(*sql.Tx) error { calls++; return nil }
	for attempt := 0; attempt < 2; attempt++ {
		applied, err := applyNamedMigration(ms.db, "applied_body_test", body)
		if err != nil || applied != (attempt == 0) {
			t.Fatalf("attempt=%d applied=%v err=%v", attempt, applied, err)
		}
	}
	if calls != 1 {
		t.Fatalf("body ran %d times", calls)
	}
}

func TestEmptyLengthMigrationLogsOnlyDebug(t *testing.T) {
	rec := installRecordingLogger(t)
	newMigrationTestStore(t)
	log := rec.String()
	if !strings.Contains(log, "[DEBUG] Media length migration: marked 0") || strings.Contains(log, "[INFO] Media length migration: marked 0") {
		t.Fatalf("empty migration log=%s", log)
	}
}
