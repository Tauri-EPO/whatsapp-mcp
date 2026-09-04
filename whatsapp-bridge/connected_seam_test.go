package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// The Bridge.Connected seam lets handler tests pass the connection check
// without a live socket; the whatsmeow calls behind it then fail cleanly.

func seamRequest(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestReadyAndHealthFollowConnected(t *testing.T) {
	const token = "test-token-0123456789"
	self := types.NewJID("5511999999999", types.DefaultUserServer)
	b := testBridge(newTestClientWithSelf(&mockLIDStore{}, self), newTestMessageStore(t), installRecordingLogger(t))
	mux := b.newRESTMux(8080, token)

	b.Connected = func() bool { return false }
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, seamRequest(http.MethodGet, "/api/ready", "", token))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"status":"disconnected"`) {
		t.Fatalf("disconnected ready = %d %s", rec.Code, rec.Body.String())
	}

	b.Connected = func() bool { return true }
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, seamRequest(http.MethodGet, "/api/ready", "", token))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body["status"] != "ok" || body["connected"] != true || body["paired"] != true {
		t.Fatalf("connected ready = %d %v", rec.Code, body)
	}
	if !strings.Contains(b.renderMetrics(), "whatsapp_bridge_connected 1") {
		t.Errorf("metrics should follow Connected too")
	}
}

func TestHandlersReportOfflineThroughConnected(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	mux := b.newRESTMux(8080, token)
	chat := "5511888888888@s.whatsapp.net"

	// Every endpoint that needs WhatsApp consults the seam, not the client.
	calls := 0
	b.Connected = func() bool { calls++; return false }
	cases := map[string]string{
		"/api/mark-read":     `{"chat_jid":"` + chat + `","message_ids":["m1"]}`,
		"/api/history":       `{"chat_jid":"` + chat + `","count":5}`,
		"/api/group/members": `{"group_jid":"120363012345678901@g.us"}`,
		"/api/group/invite":  `{"group_jid":"120363012345678901@g.us"}`,
	}
	for path, body := range cases {
		before := calls
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, seamRequest(http.MethodPost, path, body, token))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s: route missing or wrong method (%d)", path, rec.Code)
		}
		if calls == before {
			t.Errorf("%s: handler did not consult Bridge.Connected", path)
		}
		if !strings.Contains(strings.ToLower(rec.Body.String()), "not connected") {
			t.Errorf("%s offline: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
