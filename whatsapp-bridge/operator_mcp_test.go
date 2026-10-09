package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOperatorMCPRealAdminIntegration(t *testing.T) {
	if os.Getenv("WAMCP_REAL_ADMIN_TEST") != "1" {
		t.Skip("requires the Python-owned admin in the same loopback namespace")
	}
	db, err := sql.Open("sqlite", filepath.Join(os.Getenv("WAMCP_REAL_MESSAGES_TEST"), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, 2)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(runtimeSettingsSchema); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), &MessageStore{db: db}, testLogger())
	b.RuntimeDefaults, err = runtimeDefaults(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	routes := withMCPAdmin(operatorRoutes{settings: b.handleRuntimeSettings}, "fake-bridge-0123456789abcdef", newMCPAdminClient())
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, routes, testLogger()))
	defer server.Close()
	r, _ := http.NewRequest("GET", server.URL+"/operator/v1/transcription/usage", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Seconds  float64 `json:"seconds"`
		Requests int     `json:"requests"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&body) != nil || body.Seconds != 12 || body.Requests != 1 {
		t.Fatalf("real admin status=%d body=%+v", response.StatusCode, body)
	}
	t.Log("real Go operator -> Python admin -> notes.db: seconds=12 requests=1")
	status, _ := settingsRequest(t, server, "PATCH", `{"transcription.monthly_max_minutes":0.1,"transcription.cap_scope":"all"}`)
	if status != 200 {
		t.Fatal("operator PATCH failed")
	}
	r, _ = http.NewRequest("GET", server.URL+"/operator/v1/transcription/usage", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err = server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var after struct {
		Cap       float64 `json:"cap_seconds"`
		Remaining float64 `json:"remaining_seconds"`
	}
	if json.NewDecoder(response.Body).Decode(&after) != nil || after.Cap != 6 || after.Remaining != 0 {
		t.Fatalf("runtime cap propagation=%+v", after)
	}
	t.Log("real operator PATCH -> messages.db -> read-only Python consumer: cap_seconds=6 remaining_seconds=0")
}

type mcpTestTransport func(*http.Request) (*http.Response, error)

func (f mcpTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOperatorMCPHTTPForwardingAndDenyPaths(t *testing.T) {
	var calls atomic.Int64
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fake-bridge-0123456789abcdef" || r.Header.Get("Origin") != "" {
			t.Error("invalid internal authentication")
		}
		switch r.URL.Path {
		case "/admin/v1/transcription/usage":
			writeJSON(w, 200, map[string]any{"seconds": 12, "requests": 1})
		case "/admin/v1/health":
			writeJSON(w, 200, map[string]string{"last_mcp_call_at": "2026-10-09T00:00:00Z"})
		default:
			t.Error("unexpected forwarding route")
		}
	}))
	defer admin.Close()
	client := newMCPAdminClient()
	transport := client.Transport
	client.Transport = mcpTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "127.0.0.1:8091" {
			t.Error("non-loopback destination")
		}
		r.URL.Host = strings.TrimPrefix(admin.URL, "http://")
		return transport.RoundTrip(r)
	})
	routes := withMCPAdmin(operatorRoutes{health: func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"connected": false, "paired": false})
	}}, "fake-bridge-0123456789abcdef", client)
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, routes, testLogger()))
	defer server.Close()
	for _, tc := range []struct {
		token, host, origin string
		want                int
	}{
		{"", "", "", 401}, {"fake-bridge-0123456789abcdef", "", "", 401}, {"fake-mcp-0123456789abcdef", "", "", 401},
		{fakeOperatorToken, "wrong.example", "", 403}, {fakeOperatorToken, "", "http://wrong.example", 403}, {fakeOperatorToken, "", "", 200},
	} {
		before := calls.Load()
		r, _ := http.NewRequest("GET", server.URL+"/operator/v1/transcription/usage", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		if tc.host != "" {
			r.Host = tc.host
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.want {
			t.Fatalf("status %d want %d", response.StatusCode, tc.want)
		}
		if tc.want != 200 && calls.Load() != before {
			t.Fatal("denied request reached admin")
		}
		if tc.want == 200 && !strings.Contains(string(body), `"seconds":12`) {
			t.Fatal("wrong forwarded usage")
		}
	}
	r, _ := http.NewRequest("GET", server.URL+"/operator/v1/health", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body map[string]any
	if json.NewDecoder(response.Body).Decode(&body) != nil || body["last_mcp_call_at"] != "2026-10-09T00:00:00Z" {
		t.Fatal("missing MCP activity")
	}
}

func TestOperatorMCPBoundedResponseRedirectAndCancellation(t *testing.T) {
	for _, body := range []string{"invalid", strings.Repeat("x", (64<<10)+1)} {
		client := newMCPAdminClient()
		client.Transport = mcpTestTransport(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := mcpAdminRead(httptest.NewRequest("GET", "/", nil), client, "fake-token", "health"); err == nil {
			t.Fatal("invalid/oversized response accepted")
		}
	}
	client := newMCPAdminClient()
	if client.Timeout != 3*time.Second || client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("unbounded/ambient proxy client")
	}
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect allowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	if _, err := mcpAdminRead(r, client, "fake-token", "health"); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestTranscriptionSettingsHTTPBoundsCeilingScopeAndAtomicClear(t *testing.T) {
	b := newSettingsBridge(t)
	var err error
	b.RuntimeDefaults, err = runtimeDefaults(func(key string) string {
		if key == transcriptionCapEnv {
			return "10"
		}
		if key == transcriptionScopeEnv {
			return "all"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	server := settingsServer(t, b)
	for _, bad := range []string{`{"transcription.monthly_max_minutes":-1}`, `{"transcription.monthly_max_minutes":true}`, `{"transcription.monthly_max_minutes":"1"}`, `{"transcription.monthly_max_minutes":1,"transcription.cap_scope":"bad"}`} {
		if status, _ := settingsRequest(t, server, "PATCH", bad); status != 400 {
			t.Fatal("invalid cap accepted")
		}
		if _, state := settingsRequest(t, server, "GET", ""); state.Version != 0 {
			t.Fatal("partial cap write")
		}
	}
	status, state := settingsRequest(t, server, "PATCH", `{"transcription.monthly_max_minutes":20,"transcription.cap_scope":"ingest"}`)
	if status != 200 || state.Settings["transcription.monthly_max_minutes"].Value != float64(10) || state.Settings["transcription.cap_scope"].Value != "all" {
		t.Fatal("deploy ceiling relaxed")
	}
	_, state = settingsRequest(t, server, "PATCH", `{"transcription.monthly_max_minutes":1}`)
	if state.Settings["transcription.monthly_max_minutes"].Value != float64(1) || state.Settings["transcription.monthly_max_minutes"].Source != "runtime" {
		t.Fatal("cap did not tighten")
	}
	_, state = settingsRequest(t, server, "PATCH", `{"transcription.monthly_max_minutes":null}`)
	if state.Settings["transcription.monthly_max_minutes"].Value != float64(10) || state.Settings["transcription.monthly_max_minutes"].Source != "env" {
		t.Fatal("cap clear failed")
	}
}

func TestTranscriptionCapEnvironmentDecimalForms(t *testing.T) {
	for _, raw := range []string{"0x1p4", "1_0", "0b10", "NaN", "Inf", "١"} {
		if _, err := runtimeDefaults(func(key string) string {
			if key == transcriptionCapEnv {
				return raw
			}
			return ""
		}); err == nil {
			t.Fatalf("non-decimal form %q accepted", raw)
		}
	}
	for raw, want := range map[string]float64{" 1.5 ": 1.5, "+1": 1, ".5": 0.5, "1.": 1, "1e2": 100, "01": 1} {
		values, err := runtimeDefaults(func(key string) string {
			if key == transcriptionCapEnv {
				return raw
			}
			return ""
		})
		if err != nil || values["transcription.monthly_max_minutes"].Value != want {
			t.Fatalf("decimal form %q: %v, %v", raw, values, err)
		}
	}
}
