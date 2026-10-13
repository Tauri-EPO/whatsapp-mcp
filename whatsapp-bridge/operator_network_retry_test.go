package main

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestTransportFailuresRetainQRBudgetAndRetryWithBackoff(t *testing.T) {
	b := newSettingsBridge(t)
	initial := newFakeOperatorClient()
	initial.connectErr = errors.New("fake offline transport")
	var replacements atomic.Int64
	ready := make(chan time.Time, 1)
	fresh := &unlinkOnConnectClient{fakeOperatorClient: newFakeOperatorClient(), emit: func() { ready <- time.Now() }}
	fresh.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-NETWORK-RECOVERY-QR", Timeout: time.Minute}
	fresh.items <- whatsmeow.QRChannelItem{Event: "success"}
	p := newOperatorPairing(b.ctx, b, initial, func() (operatorPairingClient, error) {
		if replacements.Add(1) < 4 {
			f := newFakeOperatorClient()
			f.connectErr = errors.New("fake offline transport")
			return f, nil
		}
		return fresh, nil
	}, func() bool { return false }, func() bool { return false }, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	p.state.Attempts = 1
	p.opt.retryDelay = 15 * time.Millisecond
	p.opt.attemptTimeout = time.Second
	start := time.Now()
	p.start()
	defer func() { b.cancel(); <-p.done }()
	select {
	case at := <-ready:
		if at.Sub(start) < 150*time.Millisecond {
			t.Fatal("transport retries spun without backoff")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transport failures exhausted the QR budget and parked")
	}
	if replacements.Load() != 4 || initial.connects.Load() != 1 || fresh.connects.Load() != 1 {
		t.Fatal("network recovery did not keep the QR attempt")
	}
}

func TestShutdownDuringCleanupDoesNotLogPairingFailureOrExpiry(t *testing.T) {
	b := newSettingsBridge(t)
	log := &recordingLogger{}
	b.Log = log
	b.pairingState = "pairing_required"
	started := make(chan struct{})
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), func() (operatorPairingClient, error) { close(started); <-b.ctx.Done(); return nil, b.ctx.Err() }, func() bool { return false }, func() bool { return false }, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	p.start()
	p.kick <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cleanup was not started")
	}
	b.cancel()
	<-p.done
	if strings.Contains(log.String(), "could not be initialized") || strings.Contains(log.String(), "QR sequence expired") {
		t.Fatalf("shutdown logged false pairing failure: %s", log.String())
	}
}

func TestPairedStartupTemporaryBanTransportFailureReachesGatedRetries(t *testing.T) {
	for _, cooldown := range []time.Duration{-time.Second, 100 * time.Millisecond} {
		t.Run(cooldown.String(), func(t *testing.T) {
			client := newFakeOperatorClient()
			client.connectErr = errors.New("fake offline startup transport")
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511999999999", types.DefaultUserServer)), newTestMessageStore(t), testLogger())
			expiry := time.Now().Add(cooldown)
			b.connectionProblem = &ConnectionProblem{Kind: "temporarily_banned", Code: 402, Since: time.Now(), ExpiresAt: &expiry}
			var connected atomic.Bool
			var retries, replacements atomic.Int64
			firstDial := make(chan time.Time, 1)
			b.Connected = connected.Load
			b.Disconnect = client.Disconnect
			reconnect := make(chan bool, 1)
			b.Connect = func() error {
				count := retries.Add(1)
				if count == 1 {
					firstDial <- time.Now()
				}
				if count < 3 {
					return errors.New("fake offline retry")
				}
				connected.Store(true)
				b.handleEvent(&events.Connected{}, reconnect)
				return nil
			}
			b.ReconnectInitialBackoff = time.Millisecond
			b.ReconnectMaxBackoff = time.Millisecond
			p := newOperatorPairing(b.ctx, b, client, func() (operatorPairingClient, error) {
				replacements.Add(1)
				return nil, errors.New("paired keys must survive")
			}, b.isPaired, b.Connected, io.Discard, reconnect)
			b.operatorPairing = p
			done := make(chan struct{})
			go func() { defer close(done); b.reconnectLoop(reconnect) }()
			p.start()
			defer func() { b.cancel(); <-p.done; <-done }()
			select {
			case at := <-firstDial:
				if at.Before(expiry) {
					t.Fatal("active cooldown bypassed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("persisted temporary ban parked transport recovery")
			}
			deadline := time.Now().Add(time.Second)
			for !connected.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !connected.Load() || retries.Load() != 3 || replacements.Load() != 0 || client.connects.Load() != 1 {
				t.Fatal("gated backoff did not recover existing paired session")
			}
			req := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/ready", nil)
			req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
			out := httptest.NewRecorder()
			b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef").ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("runtime readiness after recovery=%d %s", out.Code, out.Body.String())
			}
		})
	}
}
