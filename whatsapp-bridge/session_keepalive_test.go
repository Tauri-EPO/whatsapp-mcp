package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
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
		{"168", 168 * time.Hour, false},
		{"169", 0, true},     // fewer than four blips a month
		{"9999999", 0, true}, // would overflow time.Duration into a negative interval
		{"5124096", 0, true}, // would wrap to about 25 minutes
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
	if s := sessionKeepaliveSummary(0); !strings.HasPrefix(s, "off") {
		t.Errorf("a disabled keepalive must say so in the startup log, got %q", s)
	}
	if s := sessionKeepaliveSummary(12 * time.Hour); s != "every 12 h" {
		t.Errorf("summary = %q", s)
	}
}

// The roster sync shares the parser: what it accepts must not move.
func TestResolveGroupRosterSync_KeepsItsSpellings(t *testing.T) {
	for value, want := range map[string]time.Duration{"": groupRosterSyncInterval, "0": 0, "6": 6 * time.Hour, "10000": 10000 * time.Hour} {
		if got, err := resolveGroupRosterSync(value); err != nil || got != want {
			t.Errorf("resolveGroupRosterSync(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"-1", "x", "1.5"} {
		if _, err := resolveGroupRosterSync(value); err == nil {
			t.Errorf("resolveGroupRosterSync(%q) was accepted", value)
		}
	}
}

// presenceRecorder stands in for the WhatsApp client: it records every
// presence the keepalive sends and can be scripted to fail.
type presenceRecorder struct {
	mu     sync.Mutex
	sent   []types.Presence
	fail   func(state types.Presence, attempt int) error // nil = succeed
	tries  map[types.Presence]int
	events chan types.Presence
}

func newPresenceRecorder() *presenceRecorder {
	return &presenceRecorder{events: make(chan types.Presence, 256), tries: map[types.Presence]int{}}
}

func (p *presenceRecorder) send(_ context.Context, state types.Presence) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tries[state]++
	if p.fail != nil {
		if err := p.fail(state, p.tries[state]); err != nil {
			return err
		}
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

func (p *presenceRecorder) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case state := <-p.events:
		t.Fatalf("sent %q when nothing should be sent", state)
	case <-time.After(d):
	}
}

func (p *presenceRecorder) history() []types.Presence {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]types.Presence(nil), p.sent...)
}

// keepaliveBridge is a test Bridge with millisecond waits and the recorder as
// its client. The loop is started by the caller (startSessionKeepalive); the
// Bridge's own Shutdown, registered by testBridge, stops it and waits for it.
func keepaliveBridge(t *testing.T, interval time.Duration, ready *atomic.Bool, rec *presenceRecorder) *Bridge {
	t.Helper()
	b := testBridge(t, nil, nil, installRecordingLogger(t))
	b.SessionKeepalive = interval
	b.SessionKeepaliveSettle = 2 * time.Millisecond
	b.SessionKeepalivePoll = time.Millisecond
	b.SessionKeepaliveRetry = 2 * time.Millisecond
	b.SessionPresenceHold = time.Millisecond
	b.sessionPresence = rec.send
	b.sessionReady = ready.Load
	return b
}

func readyFlag(v bool) *atomic.Bool {
	var flag atomic.Bool
	flag.Store(v)
	return &flag
}

func TestSessionKeepalive_MarksTheDeviceAvailableThenUnavailableAndRepeats(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 20*time.Millisecond, readyFlag(true), rec)
	b.startSessionKeepalive()

	for round := 1; round <= 2; round++ {
		if got := rec.next(t); got != types.PresenceAvailable {
			t.Fatalf("round %d: first presence = %q, want available", round, got)
		}
		if got := rec.next(t); got != types.PresenceUnavailable {
			t.Fatalf("round %d: second presence = %q, want unavailable: the account must not stay online", round, got)
		}
	}
	if n := b.metrics.sessionKeepalives.Load(); n < 2 {
		t.Errorf("sessionKeepalives = %d, want one per blip", n)
	}
}

// While the bridge shows its QR code the socket is up and there is no session:
// nothing is sent until it is logged in, and then within the settle time
// rather than a whole interval.
func TestSessionKeepalive_WaitsForALoggedInSessionInsteadOfAWholeInterval(t *testing.T) {
	rec := newPresenceRecorder()
	ready := readyFlag(false)
	b := keepaliveBridge(t, time.Hour, ready, rec)
	b.startSessionKeepalive()

	rec.quiet(t, 60*time.Millisecond)
	ready.Store(true)
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after the session became ready: %q, want available", got)
	}
}

func TestSessionKeepalive_AFailedAvailableIsRetriedSoonAndNotCounted(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, attempt int) error {
		if state == types.PresenceAvailable && attempt <= 2 {
			return errors.New("socket closed")
		}
		return nil
	}
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.startSessionKeepalive()

	// whatsmeow switches to active delivery receipts before it writes the
	// frame, so each failed "available" is followed by an "unavailable".
	for attempt := 1; attempt <= 2; attempt++ {
		if got := rec.next(t); got != types.PresenceUnavailable {
			t.Fatalf("after failed attempt %d: %q, want unavailable to undo it", attempt, got)
		}
	}
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after two failures: %q, want available", got)
	}
	rec.next(t) // unavailable
	if n := b.metrics.sessionKeepalives.Load(); n != 1 {
		t.Errorf("sessionKeepalives = %d, want 1: the two failed attempts are not keepalives", n)
	}
}

// A socket torn down while the frame is in flight surfaces as "context
// canceled" from whatsmeow although the bridge is not shutting down. The loop
// must outlive it, or the device is logged out a month later after all.
func TestSessionKeepalive_SurvivesASendThatFailsWithContextCanceled(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, attempt int) error {
		if state == types.PresenceAvailable && attempt == 1 {
			return fmt.Errorf("failed to write frame: %w", context.Canceled)
		}
		return nil
	}
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.startSessionKeepalive()

	if got := rec.next(t); got != types.PresenceUnavailable {
		t.Fatalf("after the cancelled write: %q, want unavailable to undo it", got)
	}
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after a cancelled write: %q, want available on the retry", got)
	}
}

// Logged in, but the push name has not arrived yet: not an error worth a WARN
// every few minutes, just not ready.
func TestSessionKeepalive_NoPushNameYetIsWaitedOutQuietly(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, attempt int) error {
		if state == types.PresenceAvailable && attempt <= 3 {
			return whatsmeow.ErrNoPushName
		}
		return nil
	}
	log := installRecordingLogger(t)
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.Log = log
	b.SessionKeepaliveRetry = time.Hour // the patience before it is reported
	b.startSessionKeepalive()

	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("once the push name was there: %q, want available", got)
	}
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, "WARN") && strings.Contains(line, "Session keepalive") {
			t.Errorf("a missing push name was logged as a warning: %s", line)
		}
	}
}

// "available" went out and "unavailable" failed: only "unavailable" is tried
// again, promptly, and the blip is counted once.
func TestSessionKeepalive_AFailedUnavailableIsRetriedAloneAndCountedOnce(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, attempt int) error {
		if state == types.PresenceUnavailable && attempt <= 2 {
			return errors.New("write timed out")
		}
		return nil
	}
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.startSessionKeepalive()

	rec.next(t) // available
	if got := rec.next(t); got != types.PresenceUnavailable {
		t.Fatalf("after two failed attempts: %q, want unavailable", got)
	}
	rec.quiet(t, 40*time.Millisecond)
	if got := rec.history(); len(got) != 2 || got[0] != types.PresenceAvailable || got[1] != types.PresenceUnavailable {
		t.Fatalf("sent %v, want exactly [available unavailable]: no second available", got)
	}
	if n := b.metrics.sessionKeepalives.Load(); n != 1 {
		t.Errorf("sessionKeepalives = %d, want 1", n)
	}
}

// A push name that never comes must not disable the keepalive in silence: one
// WARN once the patience is over, not one per poll.
func TestSessionKeepalive_APushNameThatStaysMissingIsReportedOnce(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, attempt int) error {
		if state == types.PresenceAvailable && attempt <= 40 {
			return whatsmeow.ErrNoPushName
		}
		return nil
	}
	log := installRecordingLogger(t)
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.Log = log
	b.startSessionKeepalive()

	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("once the push name was there: %q, want available", got)
	}
	if n := strings.Count(log.String(), "push name"); n != 1 {
		t.Errorf("%d log lines about the push name, want exactly one WARN:\n%s", n, log.String())
	}
	if got := rec.history(); got[0] != types.PresenceAvailable {
		t.Errorf("sent %v: a refused available changes nothing in whatsmeow, so there is nothing to undo", got)
	}
}

// The bridge is told to stop while "available" is on its way out: the frame
// may still reach WhatsApp, so "unavailable" is sent before the loop returns.
func TestSessionKeepalive_ShutdownWhileAvailableIsBeingSentStillSendsUnavailable(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.sessionPresence = func(ctx context.Context, state types.Presence) error {
		if state == types.PresenceAvailable {
			b.cancel()
			return fmt.Errorf("failed to write frame: %w", context.Canceled)
		}
		return rec.send(ctx, state)
	}
	b.startSessionKeepalive()

	if got := rec.next(t); got != types.PresenceUnavailable {
		t.Fatalf("%q, want unavailable", got)
	}
	b.keepaliveLoop.Wait()
	if got := rec.history(); len(got) != 1 {
		t.Errorf("sent %v, want only the one unavailable: the loop must stop, not retry", got)
	}
}

func TestSessionKeepalive_ZeroTurnsItOff(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 0, readyFlag(true), rec)
	b.startSessionKeepalive()
	rec.quiet(t, 60*time.Millisecond)
}

// The bridge shuts down while the device is marked available: Shutdown waits
// for the loop, and the loop takes the account offline before it returns, under
// a context that is not the cancelled one.
func TestSessionKeepalive_ShutdownDuringTheHoldStillSendsUnavailable(t *testing.T) {
	rec := newPresenceRecorder()
	var offCtxErr atomic.Value
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.SessionPresenceHold = time.Hour // the shutdown arrives during the hold
	b.sessionPresence = func(ctx context.Context, state types.Presence) error {
		if state == types.PresenceUnavailable {
			if err := ctx.Err(); err != nil {
				offCtxErr.Store(err)
			}
		}
		return rec.send(ctx, state)
	}
	b.startSessionKeepalive()

	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("first presence = %q, want available", got)
	}
	b.Shutdown(5 * time.Second)

	if got := rec.history(); len(got) != 2 || got[1] != types.PresenceUnavailable {
		t.Fatalf("after Shutdown the bridge had sent %v, want [available unavailable]", got)
	}
	if err := offCtxErr.Load(); err != nil {
		t.Errorf("the unavailable send ran under a dead context: %v", err)
	}
}
