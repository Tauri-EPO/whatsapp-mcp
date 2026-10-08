package main

import (
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMediaLengthPresence(t *testing.T) {
	for _, known := range []bool{false, true} {
		var length *uint64
		if known {
			length = proto.Uint64(0)
		}
		for _, msg := range []*waE2E.Message{
			{ImageMessage: &waE2E.ImageMessage{FileLength: length}},
			{VideoMessage: &waE2E.VideoMessage{FileLength: length}},
			{AudioMessage: &waE2E.AudioMessage{FileLength: length}},
			{DocumentMessage: &waE2E.DocumentMessage{FileLength: length}},
			{StickerMessage: &waE2E.StickerMessage{FileLength: length}},
		} {
			if mediaLengthDeclared(msg) != known {
				t.Fatalf("length presence=%v", known)
			}
		}
	}
	if mediaLengthDeclared(nil) || mediaLengthDeclared(&waE2E.Message{}) {
		t.Fatal("plain message declares no length")
	}
}

func TestMediaLengthMigrationIsAtomicAndDoesNotEraseNewZeroes(t *testing.T) {
	ms := newTestMessageStore(t)
	if err := ms.StoreChat(mediaTestChat, "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("OLD1", mediaTestChat, "x", "", time.Now(), false, "document", "empty.bin", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec(`PRAGMA user_version = 1; CREATE TRIGGER fail_length BEFORE UPDATE OF file_length ON messages BEGIN SELECT RAISE(ABORT, 'blocked migration'); END`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUndeclaredMediaLengths(ms.db); err == nil {
		t.Fatal("migration failure was ignored")
	}
	var version int
	var length sql.NullInt64
	if err := ms.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id = 'OLD1'").Scan(&length); err != nil || !length.Valid || length.Int64 != 0 {
		t.Fatalf("length=%v err=%v", length, err)
	}
	if _, err := ms.db.Exec("DROP TRIGGER fail_length"); err != nil {
		t.Fatal(err)
	}
	if err := migrateUndeclaredMediaLengths(ms.db); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id = 'OLD1'").Scan(&length); err != nil || length.Valid {
		t.Fatalf("legacy length=%v err=%v", length, err)
	}
	if err := ms.StoreMessage("EMPTY1", mediaTestChat, "x", "", time.Now(), false, "document", "empty.bin", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := migrateUndeclaredMediaLengths(ms.db); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id = 'EMPTY1'").Scan(&length); err != nil || !length.Valid || length.Int64 != 0 {
		t.Fatalf("new empty length=%v err=%v", length, err)
	}
}

func TestCompleteMediaReplayKeepsLengthOnlyForSamePlaintext(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history batch"}[batch], func(t *testing.T) {
			ms := newTestMessageStore(t)
			ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
			if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			persist := func(doc *waE2E.DocumentMessage) {
				t.Helper()
				ex := extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "LENGTH1")
				replayWriter(t, ms, batch, func(w messageWriter) error {
					return persistMessage(w, "LENGTH1", mediaTestChat, "x", ts, false, ex, false, testLogger())
				})
			}
			check := func(want sql.NullInt64) {
				t.Helper()
				var got sql.NullInt64
				if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='LENGTH1' AND chat_jid=?", mediaTestChat).Scan(&got); err != nil || got != want {
					t.Fatalf("stored length=%v want=%v err=%v", got, want, err)
				}
			}
			doc := fixtureDocument()
			doc.FileLength = proto.Uint64(5)
			persist(doc)
			check(sql.NullInt64{Int64: 5, Valid: true})
			doc.FileLength, doc.URL = nil, proto.String("https://example.invalid/refreshed")
			persist(doc)
			check(sql.NullInt64{Int64: 5, Valid: true})
			doc.FileSHA256 = []byte("different plaintext hash")
			persist(doc)
			check(sql.NullInt64{})
			doc.FileLength = proto.Uint64(0)
			persist(doc)
			check(sql.NullInt64{Valid: true})
			doc.FileLength = nil
			persist(doc)
			check(sql.NullInt64{Valid: true})
		})
	}
}

func BenchmarkUndeclaredMediaLengthMigration(b *testing.B) {
	b.StopTimer()
	b.Setenv(storeDirEnv, b.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ms.db.Close() })
	if err := ms.StoreChat(mediaTestChat, "Alice", time.Now()); err != nil {
		b.Fatal(err)
	}
	const count = 350000
	if _, err := ms.db.Exec(`WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n < ?)
		INSERT INTO messages(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length)
		SELECT 'M'||n, ?, 'x', '', '2026-09-04 10:00:00+00:00', 0, 'document', 'empty.bin', 'https://example.invalid/media', '/media', zeroblob(32), zeroblob(32), zeroblob(32), 0 FROM numbers`, count, mediaTestChat); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := ms.db.Exec("UPDATE messages SET file_length = 0; PRAGMA user_version = 1"); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := migrateUndeclaredMediaLengths(ms.db); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
	}
	var migrated int
	if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE file_length IS NULL").Scan(&migrated); err != nil || migrated != count {
		b.Fatalf("migrated=%d err=%v", migrated, err)
	}
	b.ReportMetric(count, "rows")
}

func TestMediaRetryUnknownLengthDoesNotBorrowFromDifferentPlaintext(t *testing.T) {
	ms := newTestMessageStore(t)
	ts := time.Now().Truncate(time.Second)
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	doc.FileLength = proto.Uint64(5)
	if err := persistMessage(ms, "RETRYLENGTH1", mediaTestChat, "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "RETRYLENGTH1"), false, testLogger()); err != nil {
		t.Fatal(err)
	}
	check := func(hash []byte, reported uint64, want sql.NullInt64) {
		t.Helper()
		if err := ms.StoreMediaInfo("RETRYLENGTH1", mediaTestChat, "https://example.invalid/refreshed", doc.GetMediaKey(), hash, doc.GetFileEncSHA256(), reported); err != nil {
			t.Fatal(err)
		}
		var got sql.NullInt64
		if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='RETRYLENGTH1' AND chat_jid=?", mediaTestChat).Scan(&got); err != nil || got != want {
			t.Fatalf("retry length=%v want=%v err=%v", got, want, err)
		}
	}
	check(doc.GetFileSHA256(), 0, sql.NullInt64{Int64: 5, Valid: true})
	otherHash := []byte("different plaintext hash")
	check(otherHash, 0, sql.NullInt64{})
	check(otherHash, 17, sql.NullInt64{Int64: 17, Valid: true})
	check(otherHash, 0, sql.NullInt64{Int64: 17, Valid: true})
}
