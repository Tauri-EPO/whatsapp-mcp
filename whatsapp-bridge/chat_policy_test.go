package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatPolicy_ParseAndAllow(t *testing.T) {
	open := parseChatPolicy("")
	if open.restricted || !open.Allows("anyone@g.us") || !open.Allows("") {
		t.Fatal("empty env must be unrestricted")
	}

	p := parseChatPolicy(" 5511999999999, *@g.us ,, 120363000000000009@G.US:3@x ")
	if !p.restricted {
		t.Fatal("expected restricted policy")
	}
	cases := map[string]bool{
		"5511999999999":                  true, // bare number as accepted by /api/send
		"5511999999999@s.whatsapp.net":   true,
		"5511999999999:7@s.whatsapp.net": true, // device suffix ignored
		"5511888888888":                  false,
		"120363000000000001@g.us":        true, // wildcard
		"5555@lid":                       false,
		"":                               false,
	}
	for target, want := range cases {
		if got := p.Allows(target); got != want {
			t.Errorf("Allows(%q) = %v, want %v", target, got, want)
		}
	}
	if !strings.Contains(p.Summary(), "restricted to 2 chat(s)") || !strings.Contains(p.Summary(), "*@g.us") {
		t.Fatalf("unexpected summary %q", p.Summary())
	}
}

func TestRejectByChatPolicy(t *testing.T) {
	p := parseChatPolicy("5511999999999")

	rec := httptest.NewRecorder()
	if rejectByChatPolicy(rec, p, "5511999999999@s.whatsapp.net") {
		t.Fatal("allowed target must not be rejected")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatal("no response must be written for an allowed target")
	}

	rec = httptest.NewRecorder()
	if !rejectByChatPolicy(rec, p, "5511888888888@s.whatsapp.net") {
		t.Fatal("blocked target must be rejected")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["success"] != false || !strings.Contains(body["message"].(string), chatPolicyEnv) {
		t.Fatalf("unexpected body %v", body)
	}
}

// The policy must run before any WhatsApp interaction: with a restricted
// policy and a nil client, a blocked /api/send returns 403 instead of
// touching the (absent) connection.
func TestRESTSendRejectedByChatPolicy(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, testLogger())
	b.Policy = parseChatPolicy("5511999999999")
	mux := b.newRESTMux(8080, "test-token-0123456789", []string{t.TempDir()})

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send",
		strings.NewReader(`{"recipient":"5511888888888","message":"hi"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer test-token-0123456789")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}
