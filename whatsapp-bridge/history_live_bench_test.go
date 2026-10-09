package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
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
					if err := batch.StoreMessage(storedMessage{
						ID:         fmt.Sprintf("MARKER%d", iteration),
						ChatJID:    phonePN.String(),
						Sender:     phonePN.User,
						Content:    "marker searchable",
						Timestamp:  time.Unix(1772359200, 0),
						FileLength: 0,
					}); err != nil {
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

// Observe first-attempt live persistence under a real concurrent history sync.
// Retries are disabled only for this measurement, so every rejected live row
// appears in its own bridge's failure counter; history retains normal retries.
type firstAttemptBusyLog struct {
	waLog.Logger
	busy, other atomic.Int64
}

// Isolate the live transaction used by handleMessage, separating acquisition
// failures from write failures; the concurrent history path remains unchanged.
func BenchmarkHistoryTransactionFirstAttemptBusy(b *testing.B) {
	for _, mode := range []string{"deferred", "immediate"} {
		b.Run(mode, func(b *testing.B) {
			b.Setenv(storeDirEnv, b.TempDir())
			ms, err := NewMessageStore()
			if err != nil {
				b.Fatal(err)
			}
			if err := ms.Close(); err != nil {
				b.Fatal(err)
			}
			ms.db, err = sql.Open("sqlite", sqliteURI(messagesDBPath(), sqliteWriterOptions+"&_txlock="+mode))
			if err != nil {
				b.Fatal(err)
			}
			boundPool(ms.db, messagesPoolConns)
			defer func() { _ = ms.Close() }()
			stamp := time.Unix(1772359200, 0)
			if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
				b.Fatal(err)
			}
			switches := testSwitches()
			switches.MediaAutoDownload, switches.WebhookEnabled = false, false
			history := newBridge(newTestClient(&mockLIDStore{}), ms, waLog.Noop, "", nil, switches)
			defer history.Shutdown(time.Second)
			ex := extractMessage(&waE2E.Message{Conversation: proto.String("live searchable")}, stamp, "")
			var busy [4]atomic.Int64 // begin, write, prepare, commit
			var earlyWrite, other atomic.Int64
			const writers, perWriter, historyRows = 8, 2000, 5000
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				fixture := largeHistoryFixture(historyRows)
				for row, msg := range fixture.Data.Conversations[0].Messages {
					id := fmt.Sprintf("H%d-%d", iteration, row)
					msg.Message.Key.ID = &id
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				wg.Add(writers + 1)
				go func() { defer wg.Done(); <-start; history.handleHistorySync(fixture) }()
				for writer := range writers {
					go func() {
						defer wg.Done()
						<-start
						for row := 0; row < perWriter; row++ {
							id := fmt.Sprintf("L%d-%d-%d", iteration, writer, row)
							started := time.Now()
							err := ms.Batch(func(batch *messageBatch) error {
								return persistMessage(batch, id, phonePN.String(), phonePN.String(), stamp, false, ex, true, waLog.Noop)
							})
							elapsed := time.Since(started)
							if err == nil {
								continue
							}
							if !isBusyError(err) {
								other.Add(1)
								continue
							}
							phase := 1
							switch {
							case strings.HasPrefix(err.Error(), "begin batch:"):
								phase = 0
							case strings.HasPrefix(err.Error(), "prepare batch insert:"):
								phase = 2
							case strings.HasPrefix(err.Error(), "commit batch:"):
								phase = 3
							}
							busy[phase].Add(1)
							if phase == 1 && elapsed < 100*time.Millisecond {
								earlyWrite.Add(1)
							}
						}
					}()
				}
				close(start)
				wg.Wait()
			}
			b.StopTimer()
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				b.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&indexed); err != nil {
				b.Fatal(err)
			}
			var failed int64
			for i := range busy {
				failed += busy[i].Load()
			}
			if other.Load() != 0 || rows != indexed || int64(rows)+failed != int64(b.N*(historyRows+writers*perWriter)) || history.metrics.historyMessages.Load() != int64(b.N*historyRows) || history.metrics.storeFailures.Load() != 0 {
				b.Fatalf("rows=%d FTS=%d busy=%d other=%d history=%d historyfailures=%d", rows, indexed, failed, other.Load(), history.metrics.historyMessages.Load(), history.metrics.storeFailures.Load())
			}
			b.ReportMetric(writers*perWriter, "live-attempts")
			for i, name := range []string{"begin-BUSY", "write-BUSY", "prepare-BUSY", "commit-BUSY"} {
				b.ReportMetric(float64(busy[i].Load())/float64(b.N), name)
			}
			b.ReportMetric(float64(earlyWrite.Load())/float64(b.N), "early-write-BUSY")
		})
	}
}

func (l *firstAttemptBusyLog) Errorf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if strings.Contains(line, "SQLITE_BUSY") || strings.Contains(line, "SQLITE_LOCKED") {
		l.busy.Add(1)
	} else {
		l.other.Add(1)
	}
}

func BenchmarkHistoryLiveFirstAttemptBusy(b *testing.B) {
	for _, mode := range []string{"deferred", "immediate"} {
		b.Run(mode, func(b *testing.B) {
			b.Setenv(storeDirEnv, b.TempDir())
			ms, err := NewMessageStore()
			if err != nil {
				b.Fatal(err)
			}
			if err := ms.Close(); err != nil {
				b.Fatal(err)
			}
			options := sqliteWriterOptions + "&_txlock=" + mode
			ms.db, err = sql.Open("sqlite", sqliteURI(messagesDBPath(), options))
			if err != nil {
				b.Fatal(err)
			}
			boundPool(ms.db, messagesPoolConns)
			defer func() { _ = ms.Close() }()
			switches := testSwitches()
			switches.MediaAutoDownload, switches.WebhookEnabled = false, false
			history := newBridge(newTestClient(&mockLIDStore{}), ms, waLog.Noop, "", nil, switches)
			observed := &firstAttemptBusyLog{Logger: waLog.Noop}
			live := newBridge(newTestClient(&mockLIDStore{}), ms, observed, "", nil, switches)
			defer history.Shutdown(time.Second)
			defer live.Shutdown(time.Second)
			live.StoreRetryDelays = nil
			const writers, perWriter, historyRows = 8, 2000, 5000
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				fixture := largeHistoryFixture(historyRows)
				for row, msg := range fixture.Data.Conversations[0].Messages {
					id := fmt.Sprintf("H%d-%d", iteration, row)
					msg.Message.Key.ID = &id
				}
				var wg sync.WaitGroup
				start := make(chan struct{})
				wg.Add(writers + 1)
				go func() { defer wg.Done(); <-start; history.handleHistorySync(fixture) }()
				for writer := range writers {
					go func() {
						defer wg.Done()
						<-start
						for row := 0; row < perWriter; row++ {
							msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "live searchable")
							msg.Info.ID = fmt.Sprintf("L%d-%d-%d", iteration, writer, row)
							live.handleMessage(msg)
						}
					}()
				}
				close(start)
				wg.Wait()
			}
			b.StopTimer()
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				b.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&indexed); err != nil {
				b.Fatal(err)
			}
			failed := live.metrics.storeFailures.Load()
			if failed != observed.busy.Load() || observed.other.Load() != 0 {
				b.Fatalf("live failures=%d BUSY/LOCKED errors=%d other errors=%d", failed, observed.busy.Load(), observed.other.Load())
			}
			if rows != indexed || int64(rows)+failed != int64(b.N*(historyRows+writers*perWriter)) || history.metrics.historyMessages.Load() != int64(b.N*historyRows) || history.metrics.storeFailures.Load() != 0 || live.metrics.messagesStored.Load()+failed != int64(b.N*writers*perWriter) {
				b.Fatalf("rows=%d FTS=%d live=%d failed=%d history=%d historyfailed=%d", rows, indexed, live.metrics.messagesStored.Load(), failed, history.metrics.historyMessages.Load(), history.metrics.storeFailures.Load())
			}
			b.ReportMetric(float64(failed)/float64(b.N), "live-first-attempt-BUSY")
			b.ReportMetric(writers*perWriter, "live-attempts")
		})
	}
}
