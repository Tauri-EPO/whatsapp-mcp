package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestPeerHistoryTimestampDomain(t *testing.T) {
	for _, seconds := range []uint64{253402300799, 253402300800, 1 << 63, ^uint64(0)} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			ms := newTestMessageStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			fixture := shareHistoryFixture(2)
			// An ordinary timestamp in the same chunk must not mask an invalid
			// row when SQLite compares the fixed-width timestamp strings.
			fixture.Data.Conversations[0].Messages[1].Message.MessageTimestamp = proto.Uint64(seconds)
			plain, err := proto.Marshal(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			const chat = "120363000000000001@g.us"
			b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, chat, "SHARE", false)
			waitHistoryShares(t, b)
			rows := queryMessageCount(b.Store, chat)
			warns := strings.Count(rec.String(), "Shared history import failed:")
			if seconds == 253402300799 {
				var raw string
				if err := ms.db.QueryRow("SELECT CAST(timestamp AS TEXT) FROM messages WHERE id='H1' AND chat_jid=?", chat).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				parsed, err := parseDBTime(raw)
				if err != nil || parsed.Year() != 9999 || len(raw) != 25 || rows != 2 || warns != 0 {
					t.Fatalf("valid timestamp refused: rows=%d warns=%d raw=%q error=%v", rows, warns, raw, err)
				}
			} else if rows != 0 || warns != 1 || b.metrics.historyMessages.Load() != 0 {
				t.Fatalf("invalid timestamp reached archive: rows=%d warns=%d imported=%d", rows, warns, b.metrics.historyMessages.Load())
			}
			if requests.Load() != 1 {
				t.Fatalf("SDK requests=%d", requests.Load())
			}
		})
	}
}

func TestPhoneLocationMarkersUseTransactionalLiveRow(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	chat := types.NewJID("120363000000000001", types.GroupServer)
	stamp := time.Unix(1700000000, 0)
	if err := ms.StoreChat(chat.String(), "group", stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkChatRead(chat.String(), stamp); err != nil {
		t.Fatal(err)
	}
	fixture := shareHistoryFixture(1)
	conversation := fixture.Data.Conversations[0]
	conversation.UnreadCount = proto.Uint32(0)
	row := conversation.Messages[0].Message
	row.Participant, row.MessageTimestamp, row.Message = proto.String(phonePN.String()), proto.Uint64(1900000000), livePosition(3, 0.9)
	calls := 0
	b.historyBatchWriter = func(fn func(*messageBatch) error) error {
		calls++
		if calls == 1 {
			// Deliver the original through the actual live handler after history
			// setup, immediately before its canonical IMMEDIATE transaction.
			event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			event.Info.ID, event.Info.Timestamp, event.Message = "H0", stamp, livePosition(0, 0.25)
			b.handleMessage(event)
		}
		return ms.Batch(fn)
	}
	b.handleHistorySync(fixture)
	var activity, read, saved time.Time
	var location string
	if err := ms.db.QueryRow("SELECT last_message_time,last_read_time FROM chats WHERE jid=?", chat.String()).Scan(&activity, &read); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT timestamp,location FROM messages WHERE id='H0' AND chat_jid=?", chat.String()).Scan(&saved, &location); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !saved.Equal(stamp) || !activity.Equal(stamp) || !read.Equal(stamp) || !strings.Contains(location, `"latitude":0.9`) {
		t.Fatalf("live/history interleaving lost state: calls=%d saved=%v activity=%v read=%v location=%s", calls, saved, activity, read, location)
	}
}
