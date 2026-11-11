package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestPeerReceiptsSurviveLIDMigration(t *testing.T) {
	ms := newTestMessageStore(t)
	lid, pn := "100000000000001@lid", "5511999999999@s.whatsapp.net"
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for _, chat := range []string{lid, pn} {
		if err := ms.StoreMessage(storedMessage{ID: "own", ChatJID: chat, Sender: "5511888888888", Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
			t.Fatal(err)
		}
		stamp := at
		if chat == lid {
			stamp = at.Add(time.Minute)
		}
		if _, err := ms.db.Exec(`INSERT INTO message_receipts VALUES (?,'own','5511999999999','s.whatsapp.net','read',?)`, chat, dbTime(stamp)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.StoreMessage(storedMessage{ID: "lid-only", ChatJID: lid, Sender: "5511888888888", Content: "Bob", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec(`INSERT INTO message_receipts VALUES (?,'lid-only','5511999999999','s.whatsapp.net','read',?)`, lid, dbTime(at)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	wa, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wa.Close() })
	if _, err = wa.Exec(`CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES('100000000000001','5511999999999')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	var chat, stamp string
	if err := ms.db.QueryRow(`SELECT chat_jid,CAST(at AS TEXT) FROM message_receipts WHERE message_id='own'`).Scan(&chat, &stamp); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 || chat != pn || stamp != dbTime(at.Add(time.Minute)) {
		t.Fatalf("receipts=%d chat=%s time=%s", count, chat, stamp)
	}
	if err := ms.db.QueryRow(`SELECT chat_jid FROM message_receipts WHERE message_id='lid-only'`).Scan(&chat); err != nil || chat != pn {
		t.Fatal(chat, err)
	}
}

func TestPeerReceiptEventIsAtomic(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
	group := types.NewJID("120363000000000001", types.GroupServer)
	ids := make([]types.MessageID, 512)
	tx, err := ms.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%03d", i)
		if _, err := tx.Exec(`INSERT INTO messages(id,chat_jid,is_from_me,content) VALUES(?,?,1,'Alice')`, ids[i], group.String()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec(`CREATE TRIGGER refuse_receipt BEFORE INSERT ON message_receipts WHEN NEW.message_id='id-300' BEGIN SELECT RAISE(ABORT,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	b.storePeerReceipts(&events.Receipt{MessageSource: types.MessageSource{Chat: group, Sender: types.NewJID("5511999999999", types.DefaultUserServer)}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	var count int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partially persisted one event: %d receipts", count)
	}
	if _, err := ms.db.Exec(`DROP TRIGGER refuse_receipt`); err != nil {
		t.Fatal(err)
	}
	b.storePeerReceipts(&events.Receipt{MessageSource: types.MessageSource{Chat: group, Sender: types.NewJID("5511999999999", types.DefaultUserServer)}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&count); err != nil || count != len(ids) {
		t.Fatal(count, err)
	}
}

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
	b.storePeerReceipts(receipt)
	receipt.Sender, receipt.SenderAlt = pn, types.EmptyJID
	receipt.Timestamp = at.Add(time.Minute)
	b.storePeerReceipts(receipt)
	receipt.Timestamp = at.Add(-time.Minute)
	b.storePeerReceipts(receipt)
	var chat, id, recipient, server, kind, stamp string
	if err := ms.db.QueryRow(`SELECT chat_jid,message_id,recipient,recipient_server,type,CAST(at AS TEXT) FROM message_receipts`).Scan(&chat, &id, &recipient, &server, &kind, &stamp); err != nil {
		t.Fatalf("%v logs=%v", err, b.Log)
	}
	if chat != group.String() || id != "own" || recipient != pn.User || server != pn.Server || kind != "read" || stamp != dbTime(at.Add(time.Minute)) {
		t.Fatalf("unexpected receipt: %s %s %s %s %s %s", chat, id, recipient, server, kind, stamp)
	}
	for _, typ := range []types.ReceiptType{types.ReceiptTypeDelivered, types.ReceiptTypePlayed} {
		receipt.Type, receipt.Timestamp = typ, at
		b.storePeerReceipts(receipt)
	}
	receipt.Sender = types.NewJID("100000000000002", types.HiddenUserServer)
	b.storePeerReceipts(receipt)
	for _, typ := range []types.ReceiptType{types.ReceiptTypeReadSelf, types.ReceiptTypeSender, types.ReceiptTypeRetry} {
		receipt.Type = typ
		b.storePeerReceipts(receipt)
	}
	receipt.Type, receipt.IsFromMe = types.ReceiptTypePlayed, true
	b.storePeerReceipts(receipt)
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
	b.storePeerReceipts(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid, SenderAlt: pn}, MessageIDs: []string{"own"}, Timestamp: at, Type: types.ReceiptTypeDelivered})
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
		b.storePeerReceipts(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: peer, IsGroup: true}, MessageIDs: []string{"same", "later"}, Type: types.ReceiptTypeRead, Timestamp: at.Add(time.Hour)})
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
	result, err = b.deleteForMeLocal(context.Background(), []deleteForMeRef{{row: mediaRow{ID: "later", ChatJID: group.String()}, fromMe: true}})
	if err != nil || result.Messages != 1 {
		t.Fatal(result, err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after delete-for-me receipts=%d, want none", n)
	}
}

type gatedReceiptLIDs struct {
	mockLIDStore
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (m *gatedReceiptLIDs) GetPNForLID(ctx context.Context, _ types.JID) (types.JID, error) {
	m.calls.Add(1)
	select {
	case m.entered <- struct{}{}:
	default:
	}
	select {
	case <-m.release:
		return types.NewJID("5511999999999", types.DefaultUserServer), nil
	case <-ctx.Done():
		return types.EmptyJID, ctx.Err()
	}
}
func TestPeerReceiptWorkerBoundedAndLifecycle(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	lids := &gatedReceiptLIDs{entered: make(chan struct{}, 128), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(lids.release) }) }
	defer release()
	b := testBridge(t, newTestClient(lids), ms, installRecordingLogger(t))
	processed := make(chan struct{}, 128)
	allowNext := make(chan struct{})
	b.peerReceiptProcessed = func() {
		processed <- struct{}{}
		select {
		case <-allowNext:
		case <-b.ctx.Done():
		}
	}
	at := time.Now()
	if err := ms.StoreChat("5511999999999@s.whatsapp.net", "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "own", ChatJID: "5511999999999@s.whatsapp.net", Sender: "5511888888888", Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	event := &events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []types.MessageID{"own"}, Type: types.ReceiptTypeRead, Timestamp: at}
	returned := make(chan struct{})
	go func() { b.handleEvent(event, nil); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("event handler blocked on LID/database work")
	}
	select {
	case <-lids.entered:
	case <-time.After(time.Second):
		t.Fatal("worker never reached LID lookup")
	}
	event.MessageIDs[0] = "mutated after dispatch"
	queued := *event
	queued.MessageIDs = []types.MessageID{"own"}
	oversized := queued
	oversized.MessageIDs = make([]types.MessageID, 16385)
	b.handleEvent(&oversized, nil)
	oversized.MessageIDs = []types.MessageID{strings.Repeat("a", (1<<20)+1)}
	b.handleEvent(&oversized, nil)
	if len(b.peerReceiptQueue) != 0 || lids.calls.Load() != 1 {
		t.Fatal("event budget or sender lookup bound violated")
	}
	for range 65 {
		b.handleEvent(&queued, nil)
	}
	if len(b.peerReceiptQueue) != 64 {
		t.Fatal("unbounded receipt queue", len(b.peerReceiptQueue))
	}
	release()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("receipt did not complete")
	}
	var count int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE message_id='own'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if lids.calls.Load() != 1 {
		t.Fatal("resolved the same sender twice", lids.calls.Load())
	}
	close(allowNext)
	b.Shutdown(5 * time.Second)
	select {
	case <-b.peerReceiptDone:
	default:
		t.Fatal("receipt worker survived shutdown")
	}
	before := len(b.peerReceiptQueue)
	b.handleEvent(&queued, nil)
	if len(b.peerReceiptQueue) != before {
		t.Fatal("accepted work after shutdown")
	}
}

func TestPeerReceiptShutdownCancelsLookup(t *testing.T) {
	ms := newTestMessageStore(t)
	lids := &gatedReceiptLIDs{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(lids.release)
	b := testBridge(t, newTestClient(lids), ms, installRecordingLogger(t))
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	b.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []types.MessageID{"own"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()}, nil)
	select {
	case <-lids.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin lookup")
	}
	b.Shutdown(time.Second)
	select {
	case <-b.peerReceiptDone:
	default:
		t.Fatal("cancelled worker survived shutdown")
	}
}
