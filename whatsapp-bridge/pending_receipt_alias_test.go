package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestPendingReceiptsAliasDeniesUnrelatedChats(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	group := types.NewJID("120363000000000001", types.GroupServer)
	at := time.Now()
	for _, chat := range []types.JID{lid, group} {
		dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: pn}, MessageIDs: []string{"same"}, Type: types.ReceiptTypeRead, Timestamp: at})
	}
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: types.NewJID("100000000000002", types.HiddenUserServer)}
	original := buildTextMessage(pn, *b.currentClient().Store.ID, types.EmptyJID, types.EmptyJID, true, "Alice")
	original.Info.ID = "same"
	b.handleEvent(original, nil)
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 || pendingReceiptCount(t, ms) != 2 {
		t.Fatal("unrelated chat consumed", durable, err)
	}
	// The exact verified twin is a positive control; the group stays separate.
	mapping.lidByPN[pn] = lid
	b.handleEvent(original, nil)
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE chat_jid=?`, pn.String()).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != 1 {
		t.Fatal("verified twin not consumed", durable, err)
	}
	if err := ms.StoreChat(group.String(), "Bob", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "same", ChatJID: group.String(), Content: "Bob", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	if pendingReceiptCount(t, ms) != 0 {
		t.Fatal("exact group key not consumed")
	}
}

func TestPendingReceiptsAliasPreparationRechecksAndRetries(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	_, _, stale, err := b.preparePendingEdit(b.ctx, "late", pn.String(), b.currentClient().Store.ID.String(), true)
	if err != nil {
		t.Fatal(err)
	}
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"late"}, Type: types.ReceiptTypeRead, Timestamp: at})
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: lid}
	if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	row := storedMessage{ID: "late", ChatJID: pn.String(), Sender: b.currentClient().Store.ID.String(), Content: "Alice", Timestamp: at, IsFromMe: true, EditPreparation: stale}
	if err := ms.StoreMessage(row); !errors.Is(err, errPendingEditPreparationChanged) {
		t.Fatal("stale receipt preparation accepted", err)
	}
	var originals int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&originals); err != nil || originals != 0 || pendingReceiptCount(t, ms) != 1 {
		t.Fatal(originals, err)
	}
	attempts := 0
	if err := retryPendingEditPreparation(func() error {
		attempts++
		if attempts > 1 {
			_, alias, prepared, err := b.preparePendingEdit(b.ctx, row.ID, row.ChatJID, row.Sender, true)
			if err != nil {
				return err
			}
			row.EditChatAlias, row.EditPreparation = alias, prepared
		}
		return ms.StoreMessage(row)
	}); err != nil || attempts != 2 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(attempts, err)
	}
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE chat_jid=? AND message_id='late'`, pn.String()).Scan(&durable); err != nil || durable != 1 {
		t.Fatal("retry lost receipt", durable, err)
	}
}

type receiptAliasGate struct {
	*mockLIDStore
	entered chan struct{}
	release chan struct{}
}

func (g *receiptAliasGate) GetLIDForPN(ctx context.Context, _ types.JID) (types.JID, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
		return types.NewJID("100000000000001", types.HiddenUserServer), nil
	case <-ctx.Done():
		return types.EmptyJID, ctx.Err()
	}
}

func TestPendingReceiptsUnrelatedGroupNeedsNoSessionLookup(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	gate := &receiptAliasGate{mockLIDStore: &mockLIDStore{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	b.currentClient().Store.LIDs = gate
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	group := types.NewJID("120363000000000001", types.GroupServer)
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: group, Sender: pn}, MessageIDs: []string{"same"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	_, alias, _, err := b.preparePendingEdit(ctx, "same", pn.String(), b.currentClient().Store.ID.String(), true)
	if err != nil || alias != "" || len(gate.entered) != 0 || pendingReceiptCount(t, ms) != 1 {
		t.Fatal("unrelated group caused a session lookup", alias, err)
	}
}

func TestPendingReceiptsAliasLookupDrainsArchiveAndCancels(t *testing.T) {
	for _, cancelLookup := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelLookup), func(t *testing.T) {
			ms, b, dispatch := pendingReceiptBridgeFixture(t)
			gate := &receiptAliasGate{mockLIDStore: &mockLIDStore{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
			b.currentClient().Store.LIDs = gate // before the receipt worker starts
			pn := types.NewJID("5511999999999", types.DefaultUserServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			at := time.Now()
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"blocked"}, Type: types.ReceiptTypeRead, Timestamp: at})
			if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
				t.Fatal(err)
			}
			ms.db.SetMaxOpenConns(1) // any surviving archive cursor blocks our probe
			ctx, cancel := context.WithCancel(b.ctx)
			type result struct {
				alias    string
				prepared *pendingEditPreparation
				err      error
			}
			done := make(chan result, 1)
			writer := make(chan error, 1)
			readDone, writeDone, writeStarted := false, false, false
			defer func() {
				cancel()
				if !readDone {
					<-done
				}
				if writeStarted && !writeDone {
					<-writer
				}
			}()
			go func() {
				_, alias, prepared, err := b.preparePendingEdit(ctx, "blocked", pn.String(), b.currentClient().Store.ID.String(), true)
				done <- result{alias, prepared, err}
			}()
			select {
			case <-gate.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("SDK lookup never started")
			}
			writeStarted = true
			go func() {
				writer <- ms.BatchContext(ctx, func(batch *messageBatch) error {
					return batch.StoreMessage(storedMessage{ID: "probe", ChatJID: pn.String(), Content: "Bob", Timestamp: at})
				})
			}()
			select {
			case err := <-writer:
				writeDone = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("SDK lookup holds archive connection or writer")
			}
			if cancelLookup {
				cancel()
			} else {
				close(gate.release)
			}
			var got result
			select {
			case got = <-done:
				readDone = true
			case <-time.After(10 * time.Second):
				t.Fatal("lookup failed to release/cancel")
			}
			if cancelLookup {
				if !errors.Is(got.err, context.Canceled) || pendingReceiptCount(t, ms) != 1 {
					t.Fatal(got.err)
				}
			} else {
				if got.err != nil || got.alias != lid.String() {
					t.Fatal(got)
				}
				if err := ms.StoreMessage(storedMessage{ID: "blocked", ChatJID: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: true, EditChatAlias: got.alias, EditPreparation: got.prepared}); err != nil {
					t.Fatal(err)
				}
				if pendingReceiptCount(t, ms) != 0 {
					t.Fatal("released lookup did not consume twin")
				}
			}
		})
	}
}

func TestPendingReceiptsStartupAliasCollisionBounds(t *testing.T) {
	for _, caps := range []bool{false, true} {
		t.Run(fmt.Sprintf("caps=%v", caps), func(t *testing.T) {
			ms, _, dispatch := pendingReceiptBridgeFixture(t)
			pn := types.NewJID("5511999999999", types.DefaultUserServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			first := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			now := first
			ms.editNow = func() time.Time { return now }
			ids := []string{"same"}
			if caps {
				ids = make([]string, pendingReceiptReaderCap)
				for i := range ids {
					ids[i] = fmt.Sprintf("lid-%03d", i)
				}
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: pn}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: first.Add(time.Minute)})
			now = first.Add(time.Hour)
			if caps {
				for i := range ids {
					ids[i] = fmt.Sprintf("phone-%03d", i)
				}
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: pn, Sender: pn}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: first})
			path := receiptSessionMapping(t)
			for range 2 {
				if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
					t.Fatal(err)
				}
			}
			if caps {
				if n := pendingReceiptCount(t, ms); n != pendingReceiptReaderCap {
					t.Fatal("merged reader cap", n)
				}
				var newer int
				if err := ms.db.QueryRow(`SELECT COUNT(*) FROM pending_receipts WHERE message_id LIKE 'phone-%'`).Scan(&newer); err != nil || newer != pendingReceiptReaderCap {
					t.Fatal("merge eviction ignored first arrival", newer, err)
				}
			} else {
				var chat, stamp string
				var arrived, expires int64
				if err := ms.db.QueryRow(`SELECT chat_jid,CAST(at AS TEXT),arrived_ms,expires_ms FROM pending_receipts`).Scan(&chat, &stamp, &arrived, &expires); err != nil {
					t.Fatal(err)
				}
				if pendingReceiptCount(t, ms) != 1 || chat != pn.String() || stamp != dbTime(first.Add(time.Minute)) || arrived != first.UnixMilli() || expires != first.Add(pendingReceiptTTL).UnixMilli() {
					t.Fatal(chat, stamp, arrived, expires)
				}
			}
			var orphan, archived int
			if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM pending_receipts WHERE chat_jid=?),(SELECT COUNT(*) FROM chats)`, lid.String()).Scan(&orphan, &archived); err != nil || orphan != 0 || archived != 0 {
				t.Fatal(orphan, archived, err)
			}
		})
	}
}

func TestPendingReceiptsStartupAliasDoesNotPromoteOutsideCanonicalWrite(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own=%v", own), func(t *testing.T) {
			ms, _, dispatch := pendingReceiptBridgeFixture(t)
			pn := types.NewJID("5511999999999", types.DefaultUserServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			at := time.Now()
			if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
				t.Fatal(err)
			}
			row := storedMessage{ID: "existing", ChatJID: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: own}
			if err := ms.StoreMessage(row); err != nil {
				t.Fatal(err)
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"existing"}, Type: types.ReceiptTypeRead, Timestamp: at})
			if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(receiptSessionMapping(t), testLogger()); err != nil {
				t.Fatal(err)
			}
			var durable int
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 {
				t.Fatal("startup promoted", durable, err)
			}
			want := 0
			if own {
				want = 1
			}
			if pendingReceiptCount(t, ms) != want {
				t.Fatal("startup inbound not discarded")
			}
			if err := ms.StoreMessage(row); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != want || pendingReceiptCount(t, ms) != 0 {
				t.Fatal(durable, err)
			}
		})
	}
}

func TestPendingReceiptsStartupAliasEnforcesKeyBytes(t *testing.T) {
	ms, _, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	id := strings.Repeat("a", pendingReceiptKeyBytes-len(lid.String())-len(pn.User)-len(pn.Server)-len("read"))
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: pn}, MessageIDs: []string{id}, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	if pendingReceiptCount(t, ms) != 1 {
		t.Fatal("at-limit fixture missing")
	}
	if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(receiptSessionMapping(t), testLogger()); err != nil {
		t.Fatal(err)
	}
	if pendingReceiptCount(t, ms) != 0 {
		t.Fatal("longer canonical identity exceeded byte limit")
	}
}
