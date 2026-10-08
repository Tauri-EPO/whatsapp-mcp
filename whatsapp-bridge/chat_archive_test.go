package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

const archiveTestChat = "5511999999999@s.whatsapp.net"

func TestArchivePatch(t *testing.T) {
	for _, tc := range []struct {
		chat, sender string
		fromMe       bool
	}{
		{archiveTestChat, archiveTestChat, true},
		{"120363000000000001@g.us", "100000000000001@lid", false},
	} {
		for _, archived := range []bool{true, false} {
			store := newTestMessageStore(t)
			ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			for _, id := range []string{"M2", "M1"} {
				if err := store.StoreMessage(id, tc.chat, tc.sender, "hello", ts, tc.fromMe, "", "", "", nil, nil, nil, 0, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.StoreMessage("M3", tc.chat, tc.sender, "older", ts.Add(-time.Hour), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
				t.Fatal(err)
			}
			calls := 0
			deps := archiveDeps{store: store, connected: func() bool { return true },
				resolve: func(_ context.Context, raw string) (types.JID, error) {
					if raw == archiveTestChat {
						raw = "100000000000001@lid"
					}
					return types.ParseJID(raw)
				},
				send: func(ctx context.Context, patch appstate.PatchInfo) error {
					calls++
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 20*time.Second {
						t.Error("send must finish before the MCP 30-second read timeout")
					}
					act := patch.Mutations[0].Value.GetArchiveChatAction()
					rangeMsg := act.GetMessageRange()
					key := rangeMsg.GetMessages()[0].GetKey()
					if act.GetArchived() != archived || rangeMsg.GetLastMessageTimestamp() != ts.Unix() || key.GetID() != "M1" || key.GetFromMe() != tc.fromMe {
						t.Fatalf("wrong anchor/action: %v", act)
					}
					want := tc.chat
					if tc.chat == archiveTestChat {
						want = "100000000000001@lid"
					}
					if patch.Mutations[0].Index[1] != want || key.GetRemoteJID() != want {
						t.Fatalf("wrong target: %v %v", patch.Mutations[0].Index, key)
					}
					if tc.chat != archiveTestChat && key.GetParticipant() != tc.sender {
						t.Fatalf("lost sender namespace: %v", key)
					}
					return nil
				},
			}
			flag := "false"
			if archived {
				flag = "true"
			}
			rec := httptest.NewRecorder()
			handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/api/chat/archive", strings.NewReader(`{"chat_jid":"`+tc.chat+`","archived":`+flag+`}`)))
			if rec.Code != 200 || calls != 1 || !strings.Contains(rec.Body.String(), `"success":true`) {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		}
	}
}

func TestArchiveFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		connected           bool
		seed                bool
		resolveErr, sendErr bool
		status              int
	}{
		{"bad JSON", `{`, true, false, false, false, 400},
		{"missing flag", `{"chat_jid":"` + archiveTestChat + `"}`, true, false, false, false, 400},
		{"bad JID", `{"chat_jid":"@","archived":true}`, true, false, false, false, 403},
		{"disconnected", `{"chat_jid":"` + archiveTestChat + `","archived":true}`, false, false, false, false, 503},
		{"no anchor", `{"chat_jid":"` + archiveTestChat + `","archived":true}`, true, false, false, false, 404},
		{"resolve failure", `{"chat_jid":"` + archiveTestChat + `","archived":true}`, true, true, true, false, 400},
		{"send failure", `{"chat_jid":"` + archiveTestChat + `","archived":true}`, true, true, false, true, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMessageStore(t)
			if tc.seed {
				if err := store.StoreMessage("M1", archiveTestChat, archiveTestChat, "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			deps := archiveDeps{store: store, connected: func() bool { return tc.connected },
				resolve: func(_ context.Context, raw string) (types.JID, error) {
					if tc.resolveErr {
						return types.EmptyJID, errors.New("lookup failed")
					}
					return types.ParseJID(raw)
				},
				send: func(context.Context, appstate.PatchInfo) error { calls++; return errors.New("send failed") },
			}
			rec := httptest.NewRecorder()
			handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/api/chat/archive", strings.NewReader(tc.body)))
			if rec.Code != tc.status || (calls > 0) != tc.sendErr || !strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestArchiveMuxGuards(t *testing.T) {
	for _, tc := range []struct {
		name, allow, deny, chats string
		readOnly, token          bool
		method                   string
		status                   int
	}{
		{"token", "", "", "", false, false, http.MethodPost, 401},
		{"read-only", "archive_chat", "", "", true, true, http.MethodPost, 403},
		{"allow tools", "send_message", "", "", false, true, http.MethodPost, 403},
		{"deny tools", "archive_chat", "archive_chat", "", false, true, http.MethodPost, 403},
		{"chat list", "", "", "5511888888888", false, true, http.MethodPost, 403},
		{"method", "", "", "", false, true, http.MethodGet, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.ReadOnly = readOnlyPolicy{enabled: tc.readOnly}
			b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
			b.Policy = parseChatPolicy(tc.chats)
			b.Connected = func() bool { t.Fatal("connected called past guard"); return false }
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:8080/api/chat/archive", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`))
			if tc.token {
				req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			}
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestArchiveUnknownGroupAnchorRejected(t *testing.T) {
	group := "120363000000000001@g.us"
	for _, sender := range []string{"120363000000000001", "100000000000001", "", group} {
		t.Run(sender, func(t *testing.T) {
			store := newTestMessageStore(t)
			if err := store.StoreMessage("M1", group, sender, "history", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
				t.Fatal(err)
			}
			calls := 0
			deps := archiveDeps{store: store, connected: func() bool { return true },
				resolve: func(_ context.Context, raw string) (types.JID, error) { return types.ParseJID(raw) },
				send:    func(context.Context, appstate.PatchInfo) error { calls++; return nil },
			}
			rec := httptest.NewRecorder()
			handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/api/chat/archive", strings.NewReader(`{"chat_jid":"`+group+`","archived":true}`)))
			if rec.Code != http.StatusUnprocessableEntity || calls != 0 || !strings.Contains(rec.Body.String(), "sender") {
				t.Fatalf("status=%d sends=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}

func TestArchiveTwinsAndPointerRows(t *testing.T) {
	const lid = "100000000000001@lid"
	for _, requested := range []string{archiveTestChat, lid} {
		for _, restricted := range []bool{false, true} {
			store := newTestMessageStore(t)
			ts := time.Now()
			for _, row := range []struct {
				id, chat, kind string
				ts             time.Time
			}{
				{"PN", archiveTestChat, "", ts.Add(-time.Hour)}, {"LID", lid, "", ts},
				{"REACTION", lid, "reaction", ts.Add(time.Hour)}, {"VOTE", lid, "poll_vote", ts.Add(2 * time.Hour)},
			} {
				if err := store.StoreMessage(row.id, row.chat, lid, "hello", row.ts, false, row.kind, "", "", nil, nil, nil, 0, ""); err != nil {
					t.Fatal(err)
				}
			}
			policy := chatPolicy{}
			if restricted {
				policy = parseChatPolicy(requested)
			}
			deps := archiveDeps{store: store, policy: policy, connected: func() bool { return true },
				resolve: func(context.Context, string) (types.JID, error) { return types.ParseJID(lid) },
				twin:    func(context.Context, types.JID) (types.JID, error) { return types.ParseJID(archiveTestChat) },
				send: func(_ context.Context, patch appstate.PatchInfo) error {
					want := "LID"
					if restricted && requested == archiveTestChat {
						want = "PN"
					}
					if got := patch.Mutations[0].Value.GetArchiveChatAction().GetMessageRange().GetMessages()[0].GetKey().GetID(); got != want {
						t.Errorf("anchor=%s want=%s", got, want)
					}
					return nil
				},
			}
			rec := httptest.NewRecorder()
			handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+requested+`","archived":true}`)))
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		}
	}
}

func TestArchiveOutcomeAndTargetRefusals(t *testing.T) {
	for _, chat := range []string{"status@broadcast", "120363000000000001@broadcast", "120363000000000001@newsletter"} {
		rec := httptest.NewRecorder()
		handleArchiveChat(archiveDeps{})(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+chat+`","archived":true}`)))
		if rec.Code != 400 {
			t.Fatalf("status=%d", rec.Code)
		}
	}
	store := newTestMessageStore(t)
	if err := store.StoreMessage("M1", archiveTestChat, archiveTestChat, "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	deps := archiveDeps{store: store, connected: func() bool { return true }, resolve: func(_ context.Context, raw string) (types.JID, error) { return types.ParseJID(raw) },
		send: func(context.Context, appstate.PatchInfo) error {
			return errors.New("failed to fetch app state after sending update: deadline exceeded")
		},
	}
	rec := httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sent":true`) || !strings.Contains(rec.Body.String(), `"confirmed":false`) || !strings.Contains(rec.Body.String(), "Do not retry") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	deps.send = func(context.Context, appstate.PatchInfo) error { return appStateNotSentError{context.DeadlineExceeded} }
	rec = httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)))
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), "nothing sent") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if _, err := store.db.Exec("UPDATE messages SET timestamp = 'unreadable'"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "invalid_argument") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestArchiveResolutionDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	deps := archiveDeps{connected: func() bool { return true }, resolve: func(ctx context.Context, _ string) (types.JID, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("resolution has no deadline")
		}
		<-ctx.Done()
		return types.EmptyJID, ctx.Err()
	}, send: func(context.Context, appstate.PatchInfo) error { t.Fatal("late send"); return nil }}
	rec := httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)).WithContext(ctx))
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), "nothing sent") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestArchiveParticipantCancellation(t *testing.T) {
	store := newTestMessageStore(t)
	group := "120363000000000001@g.us"
	if err := store.StoreMessage("M1", group, archiveTestChat, "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := archiveDeps{store: store, connected: func() bool { return true },
		resolve: func(_ context.Context, raw string) (types.JID, error) {
			if raw == archiveTestChat {
				cancel()
				return types.EmptyJID, context.Canceled
			}
			return types.ParseJID(raw)
		}, send: func(context.Context, appstate.PatchInfo) error { t.Fatal("late send"); return nil },
	}
	rec := httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+group+`","archived":true}`)).WithContext(ctx))
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), "nothing sent; safe to retry") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestArchiveDefiniteRejections(t *testing.T) {
	for _, failure := range []error{whatsmeow.ErrAppStateUpdate, whatsmeow.ErrNotConnected,
		errors.New("no app state keys found, creating app state keys is not yet supported")} {
		t.Run(failure.Error(), func(t *testing.T) {
			store := newTestMessageStore(t)
			if err := store.StoreMessage("M1", archiveTestChat, archiveTestChat, "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
				t.Fatal(err)
			}
			deps := archiveDeps{store: store, connected: func() bool { return true },
				resolve: func(_ context.Context, raw string) (types.JID, error) { return types.ParseJID(raw) },
				send:    func(context.Context, appstate.PatchInfo) error { return failure },
			}
			rec := httptest.NewRecorder()
			handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)))
			if rec.Code != 503 || !strings.Contains(rec.Body.String(), "not applied; safe to retry") || strings.Contains(rec.Body.String(), "unknown") {
				t.Fatal(rec.Code, rec.Body.String())
			}
		})
	}
}

func TestArchiveAcceptedPatchDisconnectedConfirmation(t *testing.T) {
	store := newTestMessageStore(t)
	if err := store.StoreMessage("M1", archiveTestChat, archiveTestChat, "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	deps := archiveDeps{store: store, connected: func() bool { return true },
		resolve: func(_ context.Context, raw string) (types.JID, error) { return types.ParseJID(raw) },
		send: func(context.Context, appstate.PatchInfo) error {
			return fmt.Errorf("failed to fetch app state after sending update: %w", whatsmeow.ErrNotConnected)
		},
	}
	rec := httptest.NewRecorder()
	handleArchiveChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","archived":true}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sent":true`) || !strings.Contains(rec.Body.String(), "Do not retry automatically") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestAppStateWriterSerializesAndCancelsWait(t *testing.T) {
	var calls atomic.Int32
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	b := testBridge(t, nil, newTestMessageStore(t), testLogger())
	b.SendAppState = func(ctx context.Context, _ appstate.PatchInfo) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() { done <- b.sendAppState(context.Background(), appstate.PatchInfo{}) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := b.sendAppState(ctx, appstate.PatchInfo{})
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("err=%v sends=%d", err, calls.Load())
	}
}
