package main

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBatchStoresRowsAndCommits(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "5511999999999@s.whatsapp.net"
	if err := ms.StoreChat(chat, "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	err := ms.Batch(func(b *messageBatch) error {
		for i := 0; i < 50; i++ {
			if err := b.StoreMessage(fmt.Sprintf("B%d", i), chat, "5511999999999", fmt.Sprintf("hello %d", i),
				time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
				return err
			}
		}
		if err := b.MarkViewOnce("B1", chat); err != nil {
			return err
		}
		return b.StorePoll("B2", chat, &pollCreation{Question: "Q", Options: []string{"a", "b"}, SelectableCount: 1}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	var n, polls int
	var viewOnce bool
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, chat).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow(`SELECT view_once FROM messages WHERE id = 'B1' AND chat_jid = ?`, chat).Scan(&viewOnce); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM polls WHERE message_id = 'B2'`).Scan(&polls); err != nil {
		t.Fatal(err)
	}
	if n != 50 || !viewOnce || polls != 1 {
		t.Fatalf("rows=%d viewOnce=%v polls=%d", n, viewOnce, polls)
	}
	// Same row again through the single-row path: upsert, not duplicate.
	if err := ms.StoreMessage("B0", chat, "5511999999999", "edited", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	var content string
	_ = ms.db.QueryRow(`SELECT content FROM messages WHERE id = 'B0' AND chat_jid = ?`, chat).Scan(&content)
	if content != "edited" {
		t.Fatalf("upsert content = %q", content)
	}
}

func TestBatchRollsBackOnError(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "5511999999999@s.whatsapp.net"
	_ = ms.StoreChat(chat, "Alice", time.Now())
	boom := errors.New("boom")
	err := ms.Batch(func(b *messageBatch) error {
		if err := b.StoreMessage("R1", chat, "x", "one", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'R1'`).Scan(&n)
	if n != 0 {
		t.Fatal("row must be rolled back")
	}
}

func BenchmarkStoreMessagesOneByOne(b *testing.B) {
	ms := newTestMessageStore(b)
	const chat = "5511999999999@s.whatsapp.net"
	_ = ms.StoreChat(chat, "Alice", time.Now())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ms.StoreMessage(fmt.Sprintf("S%d", i), chat, "x", "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, "")
	}
}

func BenchmarkStoreMessagesBatch(b *testing.B) {
	ms := newTestMessageStore(b)
	const chat = "5511999999999@s.whatsapp.net"
	_ = ms.StoreChat(chat, "Alice", time.Now())
	b.ResetTimer()
	_ = ms.Batch(func(batch *messageBatch) error {
		for i := 0; i < b.N; i++ {
			_ = batch.StoreMessage(fmt.Sprintf("T%d", i), chat, "x", "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, "")
		}
		return nil
	})
}
