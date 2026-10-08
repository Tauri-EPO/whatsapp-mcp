package main

import (
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Measures the actual history handler and concurrent live event on a temporary
// file-backed production store, including FTS. No wall-time assertion: machines
// differ; the chunk-bound test owns the deterministic transaction guarantee.
func BenchmarkHistoryLiveWriteFile(b *testing.B) {
	b.Setenv(storeDirEnv, b.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	switches := testSwitches()
	switches.MediaAutoDownload = false
	switches.WebhookEnabled = false
	bridge := newBridge(newTestClient(&mockLIDStore{}), ms, waLog.Noop, "", nil, switches)
	defer bridge.Shutdown(time.Second)
	var batchTotal, liveTotal, longestBatch time.Duration
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		fixture := largeHistoryFixture(5000)
		for row, msg := range fixture.Data.Conversations[0].Messages {
			id := fmt.Sprintf("H%d-%d", iteration, row)
			msg.Message.Key.ID = &id
		}
		firstWritten, attempted := make(chan struct{}), make(chan struct{})
		done := make(chan time.Duration, 1)
		go func() {
			<-firstWritten
			started := time.Now()
			close(attempted)
			live := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "live searchable")
			live.Info.ID = fmt.Sprintf("LIVE%d", iteration)
			bridge.handleMessage(live)
			done <- time.Since(started)
		}()
		first := true
		bridge.historyBatchWriter = func(fn func(*messageBatch) error) error {
			started := time.Now()
			err := ms.Batch(func(batch *messageBatch) error {
				if first {
					first = false
					// One marker holds the SQLite writer before the concurrent live event
					// starts. All 5,000 history rows then use the real handler callback.
					if err := batch.StoreMessage(fmt.Sprintf("MARKER%d", iteration), phonePN.String(), phonePN.User, "marker searchable", time.Unix(1772359200, 0), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
						return err
					}
					close(firstWritten)
					<-attempted
				}
				return fn(batch)
			})
			longestBatch = max(longestBatch, time.Since(started))
			return err
		}
		started := time.Now()
		bridge.handleHistorySync(fixture)
		batchTotal += time.Since(started)
		liveTotal += <-done
	}
	b.StopTimer()
	var rows, indexed int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
		b.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
		b.Fatal(err)
	}
	if rows != b.N*5002 || indexed != rows || bridge.metrics.messagesStored.Load() != int64(b.N) || bridge.metrics.historyMessages.Load() != int64(b.N*5000) || bridge.metrics.storeFailures.Load() != 0 {
		b.Fatalf("rows=%d indexed=%d live=%d history=%d failures=%d", rows, indexed, bridge.metrics.messagesStored.Load(), bridge.metrics.historyMessages.Load(), bridge.metrics.storeFailures.Load())
	}
	b.ReportMetric(float64(batchTotal.Nanoseconds())/float64(b.N)/1e6, "batch-ms")
	b.ReportMetric(float64(liveTotal.Nanoseconds())/float64(b.N)/1e6, "live-ms")
	b.ReportMetric(float64(longestBatch.Nanoseconds())/1e6, "max-batch-ms")
}
