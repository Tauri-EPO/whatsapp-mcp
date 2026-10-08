package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestGroupRenameRetriesAndReportsExhaustion(t *testing.T) {
	for _, free := range []bool{true, false} {
		t.Run(fmt.Sprint(free), func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			chat := types.NewJID("120363000000000001", types.GroupServer)
			if err := ms.StoreChat(chat.String(), "Old group", time.Now()); err != nil {
				t.Fatal(err)
			}
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			release := lock()
			waits := 0
			b.storeRetryWait = func(time.Duration) bool {
				waits++
				if free {
					release()
				}
				return true
			}
			b.handleEvent(&events.GroupInfo{JID: chat, Name: &types.GroupName{Name: "New group"}}, nil)
			release()
			var name string
			if err := ms.db.QueryRow("SELECT name FROM chats WHERE jid=?", chat.String()).Scan(&name); err != nil {
				t.Fatal(err)
			}
			wantWaits, wantFailures := 2, int64(1)
			if free {
				wantWaits, wantFailures = 1, 0
			}
			if (name == "New group") != free || waits != wantWaits || b.metrics.storeFailures.Load() != wantFailures || len(errorLines(rec.String())) != int(wantFailures) {
				t.Fatalf("name=%s waits=%d failures=%d logs=%s", name, waits, b.metrics.storeFailures.Load(), rec.String())
			}
		})
	}
}

func TestMarkReadRetriesOnlyBookkeepingAndReturnsArchiveWarning(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, free := range []bool{false, true} {
			t.Run(fmt.Sprintf("whole=%v/free=%v", whole, free), func(t *testing.T) {
				ms, lock := lockedProductionStore(t)
				now := time.Now().Add(-time.Minute).Truncate(time.Second)
				seedMarkReadChat(t, ms, phonePN.String(), nil)
				seedMarkReadMessage(t, ms, phonePN.String(), "READ1", phonePN.User, now, false, nil)
				rec := installRecordingLogger(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
				var release func()
				waits, effects := 0, 0
				b.storeRetryWait = func(time.Duration) bool {
					waits++
					if free {
						release()
					}
					return true
				}
				deps := newMarkReadDeps(t, ms, &markReadRecorder{})
				deps.storeWrite = b.storeLive
				deps.markRead = func(context.Context, []types.MessageID, time.Time, types.JID, types.JID) error {
					effects++
					release = lock()
					return nil
				}
				body := map[string]any{"chat_jid": phonePN.String()}
				if !whole {
					body["message_ids"] = []string{"READ1"}
				}
				rr := postMarkRead(t, deps, body)
				response := decodeMarkRead(t, rr)
				release()
				marker := chatReadMarker(t, ms, phonePN.String())
				wantWaits, wantFailures := 2, int64(1)
				if free {
					wantWaits, wantFailures = 1, 0
				}
				if rr.Code != http.StatusOK || !response.Success || effects != 1 || waits != wantWaits || marker.Equal(now) != free || strings.Contains(response.Message, "archive update failed") == free || strings.Contains(response.Message, "SQLITE") || b.metrics.storeFailures.Load() != wantFailures || len(errorLines(rec.String())) != int(wantFailures) {
					t.Fatalf("HTTP=%d response=%+v marker=%v effects=%d waits=%d failures=%d logs=%s", rr.Code, response, marker, effects, waits, b.metrics.storeFailures.Load(), rec.String())
				}
			})
		}
	}
}
