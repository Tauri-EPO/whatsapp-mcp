package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestExpiredBanThenUnlinkFencesRestart(t *testing.T) {
	b, container := revokedSessionFixture(t)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	b.problemNow = func() time.Time { return now }
	b.recordConnectionProblem(402, 101, time.Second)
	now = now.Add(2 * time.Second)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	if _, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)(); err != nil {
		t.Fatal(err)
	}
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	restarted, pending, err := initialSessionDevice(b.ctx, container, saved)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.ID == nil {
		if len(devices) != 0 || saved == nil || !saved.isUnlinked() || !saved.SessionRemoved || saved.Kind != "temporarily_banned" {
			t.Fatalf("revocation was not durably removed alongside ban: rows=%d saved=%+v", len(devices), saved)
		}
		return
	}
	var dials atomic.Int64
	client := newRuntimeClient(restarted, testLogger())
	client.SetWebsocketHTTPClient(&http.Client{Transport: mcpTestTransport(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, errors.New("fake revoked-session transport refusal")
	})})
	b.installClient(client, true, reconnect)
	b.connectionMu.Lock()
	b.connectionProblem, b.pairingState = saved, ""
	b.connectionMu.Unlock()
	ctx, cancel := context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	_ = connectOrPair(ctx, client, true, pairingOptions{attempts: 1, log: testLogger(), beforeDial: func() error { return b.waitConnectionAllowedContext(ctx) }})
	if dials.Load() != 0 {
		t.Fatalf("revoked session reached Connect after expired ban + unlink: pending=%t saved=%+v dials=%d", pending, saved, dials.Load())
	}
}
