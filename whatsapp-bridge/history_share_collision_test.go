package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestPeerHistoryJobCancellationStopsBetweenChunks(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	attempts := 0
	b.historyBatchWriter = func(write func(*messageBatch) error) error {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return ms.Batch(write)
	}
	b.handleHistorySyncWithSharesContext(ctx, shareHistoryFixture(historyBatchMessages*3), false, true)
	var count int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || count != historyBatchMessages || b.metrics.historyMessages.Load() != historyBatchMessages || b.metrics.storeFailures.Load() != 0 || b.ctx.Err() != nil {
		t.Fatalf("peer exceeded cancellation boundary or stopped bridge: attempts=%d rows=%d imported=%d failures=%d lifecycle=%v", attempts, count, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load(), b.ctx.Err())
	}
	var activity time.Time
	if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid='120363000000000001@g.us'").Scan(&activity); err != nil || !activity.Equal(time.Unix(1772359200, 0)) {
		t.Fatalf("committed cancelled chunk lost activity: %v err=%v", activity, err)
	}
	// A replay that skips all keys already committed must retain their activity.
	b.historyBatchWriter = nil
	b.handleHistorySyncWithShares(shareHistoryFixture(historyBatchMessages), false, true)
	var replayActivity time.Time
	if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid='120363000000000001@g.us'").Scan(&replayActivity); err != nil || !replayActivity.Equal(activity) {
		t.Fatalf("replay lost committed activity: %v err=%v", replayActivity, err)
	}
}

func TestPeerHistoryActivityFailureRollsBackRows(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	if _, err := ms.db.Exec(`CREATE TRIGGER reject_peer_activity BEFORE UPDATE OF last_message_time ON chats
		WHEN NEW.last_message_time IS NOT NULL BEGIN SELECT RAISE(ABORT,'marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	b.handleHistorySyncWithShares(shareHistoryFixture(3), false, true)
	var count int
	var activity any
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid='120363000000000001@g.us'").Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if count != 0 || activity != nil || b.metrics.historyMessages.Load() != 0 || b.metrics.storeFailures.Load() != 3 {
		t.Fatalf("activity failure committed partial rows: rows=%d activity=%v imported=%d failures=%d", count, activity, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load())
	}
}

func TestPeerLocationCannotUpdateExistingAuthor(t *testing.T) {
	for _, author := range []string{"same sender", "other sender", "other namespace", "own message"} {
		t.Run(author, func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.MediaAutoDownload = false
			chat := types.NewJID("120363000000000001", types.GroupServer)
			sender, fromMe := phonePN, false
			switch author {
			case "other sender":
				sender = selfPhone
			case "other namespace":
				sender = types.NewJID(phonePN.User, types.HiddenUserServer)
			case "own message":
				sender, fromMe = selfPhone, true
			}
			initial := buildTextMessage(chat, sender, types.EmptyJID, types.EmptyJID, fromMe, "")
			initial.Info.ID, initial.Info.Timestamp = "H0", time.Unix(1700000000, 0)
			initial.Message = &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{DegreesLatitude: proto.Float64(0.25), DegreesLongitude: proto.Float64(0.5), SequenceNumber: proto.Int64(0), Caption: proto.String("Original location"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIGINAL_QUOTE")}}}
			b.handleMessage(initial)
			before := shareArchiveSnapshot(t, ms, "H0", chat.String())
			fixture := shareHistoryFixture(2)
			update := fixture.Data.Conversations[0].Messages[0].Message
			update.Participant, update.MessageTimestamp = proto.String(phonePN.String()), proto.Uint64(1900000000)
			update.Message = &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{DegreesLatitude: proto.Float64(0.9), DegreesLongitude: proto.Float64(0.1), SequenceNumber: proto.Int64(999), Caption: proto.String("Forged location")}}
			fresh := fixture.Data.Conversations[0].Messages[1].Message
			fresh.Participant, fresh.MessageTimestamp = proto.String(phonePN.String()), proto.Uint64(1600000000)
			plain, err := proto.Marshal(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			event.Message = &waE2E.Message{MessageHistoryBundle: bundle}
			b.handleMessage(event)
			waitHistoryShares(t, b)
			after := shareArchiveSnapshot(t, ms, "H0", chat.String())
			var count int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if before != after || count != 2 || b.metrics.historyMessages.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("peer location changed archive: before=%s after=%s rows=%d imported=%d HTTP=%d", before, after, count, b.metrics.historyMessages.Load(), requests.Load())
			}
		})
	}
}
