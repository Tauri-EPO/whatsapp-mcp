package main

import (
	"database/sql"
	"testing"
	"time"
)

func newMessagesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", testMemoryDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
			file_length INTEGER, deleted_at TIMESTAMP, quoted_message_id TEXT,
			PRIMARY KEY (id, chat_jid)
		);
	`); err != nil {
		t.Fatal(err)
	}
	return db
}

func countMatches(t *testing.T, db *sql.DB, match string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, match).Scan(&n); err != nil {
		t.Fatalf("MATCH %q: %v", match, err)
	}
	return n
}

// With FTS5 compiled in (sqlite_fts5 build tag, as in the Dockerfile and CI)
// the index is created, back-filled, kept in sync by triggers and folds
// diacritics. Without it, ensureMessagesFTS must report false and leave the
// schema clean. Both outcomes are asserted from the same test.
func TestEnsureMessagesFTS(t *testing.T) {
	db := newMessagesDB(t)
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m1', 'c', 's', 'Segue o orçamento da obra', ?, 0)`, time.Now()); err != nil {
		t.Fatal(err)
	}

	on, err := ensureMessagesFTS(db)
	if err != nil {
		t.Fatalf("ensureMessagesFTS: %v", err)
	}
	exists, err := messagesFTSExists(db)
	if err != nil {
		t.Fatal(err)
	}

	if !sqliteHasFTS5(db) {
		if on || exists {
			t.Fatalf("build without FTS5 must not enable the index (on=%v exists=%v)", on, exists)
		}
		t.Log("SQLite built without FTS5; verified the disabled path only")
		return
	}

	if !on || !exists {
		t.Fatalf("expected index active on an FTS5 build (on=%v exists=%v)", on, exists)
	}
	// Back-filled from the pre-existing row, with accent folding both ways.
	for _, q := range []string{"orcamento", "orçamento", "ORÇAMENTO", "obra"} {
		if got := countMatches(t, db, q); got != 1 {
			t.Fatalf("MATCH %q after rebuild = %d, want 1", q, got)
		}
	}
	// Whole words: "obra" must not match "sobrado".
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m2', 'c', 's', 'o sobrado ficou pronto', ?, 0)`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "obra"); got != 1 {
		t.Fatalf("whole-word match returned %d rows, want 1", got)
	}
	if got := countMatches(t, db, "sobrado"); got != 1 {
		t.Fatalf("insert trigger did not index new row (got %d)", got)
	}
	// Update + delete triggers keep the index honest.
	if _, err := db.Exec(`UPDATE messages SET content = 'texto trocado' WHERE id = 'm2'`); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "sobrado"); got != 0 {
		t.Fatalf("update trigger left stale token (got %d)", got)
	}
	if got := countMatches(t, db, "trocado"); got != 1 {
		t.Fatalf("update trigger did not index new content (got %d)", got)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "orcamento"); got != 0 {
		t.Fatalf("delete trigger left stale row (got %d)", got)
	}
	// Idempotent on restart: no error, still active, no rebuild needed.
	if on, err := ensureMessagesFTS(db); err != nil || !on {
		t.Fatalf("second ensure: on=%v err=%v", on, err)
	}
}

// A build without FTS5 must remove an index a previous (FTS5) build created,
// otherwise the triggers would make every INSERT into messages fail.
func TestEnsureMessagesFTS_TeardownWithoutModule(t *testing.T) {
	db := newMessagesDB(t)
	if sqliteHasFTS5(db) {
		t.Skip("needs a SQLite build without FTS5 to exercise the teardown path")
	}
	// Simulate leftovers with plain objects of the same names.
	if _, err := db.Exec(`
		CREATE TABLE messages_fts (content TEXT);
		CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(content) VALUES (new.content); END;
	`); err != nil {
		t.Fatal(err)
	}
	on, err := ensureMessagesFTS(db)
	if err != nil || on {
		t.Fatalf("ensure without FTS5: on=%v err=%v", on, err)
	}
	if exists, _ := messagesFTSExists(db); exists {
		t.Fatal("leftover messages_fts table was not dropped")
	}
	var triggers int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'messages_fts_%'`).Scan(&triggers)
	if triggers != 0 {
		t.Fatalf("%d leftover trigger(s) not dropped", triggers)
	}
	// And inserts keep working afterwards.
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('x', 'c', 's', 'ok', ?, 0)`, time.Now()); err != nil {
		t.Fatalf("insert after teardown failed: %v", err)
	}
}
