package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthAndReadyEndpoints(t *testing.T) {
	// An unconnected, unpaired client: alive but not ready.
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.startedAt = time.Now().Add(-90 * time.Second)
	mux := b.newRESTMux(8080, "test-token-0123456789")

	get := func(path string) (*httptest.ResponseRecorder, map[string]interface{}) {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer test-token-0123456789")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var body map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}

	rec, body := get("/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/health = %d, want 200 (liveness must not depend on WhatsApp)", rec.Code)
	}
	if body["connected"] != false || body["status"] != "awaiting_pairing" {
		t.Fatalf("unexpected health body: %v", body)
	}
	if up, _ := body["uptime_seconds"].(float64); up < 89 {
		t.Fatalf("uptime_seconds = %v", body["uptime_seconds"])
	}

	rec, body = get("/api/ready")
	if rec.Code != http.StatusServiceUnavailable || body["connected"] != false {
		t.Fatalf("/api/ready = %d %v, want 503 while disconnected", rec.Code, body)
	}
}

func TestVersionEndpointIsUnauthenticated(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	mux := b.newRESTMux(8080, "test-token-0123456789")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/version", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/version = %d, want 200 without a token", rec.Code)
	}
	var info VersionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || info.Version == "" || info.Go == "" {
		t.Fatalf("bad body %s (err %v)", rec.Body.String(), err)
	}
}

// The startup line is logged before the store is open, so it carries the build
// identity and nothing else: it used to say fts5=off on every build, next to
// the store line and /api/version saying the opposite (issue #499).
func TestStartupLineIsTheBuildIdentity(t *testing.T) {
	info := buildInfo()
	want := "whatsapp-bridge " + info.Version + ", commit " + info.Commit + ", " + info.Go + ", whatsmeow " + info.Whatsmeow
	for _, v := range []VersionInfo{info, info.withFTS(true)} {
		if got := v.String(); got != want {
			t.Errorf("startup line = %q, want %q", got, want)
		}
	}
	if info.Version == "" || info.Commit == "" || info.Go == "" || info.Whatsmeow == "" {
		t.Errorf("a field of the identity is empty: %+v", info)
	}
	if info.FTS5 {
		t.Error("the identity alone must not claim an FTS index")
	}
}

// /api/version is where the FTS5 state is reported, from the open store.
func TestVersionEndpointReportsTheStoreFTSState(t *testing.T) {
	for _, fts := range []bool{false, true} {
		ms := newTestMessageStore(t)
		ms.fts = fts
		mux := testBridge(t, nil, ms, installRecordingLogger(t)).newRESTMux(8080, "test-token-0123456789")
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/version", nil)
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad body %s: %v", rec.Body.String(), err)
		}
		if got, ok := body["fts5"].(bool); !ok || got != fts {
			t.Errorf("store fts=%v: /api/version says fts5=%v (%s)", fts, body["fts5"], rec.Body.String())
		}
	}
}
