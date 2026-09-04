package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderMetrics_ExposesCountersAndGauges(t *testing.T) {
	b := testBridge(nil, nil, installRecordingLogger(t))
	b.metrics.messagesStored.Add(3)
	b.metrics.sendFailures.Add(1)
	b.metrics.recordRequest(200)
	b.metrics.recordRequest(204)
	b.metrics.recordRequest(404)

	out := b.renderMetrics()
	for _, want := range []string{
		"# TYPE whatsapp_bridge_up gauge\nwhatsapp_bridge_up 1\n",
		"whatsapp_bridge_connected 0\n",
		"whatsapp_bridge_messages_stored_total 3\n",
		"whatsapp_bridge_send_failures_total 1\n",
		"whatsapp_bridge_messages_sent_total 0\n",
		"whatsapp_bridge_http_requests_total{class=\"2xx\"} 2\n",
		"whatsapp_bridge_http_requests_total{class=\"4xx\"} 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q:\n%s", want, out)
		}
	}
}

func TestMetricsRoute_GetOnlyAndUnauthenticated(t *testing.T) {
	t.Setenv(metricsEnv, "")
	b := testBridge(nil, nil, installRecordingLogger(t))
	mux := b.newRESTMux(8080, "secret-token")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "whatsapp_bridge_up 1") {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://localhost:8080/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", rec.Code)
	}
}

func TestMetricsRoute_DisabledByEnv(t *testing.T) {
	t.Setenv(metricsEnv, "false")
	b := testBridge(nil, nil, installRecordingLogger(t))
	mux := b.newRESTMux(8080, "secret-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8080/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics with WHATSAPP_METRICS=false = %d, want 404", rec.Code)
	}
}

func TestRequestLog_CountsStatusClass(t *testing.T) {
	installRecordingLogger(t)
	m := newMetricsRegistry()
	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }), m)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if got := m.requests["5xx"]; got != 1 {
		t.Errorf("requests[5xx] = %d, want 1", got)
	}
}

func TestWebhookSender_CountsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	b := testBridge(nil, nil, installRecordingLogger(t))
	b.Webhook = &webhookSender{client: srv.Client(), enabled: true, url: srv.URL, failures: &b.metrics.webhookFailures}
	b.Webhook.sendPayload(WebhookPayload{Sender: "s@s.whatsapp.net", Content: "hi"})
	if got := b.metrics.webhookFailures.Load(); got != 1 {
		t.Errorf("webhookFailures = %d, want 1", got)
	}
	if !bytes.Contains([]byte(b.renderMetrics()), []byte("whatsapp_bridge_webhook_failures_total 1")) {
		t.Errorf("render missing webhook failure counter")
	}
}
