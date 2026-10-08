package main

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistoryShareRejectsForeignConversationBeforeAnyImport(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, shape := range []string{"foreign only", "mixed conversations", "empty conversation ID", "direct chat conversation", "foreign message key"} {
			t.Run(map[bool]string{false: "live", true: "history"}[history]+"/"+shape, func(t *testing.T) {
				ms, _ := lockedProductionStore(t)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				const chat = "120363000000000001@g.us"
				const foreign = "120363000000000002@g.us"
				timestamp := time.Unix(1700000000, 0)
				if err := ms.StoreChat(foreign, "Original", timestamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage("H0", foreign, phonePN.String(), "Original untouched", timestamp, false, "", "", "", nil, nil, nil, nil, ""); err != nil {
					t.Fatal(err)
				}
				fixture := shareHistoryFixture(1)
				fixture.Data.Conversations[0].ID = proto.String(chat)
				switch shape {
				case "foreign only":
					fixture.Data.Conversations[0].ID = proto.String(foreign)
				case "mixed conversations":
					other := proto.Clone(fixture.Data.Conversations[0]).(*waHistorySync.Conversation)
					other.ID = proto.String(foreign)
					fixture.Data.Conversations = append(fixture.Data.Conversations, other)
				case "empty conversation ID":
					fixture.Data.Conversations[0].ID = proto.String("")
				case "direct chat conversation":
					fixture.Data.Conversations[0].ID = proto.String(phonePN.String())
				case "foreign message key":
					fixture.Data.Conversations[0].Messages[0].Message.Key.RemoteJID = proto.String(foreign)
				}
				plain, err := proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
				message := &waE2E.Message{MessageHistoryBundle: bundle}
				if history {
					outer := shareHistoryFixture(1)
					outer.Data.Conversations[0].ID = proto.String(chat)
					outer.Data.Conversations[0].Messages[0].Message.Message = message
					b.handleHistorySync(outer)
				} else {
					event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Message = message
					b.handleMessage(event)
				}
				var rows, indexed int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
					t.Fatal(err)
				}
				var content, storedTime, name, chatTime string
				if err := ms.db.QueryRow("SELECT content,CAST(timestamp AS TEXT) FROM messages WHERE id='H0' AND chat_jid=?", foreign).Scan(&content, &storedTime); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT name,CAST(last_message_time AS TEXT) FROM chats WHERE jid=?", foreign).Scan(&name, &chatTime); err != nil {
					t.Fatal(err)
				}
				if requests.Load() != 1 || rows != 1 || indexed != 0 || content != "Original untouched" || storedTime != dbTime(timestamp) || name != "Original" || chatTime != dbTime(timestamp) || b.metrics.historyMessages.Load() != 0 {
					t.Fatalf("foreign bundle changed archive: HTTP=%d rows=%d FTS=%d content=%q timestamp=%q name=%q chat_time=%q history=%d", requests.Load(), rows, indexed, content, storedTime, name, chatTime, b.metrics.historyMessages.Load())
				}
				if !strings.Contains(rec.String(), "Shared history import failed: conversation scope refused") {
					t.Fatalf("scope refusal missing: %s", rec.String())
				}
			})
		}
	}
}

func TestHistoryShareRejectsNonGroupOriginBeforeDownload(t *testing.T) {
	for _, chat := range []string{phonePN.String(), "status@broadcast", "@g.us", "120363000000000001:3@g.us", "120363000000000001@g.us@lid"} {
		t.Run(chat, func(t *testing.T) {
			ms := newTestMessageStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			plain, err := proto.Marshal(shareHistoryFixture(1).Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, chat, "SHARE", false)
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 0 || rows != 0 || !strings.Contains(rec.String(), "Shared history import failed: originating group scope refused") {
				t.Fatalf("invalid origin accepted: HTTP=%d rows=%d log=%s", requests.Load(), rows, rec.String())
			}
		})
	}
}
