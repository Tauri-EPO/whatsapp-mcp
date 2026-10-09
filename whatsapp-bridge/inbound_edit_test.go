package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func incomingEdit(id, text string, stamp int64) *waE2E.Message {
	return &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String(selfPhone.String())},
			EditedMessage: &waE2E.Message{Conversation: proto.String(text)}, TimestampMS: proto.Int64(stamp)},
	}}}
}

func TestInboundEditWrappedReplacementKeepsCaptionAndMentions(t *testing.T) {
	for _, path := range []string{"live", "history"} {
		for _, wrapper := range []string{"document-caption", "ephemeral", "nested"} {
			t.Run(path+"/"+wrapper, func(t *testing.T) {
				ms, _ := lockedProductionStore(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
				stamp := time.Unix(1772359200, 0)
				original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
				original.Info.ID, original.Info.Timestamp = "WRAPPED-TARGET", stamp
				b.handleEvent(original, nil)
				inner := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("wrappededitword"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{selfPhone.String()}}}}
				replacement := &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: inner}}
				switch wrapper {
				case "ephemeral":
					replacement = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: inner}}
				case "nested":
					replacement = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: replacement}}
				}
				edit := incomingEdit(original.Info.ID, "", stamp.Add(time.Minute).UnixMilli())
				edit.EditedMessage.Message.ProtocolMessage.EditedMessage = replacement
				if path == "live" {
					event := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Info.ID, event.Message = "WRAPPED-EDIT", edit
					b.handleEvent(event, nil)
				} else {
					fixture := largeHistoryFixture(1)
					fixture.Data.Conversations[0].Messages[0].Message.Message = edit
					b.handleHistorySync(fixture)
				}
				b.handleEvent(original, nil)
				var content, mentions string
				var edited int64
				if err := ms.db.QueryRow("SELECT content,mentions,message_edit_timestamp FROM messages WHERE id=?", original.Info.ID).Scan(&content, &mentions, &edited); err != nil {
					t.Fatal(err)
				}
				var oldFTS, newFTS int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'oldword'").Scan(&oldFTS); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'wrappededitword'").Scan(&newFTS); err != nil {
					t.Fatal(err)
				}
				if content != "wrappededitword" || mentions != selfPhone.User || edited != stamp.Add(time.Minute).UnixMilli() || oldFTS != 0 || newFTS != 1 {
					t.Fatalf("wrapped edit content=%q mentions=%q edited=%d oldFTS=%d newFTS=%d", content, mentions, edited, oldFTS, newFTS)
				}
			})
		}
	}
}

func TestHistoryEditSessionBusyRetryOutsideArchiveWriter(t *testing.T) {
	for _, release := range []bool{true, false} {
		t.Run(map[bool]string{true: "release", false: "exhaust"}[release], func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			session, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "session.db"))+"?_pragma=busy_timeout(1)")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			session.SetMaxOpenConns(2)
			if _, err := session.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?,?)", phoneLID.User, phonePN.User); err != nil {
				t.Fatal(err)
			}
			held, err := session.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = held.ExecContext(context.Background(), "ROLLBACK"); _ = held.Close() })
			container := sqlstore.NewWithDB(session, "sqlite", testLogger())
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(container.LIDMap), ms, rec)
			stamp := time.Unix(1772359200, 0)
			if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "SESSION-EDIT", ChatJID: phonePN.String(), Sender: phoneLID.String(), Content: "oldword", Timestamp: stamp}); err != nil {
				t.Fatal(err)
			}
			b.historyBatchWriter = func(write func(*messageBatch) error) error {
				err := ms.Batch(write)
				if _, lockErr := held.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); lockErr != nil {
					t.Fatal(lockErr)
				}
				return err
			}
			waits := 0
			b.storeRetryWait = func(time.Duration) bool {
				waits++
				// A session writer is held; the archive writer must still be free.
				if err := ms.StoreMessage(storedMessage{ID: "EDIT-PROBE", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "writerprobeword", Timestamp: stamp}); err != nil {
					t.Fatal(err)
				}
				if release {
					if _, err := held.ExecContext(context.Background(), "ROLLBACK"); err != nil {
						t.Fatal(err)
					}
				}
				return true
			}
			fixture := largeHistoryFixture(1)
			fixture.Data.Conversations[0].Messages[0].Message.Message = incomingEdit("SESSION-EDIT", "neweditword", stamp.Add(time.Minute).UnixMilli())
			b.handleHistorySync(fixture)
			_, _ = held.ExecContext(context.Background(), "ROLLBACK")
			var content string
			if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='SESSION-EDIT'").Scan(&content); err != nil {
				t.Fatal(err)
			}
			var probes int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'writerprobeword'").Scan(&probes); err != nil {
				t.Fatal(err)
			}
			want, wantWaits, wantFailures := "oldword", 2, int64(1)
			if release {
				want, wantWaits, wantFailures = "neweditword", 1, 0
			}
			if content != want || waits != wantWaits || probes != 1 || b.metrics.storeFailures.Load() != wantFailures || len(errorLines(rec.String())) != int(wantFailures) {
				t.Fatalf("content=%q waits=%d probes=%d failures=%d log=%s", content, waits, probes, b.metrics.storeFailures.Load(), rec.String())
			}
			if strings.Contains(rec.String(), "neweditword") {
				t.Fatal("edit content leaked in diagnostics")
			}
		})
	}
}

func TestOutboundEditWireTimestampAndNewerPhoneEditOrdering(t *testing.T) {
	for _, mode := range []string{"phone-after-ack", "phone-before-SDK-ack"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.Connected = func() bool { return true }
			if err := ms.StoreChat(phonePN.String(), "Alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "OWN-EDIT", ChatJID: phonePN.String(), Sender: selfPhone.String(), Content: "originalword", Timestamp: time.Now(), IsFromMe: true, MediaType: "", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: nil, QuotedMessageID: ""}); err != nil {
				t.Fatal(err)
			}
			wireStamp := int64(100)
			phoneEdit := func(stamp int64) {
				event := buildTextMessage(phonePN, selfPhone, types.EmptyJID, types.EmptyJID, true, "")
				event.Info.ID, event.Message = "PHONE-EDIT", incomingEdit("OWN-EDIT", "newerphoneword", stamp)
				b.handleEvent(event, nil)
			}
			var handler http.Handler = handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) (int64, error) { return wireStamp, nil }, chatPolicy{}, b.storeLive)
			if mode == "phone-before-SDK-ack" {
				b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
					wireStamp = message.GetEditedMessage().GetMessage().GetProtocolMessage().GetTimestampMS()
					if wireStamp <= 0 {
						t.Fatal("SDK edit omitted wire timestamp")
					}
					phoneEdit(wireStamp + 1)
					return whatsmeow.SendResponse{}, nil
				}
				handler = b.newRESTMux(8080, "test-token-0123456789")
			}
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/edit", strings.NewReader(`{"chat_jid":"`+phonePN.String()+`","message_id":"OWN-EDIT","text":"olderoutboundword"}`))
			request.Header.Set("Authorization", "Bearer test-token-0123456789")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("edit status=%d body=%s", response.Code, response.Body.String())
			}
			if mode == "phone-after-ack" {
				var saved int64
				if err := ms.db.QueryRow("SELECT message_edit_timestamp FROM messages WHERE id='OWN-EDIT'").Scan(&saved); err != nil || saved != wireStamp {
					t.Fatalf("archive stored acknowledgement clock instead of wire time: %d err=%v", saved, err)
				}
				phoneEdit(wireStamp + 1)
			}
			var content string
			var stamp int64
			if err := ms.db.QueryRow("SELECT content,message_edit_timestamp FROM messages WHERE id='OWN-EDIT'").Scan(&content, &stamp); err != nil || content != "newerphoneword" || stamp != wireStamp+1 {
				t.Fatalf("older acknowledged edit overwrote newer phone edit: content=%q stamp=%d err=%v", content, stamp, err)
			}
			var hits int
			if err := ms.db.QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'newerphoneword'").Scan(&hits); err != nil || hits != 1 {
				t.Fatalf("FTS stale after interleaved edit: hits=%d err=%v", hits, err)
			}
		})
	}
}

func TestInboundEditSDKParsedEventAndVerifiedLIDAuthor(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, mode := range []string{"SDK", "SDK-direct", "SDK-ephemeral", "LID-live", "LID-history"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}}), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			author := phoneLID.String()
			if strings.HasPrefix(mode, "SDK") {
				author = phonePN.String()
			}
			if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "TARGET", ChatJID: phonePN.String(), Sender: author, Content: "originalword", Timestamp: stamp, IsFromMe: false, MediaType: "", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
				t.Fatal(err)
			}
			previous := incomingEdit("TARGET", "previouseditword", stamp.Add(30*time.Second).UnixMilli()).EditedMessage.Message.ProtocolMessage
			if err := ms.ApplyMessageEdit(phonePN.String(), author, false, previous, stamp); err != nil {
				t.Fatal(err)
			}
			edit := incomingEdit("TARGET", "parsededitword", stamp.Add(time.Minute).UnixMilli())
			switch mode {
			case "SDK", "SDK-direct", "SDK-ephemeral":
				payload := edit
				if mode != "SDK" {
					payload = edit.EditedMessage.Message
				}
				if mode == "SDK-ephemeral" {
					payload = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: payload}}
				}
				event, err := b.Client.ParseWebMessage(phonePN, &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String("DELIVERY")}, Message: payload, MessageTimestamp: proto.Uint64(1772359260)})
				if err != nil || event.IsEdit != (mode == "SDK") || event.Info.ID != "TARGET" || event.Message.GetConversation() != "parsededitword" {
					t.Fatalf("unexpected pinned SDK edit shape: %+v %v", event, err)
				}
				b.handleEvent(event, nil)
			case "LID-live":
				event := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
				event.Message = edit
				b.handleEvent(event, nil)
			case "LID-history":
				fixture := largeHistoryFixture(1)
				fixture.Data.Conversations[0].Messages[0].Message.Message = edit
				b.handleHistorySync(fixture)
			}
			// The new text must survive an ordinary original-message replay.
			fixture := largeHistoryFixture(1)
			fixture.Data.Conversations[0].Messages[0].Message.Key.ID = proto.String("TARGET")
			b.handleHistorySync(fixture)
			var content string
			var editTime int64
			if err := ms.db.QueryRow("SELECT content,message_edit_timestamp FROM messages WHERE id='TARGET' AND chat_jid=?", phonePN.String()).Scan(&content, &editTime); err != nil {
				t.Fatal(err)
			}
			if content != "parsededitword" || editTime != stamp.Add(time.Minute).UnixMilli() {
				t.Fatalf("parsed edit lost: %s timestamp=%d", content, editTime)
			}
		})
	}
}

func TestInboundEditMentionsFollowTextAndResistReplays(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	chat := types.NewJID("120363000000000001", types.GroupServer)
	stamp := time.Unix(1772359200, 0)
	original := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
	original.Info.ID, original.Info.Timestamp = "MENTION-EDIT", stamp
	original.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("oldword"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{selfPhone.String()}}}}
	b.handleEvent(original, nil)
	check := func(text, mention string) {
		t.Helper()
		var content, stored string
		if err := ms.db.QueryRow("SELECT content,mentions FROM messages WHERE id=? AND chat_jid=?", original.Info.ID, chat.String()).Scan(&content, &stored); err != nil {
			t.Fatal(err)
		}
		var matched int
		// Same comma-delimited predicate used by whatsapp.mentions_me_predicate.
		if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id=? AND mentions IS NOT NULL AND (',' || mentions || ',') LIKE ?", original.Info.ID, "%,"+selfPhone.User+",%").Scan(&matched); err != nil {
			t.Fatal(err)
		}
		wantMatched := 0
		if mention == selfPhone.User {
			wantMatched = 1
		}
		if content != text || stored != mention || matched != wantMatched {
			t.Fatalf("content=%s mentions=%s matched=%d", content, stored, matched)
		}
	}
	check("oldword", selfPhone.User)
	deliver := func(text string, mentioned []string, millis int64) {
		edit := incomingEdit(original.Info.ID, "", millis)
		edit.EditedMessage.Message.ProtocolMessage.EditedMessage = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: &waE2E.ContextInfo{MentionedJID: mentioned}}}
		event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
		event.Message = edit
		b.handleEvent(event, nil)
	}
	deliver("addedword", []string{phoneLID.String()}, stamp.Add(time.Minute).UnixMilli())
	b.handleEvent(original, nil)
	check("addedword", phoneLID.User)
	deliver("removedword", nil, stamp.Add(2*time.Minute).UnixMilli())
	b.handleEvent(original, nil)
	deliver("stale mention", []string{selfPhone.String()}, stamp.Add(time.Minute).UnixMilli())
	check("removedword", "")
}

func TestInboundEditLiveAndHistoryTargetFTSAndReplay(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	for _, path := range []string{"live", "history"} {
		t.Run(path, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.MediaAutoDownload = false
			stamp := time.Unix(1772359200, 0)
			for _, chat := range []types.JID{phonePN, selfPhone} {
				if err := ms.StoreChat(chat.String(), "Alice", stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "TARGET", ChatJID: chat.String(), Sender: phonePN.String(), Content: "originalword", Timestamp: stamp, IsFromMe: false, MediaType: "", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
					t.Fatal(err)
				}
			}
			deliver := func(target, text string, millis int64, author types.JID) {
				edit := incomingEdit(target, text, millis)
				if path == "live" {
					event := buildTextMessage(phonePN, author, types.EmptyJID, types.EmptyJID, false, "")
					event.Info.ID, event.Message = "EDIT-DELIVERY", edit
					b.handleEvent(event, nil)
				} else {
					fixture := largeHistoryFixture(1)
					row := fixture.Data.Conversations[0].Messages[0].Message
					row.Key.ID, row.Participant, row.Message = proto.String("EDIT-DELIVERY"), proto.String(author.String()), edit
					b.handleHistorySync(fixture)
				}
			}
			deliver("TARGET", "replacementword", stamp.Add(time.Minute).UnixMilli(), phonePN)
			deliver("MISSING", "missingword", stamp.Add(2*time.Minute).UnixMilli(), phonePN)
			deliver("TARGET", "wrongauthorword", stamp.Add(3*time.Minute).UnixMilli(), selfPhone)
			deliver("TARGET", "olderword", stamp.UnixMilli(), phonePN)
			// Replaying an original history row must not restore stale content.
			fixture := largeHistoryFixture(1)
			fixture.Data.Conversations[0].Messages[0].Message.Key.ID = proto.String("TARGET")
			b.handleHistorySync(fixture)
			var content, other string
			var rows, fresh, old int
			if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='TARGET' AND chat_jid=?", phonePN.String()).Scan(&content); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='TARGET' AND chat_jid=?", selfPhone.String()).Scan(&other); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'replacementword'").Scan(&fresh); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'originalword'").Scan(&old); err != nil {
				t.Fatal(err)
			}
			if content != "replacementword" || other != "originalword" || rows != 2 || fresh != 1 || old != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("content=%q other=%q rows=%d FTS=%d/%d failures=%d", content, other, rows, fresh, old, b.metrics.storeFailures.Load())
			}
		})
	}
}

func TestInboundEditMissingTargetDoesNotCreateChat(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	event := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
	event.Info.ID = "ORPHAN-EDIT"
	event.Message = incomingEdit("MISSING", "unarchivedword", 1772359300000)
	b.handleEvent(event, nil)
	var messages, chats, indexed int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || chats != 0 || indexed != 0 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("orphan edit left messages=%d chats=%d FTS=%d failures=%d", messages, chats, indexed, b.metrics.storeFailures.Load())
	}
}

func TestHistoryEditsWaitForOriginalInLaterChunk(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	fixture := largeHistoryFixture(historyBatchMessages + 1)
	conv := fixture.Data.Conversations[0]
	conv.Messages[0].Message.Message = incomingEdit("TARGET", "newestword", 1772359300000)
	conv.Messages[1].Message.Message = incomingEdit("TARGET", "olderword", 1772359250000)
	conv.Messages[historyBatchMessages].Message.Key.ID = proto.String("TARGET")
	b.handleHistorySync(fixture)
	var content string
	if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='TARGET'").Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "newestword" {
		t.Fatalf("newest-first edits across chunks lost: %q", content)
	}
	// Peer archives may add original rows, but must never edit owned rows.
	b.handleHistorySyncWithShares(fixture, false, true)
	if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='TARGET'").Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "newestword" {
		t.Fatal("peer archive overwrote edit")
	}
}

func TestInboundEditBusyReleaseAndExhaustion(t *testing.T) {
	for _, path := range []string{"live", "history"} {
		for _, release := range []bool{true, false} {
			t.Run(path+"/"+map[bool]string{true: "release", false: "exhaust"}[release], func(t *testing.T) {
				ms, lock := lockedProductionStore(t)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				stamp := time.Unix(1772359200, 0)
				if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "TARGET", ChatJID: phonePN.String(), Sender: phonePN.String(), Content: "oldword", Timestamp: stamp, IsFromMe: false, MediaType: "", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
					t.Fatal(err)
				}
				unlock := lock()
				waits := 0
				b.storeRetryWait = func(time.Duration) bool {
					waits++
					if release {
						unlock()
					}
					return true
				}
				if path == "live" {
					event := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Info.ID, event.Message = "EDIT-DELIVERY", incomingEdit("TARGET", "newword", stamp.Add(time.Minute).UnixMilli())
					b.handleEvent(event, nil)
				} else {
					// Release the setup lock and reacquire exactly when the deferred
					// edit UPDATE is attempted, rather than failing chat setup first.
					unlock()
					b.historyBatchWriter = func(fn func(*messageBatch) error) error {
						err := ms.Batch(fn)
						unlock = lock()
						return err
					}
					fixture := largeHistoryFixture(1)
					fixture.Data.Conversations[0].Messages = []*waHistorySync.HistorySyncMsg{fixture.Data.Conversations[0].Messages[0]}
					fixture.Data.Conversations[0].Messages[0].Message.Message = incomingEdit("TARGET", "newword", stamp.Add(time.Minute).UnixMilli())
					b.handleHistorySync(fixture)
				}
				unlock()
				var content string
				if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='TARGET'").Scan(&content); err != nil {
					t.Fatal(err)
				}
				want, failures, wantWaits := "oldword", int64(1), 2
				if release {
					want, failures, wantWaits = "newword", 0, 1
				}
				if content != want || waits != wantWaits || b.metrics.storeFailures.Load() != failures || len(errorLines(rec.String())) != int(failures) {
					t.Fatalf("content=%q waits=%d failures=%d log=%s", content, waits, b.metrics.storeFailures.Load(), rec.String())
				}
				if strings.Contains(rec.String(), "newword") {
					t.Fatal("edit text leaked into failure log")
				}
			})
		}
	}
}
