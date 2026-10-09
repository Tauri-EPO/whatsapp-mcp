package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func pendingEditMessage(id, text string, stamp time.Time, mentioned bool) *waE2E.Message {
	edit := incomingEdit(id, text, stamp.UnixMilli())
	if mentioned {
		edit.EditedMessage.Message.ProtocolMessage.EditedMessage = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{selfPhone.String()}},
		}}
	}
	return edit
}

func pendingCount(t *testing.T, ms *MessageStore) int {
	t.Helper()
	var count int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM pending_edits").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertPendingArchive(t *testing.T, ms *MessageStore, id, chat, text, mentions string, stamp int64) {
	t.Helper()
	var gotText, gotMentions string
	var gotStamp int64
	if err := ms.db.QueryRow("SELECT content,COALESCE(mentions,''),message_edit_timestamp FROM messages WHERE id=? AND chat_jid=?", id, chat).Scan(&gotText, &gotMentions, &gotStamp); err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages_fts JOIN messages ON messages_fts.rowid=messages.rowid
		WHERE messages_fts MATCH ? AND messages.id=? AND messages.chat_jid=?`, text, id, chat).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if gotText != text || gotMentions != mentions || gotStamp != stamp || hits != 1 {
		t.Fatalf("text=%q mentions=%q edit=%d FTS=%d", gotText, gotMentions, gotStamp, hits)
	}
}

func TestPendingEditLiveOriginalAndReplay(t *testing.T) {
	for _, mode := range []string{"delayed", "failed-then-retried"} {
		t.Run(mode, func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			edit := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			edit.Info.ID, edit.Message = "EARLY-EDIT", pendingEditMessage("LATE-ORIGINAL", "neweditword", stamp.Add(time.Minute), true)
			b.handleEvent(edit, nil)
			if pendingCount(t, ms) != 1 {
				t.Fatal("early edit not retained")
			}
			var rows, chats, fts int
			if err := ms.db.QueryRow("SELECT (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM chats),(SELECT COUNT(*) FROM messages_fts)").Scan(&rows, &chats, &fts); err != nil {
				t.Fatal(err)
			}
			if rows != 0 || chats != 0 || fts != 0 {
				t.Fatalf("orphan rows=%d chats=%d FTS=%d", rows, chats, fts)
			}
			original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
			original.Info.ID, original.Info.Timestamp = "LATE-ORIGINAL", stamp
			if mode == "failed-then-retried" {
				release := lock()
				b.storeRetryWait = func(time.Duration) bool { return true }
				b.handleEvent(original, nil)
				release()
				if pendingCount(t, ms) != 1 || b.metrics.storeFailures.Load() != 1 {
					t.Fatal("failed original consumed pending edit")
				}
			}
			b.handleEvent(original, nil)
			assertPendingArchive(t, ms, original.Info.ID, phonePN.String(), "neweditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
			b.handleEvent(original, nil)
			b.handleEvent(edit, nil)
			assertPendingArchive(t, ms, original.Info.ID, phonePN.String(), "neweditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
			if pendingCount(t, ms) != 0 {
				t.Fatal("consumed edit retained")
			}
		})
	}
}

func TestPendingEditHistoryAcrossPayloadsAndReplay(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	stamp := time.Unix(1772359200, 0)
	early := largeHistoryFixture(1)
	early.Data.Conversations[0].Messages[0].Message.Message = pendingEditMessage("H0", "historyeditword", stamp.Add(time.Minute), true)
	b.handleHistorySync(early)
	var chats int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, ms) != 1 || chats != 1 {
		t.Fatalf("pending=%d chats=%d", pendingCount(t, ms), chats)
	}
	original := largeHistoryFixture(1)
	b.handleHistorySync(original)
	assertPendingArchive(t, ms, "H0", phonePN.String(), "historyeditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
	b.handleHistorySync(original)
	b.handleHistorySync(early)
	assertPendingArchive(t, ms, "H0", phonePN.String(), "historyeditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
	if pendingCount(t, ms) != 0 {
		t.Fatal("history edit replay queued")
	}
}

func TestPendingEditIdentityDenialsAndVerifiedAlias(t *testing.T) {
	for _, mode := range []string{"other-chat", "other-author", "other-namespace", "other-ownership", "verified-alias", "unverified-alias"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			mapping := &mockLIDStore{}
			if mode == "verified-alias" {
				mapping.lidByPN = map[types.JID]types.JID{phonePN: phoneLID}
			}
			b := testBridge(t, newTestClient(mapping), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			chat, author, own := phonePN.String(), phonePN.String(), false
			switch mode {
			case "other-chat":
				chat = selfPhone.String()
			case "other-author":
				author = selfPhone.String()
			case "other-namespace":
				author = types.NewJID(phonePN.User, types.HiddenUserServer).String()
			case "other-ownership":
				own = true
			case "verified-alias", "unverified-alias":
				author = phoneLID.String()
			}
			edit := pendingEditMessage("IDENTITY-TARGET", "authorizededitword", stamp.Add(time.Minute), true)
			if err := ms.ApplyMessageEdit(chat, author, own, edit.EditedMessage.Message.ProtocolMessage, stamp); err != nil {
				t.Fatal(err)
			}
			original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
			original.Info.ID, original.Info.Timestamp = "IDENTITY-TARGET", stamp
			b.handleEvent(original, nil)
			text, mentions, edited := "oldword", "", int64(0)
			if mode == "verified-alias" {
				text, mentions, edited = "authorizededitword", selfPhone.User, stamp.Add(time.Minute).UnixMilli()
			}
			assertPendingArchive(t, ms, original.Info.ID, phonePN.String(), text, mentions, edited)
		})
	}
}

func TestPendingEditNewestAndExpiryDoesNotUseProtocolTime(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	now := time.Unix(1900000000, 0)
	ms.editNow = func() time.Time { return now }
	stamp := time.Unix(1772359200, 0)
	for _, age := range []time.Duration{time.Minute, 3 * time.Minute, 2 * time.Minute, 3 * time.Minute} {
		edit := pendingEditMessage("ORDER-TARGET", fmt.Sprintf("version%dword", int(age/time.Minute)), stamp.Add(age), age == 3*time.Minute)
		if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit.EditedMessage.Message.ProtocolMessage, stamp); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "ORDER-TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp}); err != nil {
		t.Fatal(err)
	}
	assertPendingArchive(t, ms, "ORDER-TARGET", phonePN.String(), "version3word", selfPhone.User, stamp.Add(3*time.Minute).UnixMilli())
	for _, id := range []string{"EXPIRED", "ABANDONED"} {
		edit := incomingEdit(id, "expiredword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
		if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit, stamp); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(pendingEditTTL)
	if err := ms.StoreMessage(storedMessage{ID: "EXPIRED", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "originalword", Timestamp: stamp}); err != nil {
		t.Fatal(err)
	}
	assertPendingArchive(t, ms, "EXPIRED", phonePN.String(), "originalword", "", 0)
	if pendingCount(t, ms) != 0 {
		t.Fatal("expired abandoned edit not pruned")
	}
}

func TestPendingEditCapsAndReplayDoesNotExtendTTL(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	now := time.Unix(1900000000, 0)
	firstExpiry := now.Add(pendingEditTTL).UnixMilli()
	ms.editNow = func() time.Time { return now }
	stamp := time.Unix(1772359200, 0)
	senders := []string{phonePN.String(), selfPhone.String(), "12025550100@s.whatsapp.net", "12025550101@s.whatsapp.net", "15551234567@s.whatsapp.net"}
	if err := ms.Batch(func(batch *messageBatch) error {
		chats := []string{phonePN.String(), "120363000000000001@g.us", "120363000000000002@g.us",
			"120363000000000003@g.us", "120363000000000004@g.us", "120363000000000009@g.us", "1@g.us", "3@g.us", "123@g.us"}
		for index := range pendingEditGlobalCap + 1 {
			chat := chats[index/pendingEditChatCap]
			edit := incomingEdit(fmt.Sprintf("CAP-%d", index), "capword", stamp.UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := batch.ApplyMessageEdit(chat, senders[index%len(senders)], false, edit, stamp); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, ms) != pendingEditGlobalCap {
		t.Fatalf("global count=%d", pendingCount(t, ms))
	}
	var maximum int
	if err := ms.db.QueryRow("SELECT MAX(n) FROM (SELECT COUNT(*) n FROM pending_edits GROUP BY chat_jid)").Scan(&maximum); err != nil {
		t.Fatal(err)
	}
	if maximum > pendingEditChatCap {
		t.Fatalf("per-chat max=%d", maximum)
	}
	for index := range pendingEditChatCap + 1 {
		edit := incomingEdit(fmt.Sprintf("CHAT-%d", index), "capword", stamp.UnixMilli()).EditedMessage.Message.ProtocolMessage
		if err := ms.ApplyMessageEdit(phonePN.String(), senders[index%len(senders)], false, edit, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var chatCount int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM pending_edits WHERE chat_jid=?", phonePN.String()).Scan(&chatCount); err != nil {
		t.Fatal(err)
	}
	if chatCount != pendingEditChatCap || pendingCount(t, ms) > pendingEditGlobalCap {
		t.Fatalf("chat=%d global=%d", chatCount, pendingCount(t, ms))
	}
	now = now.Add(pendingEditTTL - time.Minute)
	replay := incomingEdit(fmt.Sprintf("CHAT-%d", pendingEditChatCap), "newerword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(phonePN.String(), senders[pendingEditChatCap%len(senders)], false, replay, stamp); err != nil {
		t.Fatal(err)
	}
	var retainedExpiry int64
	if err := ms.db.QueryRow("SELECT expires_ms FROM pending_edits WHERE target_id=?", fmt.Sprintf("CHAT-%d", pendingEditChatCap)).Scan(&retainedExpiry); err != nil {
		t.Fatal(err)
	}
	if retainedExpiry != firstExpiry {
		t.Fatalf("newer version refreshed TTL: got %d want %d", retainedExpiry, firstExpiry)
	}
	now = now.Add(time.Minute)
	if err := ms.ApplyMessageEdit(phonePN.String(), senders[pendingEditChatCap%len(senders)], false, replay, stamp); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, ms) != 1 {
		t.Fatal("expiry was refreshed by versions/replay")
	}
}

func TestPendingEditConsumptionRollsBackAndShutdownArchivesLive(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	stamp := time.Unix(1772359200, 0)
	edit := incomingEdit("ROLLBACK-TARGET", "atomiceditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec(`CREATE TRIGGER fail_pending_consumption BEFORE DELETE ON pending_edits BEGIN SELECT RAISE(ABORT,'controlled failure'); END`); err != nil {
		t.Fatal(err)
	}
	original := storedMessage{ID: "ROLLBACK-TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp}
	if err := ms.StoreMessage(original); err == nil {
		t.Fatal("consumption failure did not fail transaction")
	}
	var rows, fts int
	if err := ms.db.QueryRow("SELECT (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM messages_fts)").Scan(&rows, &fts); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || fts != 0 || pendingCount(t, ms) != 1 {
		t.Fatalf("partial rollback rows=%d FTS=%d pending=%d", rows, fts, pendingCount(t, ms))
	}
	if _, err := ms.db.Exec("DROP TRIGGER fail_pending_consumption"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(b.ctx)
	b.ctx = ctx
	cancel()
	event := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
	event.Info.ID = original.ID
	b.handleEvent(event, nil)
	if pendingCount(t, ms) != 0 {
		t.Fatal("shutdown failed to consume a matching edit with the live original")
	}
	assertPendingArchive(t, ms, original.ID, phonePN.String(), "atomiceditword", "", stamp.Add(time.Minute).UnixMilli())
	if err := ms.StoreMessage(original); err != nil {
		t.Fatal(err)
	}
	assertPendingArchive(t, ms, original.ID, phonePN.String(), "atomiceditword", "", stamp.Add(time.Minute).UnixMilli())
}

func TestPendingEditRejectsOversizedAndUnattributedAuthors(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	for _, mode := range []string{"oversized", "group-author", "bare-author"} {
		sender, text := phonePN.String(), "editword"
		switch mode {
		case "oversized":
			text = strings.Repeat("x", pendingEditMaxBytes+1)
		case "group-author":
			sender = "120363000000000001@g.us"
		case "bare-author":
			sender = phonePN.User
		}
		edit := incomingEdit(mode, text, stamp.UnixMilli()).EditedMessage.Message.ProtocolMessage
		if err := ms.ApplyMessageEdit(phonePN.String(), sender, false, edit, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if pendingCount(t, ms) != 0 {
		t.Fatal("unbounded/unattributed edit queued")
	}
}

func TestPendingEditDurableAcrossStoreReopen(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	edit := incomingEdit("DURABLE-TARGET", "durableeditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.StoreMessage(storedMessage{ID: "DURABLE-TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp}); err != nil {
		t.Fatal(err)
	}
	assertPendingArchive(t, reopened, "DURABLE-TARGET", phonePN.String(), "durableeditword", "", stamp.Add(time.Minute).UnixMilli())
}

func TestPendingEditConsumptionBusyReleaseExhaustAndCancel(t *testing.T) {
	for _, mode := range []string{"release", "exhaust", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			stamp := time.Unix(1772359200, 0)
			edit := incomingEdit("BUSY-ORIGINAL", "busyeditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
				t.Fatal(err)
			}
			release := lock()
			defer release()
			waits := 0
			b.storeRetryWait = func(time.Duration) bool {
				waits++
				if mode == "release" {
					release()
				}
				return mode != "shutdown"
			}
			ok := b.storeLive("original", "BUSY-ORIGINAL", phonePN.String(), func() error {
				return ms.BatchContext(b.ctx, func(batch *messageBatch) error {
					return batch.StoreMessage(storedMessage{ID: "BUSY-ORIGINAL", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp})
				})
			})
			release()
			if mode == "release" {
				if !ok || waits != 1 || pendingCount(t, ms) != 0 || len(errorLines(rec.String())) != 0 {
					t.Fatalf("ok=%v waits=%d pending=%d log=%s", ok, waits, pendingCount(t, ms), rec.String())
				}
				assertPendingArchive(t, ms, "BUSY-ORIGINAL", phonePN.String(), "busyeditword", "", stamp.Add(time.Minute).UnixMilli())
			} else {
				wantWaits := 2
				if mode == "shutdown" {
					wantWaits = 1
				}
				var rows int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if ok || waits != wantWaits || rows != 0 || pendingCount(t, ms) != 1 || b.metrics.storeFailures.Load() != 1 || len(errorLines(rec.String())) != 1 {
					t.Fatalf("ok=%v waits=%d rows=%d pending=%d failures=%d log=%s", ok, waits, rows, pendingCount(t, ms), b.metrics.storeFailures.Load(), rec.String())
				}
			}
			if strings.Contains(rec.String(), "busyeditword") {
				t.Fatal("pending edit leaked in diagnostic")
			}
		})
	}
}

func TestPendingEditCancellationDuringConsumptionRollsBack(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	edit := incomingEdit("CANCEL-TARGET", "cancellededitword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(phonePN.String(), phonePN.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ms.editNow = func() time.Time { cancel(); return time.Now() }
	err := ms.BatchContext(ctx, func(batch *messageBatch) error {
		return batch.StoreMessage(storedMessage{ID: "CANCEL-TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp})
	})
	if err == nil {
		t.Fatal("cancelled writer committed")
	}
	var rows, fts int
	if err := ms.db.QueryRow("SELECT (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM messages_fts)").Scan(&rows, &fts); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || fts != 0 || pendingCount(t, ms) != 1 {
		t.Fatalf("cancel partial rows=%d FTS=%d pending=%d", rows, fts, pendingCount(t, ms))
	}
}

func TestPendingEditAliasSessionStallLeavesWriterFreeAndCancels(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(session, sessionPoolConns)
			t.Cleanup(func() { _ = session.Close() })
			if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
				t.Fatal(err)
			}
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(container.LIDMap), ms, rec)
			stamp := time.Unix(1772359200, 0)
			edit := incomingEdit("H0", "aliaseditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(phonePN.String(), phoneLID.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			var held []*sql.Conn
			for range sessionPoolConns {
				conn, err := session.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, conn)
				t.Cleanup(func() { _ = conn.Close() })
			}
			done := make(chan struct{})
			go func() { defer close(done); b.handleHistorySync(largeHistoryFixture(1)) }()
			deadline := time.After(2 * time.Second)
			for session.Stats().WaitCount == 0 {
				select {
				case <-deadline:
					t.Fatal("pending alias lookup never reached session pool")
				default:
					runtime.Gosched()
				}
			}
			live := make(chan error, 1)
			go func() {
				live <- ms.StoreMessage(storedMessage{ID: "ALIAS-PROBE", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "writerprobeword", Timestamp: stamp})
			}()
			select {
			case err := <-live:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("pending alias lookup holds archive writer")
			}
			if shutdown {
				b.cancel()
			} else {
				for _, conn := range held {
					_ = conn.Close()
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("pending lookup failed to release/cancel")
			}
			if shutdown {
				var rows int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='H0'").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != 0 || pendingCount(t, ms) != 1 {
					t.Fatalf("cancel rows=%d pending=%d", rows, pendingCount(t, ms))
				}
			} else {
				assertPendingArchive(t, ms, "H0", phonePN.String(), "aliaseditword", "", stamp.Add(time.Minute).UnixMilli())
			}
			if b.metrics.storeFailures.Load() != 0 || len(errorLines(rec.String())) != 0 {
				t.Fatalf("alias read flooded failures: %s", rec.String())
			}
		})
	}
}

func TestPendingEditLIDChatCanonicalizationLiveHistoryAndStartup(t *testing.T) {
	for _, mode := range []string{"live-PN", "live-LID-hint", "history", "startup"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			mapping := &mockLIDStore{}
			b := testBridge(t, newTestClient(mapping), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			early := buildTextMessage(phoneLID, phoneLID, types.EmptyJID, types.EmptyJID, false, "")
			early.Info.ID, early.Message = "UNRESOLVED-EDIT", pendingEditMessage("H0", "canonicaleditword", stamp.Add(time.Minute), true)
			b.handleEvent(early, nil)
			mapping.lidByPN = map[types.JID]types.JID{phonePN: phoneLID}
			mapping.pnByLID = map[types.JID]types.JID{phoneLID: phonePN}
			if mode == "startup" {
				path := filepath.Join(t.TempDir(), "whatsapp.db")
				session, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				boundPool(session, sessionPoolConns)
				t.Cleanup(func() { _ = session.Close() })
				if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
					t.Fatal(err)
				}
				if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
					t.Fatal(err)
				}
				var chat string
				var chats int
				if err := ms.db.QueryRow("SELECT chat_jid FROM pending_edits").Scan(&chat); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil {
					t.Fatal(err)
				}
				if chat != phonePN.String() || chats != 0 {
					t.Fatalf("canonical pending chat=%s orphan chats=%d", chat, chats)
				}
			}
			if mode == "history" {
				b.handleHistorySync(largeHistoryFixture(1))
			} else {
				original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
				if mode == "live-LID-hint" {
					original = buildTextMessage(phoneLID, phoneLID, phonePN, types.EmptyJID, false, "oldword")
				}
				original.Info.ID, original.Info.Timestamp = "H0", stamp
				b.handleEvent(original, nil)
			}
			assertPendingArchive(t, ms, "H0", phonePN.String(), "canonicaleditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
			if pendingCount(t, ms) != 0 {
				t.Fatal("canonicalized edit not consumed")
			}
		})
	}
}

func TestPendingEditOnlyHistoryPreservesOtherConversationMetadata(t *testing.T) {
	for _, onlyEdits := range []bool{false, true} {
		t.Run(fmt.Sprintf("edit-only=%v", onlyEdits), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			fixture := largeHistoryFixture(1)
			conversation := fixture.Data.Conversations[0]
			conversation.ID = proto.String("120363000000000001@g.us")
			conversation.Name = proto.String("Alice")
			conversation.EphemeralExpiration = proto.Uint32(86400)
			conversation.EphemeralSettingTimestamp = proto.Int64(1772359200)
			conversation.Messages[0].Message.Message = &waE2E.Message{}
			conversation.Messages[0].Message.Participant = proto.String(phonePN.String())
			if onlyEdits {
				conversation.Messages[0].Message.Message = incomingEdit("ABSENT", "pendingword", 1772359260000)
			}
			b.handleHistorySync(fixture)
			var chats int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil {
				t.Fatal(err)
			}
			if onlyEdits && pendingCount(t, ms) != 1 {
				t.Fatal("edit-only history lost pending replacement")
			}
			var name string
			var expiry int
			if err := ms.db.QueryRow("SELECT name,ephemeral_expiration FROM chats").Scan(&name, &expiry); err != nil {
				t.Fatal(err)
			}
			if chats != 1 || name != "Alice" || expiry != 86400 {
				t.Fatalf("metadata chats=%d name=%s expiry=%d", chats, name, expiry)
			}
		})
	}
}

func TestPendingEditStartupAliasMergeKeepsNewestAndFirstExpiry(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	now := time.Now()
	first := now
	ms.editNow = func() time.Time { return now }
	for index, chat := range []string{phoneLID.String(), phonePN.String()} {
		edit := pendingEditMessage("MERGED-TARGET", fmt.Sprintf("merged%dword", index), stamp.Add(time.Duration(index+1)*time.Minute), index == 0)
		if err := ms.ApplyMessageEdit(chat, phonePN.String(), false, edit.EditedMessage.Message.ProtocolMessage, stamp); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	session, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	boundPool(session, sessionPoolConns)
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
		t.Fatal(err)
	}
	if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
		t.Fatal(err)
	}
	var text, mentions string
	var arrival, expiry, edited int64
	if err := ms.db.QueryRow("SELECT content,mentions,arrived_ms,expires_ms,edit_timestamp FROM pending_edits").Scan(&text, &mentions, &arrival, &expiry, &edited); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, ms) != 1 || text != "merged1word" || mentions != "" || arrival != first.UnixMilli() || expiry != first.Add(pendingEditTTL).UnixMilli() || edited != stamp.Add(2*time.Minute).UnixMilli() {
		t.Fatalf("count=%d text=%q mentions=%q arrived=%d expiry=%d edit=%d", pendingCount(t, ms), text, mentions, arrival, expiry, edited)
	}
}

func TestPendingEditOnlyHistoryUpdatesExistingChatSettingsOnSend(t *testing.T) {
	for _, expiration := range []uint32{86400, 0} {
		t.Run(fmt.Sprintf("expiration=%d", expiration), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.Connected = func() bool { return true }
			b.Send = b.sendBackend()
			chat := "120363000000000001@g.us"
			stamp := time.Unix(1772359200, 0)
			if err := ms.EnsureChat(chat, "Alice"); err != nil {
				t.Fatal(err)
			}
			if err := ms.UpdateChatEphemeralSettings(chat, 3600, stamp.Unix()); err != nil {
				t.Fatal(err)
			}
			fixture := largeHistoryFixture(1)
			conversation := fixture.Data.Conversations[0]
			conversation.ID = proto.String(chat)
			conversation.Messages[0].Message.Participant = proto.String(phonePN.String())
			conversation.Messages[0].Message.Message = incomingEdit("ABSENT", "pendingword", stamp.Add(time.Minute).UnixMilli())
			conversation.EphemeralExpiration = proto.Uint32(expiration)
			conversation.EphemeralSettingTimestamp = proto.Int64(stamp.Add(2 * time.Minute).Unix())
			b.handleHistorySync(fixture)
			// A sparse replay must not erase the authoritative timer just read.
			conversation.EphemeralSettingTimestamp = nil
			conversation.EphemeralExpiration = nil
			b.handleHistorySync(fixture)
			var observedExpiration uint32
			calls := 0
			b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
				calls++
				observedExpiration = message.GetExtendedTextMessage().GetContextInfo().GetExpiration()
				return whatsmeow.SendResponse{ID: "TIMER-SEND", Timestamp: stamp}, nil
			}
			body, err := json.Marshal(SendMessageRequest{Recipient: chat, Message: "outboundsendword"})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(response, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
			if response.Code != http.StatusOK || calls != 1 || observedExpiration != expiration {
				t.Fatalf("HTTP=%d calls=%d wire expiration=%d want=%d body=%s", response.Code, calls, observedExpiration, expiration, response.Body.String())
			}
		})
	}
}

func TestPendingEditEmptyChatCannotMatchDefaultAlias(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	edit := incomingEdit("EMPTY-CHAT-TARGET", "wrongchatword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit("", phonePN.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "EMPTY-CHAT-TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "originalword", Timestamp: stamp}); err != nil {
		t.Fatal(err)
	}
	assertPendingArchive(t, ms, "EMPTY-CHAT-TARGET", phonePN.String(), "originalword", "", 0)
}

func TestPendingEditSameIDInOtherGroupDoesNotReadBrokenSession(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, mode := range []string{"live", "history"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(session, sessionPoolConns)
			t.Cleanup(func() { _ = session.Close() })
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(container.LIDMap), ms, rec)
			stamp := time.Unix(1772359200, 0)
			firstChat := "120363000000000001@g.us"
			secondChat := types.NewJID("120363000000000002", types.GroupServer)
			edit := incomingEdit("COLLIDING-TARGET", "firstchateditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(firstChat, phoneLID.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			if mode == "live" {
				original := buildTextMessage(secondChat, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
				original.Info.ID, original.Info.Timestamp = "COLLIDING-TARGET", stamp
				b.handleEvent(original, nil)
			} else {
				fixture := largeHistoryFixture(1)
				conversation := fixture.Data.Conversations[0]
				conversation.ID = proto.String(secondChat.String())
				row := conversation.Messages[0].Message
				row.Key.ID = proto.String("COLLIDING-TARGET")
				row.Participant = proto.String(phonePN.String())
				row.Message = &waE2E.Message{Conversation: proto.String("originalword")}
				b.handleHistorySync(fixture)
			}
			assertPendingArchive(t, ms, "COLLIDING-TARGET", secondChat.String(), "originalword", "", 0)
			if pendingCount(t, ms) != 1 || b.metrics.storeFailures.Load() != 0 || len(errorLines(rec.String())) != 0 {
				t.Fatalf("pending=%d failures=%d log=%s", pendingCount(t, ms), b.metrics.storeFailures.Load(), rec.String())
			}
		})
	}
}

func TestPendingEditExpiredAliasCannotBlockOriginalOnBrokenSession(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms, _ := lockedProductionStore(t)
	session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(session, sessionPoolConns)
	t.Cleanup(func() { _ = session.Close() })
	container := sqlstore.NewWithDB(session, "sqlite", testLogger())
	b := testBridge(t, newTestClient(container.LIDMap), ms, testLogger())
	stamp := time.Unix(1772359200, 0)
	now := time.Now()
	ms.editNow = func() time.Time { return now }
	chat := types.NewJID("120363000000000001", types.GroupServer)
	edit := incomingEdit("EXPIRED-ALIAS", "expirededitword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(chat.String(), phoneLID.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	now = now.Add(pendingEditTTL)
	original := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
	original.Info.ID, original.Info.Timestamp = "EXPIRED-ALIAS", stamp
	b.handleEvent(original, nil)
	assertPendingArchive(t, ms, "EXPIRED-ALIAS", chat.String(), "originalword", "", 0)
	if pendingCount(t, ms) != 0 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("expired pending=%d failures=%d", pendingCount(t, ms), b.metrics.storeFailures.Load())
	}
}

func TestPendingEditUnverifiedOtherDMDoesNotBlockOriginal(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, mode := range []string{"live", "history"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(session, sessionPoolConns)
			t.Cleanup(func() { _ = session.Close() })
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			b := testBridge(t, newTestClient(container.LIDMap), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			edit := incomingEdit("DM-COLLISION", "otherdmword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(phoneLID.String(), phoneLID.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			if mode == "live" {
				original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
				original.Info.ID, original.Info.Timestamp = "DM-COLLISION", stamp
				b.handleEvent(original, nil)
			} else {
				fixture := largeHistoryFixture(1)
				conversation := fixture.Data.Conversations[0]
				conversation.ID = proto.String(phonePN.String())
				row := conversation.Messages[0].Message
				row.Key.ID = proto.String("DM-COLLISION")
				row.Message = &waE2E.Message{Conversation: proto.String("originalword")}
				b.handleHistorySync(fixture)
			}
			assertPendingArchive(t, ms, "DM-COLLISION", phonePN.String(), "originalword", "", 0)
			if pendingCount(t, ms) != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("pending=%d failures=%d", pendingCount(t, ms), b.metrics.storeFailures.Load())
			}
		})
	}
}

func TestPendingEditOnlyHistoryRefreshesExistingChatName(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	chat := "120363000000000001@g.us"
	if err := ms.EnsureChat(chat, "Group 120363000000000001"); err != nil {
		t.Fatal(err)
	}
	fixture := largeHistoryFixture(1)
	conversation := fixture.Data.Conversations[0]
	conversation.ID = proto.String(chat)
	conversation.Name = proto.String("Alice")
	conversation.Messages[0].Message.Participant = proto.String(phonePN.String())
	conversation.Messages[0].Message.Message = incomingEdit("ABSENT", "pendingword", 1772359260000)
	b.handleHistorySync(fixture)
	var name string
	if err := ms.db.QueryRow("SELECT name FROM chats WHERE jid=?", chat).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Alice" || pendingCount(t, ms) != 1 {
		t.Fatalf("name=%q pending=%d", name, pendingCount(t, ms))
	}
}

func TestPendingEditLiveShutdownStillArchivesOriginal(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, pendingAlias := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending-alias=%v", pendingAlias), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			chat := types.NewJID("120363000000000001", types.GroupServer)
			stamp := time.Unix(1772359200, 0)
			if pendingAlias {
				edit := incomingEdit("SHUTDOWN-ORIGINAL", "aliaseditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
				if err := ms.ApplyMessageEdit(chat.String(), phoneLID.String(), false, edit, stamp); err != nil {
					t.Fatal(err)
				}
			}
			b.cancel()
			original := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
			original.Info.ID, original.Info.Timestamp = "SHUTDOWN-ORIGINAL", stamp
			b.handleEvent(original, nil)
			assertPendingArchive(t, ms, original.Info.ID, chat.String(), "originalword", "", 0)
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("rows=%d failures=%d", rows, b.metrics.storeFailures.Load())
			}
		})
	}
}

func TestPendingEditOtherMemberBrokenSessionStillArchivesOriginal(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, mode := range []string{"live", "history"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(session, sessionPoolConns)
			t.Cleanup(func() { _ = session.Close() })
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			b := testBridge(t, newTestClient(container.LIDMap), ms, testLogger())
			chat := types.NewJID("120363000000000001", types.GroupServer)
			stamp := time.Unix(1772359200, 0)
			edit := incomingEdit("MEMBER-COLLISION", "othermemberword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(chat.String(), phoneLID.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			if mode == "live" {
				original := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
				original.Info.ID, original.Info.Timestamp = "MEMBER-COLLISION", stamp
				b.handleEvent(original, nil)
			} else {
				fixture := largeHistoryFixture(1)
				conversation := fixture.Data.Conversations[0]
				conversation.ID = proto.String(chat.String())
				row := conversation.Messages[0].Message
				row.Key.ID, row.Participant = proto.String("MEMBER-COLLISION"), proto.String(phonePN.String())
				row.Message = &waE2E.Message{Conversation: proto.String("originalword")}
				b.handleHistorySync(fixture)
			}
			assertPendingArchive(t, ms, "MEMBER-COLLISION", chat.String(), "originalword", "", 0)
			if b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("failures=%d", b.metrics.storeFailures.Load())
			}
		})
	}
}

func TestPendingEditUnknownHistoryKeepsRecipientVisibleTimer(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	b.Connected = func() bool { return true }
	b.Send = b.sendBackend()
	chat := "120363000000000001@g.us"
	fixture := largeHistoryFixture(1)
	conversation := fixture.Data.Conversations[0]
	conversation.ID, conversation.Name = proto.String(chat), proto.String("Alice")
	conversation.Messages[0].Message.Participant = proto.String(phonePN.String())
	conversation.Messages[0].Message.Message = incomingEdit("ABSENT", "pendingword", 1772359260000)
	conversation.EphemeralExpiration, conversation.EphemeralSettingTimestamp = proto.Uint32(86400), proto.Int64(1772359200)
	b.handleHistorySync(fixture)
	var observed uint32
	calls := 0
	b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		calls++
		observed = message.GetExtendedTextMessage().GetContextInfo().GetExpiration()
		return whatsmeow.SendResponse{ID: "HISTORY-TIMER-SEND", Timestamp: time.Unix(1772359200, 0)}, nil
	}
	body, err := json.Marshal(SendMessageRequest{Recipient: chat, Message: "outboundsendword"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	b.newRESTMux(8080, sendRecipientToken).ServeHTTP(response, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
	if response.Code != http.StatusOK || calls != 1 || observed != 86400 {
		t.Fatalf("HTTP=%d calls=%d wire expiration=%d", response.Code, calls, observed)
	}
	var name string
	if err := ms.db.QueryRow("SELECT name FROM chats WHERE jid=?", chat).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Alice" {
		t.Fatalf("name=%q", name)
	}
}

func TestPendingEditSenderFloodPreservesOtherMembersAndChats(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	stamp := time.Unix(1772359200, 0)
	chats := []string{"120363000000000001@g.us", "120363000000000002@g.us", "120363000000000003@g.us", "120363000000000004@g.us", "120363000000000009@g.us", "1@g.us", "3@g.us", "123@g.us"}
	if err := ms.Batch(func(batch *messageBatch) error {
		for _, chat := range []string{phonePN.String(), chats[0]} {
			edit := incomingEdit("LEGITIMATE", "legitimateword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := batch.ApplyMessageEdit(chat, selfPhone.String(), false, edit, stamp); err != nil {
				return err
			}
		}
		for _, chat := range chats {
			for index := range 128 {
				edit := incomingEdit(fmt.Sprintf("FLOOD-%d", index), "floodword", stamp.UnixMilli()).EditedMessage.Message.ProtocolMessage
				if err := batch.ApplyMessageEdit(chat, phonePN.String(), false, edit, stamp); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var legitimate, maximum int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM pending_edits WHERE target_id='LEGITIMATE'").Scan(&legitimate); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT MAX(n) FROM (SELECT COUNT(*) n FROM pending_edits GROUP BY chat_jid,sender,sender_server,is_from_me)").Scan(&maximum); err != nil {
		t.Fatal(err)
	}
	if legitimate != 2 || maximum > 32 {
		t.Fatalf("legitimate=%d maximum sender pending=%d", legitimate, maximum)
	}
	for _, chat := range []string{phonePN.String(), chats[0]} {
		if err := ms.EnsureChat(chat, "Alice"); err != nil {
			t.Fatal(err)
		}
		if err := ms.StoreMessage(storedMessage{ID: "LEGITIMATE", ChatJID: chat, Sender: selfPhone.String(), Content: "originalword", Timestamp: stamp}); err != nil {
			t.Fatal(err)
		}
		assertPendingArchive(t, ms, "LEGITIMATE", chat, "legitimateword", "", stamp.Add(time.Minute).UnixMilli())
	}
}

func TestPendingEditLiveAliasCancellationStillArchivesOriginal(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms, _ := lockedProductionStore(t)
	session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(session, sessionPoolConns)
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
		t.Fatal(err)
	}
	container := sqlstore.NewWithDB(session, "sqlite", testLogger())
	b := testBridge(t, newTestClient(container.LIDMap), ms, testLogger())
	chat := types.NewJID("120363000000000001", types.GroupServer)
	stamp := time.Unix(1772359200, 0)
	edit := incomingEdit("LIVE-ALIAS-CANCEL", "aliaseditword", stamp.Add(time.Minute).UnixMilli()).EditedMessage.Message.ProtocolMessage
	if err := ms.ApplyMessageEdit(chat.String(), phoneLID.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	for range sessionPoolConns {
		conn, err := session.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	original := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "originalword")
	original.Info.ID, original.Info.Timestamp = "LIVE-ALIAS-CANCEL", stamp
	done := make(chan struct{})
	go func() { defer close(done); b.handleEvent(original, nil) }()
	deadline := time.After(2 * time.Second)
	for session.Stats().WaitCount == 0 {
		select {
		case <-deadline:
			t.Fatal("live alias lookup never reached occupied session pool")
		default:
			runtime.Gosched()
		}
	}
	b.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled live lookup did not finish persistence")
	}
	assertPendingArchive(t, ms, original.Info.ID, chat.String(), "originalword", "", 0)
	if b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("failures=%d", b.metrics.storeFailures.Load())
	}
}

func TestPendingEditHostedSenderNamespaces(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, path := range []string{"live", "history"} {
		for _, lid := range []bool{false, true} {
			for _, mode := range []string{"same", "verified-alias", "wrong-namespace"} {
				t.Run(fmt.Sprintf("%s/lid=%v/%s", path, lid, mode), func(t *testing.T) {
					ms, _ := lockedProductionStore(t)
					mapping := &mockLIDStore{}
					b := testBridge(t, newTestClient(mapping), ms, testLogger())
					chat := types.NewJID("120363000000000001", types.GroupServer)
					author := types.NewJID(phonePN.User, types.HostedServer)
					other := types.NewJID(phoneLID.User, types.HostedLIDServer)
					if lid {
						author, other = other, author
					}
					stamp := time.Unix(1772359200, 0)
					deliver := func(sender types.JID, message *waE2E.Message) {
						if path == "live" {
							event := buildTextMessage(chat, sender, types.EmptyJID, types.EmptyJID, false, "")
							event.Info.ID, event.Info.Timestamp, event.Message = "HOSTED-TARGET", stamp, message
							b.handleEvent(event, nil)
						} else {
							fixture := largeHistoryFixture(1)
							conversation := fixture.Data.Conversations[0]
							conversation.ID = proto.String(chat.String())
							row := conversation.Messages[0].Message
							row.Key.ID, row.Participant, row.Message = proto.String("HOSTED-TARGET"), proto.String(sender.String()), message
							b.handleHistorySync(fixture)
						}
					}
					deliver(author, pendingEditMessage("HOSTED-TARGET", "hostededitword", stamp.Add(time.Minute), true))
					if pendingCount(t, ms) != 1 {
						t.Fatalf("hosted edit pending=%d", pendingCount(t, ms))
					}
					var server string
					if err := ms.db.QueryRow("SELECT sender_server FROM pending_edits").Scan(&server); err != nil {
						t.Fatal(err)
					}
					_, expectedServer := splitSenderJID(author.String())
					if server != expectedServer {
						t.Fatalf("server=%q want=%v", server, expectedServer)
					}
					originalAuthor := author
					switch mode {
					case "verified-alias":
						mapping.lidByPN = map[types.JID]types.JID{phonePN: phoneLID}
						mapping.pnByLID = map[types.JID]types.JID{phoneLID: phonePN}
						originalAuthor = other
					case "wrong-namespace":
						originalAuthor = types.NewJID(author.User, other.Server)
					}
					deliver(originalAuthor, &waE2E.Message{Conversation: proto.String("originalword")})
					text, mentions, editStamp := "hostededitword", selfPhone.User, stamp.Add(time.Minute).UnixMilli()
					if mode == "wrong-namespace" {
						text, mentions, editStamp = "originalword", "", 0
					}
					assertPendingArchive(t, ms, "HOSTED-TARGET", chat.String(), text, mentions, editStamp)
				})
			}
		}
	}
}

func TestPendingEditHistoryRevalidatesConcurrentAlias(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms, _ := lockedProductionStore(t)
	session, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(session, sessionPoolConns)
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, newTestClient(sqlstore.NewWithDB(session, "sqlite", testLogger()).LIDMap), ms, testLogger())
	var held []*sql.Conn
	for range sessionPoolConns {
		conn, err := session.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		t.Cleanup(func() { _ = conn.Close() })
	}
	chat := types.NewJID("120363000000000001", types.GroupServer)
	fixture := largeHistoryFixture(2)
	conversation := fixture.Data.Conversations[0]
	conversation.ID = proto.String(chat.String())
	conversation.Messages[0].Message.Participant = proto.String(phonePN.String())
	conversation.Messages[1].Message.Participant = proto.String(phoneLID.String())
	done := make(chan struct{})
	go func() { defer close(done); b.handleHistorySync(fixture) }()
	deadline := time.After(5 * time.Second)
	for session.Stats().WaitCount == 0 {
		select {
		case <-deadline:
			t.Fatal("second original never blocked in the session pool")
		default:
			runtime.Gosched()
		}
	}
	// The first original was prepared with no pending candidates; the second
	// original's real SDK read keeps the archive writer free for this edit.
	stamp := time.Unix(1772359200, 0)
	edit := extractMessage(pendingEditMessage("H0", "concurrenteditword", stamp.Add(time.Minute), true), stamp, "H0").edit
	if err := ms.ApplyMessageEdit(chat.String(), phoneLID.String(), false, edit, stamp); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, ms) != 1 {
		t.Fatal("concurrent edit was not retained before releasing the session pool")
	}
	for _, conn := range held {
		_ = conn.Close()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("history did not finish after the session pool was released")
	}
	assertPendingArchive(t, ms, "H0", chat.String(), "concurrenteditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
	if pendingCount(t, ms) != 0 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("pending=%d failures=%d", pendingCount(t, ms), b.metrics.storeFailures.Load())
	}
}

func TestPendingEditLivePhoneSenderLIDHint(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, mode := range []string{"incoming", "incoming-hosted", "incoming-dm", "incoming-dm-hosted", "incoming-dm-wrong-chat", "wrong-namespace", "own-untrusted-peer"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			chat := types.NewJID("120363000000000001", types.GroupServer)
			editChat := chat
			if strings.Contains(mode, "-dm") {
				chat, editChat = phonePN, phoneLID
				if mode == "incoming-dm-wrong-chat" {
					chat = selfPhone
				}
			}
			stamp := time.Unix(1772359200, 0)
			own := mode == "own-untrusted-peer"
			edit := extractMessage(pendingEditMessage("HINT-TARGET", "trustedhintword", stamp.Add(time.Minute), true), stamp, "HINT-TARGET").edit
			if err := ms.ApplyMessageEdit(editChat.String(), phoneLID.String(), own, edit, stamp); err != nil {
				t.Fatal(err)
			}
			if pendingCount(t, ms) != 1 {
				t.Fatal("hint edit was not retained before the original")
			}
			sender, alt := phonePN, phoneLID
			if own {
				sender = selfPhone
			} else if strings.HasSuffix(mode, "-hosted") {
				sender, alt = types.NewJID(phonePN.User, types.HostedServer), types.NewJID(phoneLID.User, types.HostedLIDServer)
			} else if mode == "wrong-namespace" {
				alt = types.NewJID(phoneLID.User, types.DefaultUserServer)
			}
			original := buildTextMessage(chat, sender, alt, types.EmptyJID, own, "originalword")
			original.Info.ID, original.Info.Timestamp = "HINT-TARGET", stamp
			b.handleEvent(original, nil)
			text, mentions, editStamp := "trustedhintword", selfPhone.User, stamp.Add(time.Minute).UnixMilli()
			if mode == "wrong-namespace" || mode == "own-untrusted-peer" || mode == "incoming-dm-wrong-chat" {
				text, mentions, editStamp = "originalword", "", 0
			}
			assertPendingArchive(t, ms, original.Info.ID, chat.String(), text, mentions, editStamp)
			if mode == "incoming-dm-wrong-chat" && pendingCount(t, ms) != 1 {
				t.Fatal("unrelated DM consumed the pending edit")
			}
		})
	}
}

func TestPendingEditPreparationRollbackRetryAndExhaustion(t *testing.T) {
	for _, mode := range []string{"rollback", "retry-and-repeat", "exhaustion"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{lidByPN: map[types.JID]types.JID{phonePN: phoneLID}}), ms, testLogger())
			chat := types.NewJID("120363000000000001", types.GroupServer).String()
			if err := ms.EnsureChat(chat, "Alice"); err != nil {
				t.Fatal(err)
			}
			stamp := time.Unix(1772359200, 0)
			_, _, stale, err := b.preparePendingEdit(b.ctx, "ATOMIC-TARGET", chat, phonePN.String(), false)
			if err != nil {
				t.Fatal(err)
			}
			edit := extractMessage(pendingEditMessage("ATOMIC-TARGET", "atomiceditword", stamp.Add(time.Minute), true), stamp, "ATOMIC-TARGET").edit
			if err := ms.ApplyMessageEdit(chat, phoneLID.String(), false, edit, stamp); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			write := func() error {
				attempts++
				alias, chatAlias, prepared := "", "", stale
				if attempts > 1 || mode == "exhaustion" {
					var err error
					alias, chatAlias, prepared, err = b.preparePendingEdit(b.ctx, "ATOMIC-TARGET", chat, phonePN.String(), false)
					if err != nil {
						return err
					}
				}
				if mode == "exhaustion" {
					authors := []string{"5511888888888@s.whatsapp.net", "11234567890@s.whatsapp.net", "5511999999999@s.whatsapp.net"}
					if err := ms.ApplyMessageEdit(chat, authors[attempts-1], false, edit, stamp); err != nil {
						return err
					}
				}
				return ms.BatchContext(b.ctx, func(batch *messageBatch) error {
					if err := batch.StoreMessage(storedMessage{ID: "ATOMIC-PROBE", ChatJID: chat, Sender: phonePN.String(), Content: "probeword", Timestamp: stamp}); err != nil {
						return err
					}
					original := storedMessage{ID: "ATOMIC-TARGET", ChatJID: chat, Sender: phonePN.String(), Content: "originalword", Timestamp: stamp, EditAuthorAlias: alias, EditChatAlias: chatAlias, EditPreparation: prepared}
					if err := batch.StoreMessage(original); err != nil {
						return err
					}
					return batch.StoreMessage(original) // same-key replay inside a chunk
				})
			}
			if mode == "rollback" {
				if err := write(); err != errPendingEditPreparationChanged {
					t.Fatalf("stale preparation error=%v", err)
				}
			} else {
				stored := b.storeLive("message", "ATOMIC-TARGET", chat, func() error { return retryPendingEditPreparation(write) })
				if stored != (mode == "retry-and-repeat") {
					t.Fatalf("stored=%v attempts=%d", stored, attempts)
				}
			}
			if mode == "retry-and-repeat" {
				if attempts != 2 || pendingCount(t, ms) != 0 {
					t.Fatalf("attempts=%d pending=%d", attempts, pendingCount(t, ms))
				}
				assertPendingArchive(t, ms, "ATOMIC-TARGET", chat, "atomiceditword", selfPhone.User, stamp.Add(time.Minute).UnixMilli())
				return
			}
			var rows, fts int
			if err := ms.db.QueryRow("SELECT (SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM messages_fts)").Scan(&rows, &fts); err != nil {
				t.Fatal(err)
			}
			if rows != 0 || fts != 0 || pendingCount(t, ms) < 1 {
				t.Fatalf("rollback rows=%d FTS=%d pending=%d", rows, fts, pendingCount(t, ms))
			}
			if mode == "exhaustion" && (attempts != 3 || b.metrics.storeFailures.Load() != 1) {
				t.Fatalf("attempts=%d failures=%d", attempts, b.metrics.storeFailures.Load())
			}
		})
	}
}
