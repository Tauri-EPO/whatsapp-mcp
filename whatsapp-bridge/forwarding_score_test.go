package main

import (
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestForwardingScoreExtractionAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  *waE2E.ContextInfo
		want sql.NullInt64
	}{
		{"absent", nil, sql.NullInt64{}},
		{"not_forwarded", &waE2E.ContextInfo{IsForwarded: proto.Bool(false), ForwardingScore: proto.Uint32(0)}, sql.NullInt64{}},
		{"flag_only", &waE2E.ContextInfo{IsForwarded: proto.Bool(true)}, sql.NullInt64{Int64: 0, Valid: true}},
		{"count_only", &waE2E.ContextInfo{ForwardingScore: proto.Uint32(4)}, sql.NullInt64{Int64: 4, Valid: true}},
		{"both", &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(3)}, sql.NullInt64{Int64: 3, Valid: true}},
	} {
		for _, family := range []string{"text", "image", "audio", "document", "video", "sticker", "wrapped"} {
			t.Run(tc.name+"/"+family, func(t *testing.T) {
				m := &waE2E.Message{}
				switch family {
				case "text", "wrapped":
					m.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String("forward"), ContextInfo: tc.ctx}
				case "image":
					m.ImageMessage = &waE2E.ImageMessage{Caption: proto.String("forward"), ContextInfo: tc.ctx}
				case "audio":
					m.AudioMessage = &waE2E.AudioMessage{ContextInfo: tc.ctx}
				case "document":
					m.DocumentMessage = &waE2E.DocumentMessage{ContextInfo: tc.ctx}
				case "video":
					m.VideoMessage = &waE2E.VideoMessage{ContextInfo: tc.ctx}
				case "sticker":
					m.StickerMessage = &waE2E.StickerMessage{ContextInfo: tc.ctx}
				}
				if family == "wrapped" {
					m = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: m}}
				}
				ms := newTestMessageStore(t)
				chat := "5511999999999@s.whatsapp.net"
				ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
				if err := ms.StoreChat(chat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				e := extractMessage(m, ts, "FORWARD")
				if err := persistMessage(ms, "FORWARD", chat, chat, ts, false, e, true, testLogger()); err != nil {
					t.Fatal(err)
				}
				var got sql.NullInt64
				if err := ms.db.QueryRow("SELECT forwarding_score FROM messages WHERE id=? AND chat_jid=?", "FORWARD", chat).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Fatalf("score=%+v want=%+v", got, tc.want)
				}
				// A later partial replay must never erase forwarding evidence.
				e.forwardingScore = nil
				if err := persistMessage(ms, "FORWARD", chat, chat, ts, false, e, true, testLogger()); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT forwarding_score FROM messages WHERE id=? AND chat_jid=?", "FORWARD", chat).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Fatalf("replay erased score: %+v", got)
				}
			})
		}
	}
}

func TestHistorySyncForwardingScoreAndMigration(t *testing.T) {
	ms := newTestMessageStore(t)
	// Model an existing store upgrading, then repeat startup migration.
	if _, err := ms.db.Exec("ALTER TABLE messages DROP COLUMN forwarding_score"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ensureMessageStoreSchema(ms.db); err != nil {
			t.Fatal(err)
		}
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	fixture := largeHistoryFixture(1)
	fixture.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("history forward"), ContextInfo: &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(4)}}}}}
	b.handleHistorySync(fixture)
	var score int
	if err := ms.db.QueryRow("SELECT forwarding_score FROM messages WHERE id=? AND chat_jid=?", "H0", phonePN.String()).Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 4 {
		t.Fatalf("history score=%d", score)
	}
}
