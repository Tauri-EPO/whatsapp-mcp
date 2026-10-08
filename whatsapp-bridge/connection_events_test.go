package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func connectionTestReceiver(t *testing.T) (*Bridge, <-chan connectionPayload) {
	t.Helper()
	posts := make(chan connectionPayload, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Bridge-Token") != "test-token-0123456789abcdef" {
			t.Error("missing existing webhook credential")
		}
		var p connectionPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		posts <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.Webhook = &webhookSender{client: server.Client(), url: server.URL, enabled: true, token: "test-token-0123456789abcdef", failures: &b.metrics.webhookFailures}
	b.ForwardConnection = true
	return b, posts
}

func TestConnectionWebhookDebouncesBriefDisconnectAndPublishesLongGap(t *testing.T) {
	b, posts := connectionTestReceiver(t)
	ch := make(chan bool, 1)
	b.handleEvent(&events.Connected{}, ch)
	b.connectionEvents.Wait()
	if p := <-posts; p.State != "connected" || p.Type != "connection" {
		t.Fatalf("payload=%+v", p)
	}
	entered := make(chan struct{}, 2)
	fire := make(chan struct{})
	b.connectionEventWait = func(ctx context.Context, d time.Duration) bool {
		if d != 5*time.Second {
			t.Errorf("default debounce=%v", d)
		}
		entered <- struct{}{}
		select {
		case <-fire:
			return true
		case <-ctx.Done():
			return false
		}
	}
	b.handleEvent(&events.Disconnected{}, ch)
	<-entered
	b.handleEvent(&events.Connected{}, ch)
	b.connectionEvents.Wait()
	if len(posts) != 0 {
		t.Fatal("brief gap emitted a POST")
	}
	b.handleEvent(&events.Disconnected{}, ch)
	<-entered
	close(fire)
	b.connectionEvents.Wait()
	if len(posts) != 1 {
		t.Fatalf("long gap POST count=%d", len(posts))
	}
	if p := <-posts; p.State != "disconnected" {
		t.Fatalf("payload=%+v", p)
	}
}

func TestConnectionLogoutPostsBeforeExitAndCarriesClassifiedProblem(t *testing.T) {
	b, posts := connectionTestReceiver(t)
	b.Exit = func(_ string, code int) {
		if code != 3 || len(posts) != 1 {
			t.Errorf("exit before webhook: code=%d posts=%d", code, len(posts))
		}
	}
	b.handleEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	p := <-posts
	if p.State != "logged_out" || p.Problem == nil || p.Problem.Kind != "unlinked" || p.Problem.Code != 401 {
		t.Fatalf("payload=%+v", p)
	}
}

func TestConnectionWebhooksAreOptInAndContainOnlySafePasskeyState(t *testing.T) {
	b, posts := connectionTestReceiver(t)
	b.ForwardConnection = false
	b.notifyConnection("connected", "authenticated", true, false)
	b.connectionEvents.Wait()
	if len(posts) != 0 {
		t.Fatal("disabled connection webhook sent")
	}
	b.ForwardConnection = true
	b.setPairingState("passkey_required")
	b.connectionEvents.Wait()
	p := <-posts
	if p.State != "pairing_required" || p.PairingState != "passkey_required" || p.Problem != nil {
		t.Fatalf("payload=%+v", p)
	}
	var body map[string]any
	data, _ := json.Marshal(p)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	for key := range body {
		switch key {
		case "type", "state", "reason", "at", "pairing_state":
		default:
			t.Fatalf("unexpected potentially sensitive field %s", key)
		}
	}
}
