package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func webhookReviewRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, nil)
	r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestWebhookHealthCannotBypassDetailDenial(t *testing.T) {
	for _, allow := range []bool{false, true} {
		b, operator, destination, _, logger := runtimeWebhookFixture(t, nil)
		if code, _ := configureWebhookFields(t, operator, destination+"/private-receiver-path?private-query", map[string]any{"authorization": "Bearer fake-private-authorization"}); code != 200 {
			t.Fatal("configuration failed")
		}
		if allow {
			b.Tools = mustToolPolicy(t, "bridge_status", "")
		} else {
			b.Tools = mustToolPolicy(t, "", "get_webhook")
		}
		b.RuntimeWebhook.mu.Lock()
		b.RuntimeWebhook.state.Reason = "https://example.com/private-receiver-path?private-query"
		b.RuntimeWebhook.mu.Unlock()
		h := b.runtimeRESTHandler(8080, readOnlyTestToken)
		rec := webhookReviewRequest(h, "GET", "/api/health")
		var body struct{ Webhook map[string]any }
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Webhook) != 4 || body.Webhook["reason"] != "runtime webhook reason unavailable" {
			t.Fatal("health did not preserve the compact, sanitized boundary")
		}
		if denied := webhookReviewRequest(h, "GET", "/api/webhook"); denied.Code != 403 {
			t.Fatal("detail policy was bypassed")
		}
		b.Tools = toolPolicy{}
		detail := webhookReviewRequest(b.runtimeRESTHandler(8080, readOnlyTestToken), "GET", "/api/webhook")
		var view webhookStatusView
		if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &view) != nil || view.Reason != "runtime webhook reason unavailable" {
			t.Fatal("detailed read did not sanitize the stored reason")
		}
		for _, text := range []string{rec.Body.String(), detail.Body.String(), logger.String()} {
			for _, private := range []string{fakeWebhookSecret, "fake-private-authorization", "private-receiver-path", "private-query", b.RuntimeWebhook.snapshot().Fingerprint} {
				if strings.Contains(text, private) {
					t.Fatal("private webhook data escaped via health or logs")
				}
			}
		}
	}
}

func TestWebhookReadWorksWhileClientGateHeldAndIdle(t *testing.T) {
	for _, denied := range []bool{false, true} {
		b := newSettingsBridge(t)
		var err error
		b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
			if denied && name == denyToolsEnv {
				return "get_webhook"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		b.RuntimeWebhook = newRuntimeWebhook(b.ctx, runtimeWebhookOptions{Enabled: true})
		b.operatorLogout.Store(true)
		h := b.runtimeRESTHandler(8080, readOnlyTestToken)
		b.clientGate.Lock()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- webhookReviewRequest(h, "GET", "/api/webhook") }()
		var rec *httptest.ResponseRecorder
		select {
		case rec = <-done:
			b.clientGate.Unlock()
		case <-time.After(time.Second):
			b.clientGate.Unlock()
			<-done
			t.Fatal("webhook read waited for the SDK client gate")
		}
		want := 200
		if denied {
			want = 403
		}
		if rec.Code != want {
			t.Fatalf("idle webhook read returned %d, want %d", rec.Code, want)
		}
	}
}

func TestSlowWebhookTestDoesNotHoldClientGate(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	b, operator, destination, _, _ := runtimeWebhookFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(204)
	})
	configureWebhookHTTP(t, operator, destination)
	h := b.runtimeRESTHandler(8080, readOnlyTestToken)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- webhookReviewRequest(h, "POST", "/api/webhook/test") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic test did not reach the receiver")
	}
	if !b.clientGate.TryLock() {
		t.Fatal("slow synthetic delivery retained the SDK client gate")
	}
	b.clientGate.Unlock()
	once.Do(func() { close(release) })
	if rec := <-done; rec.Code != 200 {
		t.Fatal("synthetic test failed after receiver release")
	}
}

func TestSafeWebhookReason(t *testing.T) {
	for _, reason := range []string{"http_600", "http_999", "http_000", "delivery_failed", ""} {
		if safeWebhookReason(reason) != reason {
			t.Fatal("valid reason code changed")
		}
	}
	for _, reason := range []string{"http_9999", "http_99", "http_٩٩٩", "Bearer fake-private-authorization", "https://example.com/private-path?private-query"} {
		if safeWebhookReason(reason) != "runtime webhook reason unavailable" {
			t.Fatal("unrecognized reason was not sanitized")
		}
	}
}
