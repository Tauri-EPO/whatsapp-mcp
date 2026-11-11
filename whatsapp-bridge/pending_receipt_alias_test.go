package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestPendingReceiptsReviewArchivedTwin(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, own := range []bool{false, true} {
			t.Run(fmt.Sprintf("restart=%v/own=%v", restart, own), func(t *testing.T) {
				ms, b, dispatch := pendingReceiptBridgeFixture(t)
				pn := types.NewJID("5511999999999", types.DefaultUserServer)
				lid := types.NewJID("100000000000001", types.HiddenUserServer)
				at := time.Now()
				chat := pn
				if !restart {
					chat = lid
				}
				if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "archived", ChatJID: chat.String(), Content: "Alice", Timestamp: at, IsFromMe: own}); err != nil {
					t.Fatal(err)
				}
				mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
				receiptChat := lid
				if !restart {
					mapping.lidByPN = map[types.JID]types.JID{pn: lid}
					mapping.pnByLID = map[types.JID]types.JID{lid: pn}
					receiptChat = pn
				}
				dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: receiptChat, Sender: receiptChat}, MessageIDs: []string{"archived"}, Type: types.ReceiptTypeRead, Timestamp: at})
				if restart {
					if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(receiptSessionMapping(t), testLogger()); err != nil {
						t.Fatal(err)
					}
				}
				var durable int
				if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE message_id='archived' AND chat_jid=?`, chat.String()).Scan(&durable); err != nil {
					t.Fatal(err)
				}
				want := 0
				if own {
					want = 1
				}
				if durable != want || pendingReceiptCount(t, ms) != 0 {
					t.Fatalf("durable=%d pending=%d want=%d", durable, pendingReceiptCount(t, ms), want)
				}
			})
		}
	}
}

func TestPendingReceiptsArchivedTwinWithWireHint(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own=%v", own), func(t *testing.T) {
			ms, _, dispatch := pendingReceiptBridgeFixture(t)
			pn := types.NewJID("5511999999999", types.DefaultUserServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			at := time.Now()
			if err := ms.StoreChat(lid.String(), "Alice", at); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "hint", ChatJID: lid.String(), Content: "Alice", Timestamp: at, IsFromMe: own}); err != nil {
				t.Fatal(err)
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: pn, Sender: pn, SenderAlt: lid}, MessageIDs: []string{"hint"}, Type: types.ReceiptTypeRead, Timestamp: at})
			var durable int
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE chat_jid=? AND message_id='hint'`, lid.String()).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			want := 0
			if own {
				want = 1
			}
			if durable != want || pendingReceiptCount(t, ms) != 0 {
				t.Fatal(durable, pendingReceiptCount(t, ms))
			}
		})
	}
}

func TestPendingReceiptsReviewOutbound(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"sent"}, Type: types.ReceiptTypeRead, Timestamp: at})
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: lid}
	mapping.pnByLID = map[types.JID]types.JID{lid: pn}
	b.Connected = func() bool { return true }
	sends := 0
	b.sendMessage = func(_ context.Context, to types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
		sends++
		if to != lid {
			t.Errorf("wire destination %s", to)
		}
		return whatsmeow.SendResponse{ID: "sent", Timestamp: at}, nil
	}
	b.Send = b.sendBackend()
	response := httptest.NewRecorder()
	b.handleSend(nil)(response, httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{"recipient":"5511999999999@s.whatsapp.net","message":"Alice"}`)))
	if response.Code != http.StatusOK || sends != 1 || !strings.Contains(response.Body.String(), `"message_id":"sent"`) {
		t.Fatal(response.Code, response.Body.String(), sends)
	}
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE message_id='sent' AND chat_jid=?`, pn.String()).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(durable, pendingReceiptCount(t, ms), err)
	}
}

func TestPendingReceiptsOutboundNonOriginalStanzas(t *testing.T) {
	for _, kind := range []string{"react", "edit", "delete"} {
		for _, early := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/early=%v", kind, early), func(t *testing.T) {
				ms, b, dispatch := pendingReceiptBridgeFixture(t)
				chat := types.NewJID("120363000000000001", types.GroupServer)
				peer := types.NewJID("5511999999999", types.DefaultUserServer)
				at := time.Now()
				if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "target", ChatJID: chat.String(), Sender: b.currentClient().Store.ID.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
					t.Fatal(err)
				}
				dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: peer}, MessageIDs: []string{"real"}, Type: types.ReceiptTypeRead, Timestamp: at})
				stanzaID := "stanza-" + kind
				receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: peer}, MessageIDs: []string{stanzaID}, Type: types.ReceiptTypeRead, Timestamp: at}
				b.Connected = func() bool { return true }
				b.RESTBind = defaultBridgeBind
				sends := 0
				b.sendMessage = func(_ context.Context, to types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
					sends++
					if to != chat {
						t.Errorf("non-original destination %s", to)
					}
					if early {
						dispatch(receipt)
					}
					return whatsmeow.SendResponse{ID: stanzaID, Timestamp: at}, nil
				}
				body := `{"chat_jid":"` + chat.String() + `","message_id":"target","text":"Bob","for_everyone":true}`
				if kind == "react" {
					body = `{"recipient":"` + chat.String() + `","message_id":"target","emoji":"👍","from_me":true}`
				}
				const token = "fake-receipts-bridge-token"
				r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/"+kind, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+token)
				response := httptest.NewRecorder()
				b.newRESTMux(8080, token).ServeHTTP(response, r)
				if response.Code != http.StatusOK || sends != 1 {
					t.Fatal(response.Code, response.Body.String(), sends)
				}
				if !early {
					dispatch(receipt)
				}
				var pending, durable int
				if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM pending_receipts WHERE message_id=?),(SELECT COUNT(*) FROM message_receipts WHERE message_id=?)`, stanzaID, stanzaID).Scan(&pending, &durable); err != nil {
					t.Fatal(err)
				}
				if pending != 0 || durable != 0 || pendingReceiptCount(t, ms) != 1 {
					t.Fatal("own stanza parked", pending, durable, pendingReceiptCount(t, ms))
				}
			})
		}
	}
}

type receiptChurnGate struct {
	*mockLIDStore
	entered chan struct{}
	release chan struct{}
}

func (g *receiptChurnGate) GetLIDForPN(ctx context.Context, _ types.JID) (types.JID, error) {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return types.EmptyJID, ctx.Err()
	}
	select {
	case <-g.release:
		return types.NewJID("100000000000001", types.HiddenUserServer), nil
	case <-ctx.Done():
		return types.EmptyJID, ctx.Err()
	}
}

func TestPendingReceiptsConcurrentLiveAndHistoryOriginals(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(fmt.Sprintf("history=%v", history), func(t *testing.T) {
			ms, b, dispatch := pendingReceiptBridgeFixture(t)
			chat := types.NewJID("120363000000000001", types.GroupServer)
			lid := types.NewJID("100000000000001", types.HiddenUserServer)
			at := time.Now()
			edit := extractMessage(pendingEditMessage("H0", "edited Alice", at.Add(time.Second), true), at, "edit").edit
			if err := ms.ApplyMessageEdit(chat.String(), lid.String(), true, edit, at); err != nil {
				t.Fatal(err)
			}
			gate := &receiptChurnGate{mockLIDStore: &mockLIDStore{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
			b.currentClient().Store.LIDs = gate
			done := make(chan struct{})
			finished := false
			defer func() {
				close(gate.release)
				if !finished {
					<-done
				}
			}()
			go func() {
				defer close(done)
				if history {
					h := largeHistoryFixture(2)
					h.Data.Conversations[0].ID = proto.String(chat.String())
					for _, row := range h.Data.Conversations[0].Messages {
						row.Message.Key.FromMe = proto.Bool(true)
					}
					b.handleHistorySync(h)
				} else {
					original := buildTextMessage(chat, *b.currentClient().Store.ID, types.EmptyJID, types.EmptyJID, true, "Alice")
					original.Info.ID = "H0"
					b.handleEvent(original, nil)
				}
			}()
			churn := 0
		wait:
			for {
				select {
				case <-done:
					finished = true
					break wait
				case <-gate.entered:
					if churn >= 3 {
						t.Fatal("unbounded metadata preparation")
					}
					target := chat
					if churn > 0 {
						target = types.NewJID(fmt.Sprintf("120363000000000%03d", churn+1), types.GroupServer)
					}
					dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: target, Sender: types.NewJID("5511999999999", types.DefaultUserServer)}, MessageIDs: []string{"H0"}, Type: types.ReceiptTypeRead, Timestamp: at})
					churn++
					gate.release <- struct{}{}
				case <-time.After(15 * time.Second):
					t.Fatal("original did not finish")
				}
			}
			var originals, receipts int
			if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE chat_jid=?),(SELECT COUNT(*) FROM message_receipts WHERE chat_jid=? AND message_id='H0')`, chat.String(), chat.String()).Scan(&originals, &receipts); err != nil {
				t.Fatal(err)
			}
			want := 1
			if history {
				want = 2
			}
			if churn != 1 || originals != want || receipts != 1 {
				t.Fatal("receipt churn rejected original", churn, originals, receipts)
			}
		})
	}
}

func TestPendingReceiptsReviewChurnNeverRejectsOriginal(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	chat := types.NewJID("120363000000000001", types.GroupServer)
	peer := types.NewJID("5511999999999", types.DefaultUserServer)
	at := time.Now()
	if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	stored := b.storeLive("message", "churn", chat.String(), func() error {
		return retryPendingMetadataPreparation(func() error {
			if _, err := ms.db.Exec(`DELETE FROM pending_receipts WHERE message_id='churn'`); err != nil {
				return err
			}
			_, alias, prepared, err := b.preparePendingMetadata(b.ctx, "churn", chat.String(), b.currentClient().Store.ID.String(), true)
			if err != nil {
				return err
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: peer}, MessageIDs: []string{"churn"}, Type: types.ReceiptTypeRead, Timestamp: at})
			return ms.StoreMessage(storedMessage{ID: "churn", ChatJID: chat.String(), Content: "Alice", Timestamp: at, IsFromMe: true, PendingChatAlias: alias, PendingPreparation: prepared})
		})
	})
	var originals int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='churn'`).Scan(&originals); err != nil || !stored || originals != 1 {
		t.Fatal("receipt churn lost original", stored, originals, err)
	}
}

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

func TestPendingReceiptsLateTwinSettlesAfterOriginalCommit(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: lid}
	at := time.Now()
	_, alias, prepared, err := b.preparePendingMetadata(b.ctx, "late", pn.String(), b.currentClient().Store.ID.String(), true)
	if err != nil || alias != "" {
		t.Fatal(alias, err)
	}
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"late"}, Type: types.ReceiptTypeRead, Timestamp: at})
	if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "late", ChatJID: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: true, PendingChatAlias: alias, PendingPreparation: prepared}); err != nil {
		t.Fatal(err)
	}
	if pendingReceiptCount(t, ms) != 1 {
		t.Fatal("late twin was not retained before reconciliation")
	}
	b.reconcilePendingReceiptOriginals(b.ctx, pn.String(), []string{"late"})
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts WHERE chat_jid=? AND message_id='late'`, pn.String()).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(durable, err)
	}
}

func TestPendingMetadataSharesChatAliasLookup(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"shared"}, Type: types.ReceiptTypeRead, Timestamp: at})
	edit := extractMessage(pendingEditMessage("shared", "edited Alice", at.Add(time.Second), true), at, "edit").edit
	if err := ms.ApplyMessageEdit(lid.String(), lid.String(), false, edit, at); err != nil {
		t.Fatal(err)
	}
	mapping := &historyTwinLIDs{mockLIDStore: mockLIDStore{lidByPN: map[types.JID]types.JID{pn: lid}}}
	b.currentClient().Store.LIDs = mapping
	author, alias, prepared, err := b.preparePendingMetadata(b.ctx, "shared", pn.String(), pn.String(), false)
	if err != nil || mapping.calls != 1 || alias != lid.String() || author != lid.String() {
		t.Fatal("duplicate chat lookup", mapping.calls, author, alias, err)
	}
	if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "shared", ChatJID: pn.String(), Sender: pn.String(), Content: "Alice", Timestamp: at, EditAuthorAlias: author, PendingChatAlias: alias, PendingPreparation: prepared}); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := ms.db.QueryRow(`SELECT content FROM messages WHERE id='shared' AND chat_jid=?`, pn.String()).Scan(&content); err != nil || content != "edited Alice" || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(content, err)
	}
}

type receiptAliasGate struct {
	*mockLIDStore
	entered chan struct{}
	release chan struct{}
}

func TestPendingReceiptsOutboundAliasCancellationKeepsOriginal(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"cancelled-alias"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
	gate := &receiptAliasGate{mockLIDStore: &mockLIDStore{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	b.currentClient().Store.LIDs = gate
	done := make(chan error, 1)
	finished := false
	defer func() {
		b.cancel()
		if !finished {
			<-done
		}
	}()
	go func() {
		_, err := b.persistOutbound(pn, sentMessage{ID: "cancelled-alias", Timestamp: time.Now()}, "Alice", outboundMedia{}, "")
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("lookup did not start")
	}
	b.cancel()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled lookup lost original")
	}
	var originals int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='cancelled-alias' AND chat_jid=? AND is_from_me=1`, pn.String()).Scan(&originals); err != nil || originals != 1 {
		t.Fatal(originals, err)
	}
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
	_, alias, _, err := b.preparePendingMetadata(ctx, "same", pn.String(), b.currentClient().Store.ID.String(), true)
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
				prepared *pendingMetadataPreparation
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
				_, alias, prepared, err := b.preparePendingMetadata(ctx, "blocked", pn.String(), b.currentClient().Store.ID.String(), true)
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
				if err := ms.StoreMessage(storedMessage{ID: "blocked", ChatJID: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: true, PendingChatAlias: got.alias, PendingPreparation: got.prepared}); err != nil {
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
			ms.pendingNow = func() time.Time { return now }
			ids := []string{"same"}
			if caps {
				ids = make([]string, pendingReceiptReaderCap)
				for i := range ids {
					ids[i] = fmt.Sprintf("lid-%03d", i)
				}
			}
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: pn}, MessageIDs: ids, Type: types.ReceiptTypeRead, Timestamp: first.Add(time.Minute)})
			now = first.Add(30 * time.Second)
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

func TestPendingReceiptsStartupSettlesAlreadyArchivedOriginal(t *testing.T) {
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
			want := 0
			if own {
				want = 1
			}
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != want || pendingReceiptCount(t, ms) != 0 {
				t.Fatal("startup did not settle known original", durable, err)
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

func TestPendingReceiptsStartupAfterOriginalChatMigration(t *testing.T) {
	for _, sourceLID := range []bool{false, true} {
		for _, own := range []bool{false, true} {
			t.Run(fmt.Sprintf("source-lid=%v/own=%v", sourceLID, own), func(t *testing.T) {
				ms, _, dispatch := pendingReceiptBridgeFixture(t)
				pn := types.NewJID("5511999999999", types.DefaultUserServer)
				lid := types.NewJID("100000000000001", types.HiddenUserServer)
				source, receiptChat := pn, lid
				if sourceLID {
					source, receiptChat = lid, pn
				}
				at := time.Now()
				dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: receiptChat, Sender: receiptChat}, MessageIDs: []string{"migrated"}, Type: types.ReceiptTypeRead, Timestamp: at})
				if err := ms.StoreChat(source.String(), "Alice", at); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "migrated", ChatJID: source.String(), Content: "Alice", Timestamp: at, IsFromMe: own}); err != nil {
					t.Fatal(err)
				}
				if pendingReceiptCount(t, ms) != 1 {
					t.Fatal("unmapped twin control missing")
				}
				path := receiptSessionMapping(t)
				for range 2 {
					if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
						t.Fatal(err)
					}
				}
				var originals, durable int
				if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE id='migrated' AND chat_jid=?),(SELECT COUNT(*) FROM message_receipts WHERE message_id='migrated' AND chat_jid=?)`, pn.String(), pn.String()).Scan(&originals, &durable); err != nil {
					t.Fatal(err)
				}
				want := 0
				if own {
					want = 1
				}
				if originals != 1 || durable != want || pendingReceiptCount(t, ms) != 0 {
					t.Fatal("startup order stranded receipts", originals, durable, pendingReceiptCount(t, ms))
				}
			})
		}
	}
}

func TestPendingReceiptsLateHistoryTwinSettlesWithoutRetry(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	writes := 0
	b.historyBatchWriter = func(write func(*messageBatch) error) error {
		writes++
		if writes == 1 {
			dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"H0"}, Type: types.ReceiptTypeRead, Timestamp: time.Now()})
			mapping.lidByPN = map[types.JID]types.JID{pn: lid}
			mapping.pnByLID = map[types.JID]types.JID{lid: pn}
		}
		return ms.BatchContext(b.ctx, write)
	}
	h := largeHistoryFixture(1)
	h.Data.Conversations[0].ID = proto.String(pn.String())
	h.Data.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(true)
	b.handleHistorySync(h)
	var originals, durable int
	if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE id='H0' AND chat_jid=?),(SELECT COUNT(*) FROM message_receipts WHERE message_id='H0' AND chat_jid=?)`, pn.String(), pn.String()).Scan(&originals, &durable); err != nil || writes != 1 || originals != 1 || durable != 1 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(writes, originals, durable, err)
	}
}

func TestPendingReceiptsLateReconcileFailureKeepsOriginal(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: lid, Sender: lid}, MessageIDs: []string{"committed"}, Type: types.ReceiptTypeRead, Timestamp: at})
	if err := ms.StoreChat(pn.String(), "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "committed", ChatJID: pn.String(), Content: "Alice", Timestamp: at, IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: lid}
	if _, err := ms.db.Exec(`CREATE TRIGGER refuse_late_receipt BEFORE INSERT ON message_receipts BEGIN SELECT RAISE(ROLLBACK,'refused'); END`); err != nil {
		t.Fatal(err)
	}
	b.reconcilePendingReceiptOriginals(b.ctx, pn.String(), []string{"committed"})
	var originals, durable int
	if err := ms.db.QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE id='committed'),(SELECT COUNT(*) FROM message_receipts)`).Scan(&originals, &durable); err != nil || originals != 1 || durable != 0 || pendingReceiptCount(t, ms) != 1 {
		t.Fatal("late metadata lost original", originals, durable, err)
	}
	if _, err := ms.db.Exec(`DROP TRIGGER refuse_late_receipt`); err != nil {
		t.Fatal(err)
	}
	b.reconcilePendingReceiptOriginals(b.ctx, pn.String(), []string{"committed"})
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 1 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal(durable, err)
	}
}

func TestPendingReceiptsCanonicalInboundDoesNotBorrowOwnTwin(t *testing.T) {
	ms, b, dispatch := pendingReceiptBridgeFixture(t)
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	at := time.Now()
	for _, chat := range []types.JID{pn, lid} {
		if err := ms.StoreChat(chat.String(), "Alice", at); err != nil {
			t.Fatal(err)
		}
		if err := ms.StoreMessage(storedMessage{ID: "collision", ChatJID: chat.String(), Content: "Alice", Timestamp: at, IsFromMe: chat == lid}); err != nil {
			t.Fatal(err)
		}
	}
	// A restored old pending alias must obey the canonical original's ownership.
	if _, err := ms.db.Exec(`INSERT INTO pending_receipts VALUES(?,'collision','5511999999999','s.whatsapp.net','read',?,?,?)`, lid.String(), dbTime(at), at.UnixMilli(), at.Add(pendingReceiptTTL).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	mapping := b.currentClient().Store.LIDs.(*mockLIDStore)
	mapping.lidByPN = map[types.JID]types.JID{pn: lid}
	mapping.pnByLID = map[types.JID]types.JID{lid: pn}
	b.reconcilePendingReceiptOriginals(b.ctx, pn.String(), []string{"collision"})
	dispatch(&events.Receipt{MessageSource: types.MessageSource{Chat: pn, Sender: pn}, MessageIDs: []string{"collision"}, Type: types.ReceiptTypeRead, Timestamp: at})
	var durable int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM message_receipts`).Scan(&durable); err != nil || durable != 0 || pendingReceiptCount(t, ms) != 0 {
		t.Fatal("inbound borrowed own twin", durable, err)
	}
}
