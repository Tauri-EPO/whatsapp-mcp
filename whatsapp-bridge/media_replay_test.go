package main

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMediaReplayKeepsOneDownloadableSnapshot(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history batch"}[batch], func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			ts := time.Now().Truncate(time.Second)
			if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			persist := func(msg *waE2E.Message) {
				t.Helper()
				ex := extractMessage(msg, ts, "REPLAY1")
				write := func(w messageWriter) error {
					return persistMessage(w, "REPLAY1", mediaTestChat, "5511999999999@s.whatsapp.net", ts, false, ex, false, testLogger())
				}
				var err error
				if batch {
					err = ms.Batch(func(b *messageBatch) error { return write(b) })
				} else {
					err = write(ms)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			check := func(doc *waE2E.DocumentMessage) {
				t.Helper()
				var kind, name, url string
				var direct sql.NullString
				var key, sha, enc []byte
				var length uint64
				if err := ms.db.QueryRow(`SELECT media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = 'REPLAY1' AND chat_jid = ?`, mediaTestChat).Scan(&kind, &name, &url, &direct, &key, &sha, &enc, &length); err != nil {
					t.Fatal(err)
				}
				if kind != "document" || name != doc.GetFileName() || url != doc.GetURL() || direct.String != doc.GetDirectPath() || !bytes.Equal(key, doc.GetMediaKey()) || !bytes.Equal(sha, doc.GetFileSHA256()) || !bytes.Equal(enc, doc.GetFileEncSHA256()) || length != doc.GetFileLength() {
					t.Fatalf("snapshot changed: kind=%q name=%q url=%q direct=%q length=%d", kind, name, url, direct.String, length)
				}
			}
			original := fixtureDocument()
			persist(&waE2E.Message{Conversation: proto.String("placeholder before media arrives")})
			persist(&waE2E.Message{DocumentMessage: original})
			b := testBridge(t, nil, ms, testLogger())
			transfers := 0
			b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, relPath string) (int64, error) {
				transfers++
				return writeLikeDownloadToPath(relPath, []byte("document bytes"))
			}
			fetch := func() {
				t.Helper()
				if ok, _, _, _, err := b.downloadMedia(t.Context(), "REPLAY1", mediaTestChat); err != nil || !ok {
					t.Fatalf("download: ok=%v err=%v", ok, err)
				}
			}
			fetch()
			ts = ts.Add(time.Hour) // A placeholder's timestamp must not move the existing cache path.
			for _, replay := range []*waE2E.Message{
				{Conversation: proto.String("placeholder")},
				{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("stub.txt")}},
				{DocumentMessage: &waE2E.DocumentMessage{URL: proto.String("https://example.invalid/stub"), DirectPath: proto.String("/stub"), MediaKey: []byte("different incomplete key")}},
			} {
				persist(replay)
				check(original)
				fetch()
				if transfers != 1 {
					t.Fatal("replay hid the cached file")
				}
			}
			refresh := fixtureDocument()
			refresh.URL, refresh.DirectPath = nil, proto.String("/fresh")
			refresh.MediaKey, refresh.FileSHA256, refresh.FileEncSHA256 = []byte("fresh key"), []byte("fresh sha"), []byte("fresh enc")
			refresh.FileLength = proto.Uint64(0)
			persist(&waE2E.Message{DocumentMessage: refresh})
			check(refresh)
		})
	}
}

func TestMediaReplayRefreshIsAtomic(t *testing.T) {
	for _, mode := range []string{"live", "history batch", "outbound"} {
		t.Run(mode, func(t *testing.T) {
			ms := newTestMessageStore(t)
			ts := time.Now().Truncate(time.Second)
			if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			persist := func(doc *waE2E.DocumentMessage) error {
				if mode == "outbound" {
					media := outboundMediaColumns(doc.GetFileName(), whatsmeow.UploadResponse{
						URL: doc.GetURL(), DirectPath: doc.GetDirectPath(), MediaKey: doc.GetMediaKey(),
						FileSHA256: doc.GetFileSHA256(), FileEncSHA256: doc.GetFileEncSHA256(), FileLength: doc.GetFileLength(),
					})
					return media.store(ms, "ATOMIC1", mediaTestChat, "5511999999999@s.whatsapp.net", "document", ts, "")
				}
				ex := extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "ATOMIC1")
				write := func(w messageWriter) error {
					return persistMessage(w, "ATOMIC1", mediaTestChat, "5511999999999@s.whatsapp.net", ts, false, ex, false, testLogger())
				}
				if mode == "history batch" {
					return ms.Batch(func(b *messageBatch) error { return write(b) })
				}
				return write(ms)
			}
			original := fixtureDocument()
			if err := persist(original); err != nil {
				t.Fatal(err)
			}
			if _, err := ms.db.Exec(`CREATE TRIGGER fail_path BEFORE UPDATE OF direct_path ON messages BEGIN SELECT RAISE(ABORT, 'blocked refresh'); END`); err != nil {
				t.Fatal(err)
			}
			refresh := fixtureDocument()
			refresh.URL, refresh.DirectPath, refresh.MediaKey = proto.String("https://example.invalid/fresh"), proto.String("/fresh"), []byte("fresh key")
			if err := persist(refresh); err == nil {
				t.Fatal("a rejected path update must fail the entire snapshot write")
			}
			var url, path string
			var key []byte
			if err := ms.db.QueryRow(`SELECT url, direct_path, media_key FROM messages WHERE id = 'ATOMIC1' AND chat_jid = ?`, mediaTestChat).Scan(&url, &path, &key); err != nil {
				t.Fatal(err)
			}
			if url != original.GetURL() || path != original.GetDirectPath() || !bytes.Equal(key, original.GetMediaKey()) {
				t.Fatal("failed refresh partially replaced the snapshot")
			}
		})
	}
}
