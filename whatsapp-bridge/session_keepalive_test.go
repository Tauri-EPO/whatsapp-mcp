package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
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
	for _, value := range []string{"-1", "x", "1.5", "9999999", "5124096"} {
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
	t.Setenv(storeDirEnv, t.TempDir())
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
	if readSessionBlip(b.StoreRoot).IsZero() {
		t.Fatal("shutdown during the hold did not remember the successful blip")
	}
}

func TestSessionKeepalive_ClockCorrectionDiscardsFuturePersistedState(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 12*time.Hour, readyFlag(true), rec)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b.sessionNow = func() time.Time { return now }
	b.SessionKeepaliveSettle = 0
	if err := writeSessionBlip(b.StoreRoot, now.Add(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	b.startSessionKeepalive()
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after correcting the clock: %q, want available", got)
	}
	rec.next(t)
	b.Shutdown(5 * time.Second)
	if got := readSessionBlip(b.StoreRoot); !got.Equal(now) {
		t.Fatalf("state after clock correction = %v, want %v", got, now)
	}
}

func TestSessionKeepalive_FailedAvailableDoesNotPersistABlip(t *testing.T) {
	rec := newPresenceRecorder()
	rec.fail = func(state types.Presence, _ int) error {
		if state == types.PresenceAvailable {
			return errors.New("socket closed")
		}
		return nil
	}
	b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
	b.startSessionKeepalive()
	if got := rec.next(t); got != types.PresenceUnavailable {
		t.Fatalf("after failed available: %q, want unavailable", got)
	}
	b.Shutdown(5 * time.Second)
	if _, err := b.StoreRoot.Lstat(sessionKeepaliveFile); !os.IsNotExist(err) {
		t.Fatalf("failed available created state: %v", err)
	}
}

func TestSessionKeepalive_BackwardClockStepWhileRunningBlipsAgain(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 12*time.Hour, readyFlag(true), rec)
	stamp := time.Date(2027, 10, 7, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(stamp.UnixNano())
	b.sessionNow = func() time.Time { return time.Unix(0, now.Load()) }
	b.SessionKeepaliveSettle = 0
	b.startSessionKeepalive()
	rec.next(t)
	rec.next(t)
	rec.quiet(t, 20*time.Millisecond)
	now.Store(stamp.AddDate(-1, 0, 0).UnixNano())
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("after backward clock step: %q, want available", got)
	}
	rec.next(t)
	b.Shutdown(5 * time.Second)
	if got := readSessionBlip(b.StoreRoot); !got.Equal(stamp.AddDate(-1, 0, 0)) {
		t.Fatalf("saved blip after clock step = %v", got)
	}
}

func TestSessionKeepalive_NewPairingDiscardsThePreviousTimestamp(t *testing.T) {
	rec := newPresenceRecorder()
	ready := readyFlag(false)
	b := keepaliveBridge(t, 12*time.Hour, ready, rec)
	b.Client = &whatsmeow.Client{Store: &store.Device{}}
	if err := writeSessionBlip(b.StoreRoot, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.startSessionKeepalive()
	if _, err := b.StoreRoot.Lstat(sessionKeepaliveFile); !os.IsNotExist(err) {
		t.Fatalf("old pairing timestamp still present: %v", err)
	}
	rec.quiet(t, 20*time.Millisecond)
	ready.Store(true)
	if got := rec.next(t); got != types.PresenceAvailable {
		t.Fatalf("newly paired session: %q, want available", got)
	}
	rec.next(t)
	b.Shutdown(5 * time.Second)
	if !strings.Contains(b.Log.(*recordingLogger).String(), "new pairing") {
		t.Fatal("new pairing was not reported")
	}
}

func TestSessionKeepalive_FailedStateWriteKeepsTheInMemoryInterval(t *testing.T) {
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 12*time.Hour, readyFlag(true), rec)
	part := sessionKeepaliveFile + ".part"
	if err := b.StoreRoot.Mkdir(part, storeDirMode); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRoot.WriteFile(part+"/occupied", []byte("x"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(stamp.UnixNano())
	b.sessionNow = func() time.Time { return time.Unix(0, now.Load()) }
	b.SessionKeepaliveSettle = 0
	b.startSessionKeepalive()
	for round := 0; round < 6; round++ {
		now.Store(stamp.Add(time.Duration(round) * b.SessionKeepalive).UnixNano())
		if got := rec.next(t); got != types.PresenceAvailable {
			t.Fatalf("round %d: %q, want available", round, got)
		}
		if got := rec.next(t); got != types.PresenceUnavailable {
			t.Fatalf("round %d: %q, want unavailable", round, got)
		}
		rec.quiet(t, 20*time.Millisecond)
	}
	b.Shutdown(5 * time.Second)
	if got := strings.Count(b.Log.(*recordingLogger).String(), "[WARN] Session keepalive: could not remember"); got != 6 {
		t.Fatalf("write warnings = %d, want one per blip (6)", got)
	}
	if _, err := b.StoreRoot.Lstat(sessionKeepaliveFile); !os.IsNotExist(err) {
		t.Fatalf("failed write left a timestamp: %v", err)
	}
}

func TestSessionKeepalive_RestartWaitsForTheRemainingWallClockInterval(t *testing.T) {
	stamp := time.Date(2026, 10, 7, 12, 0, 0, 123, time.UTC)
	var now atomic.Int64
	now.Store(stamp.UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	rec := newPresenceRecorder()
	b := keepaliveBridge(t, 12*time.Hour, readyFlag(true), rec)
	b.SessionKeepaliveSettle = 0
	b.sessionNow = clock
	b.startSessionKeepalive()
	rec.next(t)                 // available
	rec.next(t)                 // unavailable
	b.Shutdown(5 * time.Second) // waits for the state write too
	if got := readSessionBlip(b.StoreRoot); !got.Equal(stamp) {
		t.Fatalf("saved blip = %v, want %v", got, stamp)
	}
	info, err := b.StoreRoot.Stat(sessionKeepaliveFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != storeFileMode {
		t.Fatalf("state mode = %04o, want 0600", info.Mode().Perm())
	}

	restartedRec := newPresenceRecorder()
	restarted := keepaliveBridge(t, 12*time.Hour, readyFlag(true), restartedRec)
	restarted.StoreRoot = b.StoreRoot
	restarted.SessionKeepaliveSettle = 0
	restarted.sessionNow = clock
	now.Store(stamp.Add(time.Hour).UnixNano())
	restarted.startSessionKeepalive()
	restartedRec.quiet(t, 20*time.Millisecond)
	if log := restarted.Log.(*recordingLogger).String(); !strings.Contains(log, "last remembered blip "+stamp.Format(time.RFC3339Nano)) || !strings.Contains(log, "next due "+stamp.Add(12*time.Hour).Format(time.RFC3339Nano)) {
		t.Fatalf("restart did not report the remembered time and next due time: %s", log)
	}
	now.Store(stamp.Add(12*time.Hour - time.Nanosecond).UnixNano())
	restartedRec.quiet(t, 20*time.Millisecond)
	now.Store(stamp.Add(12 * time.Hour).UnixNano())
	if got := restartedRec.next(t); got != types.PresenceAvailable {
		t.Fatalf("at the interval boundary: %q, want available", got)
	}
	if got := restartedRec.next(t); got != types.PresenceUnavailable {
		t.Fatalf("after available: %q, want unavailable", got)
	}
	restarted.Shutdown(5 * time.Second)
	if got := readSessionBlip(b.StoreRoot); !got.Equal(stamp.Add(12 * time.Hour)) {
		t.Fatalf("second saved blip = %v", got)
	}
}

func TestSessionKeepalive_MissingOrCorruptStateIsRewrittenAfterTheBlip(t *testing.T) {
	for _, value := range []string{"missing", "", "not a timestamp", strings.Repeat("x", 100)} {
		t.Run(value, func(t *testing.T) {
			rec := newPresenceRecorder()
			b := keepaliveBridge(t, time.Hour, readyFlag(true), rec)
			if value != "missing" {
				if err := b.StoreRoot.WriteFile(sessionKeepaliveFile, []byte(value), storeFileMode); err != nil {
					t.Fatal(err)
				}
			}
			if got := readSessionBlip(b.StoreRoot); !got.IsZero() {
				t.Fatalf("invalid state read as %v", got)
			}
			b.startSessionKeepalive()
			if got := rec.next(t); got != types.PresenceAvailable {
				t.Fatalf("first presence = %q", got)
			}
			rec.next(t)
			b.Shutdown(5 * time.Second)
			if got := readSessionBlip(b.StoreRoot); got.IsZero() {
				t.Fatal("successful blip did not replace the invalid state")
			}
		})
	}
}

func TestSessionBlipStateRefusesLinksAndNeverWritesTheirTargets(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stamp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	outside := filepath.Join(t.TempDir(), "state")
	original := []byte(stamp.Format(time.RFC3339Nano))
	if err := os.WriteFile(outside, original, storeFileMode); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sessionKeepaliveFile, sessionKeepaliveFile + ".part"} {
		if err := root.Symlink(outside, name); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if got := readSessionBlip(root); !got.IsZero() {
		t.Fatal("state reader followed a symlink")
	}
	if err := writeSessionBlip(root, stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside) //nolint:gosec // outside is a fixed filename in t.TempDir, never user input
	if err != nil || string(data) != string(original) {
		t.Fatalf("link target changed: %q, %v", data, err)
	}
	if got := readSessionBlip(root); !got.Equal(stamp.Add(time.Hour)) {
		t.Fatalf("replacement state = %v", got)
	}
	if err := root.Remove(sessionKeepaliveFile); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir(sessionKeepaliveFile, storeDirMode); err != nil {
		t.Fatal(err)
	}
	if got := readSessionBlip(root); !got.IsZero() {
		t.Fatal("directory read as state")
	}
}

func TestSessionBlipStateRefusesAnInternalLink(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stamp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	original := []byte(stamp.Format(time.RFC3339Nano))
	if err := root.WriteFile("state-target", original, storeFileMode); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("state-target", sessionKeepaliveFile); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := readSessionBlip(root); !got.IsZero() {
		t.Fatal("state reader followed a link inside the store")
	}
	if err := writeSessionBlip(root, stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile("state-target")
	if err != nil || string(data) != string(original) {
		t.Fatalf("internal link target changed: %q, %v", data, err)
	}
}
