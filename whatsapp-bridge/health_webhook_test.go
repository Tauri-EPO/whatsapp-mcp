package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaskedWebhookURL(t *testing.T) {
	for i, tc := range []struct{ raw, want string }{
		{"https://example.com/hooks/private-token-abcd", "https://example.com/…abcd"},
		{"https://example.com/hooks/private-token-abcd?key=private-query#private-fragment", "https://example.com/…abcd"},
		{"https://private-user:private-password@example.com/hooks/private-token-abcd", "https://example.com/…abcd"},
		{"https://example.com/abc", "https://example.com/…"},
		{"https://example.com/", "https://example.com/…"},
		{"https://example.com", "https://example.com/…"},
		{"https://example.com:8443/hooks/private-token-abcd", "https://example.com:8443/…abcd"},
		{"https://example.com/%0Aprivate-token-abcd", "https://example.com/…abcd"},
		{"https://example.com/%zz", ""},
		{"/hooks/private-token-abcd", ""},
	} {
		if got := maskedWebhookURL(tc.raw); got != tc.want {
			t.Errorf("masking case %d returned an unexpected URL", i)
		}
	}
}

func TestWebhookStatusUsesSnapshotQueue(t *testing.T) {
	queue := make(chan runtimeWebhookDelivery, 2)
	queue <- runtimeWebhookDelivery{}
	queue <- runtimeWebhookDelivery{}
	b := &Bridge{RuntimeWebhook: &runtimeWebhook{
		state:      runtimeWebhookState{URL: "https://example.com/hooks/private-token-abcd", Enabled: true, Queued: 99, Dropped: 7},
		generation: &runtimeWebhookGeneration{queue: queue},
	}}
	if got := b.webhookStatus(); got.Queued != 2 || got.Dropped != 7 {
		t.Fatal("view did not use live snapshot queue")
	}
}

func TestHealthWebhookViewLifecycleAndAuth(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNoContent)
	b, operator, destination, receipts, _ := runtimeWebhookFixture(t, &status)
	runtime := b.RuntimeWebhook
	runtime.options.FailureLimit = 1
	handler := b.runtimeRESTHandler(8080, "test-token-0123456789")
	const path = "/hooks/private-token-abcd"
	const query = "key=private-query"
	const authorization = "Bearer fake-receiver-credential"
	get := func(h http.Handler, endpoint, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+endpoint, nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	read := func() webhookStatusView {
		t.Helper()
		rec := get(handler, "/api/health", "Bearer test-token-0123456789")
		if rec.Code != http.StatusOK {
			t.Fatalf("health returned %d", rec.Code)
		}
		var body struct{ Webhook webhookStatusView }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{fakeWebhookSecret, authorization, path, query, "secret_fingerprint"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Fatal("private webhook field leaked in REST health")
			}
		}
		if fingerprint := runtime.snapshot().Fingerprint; fingerprint != "" && strings.Contains(rec.Body.String(), fingerprint) {
			t.Fatal("secret fingerprint leaked in REST health")
		}
		return body.Webhook
	}
	b.RuntimeWebhook = nil
	if got := read(); got.Configured || got.Enabled || got.Reason != "runtime webhook disabled" {
		t.Fatal("incorrect runtime-off view")
	}
	b.RuntimeWebhook = runtime
	if got := read(); got.Configured || got.Enabled || got.Reason != "runtime webhook has no destination" {
		t.Fatal("incorrect no-destination view")
	}
	if code, _ := configureWebhookFields(t, operator, destination+path+"?"+query, map[string]any{"authorization": authorization}); code != 200 {
		t.Fatalf("configure = %d", code)
	}
	if got := read(); !got.Configured || !got.Enabled || got.URL != strings.TrimSuffix(destination, "/")+"/…abcd" || got.Failures != 0 || got.Reason != "" {
		t.Fatal("incorrect configured view")
	}
	for _, h := range []http.Handler{handler, b.newRESTMux(8080, "test-token-0123456789")} {
		for _, auth := range []string{"", "Bearer wrong-token"} {
			for _, endpoint := range []string{"/api/health", "/api/ready"} {
				rec := get(h, endpoint, auth)
				if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "webhook") {
					t.Fatalf("unauthenticated %s returned %d", endpoint, rec.Code)
				}
			}
		}
		for _, endpoint := range []string{"/api/version", "/metrics"} {
			if rec := get(h, endpoint, ""); strings.Contains(rec.Body.String(), "private-token") || strings.Contains(rec.Body.String(), "…abcd") {
				t.Fatalf("webhook destination on public endpoint %s", endpoint)
			}
		}
	}
	runtime.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	_ = receiveWebhook(t, receipts)
	deadline := time.Now().Add(5 * time.Second)
	for runtime.snapshot().LastSuccess == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := read(); got.LastSuccess == nil || got.Queued != 0 || got.Dropped != 0 {
		t.Fatal("incorrect successful-delivery view")
	}
	status.Store(http.StatusForbidden)
	runtime.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	_ = receiveWebhook(t, receipts)
	deadline = time.Now().Add(5 * time.Second)
	for runtime.snapshot().Enabled && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := read(); !got.Configured || got.Enabled || got.Failures != 1 || got.Reason != "http_403" || got.Dropped != 1 || got.LastSuccess == nil {
		t.Fatal("incorrect disabled-after-failure view")
	}
	// The private operator response still has its original full snapshot.
	if code, data := webhookOperatorRequest(t, operator, http.MethodGet, "/operator/v1/health", ""); code != 200 || !strings.Contains(string(data), path) || !strings.Contains(string(data), "secret_fingerprint") {
		t.Fatalf("operator health changed: %d", code)
	}
}
