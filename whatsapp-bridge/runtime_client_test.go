package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestRuntimePrivatePairingLoggerDropsSDKCredentialsAtDebug(t *testing.T) {
	var output bytes.Buffer
	client := whatsmeow.NewClient(newTestClient(&mockLIDStore{}).Store, privatePairingLogger{newJSONLogger("sdk", "DEBUG", &output)})
	client.Log.Sub("QRChannel").Debugf("Emitting QR code %s", "FAKE-SECRET-QR")
	client.Log.Sub("Recv").Sub("Frame").Debugf("%s", "FAKE-SECRET-PASSKEY")
	client.Log.Debugf("Errored frame hex: %s", "FAKE-SECRET-FRAME")
	client.Log.Sub("QRChannel").Infof("safe pairing transition")
	if strings.Contains(output.String(), "FAKE-SECRET") || !strings.Contains(output.String(), "safe pairing transition") {
		t.Fatalf("SDK credential filtering failed: %s", output.String())
	}
}

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

func TestRuntimeSDKPrePairHookAndTerminalEventControlCompletion(t *testing.T) {
	client := newTestClient(&mockLIDStore{})
	b := testBridge(t, client, newTestMessageStore(t), testLogger())
	reconnect := make(chan bool, 1)
	b.installClient(client, false, reconnect)
	p := newOperatorPairing(b.ctx, b, client, nil, b.isPaired, b.Connected, io.Discard, reconnect)
	b.operatorPairing = p
	if client.PrePairCallback(types.EmptyJID, "", "") {
		t.Fatal("SDK could save a device outside an active attempt")
	}
	p.state.State = "awaiting_qr"
	if !client.PrePairCallback(types.EmptyJID, "", "") || client.PrePairCallback(types.EmptyJID, "", "") {
		t.Fatal("SDK completion admission was absent or duplicated")
	}
	if p.snapshot().State != "completing" {
		t.Fatal("SDK pre-pair hook did not publish its save state")
	}
	done := p.completionDone
	b.handleClientEvent(client, &events.PairSuccess{}, reconnect)
	select {
	case <-done:
	default:
		t.Fatal("terminal SDK event did not release completion")
	}
	if !b.isPaired() || p.snapshot().State != "paired" {
		t.Fatal("terminal SDK event did not retain the linked device")
	}
	fresh := newTestClient(&mockLIDStore{})
	b.installClient(fresh, false, reconnect)
	if client.PrePairCallback(types.EmptyJID, "", "") {
		t.Fatal("retired client could save another device")
	}
}
