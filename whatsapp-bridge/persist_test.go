package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractMessageDoesNotMutateInput(t *testing.T) {
	inner := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("secret"), URL: proto.String("https://x/enc"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(9)}}
	wrapper := &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: inner}}
	ex := extractMessage(wrapper, time.Now(), "ID1")
	if !ex.viewOnce || ex.mediaType != "image" || ex.inner != inner || ex.fileLen != 9 {
		t.Fatalf("extract = %+v", ex)
	}
	if wrapper.ViewOnceMessageV2 == nil {
		t.Fatal("input message must not be mutated")
	}
	if ex.empty() {
		t.Fatal("image is not empty")
	}
	if extractMessage(&waE2E.Message{}, time.Now(), "x").empty() != true {
		t.Fatal("empty message must be empty")
	}
	if extractMessage(nil, time.Now(), "x").empty() != true {
		t.Fatal("nil message must be empty")
	}
}

func TestPersistMessageWritesSideTables(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "5511999999999@s.whatsapp.net"
	_ = ms.StoreChat(chat, "A", time.Now())
	poll := extractMessage(pollCreationMsg("Q?", "a", "b"), time.Now(), "P1")
	if err := persistMessage(ms, "P1", chat, "x", time.Now(), false, poll, true, testLogger()); err != nil {
		t.Fatal(err)
	}
	if p, err := ms.GetPoll("P1", chat); err != nil || len(p.Options) != 2 {
		t.Fatalf("poll side table: %v %+v", err, p)
	}
	vo := extractMessage(&waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("once")}}}, time.Now(), "V1")
	if err := persistMessage(ms, "V1", chat, "x", time.Now(), false, vo, true, testLogger()); err != nil {
		t.Fatal(err)
	}
	var flag bool
	_ = ms.db.QueryRow(`SELECT view_once FROM messages WHERE id = 'V1'`).Scan(&flag)
	if !flag {
		t.Fatal("view_once flag not set")
	}
	// Inside a batch the same function works against the transaction.
	err := ms.Batch(func(b *messageBatch) error {
		return persistMessage(b, "B1", chat, "x", time.Now(), false, extractMessage(&waE2E.Message{Conversation: proto.String("hi")}, time.Now(), "B1"), false, testLogger())
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'B1'`).Scan(&n)
	if n != 1 {
		t.Fatal("batched row missing")
	}
}
