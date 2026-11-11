package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func blockItems() []types.BlocklistItem {
	return []types.BlocklistItem{
		{LID: types.NewJID("100000000000001", types.HiddenUserServer), PN: types.NewJID("5511999999999", types.DefaultUserServer)},
		{LID: types.NewJID("100000000000002", types.HiddenUserServer), PN: types.NewJID("5511888888888", types.DefaultUserServer)},
	}
}

func TestBlockSendsResolvedAction(t *testing.T) {
	for _, tc := range []struct {
		body, wantJID string
		want          events.BlocklistChangeAction
	}{
		{`{"jid":"5511999999999","blocked":true}`, "5511999999999@s.whatsapp.net", events.BlocklistChangeActionBlock},
		{`{"jid":"100000000000001@lid","blocked":false}`, "100000000000001@lid", events.BlocklistChangeActionUnblock},
	} {
		calls := 0
		deps := blockDeps{policy: chatPolicy{}, connected: func() bool { return true },
			update: func(_ context.Context, jid types.JID, action events.BlocklistChangeAction) ([]types.BlocklistItem, error) {
				calls++
				if jid.String() != tc.wantJID || action != tc.want {
					t.Fatalf("jid=%s action=%s", jid, action)
				}
				return blockItems(), nil
			}}
		rec := httptest.NewRecorder()
		handleBlock(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
		var body struct {
			Blocklist []blocklistEntry `json:"blocklist"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 200 || calls != 1 || len(body.Blocklist) != 2 {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
		}
	}
}

func TestBlockRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name, body, chats string
		connected         bool
		status            int
	}{
		{"bad json", `{`, "", true, 400},
		{"missing flag", `{"jid":"5511999999999"}`, "", true, 400},
		{"missing jid", `{"blocked":true}`, "", true, 400},
		{"group", `{"jid":"120363000000000001@g.us","blocked":true}`, "", true, 400},
		{"newsletter", `{"jid":"120363000000000001@newsletter","blocked":true}`, "", true, 400},
		{"device suffix", `{"jid":"5511999999999:2@s.whatsapp.net","blocked":true}`, "", true, 400},
		{"two servers", `{"jid":"5511888888888@s.whatsapp.net@g.us","blocked":true}`, "", true, 400},
		{"empty user", `{"jid":"@lid","blocked":true}`, "", true, 400},
		{"letters", `{"jid":"bob","blocked":true}`, "", true, 400},
		{"allow-list", `{"jid":"5511999999999","blocked":true}`, "5511888888888", true, 403},
		{"disconnected", `{"jid":"5511999999999","blocked":true}`, "", false, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := blockDeps{policy: parseChatPolicy(tc.chats), connected: func() bool { return tc.connected },
				update: func(context.Context, types.JID, events.BlocklistChangeAction) ([]types.BlocklistItem, error) {
					t.Fatal("blocklist updated")
					return nil, nil
				}}
			rec := httptest.NewRecorder()
			handleBlock(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestBlockUpdateFailureIs502(t *testing.T) {
	deps := blockDeps{policy: chatPolicy{}, connected: func() bool { return true },
		update: func(context.Context, types.JID, events.BlocklistChangeAction) ([]types.BlocklistItem, error) {
			return nil, errors.New("no LID found")
		}}
	rec := httptest.NewRecorder()
	handleBlock(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jid":"5511999999999","blocked":true}`)))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "no LID found") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBlocklistFilteredByAllowList(t *testing.T) {
	deps := blockDeps{policy: parseChatPolicy("5511999999999"), connected: func() bool { return true },
		list: func(context.Context) ([]types.BlocklistItem, error) { return blockItems(), nil }}
	rec := httptest.NewRecorder()
	handleBlocklist(deps)(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body struct {
		Blocklist []blocklistEntry `json:"blocklist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(body.Blocklist) != 1 || body.Blocklist[0].Phone != "5511999999999@s.whatsapp.net" || body.Blocklist[0].LID != "100000000000001@lid" {
		t.Fatalf("blocklist=%+v", body.Blocklist)
	}
}

func TestBlockMuxGuards(t *testing.T) {
	for _, tc := range []struct {
		name, path, method, allow, deny string
		readOnly                        bool
		status                          int
	}{
		{"read-only block", "/api/block", http.MethodPost, "", "", true, 403},
		{"deny block", "/api/block", http.MethodPost, "", "block_contact", false, 403},
		{"allow other", "/api/block", http.MethodPost, "send_message", "", false, 403},
		{"block GET", "/api/block", http.MethodGet, "", "", false, 405},
		{"blocklist POST", "/api/blocklist", http.MethodPost, "", "", false, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.ReadOnly = readOnlyPolicy{enabled: tc.readOnly}
			b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
			b.Connected = func() bool { t.Fatal("connected called past guard"); return false }
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:8080"+tc.path, strings.NewReader(`{"jid":"5511999999999","blocked":true}`))
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
