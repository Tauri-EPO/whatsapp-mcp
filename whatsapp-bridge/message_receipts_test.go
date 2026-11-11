package main

import (
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
