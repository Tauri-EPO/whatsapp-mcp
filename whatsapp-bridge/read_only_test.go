package main

// WHATSAPP_READ_ONLY: mutating endpoints answer 403, reads keep working.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const readOnlyTestToken = "test-token-0123456789"

func TestParseReadOnly(t *testing.T) {
	cases := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "", want: false},
		{raw: "   ", want: false},
		{raw: "1", want: true},
		{raw: "true", want: true},
		{raw: "TRUE", want: true},
		{raw: " yes ", want: true},
		{raw: "on", want: true},
		{raw: "0", want: false},
		{raw: "false", want: false},
		{raw: "no", want: false},
		{raw: "OFF", want: false},
		// A security switch must not fall back to "off" on a typo.
		{raw: "treu", wantErr: true},
		{raw: "2", wantErr: true},
		{raw: "read-only", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseReadOnly(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseReadOnly(%q) = %v, want error", tc.raw, got.enabled)
			} else if !strings.Contains(err.Error(), readOnlyEnv) {
				t.Errorf("parseReadOnly(%q) error %q does not name %s", tc.raw, err, readOnlyEnv)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseReadOnly(%q) unexpected error: %v", tc.raw, err)
		} else if got.enabled != tc.want {
			t.Errorf("parseReadOnly(%q) = %v, want %v", tc.raw, got.enabled, tc.want)
		}
	}
}

func TestReadOnlySummary(t *testing.T) {
	if s := (readOnlyPolicy{}).Summary(); !strings.Contains(s, readOnlyEnv) || !strings.Contains(s, "unset") {
		t.Errorf("disabled summary = %q", s)
	}
	if s := (readOnlyPolicy{enabled: true}).Summary(); !strings.Contains(s, readOnlyEnv) || !strings.Contains(s, "403") {
		t.Errorf("enabled summary = %q", s)
	}
}

// readOnlyRequest issues an authenticated request against a mux built from a
// bridge with the given policy.
func readOnlyRequest(t *testing.T, enabled bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	b := testBridge(t, nil, newTestMessageStore(t), testLogger())
	b.ReadOnly = readOnlyPolicy{enabled: enabled}
	b.MediaRoots = []string{t.TempDir()}
	mux := b.newRESTMux(8080, readOnlyTestToken)

	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Every endpoint with a side effect. The bodies are valid on purpose: the
// refusal must happen before parsing, connecting or touching the store.
var readOnlyMutatingEndpoints = []struct {
	path string
	body string
}{
	{"/api/send", `{"recipient":"5511999999999","message":"hi"}`},
	{"/api/react", `{"recipient":"5511999999999@s.whatsapp.net","message_id":"M1","emoji":"👍"}`},
	{"/api/typing", `{"chat_jid":"5511999999999@s.whatsapp.net","is_typing":true}`},
	{"/api/mark-read", `{"chat_jid":"5511999999999@s.whatsapp.net","message_ids":["M1"]}`},
	{"/api/delete", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1","for_everyone":true}`},
	{"/api/edit", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1","text":"new"}`},
	{"/api/forward", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1","to_chat_jid":"120363000000000001@g.us"}`},
	{"/api/group/participants", `{"group_jid":"120363000000000001@g.us","action":"remove","participants":["5511999999999"]}`},
	{"/api/group/subject", `{"group_jid":"120363000000000001@g.us","name":"new"}`},
	{"/api/group/invite", `{"group_jid":"120363000000000001@g.us","reset":true}`},
	{"/api/group/leave", `{"group_jid":"120363000000000001@g.us"}`},
	{"/api/media/purge", `{"chat_jid":"5511999999999@s.whatsapp.net","dry_run":false}`},
	{"/api/history", `{"chat_jid":"5511999999999@s.whatsapp.net","count":50}`},
}

func TestReadOnlyRefusesMutatingEndpoints(t *testing.T) {
	for _, tc := range readOnlyMutatingEndpoints {
		rec := readOnlyRequest(t, true, http.MethodPost, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403; body: %s", tc.path, rec.Code, rec.Body.String())
			continue
		}
		var body apiError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: body is not JSON: %v", tc.path, err)
			continue
		}
		if body.Success {
			t.Errorf("%s: success = true on a refusal", tc.path)
		}
		if body.Error.Code != "denied" {
			t.Errorf("%s: error.code = %q, want denied", tc.path, body.Error.Code)
		}
		if !strings.Contains(body.Message, readOnlyEnv) || !strings.Contains(body.Message, tc.path) {
			t.Errorf("%s: message %q should name the variable and the endpoint", tc.path, body.Message)
		}
	}
}

// Disabled policy: the same requests reach their handler. They fail for other
// reasons (no WhatsApp connection, unknown message), never with the 403 above.
func TestReadOnlyDisabledLetsMutatingEndpointsThrough(t *testing.T) {
	for _, tc := range readOnlyMutatingEndpoints {
		rec := readOnlyRequest(t, false, http.MethodPost, tc.path, tc.body)
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), readOnlyEnv) {
			t.Errorf("%s: refused while %s is unset", tc.path, readOnlyEnv)
		}
	}
}

// Reads stay open in read-only mode; /api/download only fills the local cache.
func TestReadOnlyKeepsReadEndpoints(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/health", ""},
		{http.MethodGet, "/api/ready", ""},
		{http.MethodGet, "/api/version", ""},
		{http.MethodPost, "/api/poll", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1"}`},
		{http.MethodPost, "/api/group/members", `{"group_jid":"120363000000000001@g.us"}`},
		{http.MethodPost, "/api/download", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1"}`},
	} {
		rec := readOnlyRequest(t, true, tc.method, tc.path, tc.body)
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), readOnlyEnv) {
			t.Errorf("%s: refused as mutating, but it is a read", tc.path)
		}
	}
}
