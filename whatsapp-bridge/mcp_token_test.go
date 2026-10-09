package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tokenRequest(b *Bridge, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	b.handleMCPToken(w, httptest.NewRequest(method, "/operator/v1/mcp-token", strings.NewReader(body)))
	return w
}

func storedToken(t *testing.T, b *Bridge) mcpTokenState {
	t.Helper()
	tx, err := b.Store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	s, err := readMCPToken(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMCPTokenRotationPersistenceExpirySecondRotationAndClear(t *testing.T) {
	b := newSettingsBridge(t)
	b.MCPEnvHash = tokenHash("fake-original-mcp-token")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	b.sendNow = func() time.Time { return now }
	log := &recordingLogger{}
	b.Log = log
	next := tokenHash("fake-next-mcp-token")
	third := tokenHash("fake-third-mcp-token")
	w := tokenRequest(b, "POST", `{"sha256":"`+next+`","previous_valid_until":"2026-10-12T12:00:00Z"}`)
	s := storedToken(t, b)
	if w.Code != 200 || s.Current != next || s.Previous != b.MCPEnvHash || !s.PreviousValidUntil.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rotation=%d %+v", w.Code, s)
	}
	// Fresh store/bridge, no process-local state shared.
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	fresh := testBridge(t, b.currentClient(), ms, testLogger())
	fresh.MCPEnvHash = b.MCPEnvHash
	fresh.sendNow = b.sendNow
	if restored := storedToken(t, fresh); restored != s {
		t.Fatal("rotation not persisted")
	}
	w = tokenRequest(fresh, "POST", `{"sha256":"`+third+`","token":"fake-third-mcp-token","previous_valid_until":"2026-10-09T13:00:00Z"}`)
	s = storedToken(t, b)
	if w.Code != 200 || s.Previous != next || s.Current != third {
		t.Fatal("second rotation kept oldest")
	}
	before, _ := b.settingsSnapshot(context.Background())
	for _, body := range []string{`{"sha256":"bad"}`, `{"sha256":"` + third + `","token":"fake-mismatching-token"}`, `{"sha256":"` + third + `","previous_valid_until":"bad"}`, `{"sha256":"` + third + `","extra":true}`, `null`} {
		w = tokenRequest(b, "POST", body)
		after, _ := b.settingsSnapshot(context.Background())
		if w.Code != 400 || before.Version != after.Version {
			t.Fatalf("invalid rotation=%d %s", w.Code, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	b.handleRuntimeSettings(w, httptest.NewRequest("GET", "/operator/v1/settings", nil))
	if strings.Contains(w.Body.String(), third) || strings.Contains(w.Body.String(), mcpTokenKey) {
		t.Fatal("GET leaked hashes")
	}
	w = tokenRequest(b, "DELETE", "")
	if w.Code != 200 || storedToken(t, b).Current != "" {
		t.Fatal("clear failed")
	}
	if strings.Contains(log.String(), next) || strings.Contains(log.String(), "fake-next-mcp-token") || !strings.Contains(log.String(), next[:8]) {
		t.Fatal("unsafe or missing audit")
	}
	if !strings.Contains(b.renderMetrics(), "whatsapp_runtime_settings_version 3") {
		t.Fatal("rotation did not share version clock")
	}
}

func TestSendUsageAndTokenOperatorDenyPaths(t *testing.T) {
	b := newSettingsBridge(t)
	h := newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{sendUsage: b.handleSendUsage, mcpToken: b.handleMCPToken}, b.Log)
	for _, route := range []struct{ method, path string }{{"GET", "/operator/v1/send/usage"}, {"POST", "/operator/v1/mcp-token"}, {"DELETE", "/operator/v1/mcp-token"}} {
		for _, tc := range []struct {
			token, host, origin string
			status              int
		}{{"", "127.0.0.1:8090", "", 401}, {readOnlyTestToken, "127.0.0.1:8090", "", 401}, {fakeOperatorToken, "evil.example", "", 403}, {fakeOperatorToken, "127.0.0.1:8090", "https://evil.example", 403}} {
			r := httptest.NewRequest(route.method, "http://127.0.0.1:8090"+route.path, strings.NewReader(`{}`))
			r.Host = tc.host
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("deny %s=%d wanted %d", route.path, w.Code, tc.status)
			}
		}
		for _, path := range []string{route.path, strings.Replace(route.path, "/operator/v1/", "/api/", 1)} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(route.method, "http://127.0.0.1:8080"+path, nil)
			r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, r)
			if w.Code != 404 {
				t.Fatal("operator route on data plane")
			}
		}
	}
	limitPatch(t, b, `{"send.rate_per_day":7}`)
	r := httptest.NewRequest("GET", "http://127.0.0.1:8090/operator/v1/send/usage", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var usage sendUsage
	_ = json.Unmarshal(w.Body.Bytes(), &usage)
	if w.Code != 200 || usage.DayLimit != 7 {
		t.Fatal("usage effective runtime limit")
	}
	if w := tokenRequest(b, http.MethodGet, ""); w.Code != 405 {
		t.Fatal("method not enforced")
	}
}
