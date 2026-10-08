package main

import (
	"context"
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

func TestPhoneLocationInitialTimestampAuthor(t *testing.T) {
	for _, author := range []string{"same", "sender", "namespace", "from-me", "verified-alias"} {
		for _, mode := range []string{"same-chunk", "across-chunks", "committed-prefix"} {
			t.Run(author+"/"+mode, func(t *testing.T) {
				ms := newTestMessageStore(t)
				lids := &mockLIDStore{}
				if author == "verified-alias" {
					lids.pnByLID = map[types.JID]types.JID{phoneLID: phonePN}
				}
				b := testBridge(t, newTestClient(lids), ms, testLogger())
				size := 2
				if mode != "same-chunk" {
					size = historyBatchMessages + 1
				}
				fixture := shareHistoryFixture(size)
				conversation := fixture.Data.Conversations[0]
				conversation.UnreadCount = proto.Uint32(0)
				for _, row := range conversation.Messages {
					row.Message.MessageTimestamp = proto.Uint64(1699999000)
				}
				positive := conversation.Messages[0].Message
				positive.Participant, positive.MessageTimestamp, positive.Message = proto.String(phonePN.String()), proto.Uint64(1900000000), livePosition(3, 0.9)
				initial := conversation.Messages[size-1].Message
				initial.Key.ID, initial.Participant, initial.MessageTimestamp, initial.Message = positive.Key.ID, proto.String(phonePN.String()), proto.Uint64(1700000000), livePosition(0, 0.25)
				switch author {
				case "sender":
					initial.Participant = proto.String(selfPhone.String())
				case "namespace":
					initial.Participant = proto.String(phonePN.User + "@lid")
				case "from-me":
					initial.Key.FromMe = proto.Bool(true)
				case "verified-alias":
					initial.Participant = proto.String(phoneLID.String())
				}
				if mode == "committed-prefix" {
					chunks := 0
					b.historyBatchWriter = func(fn func(*messageBatch) error) error {
						chunks++
						if chunks == 2 {
							b.cancel()
							return context.Canceled
						}
						return ms.Batch(fn)
					}
				}
				b.handleHistorySync(fixture)
				want := int64(1900000000)
				if author == "same" || author == "verified-alias" {
					want = 1700000000
				}
				var activity, read, saved time.Time
				var sender, server string
				var own bool
				if err := ms.db.QueryRow("SELECT last_message_time,last_read_time FROM chats WHERE jid=?", conversation.GetID()).Scan(&activity, &read); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT timestamp,sender,sender_server,is_from_me FROM messages WHERE id='H0' AND chat_jid=?", conversation.GetID()).Scan(&saved, &sender, &server, &own); err != nil {
					t.Fatal(err)
				}
				if !saved.Equal(time.Unix(want, 0)) || !activity.Equal(saved) || !read.Equal(saved) || sender != phonePN.User || server != types.DefaultUserServer || own {
					t.Fatalf("initial sample crossed author: saved=%v activity=%v read=%v author=%s/%s own=%t", saved, activity, read, sender, server, own)
				}
			})
		}
	}
}

func TestPhoneHistoryMarkersExcludeIntraChunkCollision(t *testing.T) {
	for _, kind := range []string{"text", "static", "live"} {
		t.Run(kind, func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			const chat = "120363000000000001@g.us"
			base := time.Unix(1700000000, 0)
			if err := ms.StoreChat(chat, "group", base.Add(150*time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := ms.MarkChatRead(chat, base.Add(100*time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage("UNREAD", chat, selfPhone.String(), "still unread", base.Add(150*time.Second), false, "", "", "", nil, nil, nil, nil, ""); err != nil {
				t.Fatal(err)
			}
			fixture := shareHistoryFixture(3)
			conversation := fixture.Data.Conversations[0]
			conversation.UnreadCount = proto.Uint32(0)
			timestamps := []uint64{1700000300, 1700000200, 1700000100}
			for i, row := range conversation.Messages {
				row.Message.Key.ID = proto.String("H0")
				row.Message.Participant = proto.String(phonePN.String())
				row.Message.MessageTimestamp = proto.Uint64(timestamps[i])
			}
			conversation.Messages[0].Message.Message = livePosition(3, 0.9)
			collision := conversation.Messages[1].Message
			collision.Participant = proto.String(selfPhone.String())
			collision.Message = &waE2E.Message{Conversation: proto.String("foreign key claim")}
			switch kind {
			case "static":
				collision.Message = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(0.5), DegreesLongitude: proto.Float64(0.5)}}
			case "live":
				collision.Message = livePosition(4, 0.5)
			}
			conversation.Messages[2].Message.Message = livePosition(0, 0.25)
			b.handleHistorySync(fixture)
			var activity, read, saved time.Time
			if err := ms.db.QueryRow("SELECT last_message_time,last_read_time FROM chats WHERE jid=?", chat).Scan(&activity, &read); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT timestamp FROM messages WHERE id='H0' AND chat_jid=?", chat).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			var unread int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages JOIN chats ON chats.jid=messages.chat_jid WHERE chat_jid=? AND timestamp>last_read_time", chat).Scan(&unread); err != nil {
				t.Fatal(err)
			}
			if !activity.Equal(base.Add(150*time.Second)) || !read.Equal(saved) || !saved.Equal(base.Add(100*time.Second)) || unread != 1 {
				t.Fatalf("rejected intra-chunk row advanced markers: activity=%v read=%v saved=%v unread=%d", activity, read, saved, unread)
			}
		})
	}
}
