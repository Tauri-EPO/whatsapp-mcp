package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOutboundPersistenceRetriesBusyNewAndExistingChat(t *testing.T) {
	for _, path := range []string{"new chat", "existing chat"} {
		t.Run(path, func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			now := time.Unix(1772359200, 0)
			if path == "existing chat" {
				if err := ms.StoreChat(phonePN.String(), "", now); err != nil {
					t.Fatal(err)
				}
			}
			release := lock()
			waits := 0
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, phonePN), ms, testLogger())
			b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
			if _, err := b.persistOutbound(phonePN, sentMessage{ID: "OUT1", Timestamp: now}, "searchable outbound", outboundMedia{}, ""); err != nil {
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

func TestOutboundMessageRetriesAfterChatWasWritten(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, phonePN), ms, testLogger())
	now := time.Unix(1772359200, 0)
	chatWrites, messageWrites, waits := 0, 0, 0
	release := func() {}
	b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
	messageWasBusy := false
	err := b.retryOutbound(func() error {
		chatWrites++
		if err := ms.StoreChat(phonePN.String(), "", now); err != nil {
			return err
		}
		if chatWrites == 1 {
			release = lock() // the chat succeeded; only the next message insert is BUSY
		}
		return nil
	}, func() error {
		messageWrites++
		err := (outboundMedia{}).store(ms, "OUT1", phonePN.String(), phonePN.User, "searchable outbound", now, "")
		if messageWrites == 1 {
			messageWasBusy = isBusyError(err)
		}
		return err
	})
	release()
	var indexed int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'outbound'").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if err != nil || !messageWasBusy || waits != 1 || chatWrites != 2 || messageWrites != 2 || indexed != 1 {
		t.Fatalf("error=%v messageBusy=%v waits=%d chatWrites=%d messageWrites=%d indexed=%d", err, messageWasBusy, waits, chatWrites, messageWrites, indexed)
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

func TestOutboundPersistenceStopsRetryOnShutdown(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, phonePN), ms, testLogger())
	release := lock()
	defer release()
	b.StoreRetryDelays = []time.Duration{time.Hour}
	b.cancel()
	done := make(chan error, 1)
	go func() {
		_, err := b.persistOutbound(phonePN, sentMessage{ID: "OUT1", Timestamp: time.Now()}, "outbound", outboundMedia{}, "")
		done <- err
	}()
	select {
	case err := <-done:
		if !isBusyError(err) || b.metrics.storeFailures.Load() != 1 {
			t.Fatalf("error=%v failures=%d", err, b.metrics.storeFailures.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("outbound retry ignored Bridge shutdown")
	}
}

func TestHistoryChatLossHasIdentityWithoutAnEmptyMessageID(t *testing.T) {
	ms, lock := lockedProductionStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	if err := ms.StoreChat(phonePN.String(), "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	release := lock()
	defer release()
	b.storeRetryWait = func(time.Duration) bool { release(); return false }
	b.handleHistorySync(largeHistoryFixture(1))
	lines := errorLines(rec.String())
	if b.metrics.storeFailures.Load() != 1 || len(lines) != 1 || !strings.Contains(lines[0], "history chat in "+phonePN.String()) || strings.Contains(lines[0], "  ") {
		t.Fatalf("failures=%d errors=%v", b.metrics.storeFailures.Load(), lines)
	}
}

func TestOutboundBusyBoundAndNonBusyErrors(t *testing.T) {
	for _, tc := range []struct {
		name               string
		busy               bool
		override, expected []time.Duration
	}{
		{"default busy bound", true, nil, []time.Duration{200 * time.Millisecond, time.Second}},
		{"custom Bridge budget", true, []time.Duration{3 * time.Second}, []time.Duration{3 * time.Second}},
		{"non-busy constraint", false, []time.Duration{3 * time.Second}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			busy := tc.busy
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
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, phonePN), ms, rec)
			b.Connected = func() bool { return true }
			if tc.override != nil {
				b.StoreRetryDelays = tc.override
			}
			b.storeRetryWait = func(delay time.Duration) bool {
				if waits >= len(tc.expected) || delay != tc.expected[waits] {
					t.Fatalf("outbound ignored Bridge delay: %s", delay)
				}
				waits++
				return true
			}
			remoteSends := 0
			var persistErr error
			b.Send = func(_ context.Context, recipient, message, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
				remoteSends++ // remote acceptance, followed by real local persistence
				sent := sentMessage{ID: "OUT1", Timestamp: now}
				sent.ChatJID, persistErr = b.persistOutbound(phonePN, sent, message, outboundMedia{}, "")
				return true, outboundSendStatus(recipient, persistErr), sent
			}
			b.IsOnWhatsApp = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
				return []types.IsOnWhatsAppResponse{{JID: phonePN, IsIn: true}}, nil
			}
			httpReply := httptest.NewRecorder()
			b.handleSend(nil).ServeHTTP(httpReply, httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(`{"recipient":"5511999999999","message":"must stay secret"}`)))
			err := persistErr
			release()
			if remoteSends != 1 {
				t.Fatalf("send backend was not reached: HTTP=%d %s", httpReply.Code, httpReply.Body.String())
			}
			want := len(tc.expected)
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
			var response SendMessageResponse
			if err := json.Unmarshal(httpReply.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if httpReply.Code != http.StatusOK || !response.Success || remoteSends != 1 || response.MessageID != "OUT1" || !strings.Contains(response.Message, outboundArchiveWarning) {
				t.Fatalf("sent=%d HTTP=%d response=%+v", remoteSends, httpReply.Code, response)
			}
			lines := errorLines(rec.String())
			if b.metrics.storeFailures.Load() != 1 || len(lines) != 1 || !strings.Contains(lines[0], "OUT1") || !strings.Contains(lines[0], phonePN.String()) || strings.Contains(lines[0], "must stay secret") {
				t.Fatalf("failures=%d errors=%v", b.metrics.storeFailures.Load(), lines)
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
