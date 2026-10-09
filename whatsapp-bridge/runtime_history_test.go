package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeHandoffHistoryAndLocationConsumers(t *testing.T) {
	firstID := types.NewJID("5511888888888", types.DefaultUserServer)
	for _, path := range []string{"own history", "shared history", "live location"} {
		t.Run(path, func(t *testing.T) {
			ms := newTestMessageStore(t)
			first := newTestClientWithSelf(&mockLIDStore{}, firstID)
			second := newTestClientWithSelf(&mockLIDStore{
				pnByLID: map[types.JID]types.JID{phoneLID: phonePN},
				lidByPN: map[types.JID]types.JID{phonePN: phoneLID},
			}, phonePN)
			b := testBridge(t, first, ms, testLogger())
			b.MediaAutoDownload = false
			reconnect := make(chan bool, 1)
			b.installClient(first, false, reconnect)
			b.installClient(second, false, reconnect)
			chat := "120363000000000001@g.us"
			stamp := time.Unix(1772359200, 0)
			switch path {
			case "own history":
				b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
					Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat),
						Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
							Key:              &waCommon.MessageKey{ID: proto.String("HANDOFF"), FromMe: proto.Bool(true)},
							MessageTimestamp: proto.Uint64(1772359200), Message: &waE2E.Message{Conversation: proto.String("own history")},
						}}},
					}},
				}})
				var sender string
				if err := ms.db.QueryRow("SELECT sender FROM messages WHERE id='HANDOFF'").Scan(&sender); err != nil || sender != phonePN.User {
					t.Fatalf("history retained retired account: sender=%s err=%v", sender, err)
				}
			case "shared history":
				fixture := shareHistoryFixture(1)
				fixture.Data.Conversations[0].Messages[0].Message.Participant = proto.String(phonePN.String())
				data, skipped, err := b.historyShareMessages(context.Background(), fixture.Data)
				if err != nil || skipped != 1 || len(data.Conversations[0].Messages) != 0 {
					t.Fatalf("peer bundle imported active account's own row: skipped=%d err=%v", skipped, err)
				}
			case "live location":
				if err := ms.StoreChat(chat, "Alice", stamp); err != nil {
					t.Fatal(err)
				}
				if err := persistMessage(ms, "HANDOFF", chat, phoneLID.String(), stamp, false, extractMessage(livePosition(0, 0.25), stamp, "HANDOFF"), true, testLogger()); err != nil {
					t.Fatal(err)
				}
				event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
				event.Info.ID, event.Info.Timestamp, event.Message = "HANDOFF", stamp.Add(time.Minute), livePosition(2, 0.75)
				b.handleMessage(event)
				var latitude float64
				if err := ms.db.QueryRow("SELECT json_extract(location,'$.latitude') FROM messages WHERE id='HANDOFF'").Scan(&latitude); err != nil || latitude != 0.75 {
					t.Fatalf("active LID map did not advance archived position: latitude=%v err=%v", latitude, err)
				}
			}
		})
	}
}
