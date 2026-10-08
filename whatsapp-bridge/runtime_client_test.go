package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestRuntimeClientHandoffRefreshesRealRESTIdentityAndIgnoresOldEvents(t *testing.T) {
	first := newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511999999999", types.DefaultUserServer))
	second := newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511888888888", types.DefaultUserServer))
	b := testBridge(t, first, newTestMessageStore(t), testLogger())
	b.bindRuntimeClient()
	reconnect := make(chan bool, 1)
	b.installClient(first, true, reconnect)
	handler := b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef")
	get := func(want string) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/me", nil)
		req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), want) {
			t.Fatalf("runtime identity=%d %s", out.Code, out.Body.String())
		}
	}
	get("5511999999999")
	b.installClient(second, true, reconnect)
	get("5511888888888")
	b.handleClientEvent(first, &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureUnknownLogout}, reconnect)
	first.Log.Warnf("Got %d/%s connect failure, assuming automatic reconnect will handle it", 503, "service unavailable")
	if problem, _ := b.connectionSnapshot(); problem != nil || !b.isPaired() || len(reconnect) != 0 {
		t.Fatal("retired client changed the active client's state")
	}
}

func TestRuntimeClientConcurrentHealthAndHandoff(t *testing.T) {
	first := newTestClient(&mockLIDStore{})
	b := testBridge(t, first, newTestMessageStore(t), testLogger())
	b.bindRuntimeClient()
	b.installClient(first, false, make(chan bool, 1))
	handler := b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef")
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for range 100 {
			b.installClient(newTestClient(&mockLIDStore{}), false, make(chan bool, 1))
		}
	}()
	go func() {
		defer group.Done()
		for range 100 {
			req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/health", nil)
			req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Errorf("health during handoff=%d", out.Code)
			}
		}
	}()
	group.Wait()
}
