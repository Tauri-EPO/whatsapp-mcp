package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestResolveSessionKeepalive(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"", sessionKeepaliveInterval, false},
		{"  ", sessionKeepaliveInterval, false},
		{"0", 0, false},
		{"24", 24 * time.Hour, false},
		{" 6 ", 6 * time.Hour, false},
		{"-1", 0, true},
		{"1.5", 0, true},
		{"daily", 0, true},
	}
	for _, tc := range cases {
		got, err := resolveSessionKeepalive(tc.value)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("resolveSessionKeepalive(%q) = %v, %v; want %v, error=%v", tc.value, got, err, tc.want, tc.wantErr)
		}
	}
	if s := sessionKeepaliveSummary(0); s == "" || s[:3] != "off" {
		t.Errorf("a disabled keepalive must say so in the startup log, got %q", s)
	}
}

// presenceRecorder stands in for the WhatsApp client: it records every
// presence the keepalive sends and can be told to fail.
type presenceRecorder struct {
	mu     sync.Mutex
	sent   []types.Presence
	failN  int // the first failN sends fail
	events chan types.Presence
}

func newPresenceRecorder() *presenceRecorder {
	return &presenceRecorder{events: make(chan types.Presence, 64)}
}

func (p *presenceRecorder) send(_ context.Context, state types.Presence) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failN > 0 {
		p.failN--
		return errors.New("socket closed")
	}
	p.sent = append(p.sent, state)
	select {
	case p.events <- state:
	default: // nobody is reading any more; the loop must not block on the test
	}
	return nil
}

func (p *presenceRecorder) next(t *testing.T) types.Presence {
	t.Helper()
	select {
	case state := <-p.events:
		return state
	case <-time.After(5 * time.Second):
		t.Fatal("the keepalive sent no presence in 5 s")
		return ""
	}
}

// startKeepalive runs the loop on a test Bridge with millisecond waits and
// returns once the test is over and the goroutine has stopped.
func startKeepalive(t *testing.T, interval time.Duration, connected *atomic.Bool, rec *presenceRecorder) *Bridge {
	t.Helper()
	b := testBridge(t, nil, nil, installRecordingLogger(t))
	b.SessionKeepalive = interval
	b.sessionPresence = rec.send
	b.sessionKeepaliveTiming = sessionKeepaliveTiming{start: 5 * time.Millisecond, retry: 5 * time.Millisecond, hold: time.Millisecond}
	b.Connected = connected.Load
	done := make(chan struct{})
	go func() {
		b.runSessionKeepalive()
		close(done)
	}()
	t.Cleanup(func() {
		b.cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the keepalive loop outlived its bridge")
		}
	})
	return b
}

func TestSessionKeepalive_MarksTheDeviceAvailableThenUnavailableAndRepeats(t *testing.T) {
	var connected atomic.Bool
	connected.Store(true)
	rec := newPresenceRecorder()
	b := startKeepalive(t, 20*time.Millisecond, &connected, rec)

	for round := 1; round <= 2; round++ {
		if got := rec.next(t); got != types.PresenceAvailable {
			t.Fatalf("round %d: first presence = %q, want available", round, got)
		}
		if got := rec.next(t); got != types.PresenceUnavailable {
			t.Fatalf("round %d: second presence = %q, want unavailable: the account must not stay online", round, got)
		}
	}
	if n := b.metrics.sessionKeepalives.Load(); n < 1 {
		t.Errorf("sessionKeepalives = %d, want at least 1", n)
	}
}

func TestSessionKeepalive_WaitsForTheConnectionInsteadOfAWholeInterval(t *testing.T) {
	var connected atomic.Bool
	rec := newPresenceRecorder()
	// An hour-long interval: only the retry delay can explain a send in this test.
	startKeepalive(t, time.Hour, &connected, rec)

	select {
	case state := <-rec.events:
		t.Fatalf("sent %q while disconnected", state)
	case <-time.After(60 * time.Millisecond):
	}
	connected.Store(true)
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after the socket came back: %q, want available", got)
	}
}

func TestSessionKeepalive_AFailedSendIsRetriedSoonAndNotCounted(t *testing.T) {
	var connected atomic.Bool
	connected.Store(true)
	rec := newPresenceRecorder()
	rec.failN = 2
	b := startKeepalive(t, time.Hour, &connected, rec)

	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after two failures: %q, want available", got)
	}
	rec.next(t) // unavailable
	deadline := time.Now().Add(2 * time.Second)
	for b.metrics.sessionKeepalives.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := b.metrics.sessionKeepalives.Load(); n != 1 {
		t.Errorf("sessionKeepalives = %d, want 1: the two failed attempts are not keepalives", n)
	}
}

func TestSessionKeepalive_ZeroTurnsItOff(t *testing.T) {
	var connected atomic.Bool
	connected.Store(true)
	rec := newPresenceRecorder()
	startKeepalive(t, 0, &connected, rec)

	select {
	case state := <-rec.events:
		t.Fatalf("a disabled keepalive sent %q", state)
	case <-time.After(60 * time.Millisecond):
	}
}

// A bridge that shuts down during the hold must still take the account
// offline: "available" with no "unavailable" after it leaves it showing online.
func TestSignalSessionInUse_SendsUnavailableEvenWhenCancelledDuringTheHold(t *testing.T) {
	rec := newPresenceRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	var offCtxErr error
	send := func(c context.Context, state types.Presence) error {
		if state == types.PresenceAvailable {
			cancel()
		} else {
			offCtxErr = c.Err()
		}
		return rec.send(c, state)
	}
	if err := signalSessionInUse(ctx, send, time.Hour); err != nil {
		t.Fatalf("signalSessionInUse: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sent) != 2 || rec.sent[0] != types.PresenceAvailable || rec.sent[1] != types.PresenceUnavailable {
		t.Fatalf("sent %v, want [available unavailable]", rec.sent)
	}
	if offCtxErr != nil {
		t.Errorf("the unavailable send ran under a cancelled context: %v", offCtxErr)
	}
}
