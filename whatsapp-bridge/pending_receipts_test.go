package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Use the real event worker and on-disk archive, including canonical writes.
func pendingReceiptFixture(t *testing.T) (*MessageStore, func(*events.Receipt)) {
	ms, _, dispatch := pendingReceiptBridgeFixture(t)
	return ms, dispatch
}

func pendingReceiptBridgeFixture(t *testing.T) (*MessageStore, *Bridge, func(*events.Receipt)) {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511888888888", types.DefaultUserServer)), ms, installRecordingLogger(t))
	processed := make(chan struct{}, 1)
	b.peerReceiptProcessed = func() { processed <- struct{}{} }
	return ms, b, func(receipt *events.Receipt) {
		t.Helper()
		b.handleEvent(receipt, nil)
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatal("receipt worker did not finish")
		}
	}
}

func receiptSessionMapping(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, sessionPoolConns)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES('100000000000001','5511999999999')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPendingReceiptsNewChatMapping(t *testing.T) {
	for _, mode := range []string{"live", "history", "startup", "delivery-hint"} {
		for _, own := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/own=%v", mode, own), func(t *testing.T) {
				ms, b, dispatch := pendingReceiptBridgeFixture(t)
				pn := types.NewJID("5511999999999", types.DefaultUserServer)
				lid := types.NewJID("100000000000001", types.HiddenUserServer)
				at := time.Now()
				dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"H0"}, Type: types.ReceiptTypeRead, Timestamp: at})
				if pendingReceiptCount(t, ms) != 1 {
					t.Fatal("unmapped receipt missing")
				}
				mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
				if mode != "delivery-hint" {
					mapping.lidByPN = map[types.JID]types.JID{pn: lid}
					mapping.pnByLID = map[types.JID]types.JID{lid: pn}
				}
				if mode == "startup" {
					if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(receiptSessionMapping(t), testLogger()); err != nil {
						t.Fatal(err)
					}
					var chat string
					var archived int
					if err := ms.db.QueryRow(`SELECT chat_jid FROM pending_receipts`).Scan(&chat); err != nil {
						t.Fatal(err)
					}
					if err := ms.db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&archived); err != nil || chat != pn.String() || archived != 0 {
						t.Fatal(chat, archived, err)
					}
				}
				if mode == "history" {
					history := largeHistoryFixture(1)
					history.Data.Conversations[0].ID = proto.String(pn.String())
					history.Data.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(own)
					b.handleHistorySync(history)
				} else {
					sender := pn
					if own {
						sender = *b.currentClient().Store.ID
					}
					original := buildTextMessage(pn, sender, types.EmptyJID, types.EmptyJID, own, "Alice")
					if mode == "delivery-hint" {
						original.Info.Chat = lid
						if own {
							original.Info.RecipientAlt = pn
						} else {
							original.Info.Sender, original.Info.SenderAlt = lid, pn
						}
					}
					original.Info.ID, original.Info.Timestamp = "H0", at
					b.handleEvent(original, nil)
				}
				var originals, receipts int
				if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE id='H0' AND chat_jid=?),(SELECT COUNT(*) FROM message_receipts WHERE message_id='H0' AND chat_jid=? AND recipient=? AND recipient_server=?)`, pn.String(), pn.String(), lid.User, lid.Server).Scan(&originals, &receipts); err != nil {
					t.Fatal(err)
				}
				want := 0
				if own {
					want = 1
				}
				if originals != 1 || receipts != want || pendingReceiptCount(t, ms) != 0 {
					t.Fatal(originals, receipts, pendingReceiptCount(t, ms))
				}
			})
		}
	}
}

func pendingReceiptCount(t *testing.T, ms *MessageStore) int {
	t.Helper()
	var n int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM pending_receipts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPendingReceiptsOwnershipAndFullKey(t *testing.T) {
	ms, dispatch := pendingReceiptFixture(t)
	at := time.Now()
	chat := types.NewJID("120363000000000001", types.GroupServer)
	other := "120363000000000002@g.us"
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("5511999999999", types.DefaultUserServer)}, MessageIDs: []string{"late-inbound", "early-inbound", "same"}, Type: types.ReceiptTypeRead, Timestamp: at}
	for _, jid := range []string{chat.String(), other} {
		if err := ms.StoreChat(jid, "Alice", at); err != nil {
			t.Fatal(err)
		}
	}
	store := func(id, jid string, own bool) {
		t.Helper()
		if err := ms.StoreMessage(storedMessage{ID: id, ChatJID: jid, Sender: "5511999999999", Content: "Alice", Timestamp: at, IsFromMe: own}); err != nil {
			t.Fatal(err)
		}
	}
	store("early-inbound", chat.String(), false)
	dispatch(receipt)
	if n := pendingReceiptCount(t, ms); n != 2 {
		t.Fatalf("known inbound retained: pending=%d", n)
	}
	store("late-inbound", chat.String(), false)
	store("same", other, true)
	if n := pendingReceiptCount(t, ms); n != 1 {
		t.Fatalf("inbound not discarded or same-ID other-chat consumed: pending=%d", n)
	}
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 {
		t.Fatal(durable, err)
	}
	// Reclassifying a previously inbound original cannot resurrect its receipt.
	store("late-inbound", chat.String(), true)
	store("same", chat.String(), true)
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE message_id='same' AND chat_jid=?`, chat.String()).Scan(&durable); err != nil || durable != 1 {
		t.Fatal(durable, err)
	}
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE message_id!='same'`).Scan(&durable); err != nil || durable != 0 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(durable, err)
	}
}

func TestPendingReceiptsExpiryUsesFirstArrival(t *testing.T) {
	for _, consume := range []bool{false, true} {
		t.Run(fmt.Sprintf("prune-by-original=%v", consume), func(t *testing.T) {
			ms, dispatch := pendingReceiptFixture(t)
			now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			ms.editNow = func() time.Time { return now }
			chat := types.NewJID("5511999999999", types.DefaultUserServer)
			receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: []string{"expired"}, Type: types.ReceiptTypeRead, Timestamp: now.Add(365 * 24 * time.Hour)}
			dispatch(receipt)
			var arrived, expires int64
			if err := ms.db.QueryRow(`SELECT arrived_ms,expires_ms FROM pending_receipts`).Scan(&arrived, &expires); err != nil {
				t.Fatal(err)
			}
			now = now.Add(pendingReceiptTTL - time.Millisecond)
			receipt.Timestamp = receipt.Timestamp.Add(time.Minute)
			dispatch(receipt)
			var again, deadline int64
			if err := ms.db.QueryRow(`SELECT arrived_ms,expires_ms FROM pending_receipts`).Scan(&again, &deadline); err != nil || arrived != again || expires != deadline || expires-arrived != pendingReceiptTTL.Milliseconds() {
				t.Fatal("replay refreshed lifetime", again, deadline, err)
			}
			now = now.Add(time.Millisecond)
			if !consume {
				receipt.MessageIDs = []string{"fresh"}
				dispatch(receipt)
			}
			if err := ms.StoreChat(chat.String(), "Alice", now); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "expired", ChatJID: chat.String(), Sender: chat.User, Content: "Alice", Timestamp: now, IsFromMe: true}); err != nil {
				t.Fatal(err)
			}
			var durable int
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 {
				t.Fatal("expired receipt promoted", durable, err)
			}
			want := 0
			if !consume {
				want = 1
			}
			if n := pendingReceiptCount(t, ms); n != want {
				t.Fatal("expiry not pruned", n)
			}
		})
	}
}

func TestPendingReceiptsCountAndByteBounds(t *testing.T) {
	ms, dispatch := pendingReceiptFixture(t)
	at := time.Now()
	readers := []string{"5511999999999", "5511888888888", "5511777777777", "5511666666666", "5511555555555"}
	ids := make([]string, pendingReceiptReaderCap+5)
	for i := range ids {
		ids[i] = fmt.Sprintf("unknown-%04d", i)
	}
	for g := range 5 {
		chat := types.NewJID(fmt.Sprintf("120363000000000%03d", g+1), types.GroupServer)
		for _, reader := range readers {
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID(reader, types.DefaultUserServer)}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: at})
		}
	}
	if n := pendingReceiptCount(t, ms); n != pendingReceiptGlobalCap {
		t.Fatalf("global cap: %d", n)
	}
	var readersMax, chatsMax, oldest, originals, chats int
	if err := ms.db.QueryRow(`SELECT
	 (SELECT MAX(n) FROM (SELECT COUNT(*) n FROM pending_receipts GROUP BY chat_jid,recipient,recipient_server)),
	 (SELECT MAX(n) FROM (SELECT COUNT(*) n FROM pending_receipts GROUP BY chat_jid)),
	 (SELECT COUNT(*) FROM pending_receipts WHERE message_id='unknown-0000'),
	 (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM chats)`).Scan(&readersMax, &chatsMax, &oldest, &originals, &chats); err != nil {
		t.Fatal(err)
	}
	if readersMax != pendingReceiptReaderCap || chatsMax != pendingReceiptChatCap || oldest != 0 || originals != 0 || chats != 0 {
		t.Fatal(readersMax, chatsMax, oldest, originals, chats)
	}
	if _, err := ms.db.Exec(`DELETE FROM pending_receipts`); err != nil {
		t.Fatal(err)
	}
	peer := types.NewJID(readers[0], types.DefaultUserServer)
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: peer, Sender: peer}, MessageIDs: []string{"", strings.Repeat("a", pendingReceiptKeyBytes), "small"}, Type: types.ReceiptTypeRead, Timestamp: at})
	if n := pendingReceiptCount(t, ms); n != 1 {
		t.Fatalf("oversized or empty key persisted: %d", n)
	}
	// Schema creation is idempotent and timestamp rewriting covers pending at.
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatal(err)
	}
}

func TestPendingReceiptsPromotionRollback(t *testing.T) {
	ms, dispatch := pendingReceiptFixture(t)
	at := time.Now()
	chat := types.NewJID("5511999999999", types.DefaultUserServer)
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: []string{"own"}, Type: types.ReceiptTypeRead, Timestamp: at})
	if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	message := storedMessage{ID: "own", ChatJID: chat.String(), Sender: chat.User, Content: "Alice", Timestamp: at, IsFromMe: true}
	if _, err := ms.db.Exec(`CREATE TRIGGER refuse_pending_promotion BEFORE INSERT ON message_receipts BEGIN SELECT RAISE(ROLLBACK,'refuse promotion'); END`); err != nil {
		t.Fatal(err)
	}
	err := ms.Batch(func(batch *messageBatch) error {
		_ = batch.StoreMessage(message) // even an ignored error must poison the batch
		return batch.StoreMessage(storedMessage{ID: "later", ChatJID: chat.String(), Content: "Bob", Timestamp: at, IsFromMe: true})
	})
	if err == nil {
		t.Fatal("failed promotion committed")
	}
	var originals, durable int
	if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM message_receipts)`).Scan(&originals, &durable); err != nil || originals != 0 || durable != 0 || pendingReceiptCount(t, ms) != 1 {
		t.Fatal(originals, durable, err)
	}
	if _, err := ms.db.Exec(`DROP TRIGGER refuse_pending_promotion`); err != nil {
		t.Fatal(err)
	}
	if err := ms.Batch(func(batch *messageBatch) error {
		if err := batch.StoreMessage(message); err != nil {
			return err
		}
		return errors.New("abort after promotion")
	}); err == nil || pendingReceiptCount(t, ms) != 1 {
		t.Fatal("pending receipt lost on rollback", err)
	}
	if err := ms.StoreMessage(message); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal("positive promotion control", durable, err)
	}
}

func TestPendingReceiptsArrivalOrder(t *testing.T) {
	var expected []string
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-first", true: "receipt-first"}[before], func(t *testing.T) {
			ms, dispatch := pendingReceiptFixture(t)
			at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			group := types.NewJID("120363000000000001", types.GroupServer)
			store := func() {
				if err := ms.StoreChat(group.String(), "Alice", at); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "own", ChatJID: group.String(), Sender: "5511999999999", Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
					t.Fatal(err)
				}
			}
			if !before {
				store()
			}
			for _, reader := range []types.JID{types.NewJID("5511999999999", types.DefaultUserServer), types.NewJID("100000000000001", types.HiddenUserServer)} {
				for _, kind := range []types.ReceiptType{types.ReceiptTypeDelivered, types.ReceiptTypeRead, types.ReceiptTypePlayed} {
					for _, delta := range []time.Duration{time.Minute, 0} {
						dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: group, Sender: reader, IsGroup: true}, MessageIDs: []string{"own"}, Type: kind, Timestamp: at.Add(delta)})
					}
				}
			}
			if before {
				store()
			}
			rows, err := ms.db.Query(`SELECT recipient||'@'||recipient_server||':'||type||':'||CAST(at AS TEXT) FROM message_receipts ORDER BY recipient,recipient_server,type`)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var got []string
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					t.Fatal(err)
				}
				if !strings.HasSuffix(row, dbTime(at.Add(time.Minute))) {
					t.Fatalf("newest receipt timestamp lost: %s", row)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !before {
				if len(got) != 6 {
					t.Fatalf("control receipts=%v", got)
				}
				expected = got
			} else if !reflect.DeepEqual(got, expected) {
				t.Fatalf("receipt-first=%v; original-first=%v", got, expected)
			}
		})
	}
}

func TestPendingReceiptsLiveOriginalAndLIDPhoneIdentity(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own=%v", own), func(t *testing.T) {
			ms, dispatch := pendingReceiptFixture(t)
			pn := types.NewJID("5511999999999", types.DefaultUserServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			at := time.Now()
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid, SenderAlt: pn}, MessageIDs: []string{"live"}, Type: types.ReceiptTypeDelivered, Timestamp: at})
			if n := pendingReceiptCount(t, ms); n != 1 {
				t.Fatal("receipt not deferred", n)
			}
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			original := buildTextMessage(pn, pn, types.EmptyJID, types.EmptyJID, own, "Alice")
			original.Info.ID, original.Info.Timestamp = "live", at
			b.handleEvent(original, nil)
			var receipts, originals int
			if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM message_receipts WHERE chat_jid=? AND recipient=? AND recipient_server=?),
			 (SELECT COUNT(*) FROM messages WHERE id='live' AND chat_jid=?)`, pn.String(), pn.User, pn.Server, pn.String()).Scan(&receipts, &originals); err != nil {
				t.Fatal(err)
			}
			want := 0
			if own {
				want = 1
			}
			if originals != 1 || receipts != want || pendingReceiptCount(t, ms) != 0 {
				t.Fatal(originals, receipts, want)
			}
		})
	}
}

func TestPendingReceiptsWholeEventRollback(t *testing.T) {
	ms, dispatch := pendingReceiptFixture(t)
	at := time.Now()
	chat := types.NewJID("5511999999999", types.DefaultUserServer)
	if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "own", ChatJID: chat.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"own"}
	for i := range 512 {
		ids = append(ids, fmt.Sprintf("pending-%03d", i))
	}
	if _, err := ms.db.Exec(`CREATE TRIGGER refuse_pending_receipt BEFORE INSERT ON pending_receipts WHEN NEW.message_id='pending-300' BEGIN SELECT RAISE(ABORT,'refuse second chunk'); END`); err != nil {
		t.Fatal(err)
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: at}
	dispatch(receipt)
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal("partial event committed", durable, err)
	}
	if _, err := ms.db.Exec(`DROP TRIGGER refuse_pending_receipt`); err != nil {
		t.Fatal(err)
	}
	dispatch(receipt)
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != pendingReceiptReaderCap {
		t.Fatal("positive event control", durable, err)
	}
}

func TestPendingReceiptsSurviveStoreReopen(t *testing.T) {
	ms, dispatch := pendingReceiptFixture(t)
	at := time.Now()
	chat := types.NewJID("5511999999999", types.DefaultUserServer)
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, MessageIDs: []string{"reopened"}, Type: types.ReceiptTypeRead, Timestamp: at})
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if pendingReceiptCount(t, reopened) != 1 {
		t.Fatal("pending receipt did not survive reopen")
	}
	if err := reopened.StoreChat(chat.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := reopened.StoreMessage(storedMessage{ID: "reopened", ChatJID: chat.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	var durable int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, reopened) != 0 {
		t.Fatal(durable, err)
	}
}
