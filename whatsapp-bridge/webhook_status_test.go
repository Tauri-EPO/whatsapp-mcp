package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebhookRESTReadAndSyntheticDelivery(t *testing.T) {
	var receiverStatus atomic.Int32
	receiverStatus.Store(204)
	b, operator, destination, receipts, _ := runtimeWebhookFixture(t, &receiverStatus)
	request := func(method, path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
		return rec
	}
	const auth = "Bearer " + readOnlyTestToken
	for _, tc := range []struct{ method, path string }{{"GET", "/api/webhook"}, {"POST", "/api/webhook/test"}} {
		for _, token := range []string{"", "Bearer wrong-token"} {
			if rec := request(tc.method, tc.path, token); rec.Code != 401 || strings.Contains(rec.Body.String(), "configured") {
				t.Fatalf("unauthenticated webhook endpoint returned %d", rec.Code)
			}
		}
	}
	runtime := b.RuntimeWebhook
	b.RuntimeWebhook = nil
	for _, tc := range []struct{ method, path string }{{"GET", "/api/webhook"}, {"POST", "/api/webhook/test"}} {
		if rec := request(tc.method, tc.path, auth); rec.Code != 404 || !strings.Contains(rec.Body.String(), "Runtime webhook is disabled") {
			t.Fatalf("runtime off returned %d", rec.Code)
		}
	}
	b.RuntimeWebhook = runtime
	var absent webhookStatusView
	if rec := request("GET", "/api/webhook", auth); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &absent) != nil || absent.Configured || absent.Events == nil {
		t.Fatal("absent receiver was not represented explicitly")
	}
	if rec := request("POST", "/api/webhook/test", auth); rec.Code != 409 {
		t.Fatalf("test without receiver returned %d", rec.Code)
	}
	b.ReadOnly.enabled = true
	if rec := request("GET", "/api/webhook", auth); rec.Code != 200 {
		t.Fatal("read-only blocked the webhook read")
	}
	if rec := request("POST", "/api/webhook/test", auth); rec.Code != 403 {
		t.Fatal("read-only allowed a synthetic delivery")
	}
	b.ReadOnly.enabled = false
	for _, tc := range []struct{ allow, deny, method, path string }{
		{"", "get_webhook", "GET", "/api/webhook"},
		{"", "test_webhook", "POST", "/api/webhook/test"},
		{"bridge_status", "", "GET", "/api/webhook"},
		{"bridge_status", "", "POST", "/api/webhook/test"},
	} {
		b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
		if rec := request(tc.method, tc.path, auth); rec.Code != 403 {
			t.Fatalf("tool policy allowed %s %s", tc.method, tc.path)
		}
	}
	b.Tools = mustToolPolicy(t, "get_webhook,test_webhook", "")
	const privatePath = "/hooks/private-token-abcd"
	const privateQuery = "key=private-query"
	const authorization = "Bearer fake-receiver-credential"
	if code, _ := configureWebhookFields(t, operator, destination+privatePath+"?"+privateQuery, map[string]any{"authorization": authorization, "forward_self": true}); code != 200 {
		t.Fatalf("operator configuration returned %d", code)
	}
	rec := request("GET", "/api/webhook", auth)
	var view webhookStatusView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &view) != nil || !view.Configured || !view.Enabled || !view.AuthSet || !view.ForwardSelf || len(view.Events) != 1 {
		t.Fatal("configured webhook view is incomplete")
	}
	for _, leak := range []string{privatePath, privateQuery, authorization, fakeWebhookSecret, runtime.snapshot().Fingerprint, "secret_fingerprint"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatal("private receiver data leaked in the read response")
		}
	}
	for _, status := range []int32{204, 403} {
		receiverStatus.Store(status)
		rec := request("POST", "/api/webhook/test", auth)
		var result struct {
			Delivered bool   `json:"delivered"`
			Reason    string `json:"reason"`
			EventID   string `json:"event_id"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Delivered != (status == 204) || result.EventID == "" {
			t.Fatal("synthetic delivery contract failed")
		}
		if (status == 204 && result.Reason != "") || (status == 403 && result.Reason != "http_403") {
			t.Fatal("synthetic delivery returned an unsafe or incorrect reason")
		}
		receipt := receiveWebhook(t, receipts)
		var event runtimeWebhookEvent
		if json.Unmarshal(receipt.body, &event) != nil || event.Event != "webhook.test" || event.ID != result.EventID || event.Text != "Synthetic webhook test" {
			t.Fatal("receiver did not get the expected synthetic event")
		}
	}
	if state := runtime.snapshot(); !state.Enabled || state.Failures != 0 || state.LastSuccess != nil {
		t.Fatal("synthetic test changed subscribed-event health")
	}
	for _, method := range []string{"PUT", "DELETE", "POST"} {
		if rec := request(method, "/api/webhook", auth); rec.Code != 405 {
			t.Fatalf("agent configuration method %s returned %d", method, rec.Code)
		}
	}
	if rec := request("PUT", "/operator/v1/webhook", auth); rec.Code != 404 {
		t.Fatal("operator configuration is reachable on bridge REST")
	}
}
