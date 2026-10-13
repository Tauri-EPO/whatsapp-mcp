package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func TestExpiredUnchangedTemporaryBanDoesNotEndQRAttempts(t *testing.T) {
	b := newSettingsBridge(t)
	expiry := time.Now().Add(-time.Second)
	problem := &ConnectionProblem{Kind: "temporarily_banned", Code: 402, Since: expiry.Add(-time.Second), ExpiresAt: &expiry}
	b.connectionProblem = problem
	initial := newFakeOperatorClient()
	initial.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-EXPIRED-BAN-QR", Timeout: time.Minute}
	initial.items <- whatsmeow.QRChannelItem{Event: "timeout"}
	var replacements atomic.Int64
	p := newOperatorPairing(b.ctx, b, initial, func() (operatorPairingClient, error) {
		client := newFakeOperatorClient()
		client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-EXPIRED-BAN-QR", Timeout: time.Minute}
		if replacements.Add(1) == 2 {
			client.items <- whatsmeow.QRChannelItem{Event: "success"}
		} else {
			client.items <- whatsmeow.QRChannelItem{Event: "timeout"}
		}
		return client, nil
	}, func() bool { return false }, func() bool { return false }, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	p.opt.retryDelay = time.Millisecond
	p.opt.attemptTimeout = time.Second
	p.start()
	defer func() { b.cancel(); <-p.done }()
	f := &operatorPairingFixture{b: b, p: p}
	f.wait(t, func(s operatorPairingState) bool { return s.State == "paired" && s.Attempt == 3 })
	if replacements.Load() != 2 || initial.connects.Load() != 1 {
		t.Fatal("expired unchanged restriction truncated QR sequence")
	}
	current, _ := b.connectionSnapshot()
	if current != problem {
		t.Fatal("QR retry discarded the persisted restriction instead of respecting its expiry")
	}
}

func TestExpiredBanUnlinkRejectsMidQRRestartAndExplainsExpiry(t *testing.T) {
	b := newSettingsBridge(t)
	expiry := time.Now().Add(-time.Second)
	b.connectionProblem = &ConnectionProblem{Kind: "temporarily_banned", Code: 402, Since: expiry.Add(-time.Second), ExpiresAt: &expiry, UnlinkedAt: &expiry, SessionRemoved: true}
	client := newFakeOperatorClient()
	p := newOperatorPairing(b.ctx, b, client, func() (operatorPairingClient, error) { return newFakeOperatorClient(), nil }, func() bool { return false }, func() bool { return false }, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	p.state.Attempts = 1
	p.opt.attemptTimeout = 5 * time.Second
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
	defer server.Close()
	f := &operatorPairingFixture{b: b, p: p, client: client, server: server}
	client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-ACTIVE-QR-AFTER-BAN", Timeout: time.Minute}
	p.start()
	defer func() { b.cancel(); <-p.done }()
	f.stateHTTP(t, "awaiting_qr")
	status, body := f.request(t, "POST", "pairing/restart", fakeOperatorToken, "")
	if status != 409 || !strings.Contains(body, "pairing_active") {
		t.Fatalf("expired-ban mid-QR restart=%d %s", status, body)
	}
	client.items <- whatsmeow.QRChannelItem{Event: "timeout"}
	f.stateHTTP(t, "expired")
	status, body = f.request(t, "GET", "health", fakeOperatorToken, "")
	var health map[string]any
	if status != 200 || json.Unmarshal([]byte(body), &health) != nil {
		t.Fatalf("health=%d %s", status, body)
	}
	action, _ := health["pairing_action"].(string)
	if !strings.Contains(action, "/operator/v1/pairing/restart") || health["status"] != "pairing_required" {
		t.Fatalf("expired QR after old ban has no restart guidance: %s", body)
	}
}
