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

func TestHistoryCommitsChunksAndAllowsLiveWriteBetweenThem(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	const historyRows = 1001
	commits := 0
	b.historyBatchWriter = func(fn func(*messageBatch) error) error {
		if err := ms.Batch(fn); err != nil {
			return err
		}
		commits++
		if commits == 1 {
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 500 {
				t.Fatalf("first commit stored %d rows, want bounded 500", rows)
			}
			// This calls the real live event path after the first committed chunk,
			// before history is allowed to acquire the lock for its next chunk.
			live := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "live searchable")
			live.Info.ID = "LIVE1"
			b.handleMessage(live)
			if got := b.metrics.messagesStored.Load(); got != 1 {
				t.Fatalf("live writes stored=%d", got)
			}
		}
		return nil
	}
	b.handleHistorySync(largeHistoryFixture(historyRows))
	if commits != 3 {
		t.Fatalf("commits=%d want 3", commits)
	}
	var rows int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != historyRows+1 {
		t.Fatalf("indexed=%d want=%d", rows, historyRows+1)
	}
	if got := b.metrics.historyMessages.Load(); got != historyRows {
		t.Fatalf("history count=%d", got)
	}
	if got := b.metrics.storeFailures.Load(); got != 0 {
		t.Fatalf("store failures=%d", got)
	}
}

func TestHistoryRollbackCountsOnlyTheFailedChunk(t *testing.T) {
	for _, sideUpdate := range []bool{false, true} {
		t.Run(map[bool]string{false: "message insert", true: "mentions update"}[sideUpdate], func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms, err := NewMessageStore()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ms.Close() }()
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			// Roll back the second transaction after ten writes: earlier and later
			// chunks must remain committed, and the rolled-back writes must not count.
			triggerAction := "INSERT"
			if sideUpdate {
				triggerAction = "UPDATE OF mentions"
			}
			if _, err := ms.db.Exec(fmt.Sprintf("CREATE TRIGGER fail_history BEFORE %s ON messages WHEN new.id='H510' BEGIN SELECT RAISE(ROLLBACK,'simulated rollback');END", triggerAction)); err != nil {
				t.Fatal(err)
			}
			const total = 1501
			fixture := largeHistoryFixture(total)
			if sideUpdate {
				fixture.Data.Conversations[0].Messages[510].Message.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("history searchable"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{phonePN.String()}}}}
			}
			b.handleHistorySync(fixture)
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			var indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'history'").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if indexed != rows {
				t.Fatalf("index=%d rows=%d", indexed, rows)
			}
			const want = 1001
			if rows != want || b.metrics.historyMessages.Load() != int64(want) {
				t.Fatalf("rows=%d history metric=%d want=%d", rows, b.metrics.historyMessages.Load(), want)
			}
			if got := b.metrics.storeFailures.Load(); got != 500 {
				t.Fatalf("lost rows=%d want=500", got)
			}
			if lines := errorLines(rec.String()); len(lines) != 1 || !strings.Contains(lines[0], "H510") {
				t.Fatalf("rollback must name its originating message once: %v", lines)
			}
			if !strings.Contains(rec.String(), fmt.Sprintf("stored %d of %d messages", want, total)) {
				t.Fatalf("incorrect conversation summary: %s", rec.String())
			}
		})
	}
}

func TestHistoryRetriesBusyWholeChunk(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	release := func() {}
	attempts := 0
	b.historyBatchWriter = func(fn func(*messageBatch) error) error {
		attempts++
		if attempts == 1 {
			release = lock()
		}
		return ms.Batch(fn)
	}
	waits := 0
	b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
	const rows = 501
	b.handleHistorySync(largeHistoryFixture(rows))
	var indexed int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'history'").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || indexed != rows || b.metrics.historyMessages.Load() != rows || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("retries=%d indexed=%d history=%d failures=%d", waits, indexed, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load())
	}
}

func TestBatchRejectsIgnoredSideWriteErrors(t *testing.T) {
	for _, action := range []string{"ABORT", "ROLLBACK"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms, err := NewMessageStore()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ms.Close() }()
			if err := ms.StoreChat(phonePN.String(), "Alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := ms.db.Exec(fmt.Sprintf("CREATE TRIGGER fail_side BEFORE UPDATE OF mentions ON messages WHEN new.id='FAIL' BEGIN SELECT RAISE(%s,'simulated side failure');END", action)); err != nil {
				t.Fatal(err)
			}
			err = ms.Batch(func(batch *messageBatch) error {
				if err := batch.StoreMessage("FAIL", phonePN.String(), phonePN.User, "history searchable", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
					return err
				}
				// A caller that ignores an auxiliary write error must still get
				// a failed transaction, with no tail autocommitted after rollback.
				_ = batch.SetMentions("FAIL", phonePN.String(), phonePN.String())
				_ = batch.StoreMessage("TAIL", phonePN.String(), phonePN.User, "history searchable", time.Now(), false, "", "", "", nil, nil, nil, 0, "")
				return nil
			})
			if err == nil {
				t.Fatal("ignored side failure committed a batch")
			}
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if rows != 0 || indexed != 0 {
				t.Fatalf("failed batch leaked rows=%d indexed=%d", rows, indexed)
			}
		})
	}
}

func TestLiveEventRetriesDuringHistoryAndCommitsBetweenChunks(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	writerHeld, liveBusy := make(chan struct{}), make(chan struct{})
	firstCommitted, liveDone, historyDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	first := true
	b.historyBatchWriter = func(fn func(*messageBatch) error) error {
		firstBatch := first
		if !firstBatch {
			select {
			case <-liveDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := ms.Batch(func(batch *messageBatch) error {
			if firstBatch {
				first = false
				if err := batch.StoreMessage("MARKER", phonePN.String(), phonePN.User, "marker searchable", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
					return err
				}
				close(writerHeld)
				// Hold the real SQLite writer until the live path actually gets BUSY.
				select {
				case <-liveBusy:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return fn(batch)
		})
		if firstBatch && err == nil {
			close(firstCommitted)
		}
		return err
	}
	waits := 0
	b.storeRetryWait = func(time.Duration) bool {
		waits++
		if waits == 1 {
			close(liveBusy)
		}
		select {
		case <-firstCommitted:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() { b.handleHistorySync(largeHistoryFixture(1001)); close(historyDone) }()
	defer func() { cancel(); <-historyDone }()
	select {
	case <-writerHeld:
	case <-ctx.Done():
		t.Fatal("history did not acquire the writer")
	}
	live := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "live searchable")
	live.Info.ID = "LIVE-DURING-HISTORY"
	b.handleMessage(live)
	close(liveDone)
	select {
	case <-historyDone:
	case <-ctx.Done():
		t.Fatal("history did not finish after the live write")
	}
	var indexed int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || indexed != 1003 || b.metrics.messagesStored.Load() != 1 || b.metrics.historyMessages.Load() != 1001 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("waits=%d indexed=%d live=%d history=%d failures=%d", waits, indexed, b.metrics.messagesStored.Load(), b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load())
	}
}
