package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type peerDeadlineLIDs struct {
	mockLIDStore
	bounded, unbounded atomic.Int32
}

func (s *peerDeadlineLIDs) GetPNForLID(ctx context.Context, _ types.JID) (types.JID, error) {
	if _, bounded := ctx.Deadline(); bounded {
		s.bounded.Add(1)
	} else {
		s.unbounded.Add(1)
	}
	return types.EmptyJID, nil
}

func TestPeerLocationAttributionUsesOnlyJobDeadline(t *testing.T) {
	ms := newTestMessageStore(t)
	lids := &peerDeadlineLIDs{}
	b := testBridge(t, newTestClient(lids), ms, testLogger())
	const chat = "120363000000000001@g.us"
	stamp := time.Unix(1700000000, 0)
	if err := ms.StoreChat(chat, "group", stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("H0", chat, phonePN.String(), "Original", stamp, false, "", "", "", nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	before := shareArchiveSnapshot(t, ms, "H0", chat)
	fixture := shareHistoryFixture(1)
	row := fixture.Data.Conversations[0].Messages[0].Message
	row.Participant = proto.String("100000000000009@lid")
	row.Message = livePosition(0, 0.9)
	plain, err := proto.Marshal(fixture.Data)
	if err != nil {
		t.Fatal(err)
	}
	bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
	b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, chat, "SHARE", false)
	waitHistoryShares(t, b)
	if lids.bounded.Load() != 1 || lids.unbounded.Load() != 0 || requests.Load() != 1 || before != shareArchiveSnapshot(t, ms, "H0", chat) || b.metrics.historyMessages.Load() != 0 {
		t.Fatalf("peer lookup escaped job deadline or changed archive: bounded=%d unbounded=%d HTTP=%d imported=%d", lids.bounded.Load(), lids.unbounded.Load(), requests.Load(), b.metrics.historyMessages.Load())
	}
}
