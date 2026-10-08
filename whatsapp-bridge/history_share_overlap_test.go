package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func shareArchiveSnapshot(t *testing.T, ms *MessageStore, id, chat string) string {
	t.Helper()
	rows, err := ms.db.Query("SELECT * FROM messages WHERE id=? AND chat_jid=?", id, chat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil || !rows.Next() {
		t.Fatalf("snapshot missing: %v", err)
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHistoryShareOverlapPreservesAuthoritativeRows(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, phoneHistory := range []bool{false, true} {
			for _, late := range []bool{false, true} {
				t.Run(map[bool]string{false: "live share", true: "history share"}[history]+"/"+map[bool]string{false: "live authority", true: "phone authority"}[phoneHistory]+"/"+map[bool]string{false: "existing", true: "arrives after decode"}[late], func(t *testing.T) {
					ms, _ := lockedProductionStore(t)
					self := types.NewJID("5511888888888", types.DefaultUserServer)
					b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), ms, testLogger())
					b.MediaAutoDownload = false
					const chat = "120363000000000001@g.us"
					stamp := time.Unix(1700000000, 0)
					if err := ms.StoreChat(chat, "Original group", stamp); err != nil {
						t.Fatal(err)
					}
					ownMessage := &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("Original own photo"), Mimetype: proto.String("image/jpeg"), URL: proto.String("https://example.invalid/original"), DirectPath: proto.String("/original"), MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: proto.Uint64(42), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIGINAL_QUOTE"), MentionedJID: []string{self.String()}}}}}}
					var before string
					seed := func() {
						if phoneHistory {
							original := shareHistoryFixture(1)
							original.Data.Conversations[0].Messages[0].Message.Message = ownMessage
							original.Data.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(true)
							original.Data.Conversations[0].Messages[0].Message.MessageTimestamp = proto.Uint64(1700000000)
							b.handleHistorySync(original)
						} else {
							event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), self, types.EmptyJID, types.EmptyJID, true, "")
							event.Info.ID, event.Info.Timestamp, event.Message = "H0", stamp, ownMessage
							b.handleMessage(event)
						}
						before = shareArchiveSnapshot(t, ms, "H0", chat)
					}
					if !late {
						seed()
					}
					injected := false
					b.historyBatchWriter = func(fn func(*messageBatch) error) error {
						if late && !injected {
							injected = true
							seed()
						}
						return ms.Batch(fn)
					}
					fixture := shareHistoryFixture(2)
					overlap := fixture.Data.Conversations[0].Messages[0].Message
					overlap.MessageTimestamp = proto.Uint64(1900000000)
					overlap.Message = &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Stale replacement"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Alice")}, {OptionName: proto.String("Bob")}}, SelectableOptionsCount: proto.Uint32(1), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{phonePN.String()}}}}
					fresh := fixture.Data.Conversations[0].Messages[1].Message
					fresh.MessageTimestamp = proto.Uint64(1600000000)
					fresh.Participant = proto.String(phonePN.String())
					fresh.Message = &waE2E.Message{Conversation: proto.String("Fresh historical row")}
					plain, err := proto.Marshal(fixture.Data)
					if err != nil {
						t.Fatal(err)
					}
					bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
					message := &waE2E.Message{MessageHistoryBundle: bundle}
					if history {
						outer := shareHistoryFixture(1)
						outer.Data.Conversations[0].Messages[0].Message.Message = message
						outer.Data.Conversations[0].Messages[0].Message.MessageTimestamp = proto.Uint64(1700000000)
						b.handleHistorySync(outer)
					} else {
						event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
						event.Message = message
						b.handleMessage(event)
					}
					after := shareArchiveSnapshot(t, ms, "H0", chat)
					if before == "" || after != before {
						t.Errorf("peer overwrote authoritative row: before=%s after=%s", before, after)
					}
					var count, polls int
					var activity, content string
					if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if err := ms.db.QueryRow("SELECT COUNT(*) FROM polls WHERE chat_jid=? AND message_id='H0'", chat).Scan(&polls); err != nil {
						t.Fatal(err)
					}
					if err := ms.db.QueryRow("SELECT CAST(last_message_time AS TEXT) FROM chats WHERE jid=?", chat).Scan(&activity); err != nil {
						t.Fatal(err)
					}
					if err := ms.db.QueryRow("SELECT content FROM messages WHERE chat_jid=? AND id='H1'", chat).Scan(&content); err != nil {
						t.Fatal(err)
					}
					wantHistory := int64(1)
					if phoneHistory {
						wantHistory++
					}
					if count != 2 || polls != 0 || content != "Fresh historical row" || activity != dbTime(stamp) || b.metrics.historyMessages.Load() != wantHistory || requests.Load() != 1 {
						t.Errorf("overlap affected side tables/chat/count: rows=%d polls=%d content=%q activity=%q history=%d HTTP=%d", count, polls, content, activity, b.metrics.historyMessages.Load(), requests.Load())
					}
					calls := 0
					edit := handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) error { calls++; return nil }, chatPolicy{}, b.storeLive)
					rec := httptest.NewRecorder()
					edit(rec, httptest.NewRequest(http.MethodPost, "/api/edit", strings.NewReader(`{"chat_jid":"120363000000000001@g.us","message_id":"H0","text":"Verified edit"}`)))
					if rec.Code != http.StatusOK || calls != 1 {
						t.Errorf("established own-message edit lost: status=%d edits=%d", rec.Code, calls)
					}
				})
			}
		}
	}
}
func TestHistoryShareLocationPolicyAcrossChunks(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "newest-first across chunks", true: "existing exact key"}[existing], func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			b.MediaAutoDownload = false
			chat := types.NewJID("120363000000000001", types.GroupServer)
			const initialTime = 1700000000
			first := &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{DegreesLatitude: proto.Float64(0.25), DegreesLongitude: proto.Float64(0.5), Caption: proto.String("Original location"), SequenceNumber: proto.Int64(0)}}
			if existing {
				event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
				event.Info.ID, event.Info.Timestamp, event.Message = "H0", time.Unix(initialTime, 0), first
				b.handleMessage(event)
			}
			fixture := shareHistoryFixture(historyBatchMessages + 1)
			for _, row := range fixture.Data.Conversations[0].Messages {
				row.Message.MessageTimestamp = proto.Uint64(initialTime - 10)
			}
			latest := fixture.Data.Conversations[0].Messages[0].Message
			latest.MessageTimestamp = proto.Uint64(initialTime + 60)
			latest.Message = &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{DegreesLatitude: proto.Float64(0.8), DegreesLongitude: proto.Float64(0.5), Caption: proto.String("Later location"), SequenceNumber: proto.Int64(3)}}
			original := fixture.Data.Conversations[0].Messages[historyBatchMessages].Message
			original.Key.ID, original.MessageTimestamp, original.Message = proto.String("H0"), proto.Uint64(initialTime), first
			plain, err := proto.Marshal(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			event.Message = &waE2E.Message{MessageHistoryBundle: bundle}
			b.handleMessage(event)
			var content, raw string
			var stamp, activity time.Time
			var count int
			if err := ms.db.QueryRow("SELECT content,timestamp,location FROM messages WHERE id='H0' AND chat_jid=?", chat.String()).Scan(&content, &stamp, &raw); err != nil {
				t.Fatal(err)
			}
			var position messageLocation
			if err := json.Unmarshal([]byte(raw), &position); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid=?", chat.String()).Scan(&activity); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != historyBatchMessages || !stamp.Equal(time.Unix(initialTime, 0)) || !activity.Equal(stamp) || position.Latitude == nil || *position.Latitude != 0.8 || position.Sequence == nil || *position.Sequence != 3 || position.Comment != "Original location" || !strings.Contains(content, "Original location") || requests.Load() != 1 {
				t.Fatalf("shared samples lost canonical position/origin: count=%d content=%q stamp=%v activity=%v position=%s HTTP=%d", count, content, stamp, activity, raw, requests.Load())
			}
		})
	}
}

func TestHistoryShareInterveningAuthority(t *testing.T) {
	for _, authority := range []string{"live", "phone history", "edit"} {
		t.Run(authority, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			self := types.NewJID("5511888888888", types.DefaultUserServer)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), ms, testLogger())
			b.MediaAutoDownload = false
			chat := types.NewJID("120363000000000001", types.GroupServer)
			fixture := shareHistoryFixture(historyBatchMessages + 1)
			for _, row := range fixture.Data.Conversations[0].Messages {
				row.Message.MessageTimestamp = proto.Uint64(1600000000)
				row.Message.Participant = proto.String(self.String())
			}
			overlap := fixture.Data.Conversations[0].Messages[historyBatchMessages].Message
			overlap.Key.ID, overlap.MessageTimestamp = proto.String("H0"), proto.Uint64(1900000000)
			overlap.Message = &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("Stale peer replacement"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Alice")}, {OptionName: proto.String("Bob")}}, SelectableOptionsCount: proto.Uint32(1)}}
			attempts := 0
			var before string
			b.historyBatchWriter = func(fn func(*messageBatch) error) error {
				attempts++
				if attempts == 2 {
					message := &waE2E.Message{Conversation: proto.String("Authoritative intervening text")}
					switch authority {
					case "live":
						event := buildTextMessage(chat, self, types.EmptyJID, types.EmptyJID, true, "")
						event.Info.ID, event.Info.Timestamp, event.Message = "H0", time.Unix(1700000000, 0), message
						b.handleMessage(event)
					case "phone history":
						own := shareHistoryFixture(1)
						own.Data.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(true)
						own.Data.Conversations[0].Messages[0].Message.MessageTimestamp = proto.Uint64(1700000000)
						own.Data.Conversations[0].Messages[0].Message.Message = message
						b.handleHistorySync(own)
					case "edit":
						calls := 0
						handler := handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) error { calls++; return nil }, chatPolicy{}, b.storeLive)
						rec := httptest.NewRecorder()
						handler(rec, httptest.NewRequest(http.MethodPost, "/api/edit", strings.NewReader(`{"chat_jid":"120363000000000001@g.us","message_id":"H0","text":"Authoritative intervening text"}`)))
						if rec.Code != http.StatusOK || calls != 1 {
							t.Fatalf("intervening edit status=%d calls=%d", rec.Code, calls)
						}
					}
					before = shareArchiveSnapshot(t, ms, "H0", chat.String())
				}
				return ms.Batch(fn)
			}
			plain, err := proto.Marshal(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, _ := encryptedShareServer(t, b, compressShare(t, plain), "")
			event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			event.Message = &waE2E.Message{MessageHistoryBundle: bundle}
			b.handleMessage(event)
			after := shareArchiveSnapshot(t, ms, "H0", chat.String())
			if before == "" || before != after {
				t.Errorf("peer overwrote intervening %s: before=%s after=%s", authority, before, after)
			}
			var polls int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM polls WHERE chat_jid=? AND message_id='H0'", chat.String()).Scan(&polls); err != nil {
				t.Fatal(err)
			}
			if polls != 0 {
				t.Errorf("stale overlap created auxiliary poll: %d", polls)
			}
		})
	}
}
