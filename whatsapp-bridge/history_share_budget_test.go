package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Wait for accepted jobs to finish, leaving the worker available for replays.
// The timeout is only a deadlock diagnostic; successful tests use no clock wait.
func waitHistoryShares(t *testing.T, b *Bridge) {
	t.Helper()
	b.historyShareMu.Lock()
	q := b.historyShares
	b.historyShareMu.Unlock()
	if q == nil {
		return
	}
	select {
	case <-q.idleSignal():
	case <-time.After(10 * time.Second):
		t.Fatal("history share jobs did not finish")
	}
}

func waitShareSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("history share checkpoint not reached")
	}
}

func TestHistoryShareBackgroundBudgetAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "one active one waiting overflow refused", true: "shutdown joins active and refuses waiting"}[shutdown], func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			plain, err := proto.Marshal(shareHistoryFixture(1).Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			download := b.historyShareDownload
			entered, release := make(chan struct{}), make(chan struct{})
			var calls, active atomic.Int32
			b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
				if active.Add(1) != 1 {
					t.Error("history share downloads overlapped")
				}
				defer active.Add(-1)
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return download(ctx, notif, file)
			}
			message := &waE2E.Message{MessageHistoryBundle: bundle}
			b.handleHistoryShare(message, "120363000000000001@g.us", "SHARE", false)
			waitShareSignal(t, entered)
			// The event path has returned while network work is still blocked.
			live := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "live while share waits")
			b.handleMessage(live)
			b.handleHistoryShare(message, "120363000000000001@g.us", "SHARE", false)
			b.handleHistoryShare(message, "120363000000000001@g.us", "SHARE", false)
			if strings.Count(rec.String(), "queue full") != 1 || calls.Load() != 1 {
				t.Fatalf("overflow budget lost: calls=%d log=%s", calls.Load(), rec.String())
			}
			if shutdown {
				b.Shutdown(5 * time.Second)
				if calls.Load() != 1 || active.Load() != 0 || requests.Load() != 0 || strings.Count(rec.String(), "Shared history import failed:") != 3 {
					t.Fatalf("shutdown did not join/refuse both jobs: calls=%d active=%d HTTP=%d log=%s", calls.Load(), active.Load(), requests.Load(), rec.String())
				}
			} else {
				close(release)
				waitHistoryShares(t, b)
				if calls.Load() != 2 || active.Load() != 0 || requests.Load() != 2 {
					t.Fatalf("accepted jobs lost: calls=%d active=%d HTTP=%d", calls.Load(), active.Load(), requests.Load())
				}
			}
			var liveRows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE content='live while share waits'").Scan(&liveRows); err != nil || liveRows != 1 {
				t.Fatalf("event path stalled/lost live row: count=%d err=%v", liveRows, err)
			}
		})
	}
}

func TestHistoryShareExpiredJobWarnsBeforeHTTP(t *testing.T) {
	ms := newTestMessageStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	b.historyShareJobTimeout = -time.Second
	bundle, requests := encryptedShareServer(t, b, compressShare(t, []byte{}), "")
	b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
	waitHistoryShares(t, b)
	if requests.Load() != 0 || strings.Count(rec.String(), "Shared history import failed:") != 1 {
		t.Fatalf("expired job not warned exactly once: HTTP=%d log=%s", requests.Load(), rec.String())
	}
}

// Build attacker-controlled wire without allocating the attacker's proto tree.
func shareWireRows(count int) []byte {
	desc := (*waHistorySync.Conversation)(nil).ProtoReflect().Descriptor()
	field := desc.Fields().ByName("messages").Number()
	var conv []byte
	for range count {
		conv = protowire.AppendTag(conv, field, protowire.BytesType)
		conv = protowire.AppendBytes(conv, nil)
	}
	field = (*waHistorySync.HistorySync)(nil).ProtoReflect().Descriptor().Fields().ByName("conversations").Number()
	wire := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(wire, conv)
}

func TestHistoryShareWirePreflightBoundsBeforeProtoAllocation(t *testing.T) {
	for _, count := range []int{historyShareMessageLimit, historyShareMessageLimit + 1, 1000000} {
		wire := shareWireRows(count)
		counts, err := scanHistoryShareWire(t.Context(), wire)
		if count == historyShareMessageLimit {
			if err != nil || counts.messages != count {
				t.Fatalf("exact message cap: counts=%+v err=%v", counts, err)
			}
		} else if err == nil || counts.messages != historyShareMessageLimit+1 {
			t.Fatalf("scanner did not stop at bounded count: counts=%+v err=%v", counts, err)
		}
		allocs := testing.AllocsPerRun(3, func() { _, _ = scanHistoryShareWire(t.Context(), wire) })
		if allocs > 20 {
			t.Fatalf("wire scan allocated attacker-controlled objects: %.0f allocations", allocs)
		}
		t.Logf("wire rows=%d inspected=%d fields=%d allocations=%.0f", count, counts.messages, counts.elements, allocs)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := scanHistoryShareWire(ctx, shareWireRows(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("wire scanner ignored cancellation: %v", err)
	}
}

func TestHistoryShareOversizedWireRefusedWithWarnAndNoRows(t *testing.T) {
	for _, shape := range []string{"inflated bytes", "message count", "status message count", "repeated metadata", "packed scalars"} {
		t.Run(shape, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			wire := shareWireRows(historyShareMessageLimit + 1)
			switch shape {
			case "inflated bytes":
				wire = bytes.Repeat([]byte{0}, historyShareInflatedLimit+1)
			case "status message count":
				field := (*waHistorySync.HistorySync)(nil).ProtoReflect().Descriptor().Fields().ByName("statusV3Messages").Number()
				wire = nil
				for range historyShareMessageLimit + 1 {
					wire = protowire.AppendTag(wire, field, protowire.BytesType)
					wire = protowire.AppendBytes(wire, nil)
				}
				counts, err := scanHistoryShareWire(t.Context(), wire)
				if err == nil || counts.messageInfos != historyShareMessageLimit+1 {
					t.Fatalf("non-conversation message objects unbounded: %+v err=%v", counts, err)
				}
			case "repeated metadata":
				field := (*waHistorySync.HistorySync)(nil).ProtoReflect().Descriptor().Fields().ByName("conversations").Number()
				wire = nil
				for range historyShareElementLimit + 1 {
					wire = protowire.AppendTag(wire, field, protowire.BytesType)
					wire = protowire.AppendBytes(wire, nil)
				}
			case "packed scalars":
				fixture := shareHistoryFixture(1)
				fixture.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: &waE2E.ContextInfo{ExperienceIDs: make([]uint32, historyShareElementLimit+1)}}}
				var err error
				wire, err = proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, wire), "")
			b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
			waitHistoryShares(t, b)
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 0 || requests.Load() != 1 || strings.Count(rec.String(), "Shared history import failed:") != 1 {
				t.Fatalf("oversized wire wrote rows or lacked WARN: rows=%d HTTP=%d err=%v log=%s", rows, requests.Load(), err, rec.String())
			}
		})
	}
}

type historyShareFailingLIDs struct {
	mockLIDStore
	err error
}

func (s *historyShareFailingLIDs) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.EmptyJID, s.err
}

func TestHistoryShareLookupFailureAndCancellationWarnOnce(t *testing.T) {
	for _, failure := range []error{errors.New("private lookup failure"), context.Canceled} {
		ms := newTestMessageStore(t)
		rec := installRecordingLogger(t)
		b := testBridge(t, newTestClient(&historyShareFailingLIDs{err: failure}), ms, rec)
		fixture := shareHistoryFixture(1)
		fixture.Data.Conversations[0].Messages[0].Message.Participant = proto.String(phoneLID.String())
		plain, err := proto.Marshal(fixture.Data)
		if err != nil {
			t.Fatal(err)
		}
		bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
		b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
		waitHistoryShares(t, b)
		var rows int
		if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 0 || requests.Load() != 1 || strings.Count(rec.String(), "Shared history import failed:") != 1 || strings.Contains(rec.String(), "private lookup failure") {
			t.Fatalf("lookup failure silent or persisted rows: rows=%d HTTP=%d err=%v log=%s", rows, requests.Load(), err, rec.String())
		}
		b.Shutdown(5 * time.Second)
	}
}

func TestHistoryShareOwnPhoneRowsPersistBeforeQueuedDownload(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	plain, err := proto.Marshal(shareHistoryFixture(1).Data)
	if err != nil {
		t.Fatal(err)
	}
	bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
	download := b.historyShareDownload
	b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
		var rows int
		if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='H1' AND chat_jid='120363000000000001@g.us'").Scan(&rows); err != nil || rows != 1 {
			t.Errorf("own-phone canonical rows not written before HTTP: rows=%d err=%v", rows, err)
		}
		return download(ctx, notif, file)
	}
	outer := shareHistoryFixture(2)
	outer.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{MessageHistoryBundle: bundle}
	b.handleHistorySync(outer)
	waitHistoryShares(t, b)
	if requests.Load() != 1 || b.metrics.groupHistoryShares.Load() != 1 {
		t.Fatalf("queue-only pass re-recognised or lost share: HTTP=%d shares=%d", requests.Load(), b.metrics.groupHistoryShares.Load())
	}
}

func TestHistoryShareCancellationStopsCanonicalChunks(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	plain, err := proto.Marshal(shareHistoryFixture(1001).Data)
	if err != nil {
		t.Fatal(err)
	}
	bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
	// Inject cancellation at the second canonical transaction, without sleeping
	// or replacing the real HTTP download or SQLite writer.
	b.historyShares = newHistoryShareQueue(b.ctx, func(ctx context.Context, job historyShareJob) {
		jobCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		batches := 0
		b.historyBatchWriter = func(write func(*messageBatch) error) error {
			batches++
			if batches == 2 {
				cancel()
			}
			return ms.Batch(write)
		}
		b.runHistoryShare(jobCtx, job)
	})
	b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
	waitHistoryShares(t, b)
	var rows int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != historyBatchMessages || requests.Load() != 1 || strings.Count(rec.String(), "Shared history import failed:") != 1 {
		t.Fatalf("cancelled canonical import continued: rows=%d HTTP=%d err=%v log=%s", rows, requests.Load(), err, rec.String())
	}
}
