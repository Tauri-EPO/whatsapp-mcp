package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
				resolve: func(raw string) (types.JID, error) {
					if raw == archiveTestChat {
						raw = "100000000000001@lid"
					}
					return types.ParseJID(raw)
				},
				send: func(ctx context.Context, patch appstate.PatchInfo) error {
					calls++
					if _, ok := ctx.Deadline(); !ok {
						t.Error("unbounded send")
					}
					act := patch.Mutations[0].Value.GetArchiveChatAction()
					rangeMsg := act.GetMessageRange()
					key := rangeMsg.GetMessages()[0].GetKey()
					if act.GetArchived() != archived || rangeMsg.GetLastMessageTimestamp() != ts.Unix() || key.GetID() != "M2" || key.GetFromMe() != tc.fromMe {
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
		{"bad JID", `{"chat_jid":"@","archived":true}`, true, false, false, false, 400},
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
				resolve: func(raw string) (types.JID, error) {
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
