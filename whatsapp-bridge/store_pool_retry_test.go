package main

import (
	"strings"
	"testing"
	"time"
)

func TestPoolStoreBusyReleaseAndBoundedExhaustion(t *testing.T) {
	for _, phase := range []string{"chat", "message"} {
		for _, free := range []bool{true, false} {
			t.Run(phase+map[bool]string{true: "/release", false: "/exhaust"}[free], func(t *testing.T) {
				ms, lock := lockedProductionStore(t)
				now := time.Unix(1772359200, 0)
				if phase == "message" {
					if err := ms.StoreChat(phonePN.String(), "Alice", now); err != nil {
						t.Fatal(err)
					}
				}
				rec := installRecordingLogger(t)
				b := testBridge(t, nil, ms, rec)
				release := lock()
				waits, attempts := 0, 0
				b.storeRetryWait = func(delay time.Duration) bool {
					if delay != defaultStoreRetryDelays()[waits] {
						t.Fatalf("unexpected production retry delay %v", delay)
					}
					waits++
					if free && waits == 1 {
						release()
					}
					return true
				}
				stored := b.storeLive("message", "POOL1", phonePN.String(), func() error {
					attempts++
					if phase == "chat" {
						return ms.StoreChat(phonePN.String(), "Alice", now)
					}
					return ms.StoreMessage("POOL1", phonePN.String(), phonePN.String(), "searchable", now, false, "", "", "", nil, nil, nil, 0, "")
				})
				release()
				wantWaits := len(defaultStoreRetryDelays())
				if free {
					wantWaits = 1
				}
				if stored != free || waits != wantWaits || attempts != wantWaits+1 {
					t.Fatalf("stored=%v waits=%d attempts=%d", stored, waits, attempts)
				}
				var rows int
				query := `SELECT COUNT(*) FROM chats WHERE jid=?`
				if phase == "message" {
					query = `SELECT COUNT(*) FROM messages WHERE chat_jid=? AND rowid IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'searchable')`
				}
				if err := ms.db.QueryRow(query, phonePN.String()).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				wantRows := 0
				wantFailures := int64(1)
				if free {
					wantRows = 1
					wantFailures = 0
				}
				errs := errorLines(rec.String())
				if rows != wantRows || b.metrics.storeFailures.Load() != wantFailures || len(errs) != int(wantFailures) {
					t.Fatalf("rows=%d failures=%d errors=%v", rows, b.metrics.storeFailures.Load(), errs)
				}
				if !free && (!strings.Contains(errs[0], "POOL1") || !strings.Contains(errs[0], phonePN.String()) || strings.Contains(errs[0], "searchable")) {
					t.Fatalf("error identity/content: %v", errs)
				}
			})
		}
	}
}
