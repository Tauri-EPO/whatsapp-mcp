package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestPeerReceiptsOwnMessageBatchAndIdentity(t *testing.T) {
	ms := newTestMessageStore(t)
	client := newTestClient(&mockLIDStore{})
	b := testBridge(t, client, ms, installRecordingLogger(t))
	group := types.NewJID("120363000000000001", types.GroupServer)
	other := types.NewJID("120363000000000002", types.GroupServer)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	for _, row := range []storedMessage{
		{ID: "own", ChatJID: group.String(), Sender: pn.String(), Timestamp: at, IsFromMe: true},
		{ID: "inbound", ChatJID: group.String(), Sender: pn.String(), Timestamp: at},
		{ID: "elsewhere", ChatJID: other.String(), Sender: pn.String(), Timestamp: at, IsFromMe: true},
	} {
		row.Content = "Alice"
		if err := ms.StoreMessage(row); err != nil {
			t.Fatal(err)
		}
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: group, Sender: lid, SenderAlt: pn, IsGroup: true}, MessageIDs: []string{"own", "inbound", "elsewhere", "missing"}, Type: types.ReceiptTypeRead, Timestamp: at}
	b.handleEvent(receipt, nil)
	receipt.Sender, receipt.SenderAlt = pn, types.EmptyJID
	receipt.Timestamp = at.Add(time.Minute)
	b.handleEvent(receipt, nil)
	receipt.Timestamp = at.Add(-time.Minute)
	b.handleEvent(receipt, nil)
	var chat, id, recipient, server, kind, stamp string
	if err := ms.db.QueryRow(`SELECT chat_jid,message_id,recipient,recipient_server,type,CAST(at AS TEXT) FROM message_receipts`).Scan(&chat, &id, &recipient, &server, &kind, &stamp); err != nil {
		t.Fatalf("%v logs=%v", err, b.Log)
	}
	if chat != group.String() || id != "own" || recipient != pn.User || server != pn.Server || kind != "read" || stamp != dbTime(at.Add(time.Minute)) {
		t.Fatalf("unexpected receipt: %s %s %s %s %s %s", chat, id, recipient, server, kind, stamp)
	}
	for _, typ := range []types.ReceiptType{types.ReceiptTypeDelivered, types.ReceiptTypePlayed} {
		receipt.Type, receipt.Timestamp = typ, at
		b.handleEvent(receipt, nil)
	}
	receipt.Sender = types.NewJID("100000000000002", types.HiddenUserServer)
	b.handleEvent(receipt, nil)
	for _, typ := range []types.ReceiptType{types.ReceiptTypeReadSelf, types.ReceiptTypeSender, types.ReceiptTypeRetry} {
		receipt.Type = typ
		b.handleEvent(receipt, nil)
	}
	receipt.Type, receipt.IsFromMe = types.ReceiptTypePlayed, true
	b.handleEvent(receipt, nil)
	var count int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("count=%d, want three types plus an unmapped LID participant", count)
	}
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatal(err)
	}
}

func TestPeerReceiptLIDDirectChat(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	if err := ms.StoreMessage(storedMessage{ID: "own", ChatJID: pn.String(), Sender: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	b.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid, SenderAlt: pn}, MessageIDs: []string{"own"}, Timestamp: at, Type: types.ReceiptTypeDelivered}, nil)
	var chat string
	if err := ms.db.QueryRow(`SELECT chat_jid FROM message_receipts`).Scan(&chat); err != nil {
		t.Fatalf("%v logs=%v", err, b.Log)
	}
	if chat != pn.String() {
		t.Fatal(chat)
	}
}

func TestPeerReceiptsFollowOriginalDeletion(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	self := types.NewJID("5511888888888", types.DefaultUserServer)
	peer := types.NewJID("5511999999999", types.DefaultUserServer)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), ms, installRecordingLogger(t))
	group := types.NewJID("120363000000000001", types.GroupServer)
	other := types.NewJID("120363000000000002", types.GroupServer)
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, chat := range []types.JID{group, other} {
		if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
			t.Fatal(err)
		}
		if err := ms.StoreMessage(storedMessage{ID: "same", ChatJID: chat.String(), Sender: self.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.StoreMessage(storedMessage{ID: "later", ChatJID: group.String(), Sender: self.String(), Content: "Bob", Timestamp: at.Add(time.Minute), IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []types.JID{group, other} {
		b.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: peer, IsGroup: true}, MessageIDs: []string{"same", "later"}, Type: types.ReceiptTypeRead, Timestamp: at.Add(time.Hour)}, nil)
	}
	count := func() int {
		var n int
		if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("receipt fixture count=%d", n)
	}
	result, err := b.deleteChatLocal(context.Background(), []string{group.String()}, at)
	if err != nil || result.Messages != 1 {
		t.Fatal(result, err)
	}
	if n := count(); n != 2 {
		t.Fatalf("after bounded chat delete receipts=%d, want newer original and other chat only", n)
	}
	// Every original DELETE uses the same schema cleanup, including single rows.
	if _, err := ms.db.Exec(`DELETE FROM messages WHERE chat_jid=? AND id=?`, other.String(), "same"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after single original delete receipts=%d, want only newer original", n)
	}
}
