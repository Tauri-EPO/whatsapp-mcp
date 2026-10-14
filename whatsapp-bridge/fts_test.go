package main

import (
	"database/sql"
	"fmt"
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

// The linked pure-Go driver must provide FTS5. Its real index is created,
// back-filled, kept in sync by triggers and folds diacritics. Capability-off
// teardown is exercised separately through the explicit test seam below.
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
		t.Fatal("linked SQLite driver must provide FTS5")
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
	// Simulate leftovers with plain objects of the same names.
	if _, err := db.Exec(`
		CREATE TABLE messages_fts (content TEXT);
		CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(content) VALUES (new.content); END;
	`); err != nil {
		t.Fatal(err)
	}
	on, err := ensureMessagesFTSCapability(db, false)
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

func TestMessagesFTSMigrationFailurePreservesWrites(t *testing.T) {
	for _, failure := range []string{"injected migration failure", "no such module: fts5"} {
		t.Run(failure, func(t *testing.T) {
			store := newTestMessageStore(t)
			if _, err := ensureMessagesFTS(store.db); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec("DELETE FROM schema_migrations WHERE name=?", documentFilenameFTSMigration); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(ftsTeardown + `
				CREATE TABLE messages_fts(content TEXT);
				CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
					SELECT missing_fts_module();
				END;`); err != nil {
				t.Fatal(err)
			}
			if err := store.StoreMessage(storedMessage{ID: "before-failure", ChatJID: "c", Sender: "s", Content: "broken trigger", Timestamp: time.Now()}); err == nil {
				t.Fatal("broken leftover trigger must refuse a real StoreMessage before cleanup")
			}
			on, err := ensureMessagesFTSMigration(store.db, true, func(tx *sql.Tx) error {
				if _, err := tx.Exec(ftsTeardown + ftsSchema); err != nil {
					return err
				}
				return fmt.Errorf("%s", failure)
			})
			if on || (err == nil) != (failure == "no such module: fts5") {
				t.Fatalf("on=%v err=%v", on, err)
			}
			var triggers int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'messages_fts_%'").Scan(&triggers); err != nil || triggers != 0 {
				t.Fatalf("leftover triggers=%d err=%v", triggers, err)
			}
			if exists, _ := messagesFTSExists(store.db); exists {
				t.Fatal("failed optional index remains active")
			}
			if err := store.StoreMessage(storedMessage{ID: "after-failure", ChatJID: "c", Sender: "s", Content: "still writable", Timestamp: time.Now()}); err != nil {
				t.Fatalf("StoreMessage after failed migration: %v", err)
			}
			var content string
			if err := store.db.QueryRow("SELECT content FROM messages WHERE id='after-failure'").Scan(&content); err != nil || content != "still writable" {
				t.Fatalf("stored content=%q err=%v", content, err)
			}
		})
	}
}

func TestMessagesFTSExcludesGeneratedDocumentNames(t *testing.T) {
	db := newMessagesDB(t)
	if _, err := db.Exec(`INSERT INTO messages(id, chat_jid, content, media_type, filename)
		VALUES ('generated', 'c', '', 'document', 'document_20261014_190000_generated'),
		('special[*]', 'c', '', 'document', 'document_20261014_190000_special[*]'),
		('real', 'c', '', 'document', 'document_final.pdf'),
		('other', 'c', '', 'document', 'document_20261014_190000_different.pdf');`); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureMessagesFTS(db); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "document"); got != 2 {
		t.Fatalf("rebuild document matches=%d want only two original filenames", got)
	}
	if _, err := db.Exec(`UPDATE messages SET filename='document_review.pdf' WHERE id='generated';
		UPDATE messages SET filename='document_20261014_190000_real' WHERE id='real';`); err != nil {
		t.Fatal(err)
	}
	if countMatches(t, db, "final") != 0 || countMatches(t, db, "review") != 1 || countMatches(t, db, "document") != 2 {
		t.Fatal("filename update did not replace generated/original tokens")
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE id IN ('generated','real','special[*]')`); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "document"); got != 1 {
		t.Fatalf("delete left stale document tokens=%d", got)
	}
	if _, err := db.Exec(`UPDATE messages SET id='different.pdf' WHERE id='other'`); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "document"); got != 0 {
		t.Fatalf("changed row id left generated document tokens=%d", got)
	}
}

func TestDocumentFilenameFTSMigration(t *testing.T) {
	db := newMessagesDB(t)
	// An archive from before document filename indexing, with its old triggers.
	if _, err := db.Exec(`
		CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', content_rowid='rowid');
		CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
		END;
		INSERT INTO messages(id, chat_jid, content, media_type, filename)
			VALUES ('doc', 'c', 'caption', 'document', 'Relatório técnico.pdf'),
			('image', 'c', '', 'image', 'Relatório técnico.jpg'),
			('audio', 'c', '', 'audio', 'Relatório técnico.ogg'),
			('video', 'c', '', 'video', 'Relatório técnico.mp4');
	`); err != nil {
		t.Fatal(err)
	}
	if got := countMatches(t, db, "relatorio"); got != 0 {
		t.Fatalf("legacy index filename matches = %d", got)
	}
	for range 2 {
		if on, err := ensureMessagesFTS(db); err != nil || !on {
			t.Fatalf("migrate/restart: on=%v err=%v", on, err)
		}
		for _, query := range []string{"relatorio", "RELATÓRIO", `"relatorio tecnico"`, "relator*", "caption AND tecnico", "caption NOT boleto"} {
			if got := countMatches(t, db, query); got != 1 {
				t.Fatalf("MATCH %q = %d, want document only", query, got)
			}
		}
	}
	var markers int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE name=?", documentFilenameFTSMigration).Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
	// Prove restart does not rebuild: a failing view only affects rebuild reads.
	if _, err := db.Exec(`DROP VIEW messages_fts_content;
		CREATE VIEW messages_fts_content AS SELECT rowid, content, missing_column AS filename FROM messages;`); err != nil {
		t.Fatal(err)
	}
	if on, err := ensureMessagesFTS(db); err != nil || !on {
		t.Fatalf("restart must skip rebuild: on=%v err=%v", on, err)
	}
	if _, err := db.Exec(`DROP VIEW messages_fts_content;` + ftsSchema); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		sql, absent, present string
	}{
		{`UPDATE messages SET filename='Contrato.pdf' WHERE id='doc'`, "relatorio", "contrato"},
		{`UPDATE messages SET media_type='image' WHERE id='doc'`, "contrato", "caption"},
		{`UPDATE messages SET media_type='document' WHERE id='image'`, "contrato", "relatorio"},
		{`DELETE FROM messages WHERE id='image'`, "relatorio", "caption"},
	} {
		if _, err := db.Exec(step.sql); err != nil {
			t.Fatal(err)
		}
		if got := countMatches(t, db, step.absent); got != 0 {
			t.Fatalf("stale filename %q = %d", step.absent, got)
		}
		if got := countMatches(t, db, step.present); got != 1 {
			t.Fatalf("current token %q = %d", step.present, got)
		}
	}
	// Losing FTS support drops the view/table/triggers; reactivation still
	// rebuilds even though the migration marker is already present.
	if _, err := ensureMessagesFTSCapability(db, false); err != nil {
		t.Fatal(err)
	}
	if on, err := ensureMessagesFTS(db); err != nil || !on || countMatches(t, db, "caption") != 1 {
		t.Fatalf("re-enable: on=%v err=%v", on, err)
	}
}
