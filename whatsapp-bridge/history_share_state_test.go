package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistorySharePreservesOwnReadAndEphemeralState(t *testing.T) {
	for _, route := range []string{"live share", "history share", "own phone history"} {
		t.Run(route, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			const chat = "120363000000000001@g.us"
			readAt := time.Unix(1700000000, 0)
			unreadAt := time.Unix(1772359199, 0)
			if err := ms.StoreChat(chat, "Original", unreadAt); err != nil {
				t.Fatal(err)
			}
			if err := ms.MarkChatRead(chat, readAt); err != nil {
				t.Fatal(err)
			}
			if err := ms.UpdateChatEphemeralSettings(chat, 86400, 1700000000); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage("UNREAD", chat, phonePN.String(), "Still unread", unreadAt, false, "", "", "", nil, nil, nil, nil, ""); err != nil {
				t.Fatal(err)
			}
			fixture := shareHistoryFixture(1)
			conv := fixture.Data.Conversations[0]
			conv.UnreadCount = proto.Uint32(0)
			conv.MarkedAsUnread = proto.Bool(false)
			conv.EphemeralExpiration = proto.Uint32(604800)
			conv.EphemeralSettingTimestamp = proto.Int64(1900000000)
			conv.Name = proto.String("Peer-owned name")
			if route == "own phone history" {
				b.handleHistorySync(fixture)
				waitHistoryShares(t, b)
			} else {
				plain, err := proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
				message := &waE2E.Message{MessageHistoryBundle: bundle}
				if route == "live share" {
					event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Message = message
					b.handleMessage(event)
					waitHistoryShares(t, b)
				} else {
					outer := shareHistoryFixture(1)
					outer.Data.Conversations[0].Messages[0].Message.Message = message
					b.handleHistorySync(outer)
					waitHistoryShares(t, b)
				}
				if requests.Load() != 1 {
					t.Fatalf("SDK HTTP downloads=%d", requests.Load())
				}
			}
			var rows, unread int
			var storedRead string
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid=?", chat).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT CAST(last_read_time AS TEXT) FROM chats WHERE jid=?", chat).Scan(&storedRead); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages m JOIN chats c ON c.jid=m.chat_jid WHERE m.chat_jid=? AND m.is_from_me=0 AND m.timestamp>c.last_read_time", chat).Scan(&unread); err != nil {
				t.Fatal(err)
			}
			settings, err := ms.GetChatEphemeralSettings(chat)
			if err != nil {
				t.Fatal(err)
			}
			if rows != 2 {
				t.Fatalf("message import lost rows: %d", rows)
			}
			if route == "own phone history" {
				if unread != 0 || storedRead != dbTime(time.Unix(1772359200, 0)) || settings.Expiration != 604800 || settings.SettingTimestamp != 1900000000 {
					t.Fatalf("ordinary phone metadata changed: unread=%d read=%s settings=%+v", unread, storedRead, settings)
				}
			} else if unread != 2 || storedRead != dbTime(readAt) || settings.Expiration != 86400 || settings.SettingTimestamp != 1700000000 {
				t.Fatalf("peer changed own account state: unread=%d read=%s settings=%+v", unread, storedRead, settings)
			}
		})
	}
}
