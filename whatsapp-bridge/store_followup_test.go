package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func storeWriteOwner(t *testing.T, ms *MessageStore) storeWriteFunc {
	t.Helper()
	return testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger()).storeLive
}

// The remote callback acquires the real writer lock after the remote effect,
// so validation reads succeed and only the post-effect archive write is busy.
func TestPostEffectArchiveWritesRetryWithoutRepeatingRemoteEffect(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, action := range []string{"edit", "revoke"} {
		for _, free := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/free=%v", action, free), func(t *testing.T) {
				ms, lock := lockedProductionStore(t)
				seedMessage(t, ms, "POST1", phonePN.String(), true)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				var release func()
				waits, effects := 0, 0
				b.StoreRetryDelays = []time.Duration{time.Hour, time.Hour}
				b.storeRetryWait = func(time.Duration) bool {
					waits++
					if free && waits == 1 {
						release()
					}
					return true
				}
				effect := func() { effects++; release = lock() }
				var code int
				var success bool
				var message string
				if action == "edit" {
					codeResp, resp := efPost(t, handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) (int64, error) {
						effect()
						return time.Now().UnixMilli(), nil
					}, chatPolicy{}, b.storeLive), `{"chat_jid":"`+phonePN.String()+`","message_id":"POST1","text":"new content"}`)
					code, success, message = codeResp, resp.Success, resp.Message
				} else {
					rr, resp := postDelete(handleDeleteMessage(ms, func(context.Context, types.JID, types.MessageID) error { effect(); return nil }, chatPolicy{}, b.storeLive), `{"chat_jid":"`+phonePN.String()+`","message_id":"POST1","for_everyone":true}`)
					code, success, message = rr.Code, resp.Success, resp.Message
				}
				release()
				wantWaits := 2
				if free {
					wantWaits = 1
				}
				if code != http.StatusOK || !success || effects != 1 || waits != wantWaits {
					t.Fatalf("code=%d success=%v effects=%d waits=%d", code, success, effects, waits)
				}
				var content string
				var deleted bool
				if err := ms.db.QueryRow(`SELECT content,deleted_at IS NOT NULL FROM messages WHERE id='POST1'`).Scan(&content, &deleted); err != nil {
					t.Fatal(err)
				}
				if action == "edit" && (content == "new content") != free || action == "revoke" && deleted != free {
					t.Fatalf("archive content=%q deleted=%v free=%v", content, deleted, free)
				}
				wantFailures := int64(1)
				if free {
					wantFailures = 0
				}
				errs := errorLines(rec.String())
				if b.metrics.storeFailures.Load() != wantFailures || len(errs) != int(wantFailures) {
					t.Fatalf("failures=%d logs=%v", b.metrics.storeFailures.Load(), errs)
				}
				if !free && (!strings.Contains(message, "archive") || !strings.Contains(errs[0], "POST1") || !strings.Contains(errs[0], phonePN.String()) || strings.Contains(message, "SQLITE") || strings.Contains(errs[0], "new content")) {
					t.Fatalf("warning=%q logs=%v", message, errs)
				}
			})
		}
	}
}

func TestRevokeAndCallEventsRetryRealBusyStore(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, kind := range []string{"revoke", "offer", "group offer", "accept", "reject", "terminate"} {
		for _, free := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/free=%v", kind, free), func(t *testing.T) {
				ms, lock := lockedProductionStore(t)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				now := time.Unix(1772359200, 0)
				meta := types.BasicCallMeta{CallID: "CALL1", From: phonePN, CallCreator: phonePN, Timestamp: now}
				chat := phonePN.String()
				if kind == "group offer" {
					meta.GroupJID = types.NewJID("120363000000000001", types.GroupServer)
					chat = meta.GroupJID.String()
				}
				if kind == "revoke" {
					seedMessage(t, ms, "TARGET1", chat, false)
				} else if kind != "offer" && kind != "group offer" {
					if err := ms.StoreCallOffer(meta.CallID, chat, chat, now, false, "voice", false); err != nil {
						t.Fatal(err)
					}
				}
				release := lock()
				waits := 0
				b.StoreRetryDelays = []time.Duration{time.Hour, time.Hour}
				b.storeRetryWait = func(time.Duration) bool {
					waits++
					if free && waits == 1 {
						release()
					}
					return true
				}
				switch kind {
				case "revoke":
					msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					msg.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("TARGET1")}}}
					b.handleEvent(msg, nil)
				case "offer":
					b.handleEvent(&events.CallOffer{BasicCallMeta: meta}, nil)
				case "group offer":
					b.handleEvent(&events.CallOfferNotice{BasicCallMeta: meta, Type: "group", Media: "video"}, nil)
				case "accept":
					b.handleEvent(&events.CallAccept{BasicCallMeta: meta}, nil)
				case "reject":
					b.handleEvent(&events.CallReject{BasicCallMeta: meta}, nil)
				case "terminate":
					meta.Timestamp = now.Add(90 * time.Second)
					b.handleEvent(&events.CallTerminate{BasicCallMeta: meta, Reason: "timeout"}, nil)
				}
				release()
				wantWaits := 2
				if free {
					wantWaits = 1
				}
				if waits != wantWaits {
					t.Fatalf("waits=%d want=%d", waits, wantWaits)
				}
				var changed bool
				if kind == "revoke" {
					if err := ms.db.QueryRow(`SELECT deleted_at IS NOT NULL FROM messages WHERE id='TARGET1'`).Scan(&changed); err != nil {
						t.Fatal(err)
					}
				} else {
					var n int
					predicate := map[string]string{"offer": "result='in_progress'", "group offer": "call_type='video' AND is_group=1", "accept": "result='answered'", "reject": "result='rejected'", "terminate": "result='missed' AND duration_sec=90 AND reason='timeout'"}[kind]
					if err := ms.db.QueryRow(`SELECT COUNT(*) FROM calls WHERE call_id='CALL1' AND ` + predicate).Scan(&n); err != nil {
						t.Fatal(err)
					}
					changed = n == 1
				}
				if changed != free {
					t.Fatalf("changed=%v free=%v", changed, free)
				}
				wantFailures := int64(1)
				if free {
					wantFailures = 0
				}
				errs := errorLines(rec.String())
				if b.metrics.storeFailures.Load() != wantFailures || len(errs) != int(wantFailures) {
					t.Fatalf("failures=%d logs=%v", b.metrics.storeFailures.Load(), errs)
				}
				id := "CALL1"
				if kind == "revoke" {
					id = "TARGET1"
				}
				if !free && (!strings.Contains(errs[0], id) || !strings.Contains(errs[0], chat)) {
					t.Fatalf("missing identity: %v", errs)
				}
			})
		}
	}
}

func TestLiveAuxiliaryFailureRollsBackMessageAndIndex(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	t.Setenv("WHATSAPP_MEDIA_AUTODOWNLOAD", "false")
	for _, kind := range []string{"mentions", "poll", "view once"} {
		for _, failure := range []string{"ABORT", "ROLLBACK"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				ms, _ := lockedProductionStore(t)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
					return false, "", "", "", nil
				}
				b.storeRetryWait = func(time.Duration) bool { t.Error("constraint errors must not retry"); return true }
				evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "private words")
				evt.Info.ID = "AUX1"
				table, event := "messages", "UPDATE OF mentions"
				switch kind {
				case "mentions":
					event = "UPDATE OF mentions"
					evt.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("private words"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{phonePN.String()}}}}
				case "poll":
					table, event = "polls", "INSERT"
					evt.Message = pollCreationMsg("private words", "Alice", "Bob")
				case "view once":
					event = "UPDATE OF view_once"
					evt.Message = viewOnceImage("private words")
				}
				if _, err := ms.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_aux BEFORE %s ON %s BEGIN SELECT RAISE(%s,'metadata rejected');END", event, table, failure)); err != nil {
					t.Fatal(err)
				}
				b.handleMessage(evt)
				for _, table := range []string{"messages", "messages_fts", "polls"} {
					var n int
					if err := ms.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatalf("partial %s rows=%d", table, n)
					}
				}
				errs := errorLines(rec.String())
				if b.metrics.messagesStored.Load() != 0 || b.metrics.storeFailures.Load() != 1 || len(errs) != 1 || !strings.Contains(errs[0], "AUX1") || !strings.Contains(errs[0], phonePN.String()) || strings.Contains(errs[0], "private words") || strings.Contains(errs[0], "private-path") {
					t.Fatalf("stored=%d failed=%d logs=%v", b.metrics.messagesStored.Load(), b.metrics.storeFailures.Load(), errs)
				}
			})
		}
	}
}

func TestLiveBusyRetryCommitsMessageAndAuxiliaryMetadata(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	t.Setenv("WHATSAPP_MEDIA_AUTODOWNLOAD", "false")
	for _, kind := range []string{"direct path", "mentions", "poll", "view once"} {
		t.Run(kind, func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "searchable")
			evt.Info.ID = "AUXRETRY1"
			switch kind {
			case "direct path":
				evt = buildImageMessage(phonePN, phonePN, false, "searchable")
				evt.Info.ID = "AUXRETRY1"
				evt.Message.ImageMessage.DirectPath = proto.String("/example-path")
			case "mentions":
				evt.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("searchable"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{phonePN.String()}}}}
			case "poll":
				evt.Message = pollCreationMsg("searchable", "Alice", "Bob")
			case "view once":
				evt.Message = viewOnceImage("searchable")
			}
			// Keep an existing chat while the live persistence unit finds BUSY.
			// EnsureChat can still need the writer even when its row exists.
			if err := ms.StoreChat(phonePN.String(), "Alice", evt.Info.Timestamp); err != nil {
				t.Fatal(err)
			}
			release := lock()
			waits := 0
			b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
			b.handleMessage(evt)
			if waits != 1 || b.metrics.messagesStored.Load() != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("waits=%d stored=%d failures=%d", waits, b.metrics.messagesStored.Load(), b.metrics.storeFailures.Load())
			}
			var indexed int
			if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'`).Scan(&indexed); err != nil || indexed != 1 {
				t.Fatalf("indexed=%d error=%v", indexed, err)
			}
			var present bool
			predicate := map[string]string{"direct path": "direct_path='/example-path'", "mentions": "mentions='" + phonePN.User + "'", "view once": "view_once=1"}[kind]
			query := `SELECT COUNT(*)=1 FROM messages WHERE id='AUXRETRY1' AND ` + predicate
			if kind == "poll" {
				query = `SELECT COUNT(*)=1 FROM polls WHERE message_id='AUXRETRY1' AND question='searchable'`
			}
			if err := ms.db.QueryRow(query).Scan(&present); err != nil || !present {
				t.Fatalf("metadata committed=%v error=%v", present, err)
			}
		})
	}
}
