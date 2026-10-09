package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgePagesReleaseSingleConnectionBeforeDiskCallbacks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(storeDirEnv, dir)
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stamp := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	chats := []string{"111@s.whatsapp.net", "222@s.whatsapp.net", "333@s.whatsapp.net"}
	for _, chat := range chats {
		if err := store.StoreChat(chat, "Alice", stamp); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, chat), storeDirMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Batch(func(batch *messageBatch) error {
		for index := range 280 {
			id := fmt.Sprintf("PAGE-%04d", index)
			for _, chat := range chats {
				if err := batch.StoreMessage(storedMessage{ID: id, ChatJID: chat, Sender: "111", Content: "fake cached image", Timestamp: stamp, MediaType: "image"}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Preserve a precise legacy spelling in SQL: pagination must not rebuild it
	// from a time.Time and silently reread a page at the same timestamp.
	if _, err := store.db.Exec("UPDATE messages SET timestamp='2024-01-01 00:00:00.123456+00:00'"); err != nil {
		t.Fatal(err)
	}
	for index := range 280 {
		for _, chat := range chats {
			name := mediaFileName("image", stamp, fmt.Sprintf("PAGE-%04d", index), "")
			if err := os.WriteFile(filepath.Join(dir, chat, name), []byte("fake image"), storeFileMode); err != nil {
				t.Fatal(err)
			}
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	store.db.SetMaxOpenConns(1)
	seen := map[string]bool{}
	var callbackErr error
	cut, err := store.EachMediaRowMatching("", time.Time{}, purgeCursor{}, "image", parseChatPolicy(chats[0]+","+chats[1]), 561, func(row mediaRow) bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
			callbackErr = err
			return false
		}
		if count != 840 {
			callbackErr = fmt.Errorf("archive rows changed: %d", count)
			return false
		}
		key := row.ID + "/" + row.ChatJID
		if seen[key] {
			callbackErr = fmt.Errorf("page repeated a full key")
			return false
		}
		seen[key] = true
		result := purgeOne(root, row, false)
		if !result.Purged {
			callbackErr = fmt.Errorf("real cached file was not removed: %s", result.Reason)
			return false
		}
		return true
	})
	if err != nil || callbackErr != nil || cut || len(seen) != 560 {
		t.Fatalf("paged purge cut=%v rows=%d err=%v callback=%v", cut, len(seen), err, callbackErr)
	}
	for index, chat := range chats {
		files, err := os.ReadDir(filepath.Join(dir, chat))
		want := 0
		if index == 2 {
			want = 280
		}
		if err != nil || len(files) != want {
			t.Fatalf("policy/disk result: files=%d want=%d err=%v", len(files), want, err)
		}
	}
}
