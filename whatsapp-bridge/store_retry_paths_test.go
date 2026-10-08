package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Real schema, WAL and FTS, with every pooled connection using a short busy
// timeout. The barrier releases the writer only after SQLite reports BUSY.
func lockedProductionStore(t *testing.T) (*MessageStore, func() func()) {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var connections []*sql.Conn
	for range messagesPoolConns {
		conn, err := ms.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=5"); err != nil {
			t.Fatal(err)
		}
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	lock := func() func() {
		tx, err := ms.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("UPDATE chats SET name=name"); err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Cleanup(release)
		return release
	}
	return ms, lock
}

func TestOutboundPersistenceRetriesBusyChatAndMessage(t *testing.T) {
	for _, path := range []string{"chat", "message"} {
		t.Run(path, func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			now := time.Unix(1772359200, 0)
			if path == "message" {
				if err := ms.StoreChat(phonePN.String(), "", now); err != nil {
					t.Fatal(err)
				}
			}
			release := lock()
			waits := 0
			ms.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
			if path == "chat" {
				persistOutbound(newTestClientWithSelf(&mockLIDStore{}, phonePN), ms, phonePN, sentMessage{ID: "OUT1", Timestamp: now}, "searchable outbound", outboundMedia{}, "")
			} else if err := (outboundMedia{}).store(ms, "OUT1", phonePN.String(), phonePN.User, "searchable outbound", now, ""); err != nil {
				t.Fatal(err)
			}
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'outbound'").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if waits != 1 || rows != 1 {
				t.Fatalf("retries=%d indexed rows=%d want 1/1", waits, rows)
			}
		})
	}
}

func TestHistoryChatRetriesBusy(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	release := lock()
	waits := 0
	b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
	b.handleHistorySync(largeHistoryFixture(1))
	var rows int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'history'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || rows != 1 || b.metrics.historyMessages.Load() != 1 || b.metrics.storeFailures.Load() != 0 {
		t.Fatalf("retries=%d rows=%d history=%d failures=%d", waits, rows, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load())
	}
}

func TestOutboundBusyBoundAndNonBusyErrors(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy bound", false: "non-busy constraint"}[busy], func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			now := time.Unix(1772359200, 0)
			if err := ms.StoreChat(phonePN.String(), "", now); err != nil {
				t.Fatal(err)
			}
			release := func() {}
			if busy {
				release = lock()
			} else if _, err := ms.db.Exec("CREATE TRIGGER refuse_outbound BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'constraint');END"); err != nil {
				t.Fatal(err)
			}
			waits := 0
			ms.storeRetryWait = func(time.Duration) bool { waits++; return true }
			err := (outboundMedia{}).store(ms, "OUT1", phonePN.String(), phonePN.User, "outbound", now, "")
			release()
			want := 0
			if busy {
				want = len(defaultStoreRetryDelays())
			}
			if err == nil || waits != want || isBusyError(err) != busy {
				t.Fatalf("error=%v retries=%d want=%d", err, waits, want)
			}
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("persisted %d rows after failure", rows)
			}
		})
	}
}

// Both pre-existing outer retry contracts stay in charge; the helpers inside
// these event paths must not acquire a second, multiplied retry budget.
func TestExistingLiveMessageAndPollRowOuterRetry(t *testing.T) {
	for _, poll := range []bool{false, true} {
		t.Run(map[bool]string{false: "EnsureChat and message", true: "poll message"}[poll], func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			now := time.Unix(1772359200, 0)
			if poll {
				if err := ms.StoreChat(phonePN.String(), "", now); err != nil {
					t.Fatal(err)
				}
				if err := ms.StorePoll("POLL1", phonePN.String(), &pollCreation{Question: "Q", Options: []string{"Pizza"}, SelectableCount: 1}, now); err != nil {
					t.Fatal(err)
				}
				b.PollVoteDecrypt = func(context.Context, *events.Message) ([][]byte, error) { return [][]byte{hashOf("Pizza")}, nil }
			}
			release := lock()
			waits := 0
			b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
			evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "searchable live")
			evt.Info.ID = "LIVE1"
			if poll {
				evt.Message = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("POLL1"), RemoteJID: proto.String(phonePN.String())}, Vote: &waE2E.PollEncValue{EncPayload: []byte("x"), EncIV: []byte("y")}}}
			}
			b.handleMessage(evt)
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='LIVE1'").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if waits != 1 || rows != 1 || b.metrics.storeFailures.Load() != 0 {
				t.Fatalf("retries=%d rows=%d failures=%d", waits, rows, b.metrics.storeFailures.Load())
			}
		})
	}
}
