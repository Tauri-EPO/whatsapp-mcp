package main

import (
	"fmt"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func largeHistoryFixture(rows int) *events.HistorySync {
	conversation := &waHistorySync.Conversation{ID: proto.String(phonePN.String())}
	for row := 0; row < rows; row++ {
		conversation.Messages = append(conversation.Messages, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(fmt.Sprintf("H%d", row)), FromMe: proto.Bool(false)},
			MessageTimestamp: proto.Uint64(1772359200),
			Message:          &waE2E.Message{Conversation: proto.String("history searchable")},
		}})
	}
	return &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conversation}}}
}
