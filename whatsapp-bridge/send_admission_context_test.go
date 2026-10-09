package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestSendBudgetEnableWhileWaitingDoesNotFailOpen(t *testing.T) {
	b := newSettingsBridge(t)
	b.Client = newTestClient(&failedCountingLIDs{&mockLIDStore{}})
	writer, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.Exec(`INSERT INTO runtime_settings(key,value,updated_at,version) VALUES ('send.rate_per_day','1',?,1)`, dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	done := make(chan bool, 1)
	go func() { done <- b.allowSend(w, httptest.NewRequest("POST", "/api/send", nil), phonePN.String()) }()
	// The uncommitted writer keeps the preliminary read at zero. Owning sendMu
	// proves admission has finished that read and is waiting for the WAL writer.
	deadline := time.Now().Add(time.Second)
	for b.sendMu.TryLock() {
		b.sendMu.Unlock()
		if time.Now().After(deadline) {
			_ = writer.Rollback()
			<-done
			t.Fatal("admission never reached reservation")
		}
		time.Sleep(time.Millisecond)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if allowed := <-done; allowed || w.Code != 503 || b.sendCountSkipped.Load() != 0 {
		t.Fatalf("newly enabled limit failed open: allowed=%v status=%d skipped=%d", allowed, w.Code, b.sendCountSkipped.Load())
	}
}

type deadlineCountingLIDs struct {
	*mockLIDStore
	deadline time.Time
}

func (l *deadlineCountingLIDs) GetLIDForPN(ctx context.Context, _ types.JID) (types.JID, error) {
	l.deadline, _ = ctx.Deadline()
	return types.EmptyJID, nil
}

func TestCountedActionsShareAdmissionDeadline(t *testing.T) {
	for _, action := range []string{"listed", "whole", "react"} {
		t.Run(action, func(t *testing.T) {
			b := newSettingsBridge(t)
			limitPatch(t, b, `{"send.rate_per_day":1}`)
			b.SendIncludeActions = true
			lids := &deadlineCountingLIDs{mockLIDStore: &mockLIDStore{}}
			b.Client = newTestClient(lids)
			var sentDeadline time.Time
			var calls int
			started := time.Now()
			w := httptest.NewRecorder()
			if action == "react" {
				b.sendMessage = func(ctx context.Context, _ types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
					sentDeadline, _ = ctx.Deadline()
					calls++
					return whatsmeow.SendResponse{}, nil
				}
				b.handleReact()(w, httptest.NewRequest("POST", "/api/react", strings.NewReader(`{"recipient":"5511999999999@s.whatsapp.net","message_id":"fake-id","emoji":""}`)))
			} else {
				seedMarkReadChat(t, b.Store, markReadDM, nil)
				seedMarkReadMessage(t, b.Store, markReadDM, "fake-id", "5511999999999", started.Add(-time.Minute), false, nil)
				deps := newMarkReadDeps(t, b.Store, &markReadRecorder{})
				deps.allowSend = b.allowSendAction
				deps.markRead = func(ctx context.Context, _ []types.MessageID, _ time.Time, _, _ types.JID) error {
					sentDeadline, _ = ctx.Deadline()
					calls++
					return nil
				}
				body := map[string]any{"chat_jid": markReadDM}
				if action == "listed" {
					body["message_ids"] = []string{"fake-id"}
				}
				w = postMarkRead(t, deps, body)
			}
			usage, err := b.sendUsageSnapshot(context.Background())
			if err != nil || w.Code != 200 || calls != 1 || usage.Today != 1 || lids.deadline.IsZero() || !lids.deadline.Equal(sentDeadline) || lids.deadline.After(started.Add(actionDeadline+time.Second)) {
				t.Fatalf("admission and action deadlines differ: status=%d calls=%d usage=%+v admission=%v action=%v err=%v", w.Code, calls, usage, lids.deadline, sentDeadline, err)
			}
		})
	}
}
