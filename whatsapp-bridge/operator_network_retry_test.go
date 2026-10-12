package main

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
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
