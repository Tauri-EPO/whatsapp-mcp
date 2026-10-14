package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Run with -run '^$' -bench BenchmarkDocumentFilenameFTSMigration -benchtime=1x.
// A real messages.db, one million synthetic rows and exactly 5% documents.
func BenchmarkDocumentFilenameFTSMigration(b *testing.B) {
	for range b.N {
		b.StopTimer()
		db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "messages.db"))
		if err != nil {
			b.Fatal(err)
		}
		boundPool(db, messagesPoolConns)
		if _, err := db.Exec(`PRAGMA journal_mode=WAL;
			CREATE TABLE messages(id TEXT, content TEXT, media_type TEXT, filename TEXT);
			WITH RECURSIVE seq(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM seq WHERE n<1000000)
			INSERT INTO messages SELECT printf('M%07d',n), 'synthetic archive text',
				CASE WHEN n%20=0 THEN 'document' ELSE '' END,
				CASE WHEN n%20=0 THEN printf('report_%07d.pdf',n) ELSE '' END FROM seq;
			CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', content_rowid='rowid');
			INSERT INTO messages_fts(messages_fts) VALUES('rebuild');`); err != nil {
			b.Fatal(err)
		}
		var rows, docs int
		if err := db.QueryRow("SELECT COUNT(*), SUM(media_type='document') FROM messages").Scan(&rows, &docs); err != nil || rows != 1000000 || docs != 50000 {
			b.Fatalf("rows=%d documents=%d err=%v", rows, docs, err)
		}
		b.StartTimer()
		started := time.Now()
		on, err := ensureMessagesFTS(db)
		elapsed := time.Since(started)
		b.StopTimer()
		if err != nil || !on {
			b.Fatalf("migration on=%v err=%v", on, err)
		}
		b.ReportMetric(elapsed.Seconds(), "migration_s")
		b.Logf("1000000 rows / 50000 documents: migration %.3fs", elapsed.Seconds())
		var hits int
		if err := db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'report'").Scan(&hits); err != nil || hits != 50000 {
			b.Fatalf("indexed documents=%d err=%v", hits, err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
