package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMediaPresentationReplayAndAtomicity(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history batch"}[batch], func(t *testing.T) {
			ms := newTestMessageStore(t)
			ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
			if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			original := &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), FileName: proto.String("report.pdf"), Title: proto.String("Report"), URL: proto.String(fixtureMediaURL), DirectPath: proto.String(fixtureDirectPath), MediaKey: []byte("old key"), FileSHA256: []byte("old sha"), FileEncSHA256: []byte("old enc"), FileLength: proto.Uint64(3)}
			write := func(doc *waE2E.DocumentMessage) error {
				ex := extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "PRESENT1")
				if batch {
					return ms.Batch(func(w *messageBatch) error {
						return persistMessage(w, "PRESENT1", mediaTestChat, "x", ts, false, ex, false, testLogger())
					})
				}
				return persistMessage(ms, "PRESENT1", mediaTestChat, "x", ts, false, ex, false, testLogger())
			}
			if err := write(original); err != nil {
				t.Fatal(err)
			}
			assertOriginal := func() {
				t.Helper()
				var name, url, raw string
				var key, sha []byte
				if err := ms.db.QueryRow("SELECT filename,url,media_key,file_sha256,media_presentation FROM messages WHERE id='PRESENT1'").Scan(&name, &url, &key, &sha, &raw); err != nil {
					t.Fatal(err)
				}
				var p mediaPresentation
				if err := json.Unmarshal([]byte(raw), &p); err != nil {
					t.Fatal(err)
				}
				if name != "report.pdf" || url != fixtureMediaURL || !bytes.Equal(key, original.MediaKey) || !bytes.Equal(sha, original.FileSHA256) || p.MIME != "application/pdf" || p.Name == nil || *p.Name != "report.pdf" || p.Title == nil || *p.Title != "Report" {
					t.Fatalf("snapshot changed: name=%s url=%s key=%q sha=%q presentation=%s", name, url, key, sha, raw)
				}
			}
			// A partial replay cannot replace credentials or presentation.
			if err := write(&waE2E.DocumentMessage{Mimetype: proto.String("image/png")}); err != nil {
				t.Fatal(err)
			}
			assertOriginal()
			// A complete same-file replay may omit presentation. In particular,
			// its generated filename must not erase the original filename.
			replay := proto.Clone(original).(*waE2E.DocumentMessage)
			replay.Mimetype, replay.FileName, replay.Title = nil, nil, nil
			if err := write(replay); err != nil {
				t.Fatal(err)
			}
			assertOriginal()
			// Fields supplied by a later replay are merged for the same hash.
			replay.Title = proto.String("Updated report")
			if err := write(replay); err != nil {
				t.Fatal(err)
			}
			source, found, err := ms.messageContentLookup("PRESENT1", mediaTestChat)
			if err != nil || !found || source.presentation.MIME != "application/pdf" || *source.presentation.Title != "Updated report" {
				t.Fatalf("merged presentation=%+v err=%v", source, err)
			}
			if _, err := ms.db.Exec(`CREATE TRIGGER refuse_presentation BEFORE UPDATE ON messages WHEN NEW.media_presentation != OLD.media_presentation BEGIN SELECT RAISE(ABORT, 'presentation rejected'); END`); err != nil {
				t.Fatal(err)
			}
			replacement := proto.Clone(original).(*waE2E.DocumentMessage)
			replacement.FileSHA256 = []byte("new sha")
			replacement.MediaKey = []byte("new key")
			replacement.Mimetype = proto.String("application/octet-stream")
			if err := write(replacement); err == nil || !strings.Contains(err.Error(), "presentation rejected") {
				t.Fatalf("expected atomic rejection: %v", err)
			}
			var key []byte
			var raw string
			if err := ms.db.QueryRow("SELECT media_key,media_presentation FROM messages WHERE id='PRESENT1'").Scan(&key, &raw); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(key, original.MediaKey) || !strings.Contains(raw, "Updated report") {
				t.Fatalf("partial commit: key=%q presentation=%s", key, raw)
			}
			if _, err := ms.db.Exec("DROP TRIGGER refuse_presentation"); err != nil {
				t.Fatal(err)
			}
			// A different file cannot inherit presentation for the old hash.
			replacement.Mimetype, replacement.FileName, replacement.Title = nil, nil, nil
			if err := write(replacement); err != nil {
				t.Fatal(err)
			}
			source, found, err = ms.messageContentLookup("PRESENT1", mediaTestChat)
			if err != nil || !found || source.presentation != nil || source.filename != "" {
				t.Fatalf("presentation survived a changed hash: %+v err=%v", source, err)
			}
		})
	}
}

func TestMediaPresentationColumnMigrationKeepsOldRows(t *testing.T) {
	ms := newTestMessageStore(t)
	if _, err := ms.db.Exec("ALTER TABLE messages DROP COLUMN media_presentation"); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := ms.db.Exec("INSERT INTO messages (id,chat_jid,content,timestamp,media_type,filename,file_length) VALUES ('OLD1',?,'old',?,'document','report.pdf',3)", mediaTestChat, dbTime(ts)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureMessageStoreSchema(ms.db); err != nil {
			t.Fatal(err)
		}
	}
	var content, filename string
	var presentation any
	if err := ms.db.QueryRow("SELECT content,filename,media_presentation FROM messages WHERE id='OLD1'").Scan(&content, &filename, &presentation); err != nil {
		t.Fatal(err)
	}
	if content != "old" || filename != "report.pdf" || presentation != nil {
		t.Fatalf("migration rewrote old row: %s %s %v", content, filename, presentation)
	}
}
