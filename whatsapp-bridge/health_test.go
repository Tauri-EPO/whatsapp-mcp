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
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.startedAt = time.Now().Add(-90 * time.Second)
	mux := b.newRESTMux(8080, "test-token-0123456789", nil)

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
