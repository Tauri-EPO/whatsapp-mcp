package main

import (
	"encoding/json"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestLocationVerifiedAliasRetainsOriginalAuthor(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, matched := range []bool{false, true} {
			t.Run(map[bool]string{false: "live", true: "phone history"}[history]+"/"+map[bool]string{false: "foreign mapping", true: "verified alias"}[matched], func(t *testing.T) {
				ms := newTestMessageStore(t)
				chat := types.NewJID("120363000000000001", types.GroupServer)
				stamp := time.Unix(1700000000, 0)
				if err := ms.StoreChat(chat.String(), "group", stamp); err != nil {
					t.Fatal(err)
				}
				if err := persistMessage(ms, "H0", chat.String(), phoneLID.String(), stamp, false, extractMessage(livePosition(0, 0.25), stamp, "H0"), true, testLogger()); err != nil {
					t.Fatal(err)
				}
				before := shareArchiveSnapshot(t, ms, "H0", chat.String())
				mappedPN := selfPhone
				if matched {
					mappedPN = phonePN
				}
				lids := &mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: mappedPN}}
				b := testBridge(t, newTestClient(lids), ms, testLogger())
				b.MediaAutoDownload = false
				if history {
					fixture := shareHistoryFixture(1)
					row := fixture.Data.Conversations[0].Messages[0].Message
					row.Participant, row.MessageTimestamp = proto.String(phonePN.String()), proto.Uint64(1700000060)
					row.Message = livePosition(3, 0.9)
					b.handleHistorySync(fixture)
				} else {
					event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Info.ID, event.Info.Timestamp, event.Message = "H0", stamp.Add(time.Minute), livePosition(3, 0.9)
					b.handleMessage(event)
				}
				if !matched && before != shareArchiveSnapshot(t, ms, "H0", chat.String()) {
					t.Fatal("foreign alias changed author or position")
				}
				var sender, server, quote, raw string
				var own bool
				var saved time.Time
				if err := ms.db.QueryRow("SELECT sender,sender_server,is_from_me,timestamp,quoted_message_id,location FROM messages WHERE id='H0' AND chat_jid=?", chat.String()).Scan(&sender, &server, &own, &saved, &quote, &raw); err != nil {
					t.Fatal(err)
				}
				var location messageLocation
				if err := json.Unmarshal([]byte(raw), &location); err != nil {
					t.Fatal(err)
				}
				want := 0.25
				if matched {
					want = 0.9
				}
				if sender != phoneLID.User || server != types.HiddenUserServer || own || !saved.Equal(stamp) || quote != "QUOTE" || location.Latitude == nil || *location.Latitude != want {
					t.Fatalf("alias/original metadata lost: sender=%s/%s own=%t stamp=%v quote=%q location=%s", sender, server, own, saved, quote, raw)
				}
			})
		}
	}
}

func TestPhoneLocationCollisionRetainsActivityAndReadMarker(t *testing.T) {
	for _, kind := range []string{"text", "static", "foreign live", "foreign initial"} {
		t.Run(kind, func(t *testing.T) {
			ms := newTestMessageStore(t)
			chat := types.NewJID("120363000000000001", types.GroupServer)
			stamp := time.Unix(1700000000, 0)
			if err := ms.StoreChat(chat.String(), "group", stamp); err != nil {
				t.Fatal(err)
			}
			if err := ms.MarkChatRead(chat.String(), stamp); err != nil {
				t.Fatal(err)
			}
			original, sender := &waE2E.Message{Conversation: proto.String("original text")}, phonePN
			switch kind {
			case "static":
				original = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(0.25), DegreesLongitude: proto.Float64(0.5)}}
			case "foreign live", "foreign initial":
				original, sender = livePosition(0, 0.25), selfPhone
			}
			if err := persistMessage(ms, "H0", chat.String(), sender.String(), stamp, false, extractMessage(original, stamp, "H0"), true, testLogger()); err != nil {
				t.Fatal(err)
			}
			before := shareArchiveSnapshot(t, ms, "H0", chat.String())
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			fixture := shareHistoryFixture(1)
			conversation := fixture.Data.Conversations[0]
			conversation.UnreadCount = proto.Uint32(0)
			row := conversation.Messages[0].Message
			sequence := int64(3)
			if kind == "foreign initial" {
				sequence = 0
			}
			row.Participant, row.MessageTimestamp, row.Message = proto.String(phonePN.String()), proto.Uint64(1900000000), livePosition(sequence, 0.9)
			b.handleHistorySync(fixture)
			var activity, read time.Time
			if err := ms.db.QueryRow("SELECT last_message_time,last_read_time FROM chats WHERE jid=?", chat.String()).Scan(&activity, &read); err != nil {
				t.Fatal(err)
			}
			if before != shareArchiveSnapshot(t, ms, "H0", chat.String()) || !activity.Equal(stamp) || !read.Equal(stamp) {
				t.Fatalf("consumed collision advanced state: activity=%v read=%v", activity, read)
			}
		})
	}
}
