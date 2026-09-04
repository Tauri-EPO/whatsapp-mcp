package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusForbidden, "nope")
	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 || body.Success || body.Message != "nope" || body.Error.Code != "denied" || body.Error.Message != "nope" {
		t.Fatalf("%d %+v", rec.Code, body)
	}
	for status, code := range map[int]string{400: "invalid_argument", 404: "not_found", 502: "bridge_unavailable", 500: "internal", 405: "invalid_argument"} {
		if got := errorCode(status); got != code {
			t.Errorf("%d → %s, want %s", status, got, code)
		}
	}
}

func TestRequireMethod(t *testing.T) {
	called := false
	h := requireMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) { called = true })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" || called || !strings.Contains(rec.Body.String(), "invalid_argument") {
		t.Fatalf("%d %s called=%v", rec.Code, rec.Body.String(), called)
	}
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/x", nil))
	if !called {
		t.Fatal("POST should pass through")
	}
}

func TestRequestLogLine(t *testing.T) {
	rec := installRecordingLogger(t)
	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	req := httptest.NewRequest(http.MethodPost, "/api/send", nil)
	req.RemoteAddr = "10.0.0.7:5555"
	req.Header.Set("User-Agent", "unit")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := rec.String()
	if !strings.Contains(out, "[INFO] POST /api/send → 418") || !strings.Contains(out, "from=10.0.0.7") || !strings.Contains(out, `ua="unit"`) {
		t.Fatalf("log = %q", out)
	}
	// Health probes are DEBUG so a 30 s Docker healthcheck does not fill the log.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if !strings.Contains(rec.String(), "[DEBUG] GET /api/health") {
		t.Fatalf("health probe should log at DEBUG: %q", rec.String())
	}
}

func TestEveryEndpointRejectsWrongMethodAsJSON(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	mux := b.newRESTMux(8080, token, nil)
	for _, path := range []string{"/api/send", "/api/mark-read", "/api/react", "/api/download", "/api/typing", "/api/delete", "/api/edit", "/api/forward", "/api/group/leave"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("GET %s → %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/api/health", "/api/ready"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s → %d", path, rec.Code)
		}
	}
}
