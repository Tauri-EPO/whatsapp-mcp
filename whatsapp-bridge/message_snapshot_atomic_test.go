package main

import (
	"bytes"
	"testing"
	"time"
)

func TestNamedMessageSnapshotMentionsFailureKeepsOldRow(t *testing.T) {
	for _, path := range []string{"single", "batch", "persist"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			store, err := NewMessageStore()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.StoreChat("111@s.whatsapp.net", "Alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			old := storedMessage{ID: "ATOMIC", ChatJID: "111@s.whatsapp.net", Sender: "111", Content: "old", Timestamp: time.Now(), MediaType: "image", URL: "https://example.invalid/old", MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: uint64(1), Media: messageMediaOptions{directPath: "/old"}, Mentions: `111`}
			if err := store.StoreMessage(old); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`CREATE TRIGGER refuse_snapshot_mentions BEFORE UPDATE OF mentions ON messages WHEN new.mentions = '222' BEGIN SELECT RAISE(ABORT, 'fake mentions rejection'); END;`); err != nil {
				t.Fatal(err)
			}
			incoming := old
			incoming.Content, incoming.URL, incoming.Media.directPath, incoming.Mentions = "new", "https://example.invalid/new", "/new", `222`
			incoming.FileSHA256, incoming.FileEncSHA256 = bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 32)
			switch path {
			case "single":
				err = store.StoreMessage(incoming)
			case "batch":
				err = store.Batch(func(batch *messageBatch) error { return batch.StoreMessage(incoming) })
			case "persist":
				err = store.Batch(func(batch *messageBatch) error {
					return persistMessage(batch, incoming.ID, incoming.ChatJID, incoming.Sender, incoming.Timestamp, false, extractedMessage{content: incoming.Content, mediaType: incoming.MediaType, url: incoming.URL, directPath: incoming.Media.directPath, mediaKey: incoming.MediaKey, fileSHA: incoming.FileSHA256, fileEnc: incoming.FileEncSHA256, fileLen: 1, hasLength: true, mentions: []string{"222@s.whatsapp.net"}}, false, testLogger())
				})
			}
			if err == nil {
				t.Fatal("mentions trigger did not refuse the same insert statement")
			}
			var content, url, direct, mentions string
			var plainHash, encryptedHash []byte
			if err := store.db.QueryRow("SELECT content,url,direct_path,mentions,file_sha256,file_enc_sha256 FROM messages WHERE id='ATOMIC'").Scan(&content, &url, &direct, &mentions, &plainHash, &encryptedHash); err != nil {
				t.Fatal(err)
			}
			if content != old.Content || url != old.URL || direct != old.Media.directPath || mentions != old.Mentions || !bytes.Equal(plainHash, old.FileSHA256) || !bytes.Equal(encryptedHash, old.FileEncSHA256) {
				t.Fatal("failed snapshot left a mixed old/new archive row")
			}
			if _, err := store.db.Exec("DROP TRIGGER refuse_snapshot_mentions"); err != nil {
				t.Fatal(err)
			}
			if err := store.StoreMessage(incoming); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow("SELECT content,url,direct_path,mentions FROM messages WHERE id='ATOMIC'").Scan(&content, &url, &direct, &mentions); err != nil || content != incoming.Content || url != incoming.URL || direct != incoming.Media.directPath || mentions != incoming.Mentions {
				t.Fatal("successful snapshot was not fully replaced")
			}
		})
	}
}
