package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestBatchAcquiresWriterBeforeRunningCallback(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	release := lock()
	entered := false
	err := ms.Batch(func(*messageBatch) error { entered = true; return nil })
	release()
	if !isBusyError(err) || entered {
		t.Fatalf("writer must be acquired at Begin: err=%v callback=%v", err, entered)
	}
	if err := ms.Batch(func(*messageBatch) error { entered = true; return nil }); err != nil || !entered {
		t.Fatalf("released writer: err=%v callback=%v", err, entered)
	}
}

func TestHistoryBusyExhaustionKeepsOnlyNewestContiguousPrefix(t *testing.T) {
	for _, at := range []int{0, 1, 500} {
		t.Run(fmt.Sprintf("at=%d", at), func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			fixture := largeHistoryFixture(1501)
			if at == 1 {
				if _, err := ms.db.Exec("CREATE TRIGGER bad_row BEFORE INSERT ON messages WHEN new.id='H10' BEGIN SELECT RAISE(ABORT,'bad row'); END"); err != nil {
					t.Fatal(err)
				}
			}
			for i, msg := range fixture.Data.Conversations[0].Messages {
				stamp := uint64(1772359200 - i)
				msg.Message.MessageTimestamp = &stamp
			}
			commits, attempts, waits, calls := 0, 0, 0, 0
			var release func()
			b.storeRetryWait = func(time.Duration) bool { waits++; return true }
			b.historyBatchWriter = func(fn func(*messageBatch) error) error {
				calls++
				if at == 1 && calls == 1 {
					return ms.Batch(fn)
				}
				if commits*500 < at {
					err := ms.Batch(fn)
					if err == nil {
						commits++
					}
					return err
				}
				if attempts == 0 {
					release = lock()
				}
				attempts++
				err := ms.Batch(fn)
				if attempts == 3 {
					release()
				}
				return err
			}
			b.handleHistorySync(fixture)
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if rows != at || indexed != at || b.metrics.historyMessages.Load() != int64(at) || b.metrics.storeFailures.Load() != int64(1501-at) || attempts != 3 || waits != 2 {
				t.Fatalf("rows=%d index=%d history=%d failures=%d attempts=%d waits=%d", rows, indexed, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load(), attempts, waits)
			}
			if at > 0 {
				var oldest string
				if err := ms.db.QueryRow("SELECT id FROM messages ORDER BY timestamp LIMIT 1").Scan(&oldest); err != nil {
					t.Fatal(err)
				}
				if oldest != fmt.Sprintf("H%d", at-1) {
					t.Fatalf("history anchor crossed the failed range: %s", oldest)
				}
			}
			errs := errorLines(rec.String())
			for _, required := range []string{fmt.Sprintf("%d history messages", 1501-at), phonePN.String(), fmt.Sprintf("first ID H%d", at), "last ID H1500"} {
				if len(errs) != 1 || !strings.Contains(errs[0], required) {
					t.Fatalf("loss must appear in one ERROR (%s): %v", required, errs)
				}
			}
		})
	}
}

func TestHistoryChatFailureCountsRowsOnceWithoutStartingChunks(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	if _, err := ms.db.Exec("CREATE TRIGGER fail_chat BEFORE INSERT ON chats BEGIN SELECT RAISE(ABORT,'chat denied'); END"); err != nil {
		t.Fatal(err)
	}
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	b.historyBatchWriter = func(func(*messageBatch) error) error { t.Fatal("chunks ran without their chat"); return nil }
	b.handleHistorySync(largeHistoryFixture(1501))
	if b.metrics.historyMessages.Load() != 0 || b.metrics.storeFailures.Load() != 1501 {
		t.Fatalf("history=%d failures=%d", b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load())
	}
	if errs := errorLines(rec.String()); len(errs) != 1 || !strings.Contains(errs[0], "1501 history messages") || !strings.Contains(errs[0], "first ID H0, last ID H1500") {
		t.Fatalf("chat failure: %v", errs)
	}
}

func TestHistoryShutdownStopsQuietlyBetweenChunksAndConversations(t *testing.T) {
	for _, rows := range []int{1, 1001} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			fixture := largeHistoryFixture(rows)
			other := largeHistoryFixture(1).Data.Conversations[0]
			chat := types.NewJID("120363000000000001", types.GroupServer).String()
			other.ID = &chat
			fixture.Data.Conversations = append(fixture.Data.Conversations, other)
			commits := 0
			b.historyBatchWriter = func(fn func(*messageBatch) error) error {
				err := ms.Batch(fn)
				if err == nil {
					commits++
					b.cancel()
					if err := ms.Close(); err != nil {
						t.Fatal(err)
					}
				}
				return err
			}
			b.handleHistorySync(fixture)
			if commits != 1 || b.metrics.historyMessages.Load() != int64(min(rows, 500)) || b.metrics.storeFailures.Load() != 0 || len(errorLines(rec.String())) != 0 || strings.Count(rec.String(), "History sync stopped during shutdown") != 1 {
				t.Fatalf("commits=%d history=%d failures=%d logs=%s", commits, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load(), rec.String())
			}
			probe, err := sql.Open("sqlite", sqliteURI(messagesDBPath(), sqliteReadOnlyOptions))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = probe.Close() }()
			var count int
			if err := probe.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != min(rows, 500) {
				t.Fatalf("rows after close=%d err=%v", count, err)
			}
		})
	}
}
