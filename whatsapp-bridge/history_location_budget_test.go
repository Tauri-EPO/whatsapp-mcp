package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type markerBudgetLIDs struct {
	mockLIDStore
	lookups atomic.Int32
}

func (s *markerBudgetLIDs) GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	s.lookups.Add(1)
	return s.mockLIDStore.GetPNForLID(ctx, lid)
}

func TestPhoneLocationMarkerLookupBudgetPerChunk(t *testing.T) {
	ms := newTestMessageStore(t)
	lids := &markerBudgetLIDs{mockLIDStore: mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}}}
	b := testBridge(t, newTestClient(lids), ms, testLogger())
	fixture := shareHistoryFixture(historyBatchMessages + 1)
	const chat = "120363000000000001@g.us"
	stamp := time.Unix(1700000000, 0)
	if err := ms.StoreChat(chat, "group", stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.Batch(func(batch *messageBatch) error {
		for _, row := range fixture.Data.Conversations[0].Messages {
			info := row.Message
			info.Participant, info.MessageTimestamp, info.Message = proto.String(phonePN.String()), proto.Uint64(1700000060), livePosition(0, 0.9)
			if err := persistMessage(batch, info.Key.GetID(), chat, phoneLID.String(), stamp, false, extractMessage(livePosition(0, 0.25), stamp, info.Key.GetID()), false, testLogger()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	chunks := 0
	b.historyBatchWriter = func(fn func(*messageBatch) error) error {
		chunks++
		before := lids.lookups.Load()
		err := ms.Batch(fn)
		lookups := lids.lookups.Load() - before
		// At most one marker alias lookup and one persistence alias lookup
		// for each row covered by this bounded IMMEDIATE transaction.
		if lookups > 2*historyBatchMessages {
			t.Errorf("transaction %d inspected beyond its chunk: alias lookups=%d", chunks, lookups)
		}
		return err
	}
	b.handleHistorySync(fixture)
	if chunks != 2 || queryMessageCount(ms, chat) != historyBatchMessages+1 || b.metrics.historyMessages.Load() != historyBatchMessages+1 {
		t.Fatalf("bounded phone chunks changed: chunks=%d rows=%d imported=%d", chunks, queryMessageCount(ms, chat), b.metrics.historyMessages.Load())
	}
}
