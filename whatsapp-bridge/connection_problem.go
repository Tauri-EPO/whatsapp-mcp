package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const connectionProblemFile = ".connection-problem"

// The pinned library consumes 500/503 before event dispatch. Its public Logger
// seam is the only observer for these failures: match the constant format and
// typed numeric argument, never parse server text or a rendered log line.
type connectionProblemLogger struct {
	waLog.Logger
	bridge *Bridge
	active func() bool
}

func (l connectionProblemLogger) Warnf(format string, args ...any) {
	if l.active != nil && !l.active() {
		l.Logger.Warnf(format, args...)
		return
	}
	if format == "Got %d/%s connect failure, assuming automatic reconnect will handle it" && len(args) == 2 {
		if code, ok := args[0].(int); ok && (code == 500 || code == 503) {
			l.bridge.recordConnectionProblemIf(code, 0, 0, l.active)
		}
	} else if format == "Got 503 stream error, assuming automatic reconnect will handle it" && len(args) == 0 {
		l.bridge.recordConnectionProblemIf(503, 0, 0, l.active)
	}
	l.Logger.Warnf(format, args...)
}

func (l connectionProblemLogger) Sub(module string) waLog.Logger {
	return connectionProblemLogger{Logger: l.Logger.Sub(module), bridge: l.bridge, active: l.active}
}

type ConnectionProblem struct {
	RevokedDevice  string     `json:"-"` // Private disk identity; never returned in health or webhooks.
	SessionRemoved bool       `json:"session_removed,omitempty"`
	Kind           string     `json:"kind"`
	Code           int        `json:"code"`
	TempBanReason  int        `json:"temp_ban_reason,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Since          time.Time  `json:"since"`
	BuildVersion   string     `json:"build_version,omitempty"`
}

func classifyConnectionProblem(code int, reason int, expire time.Duration, now time.Time) *ConnectionProblem {
	p := &ConnectionProblem{Kind: "other", Code: code, TempBanReason: reason, Since: now.UTC()}
	switch code {
	case 401:
		p.Kind = "unlinked"
	case 402:
		p.Kind = "temporarily_banned"
		if expire <= 0 {
			expire = time.Hour
		}
		until := now.Add(expire).UTC()
		p.ExpiresAt = &until
	case 403:
		p.Kind = "locked"
	case 405:
		p.Kind = "client_outdated"
		p.BuildVersion = connectionBuildVersion()
	case 406:
		p.Kind = "banned"
	default:
		if code >= 500 && code < 600 {
			p.Kind = "server_error"
		}
	}
	return p
}

func connectionBuildVersion() string {
	info := buildInfo()
	return info.Version + "+" + info.Commit + ";whatsmeow=" + info.Whatsmeow
}

func (p *ConnectionProblem) restrictsAccount() bool {
	return p != nil && (p.Kind == "temporarily_banned" || p.Kind == "banned" || p.Kind == "locked" || p.Kind == "client_outdated")
}

func (b *Bridge) connectionSnapshot() (*ConnectionProblem, string) {
	b.connectionMu.Lock()
	defer b.connectionMu.Unlock()
	return b.connectionProblem, b.pairingState
}

func (b *Bridge) connectionWaitSnapshot() (*ConnectionProblem, string, <-chan struct{}) {
	b.connectionMu.Lock()
	defer b.connectionMu.Unlock()
	if b.connectionChanged == nil {
		b.connectionChanged = make(chan struct{})
	}
	return b.connectionProblem, b.pairingState, b.connectionChanged
}

func (b *Bridge) connectionChangedLocked() {
	if b.connectionChanged != nil {
		close(b.connectionChanged)
	}
	b.connectionChanged = make(chan struct{})
}

func (b *Bridge) connectionNow() time.Time {
	if b.problemNow != nil {
		return b.problemNow()
	}
	return time.Now()
}

// Serialise state changes and persistence; a restart must never forget a ban.
func (b *Bridge) recordConnectionProblem(code, reason int, expire time.Duration) *ConnectionProblem {
	return b.recordConnectionProblemIf(code, reason, expire, nil)
}

func (b *Bridge) recordConnectionProblemIf(code, reason int, expire time.Duration, active func() bool) *ConnectionProblem {
	b.connectionPersistMu.Lock()
	defer b.connectionPersistMu.Unlock()
	b.connectionMu.Lock()
	if active != nil && !active() {
		b.connectionMu.Unlock()
		return nil
	}
	next := classifyConnectionProblem(code, reason, expire, b.connectionNow())
	if !next.restrictsAccount() && b.connectionProblem.restrictsAccount() {
		p := b.connectionProblem
		b.connectionMu.Unlock()
		return p
	}
	if code != 401 && b.connectionProblem != nil && b.connectionProblem.Kind == "unlinked" && b.operatorRetiredClient.Load() == b.currentClient() {
		p := b.connectionProblem
		b.connectionMu.Unlock()
		return p
	}
	if code == 401 {
		if id := b.runtimeDeviceID.Load(); id != nil {
			next.RevokedDevice = *id
		}
	}
	b.connectionProblem = next
	b.connectionChangedLocked()
	b.connectionMu.Unlock()
	if code == 402 && b.metrics != nil {
		b.metrics.mu.Lock()
		if b.metrics.tempBans == nil {
			b.metrics.tempBans = map[int]int64{}
		}
		switch reason {
		case 101, 102, 103, 104, 106:
		default:
			reason = 0
		}
		b.metrics.tempBans[reason]++
		b.metrics.mu.Unlock()
	}
	b.Log.Warnf("WhatsApp connection problem: %s (code %d, temporary-ban reason %d)", next.Kind, code, reason)
	if b.StoreRoot != nil {
		_ = b.persistConnectionSnapshot(next)
	}
	return next
}

// Caller owns connectionPersistMu. Disk I/O never owns the health/state mutex.
func (b *Bridge) persistConnectionSnapshot(problem *ConnectionProblem) error {
	var err error
	if b.problemWrite != nil {
		err = b.problemWrite(b.StoreRoot, problem)
	} else {
		err = writeConnectionProblem(b.StoreRoot, problem)
	}
	b.connectionMu.Lock()
	wasFailed := b.problemPersistenceFailed
	b.problemPersistenceFailed = err != nil
	b.connectionMu.Unlock()
	if err != nil && !wasFailed {
		b.Log.Errorf("Could not persist connection problem (account restriction=%t): %v", problem.restrictsAccount(), err)
	}
	return err
}

func (b *Bridge) persistConnectionProblemIf(expected *ConnectionProblem) bool {
	b.connectionPersistMu.Lock()
	defer b.connectionPersistMu.Unlock()
	b.connectionMu.Lock()
	same := b.connectionProblem == expected
	b.connectionMu.Unlock()
	if !same {
		return false
	}
	if b.StoreRoot != nil {
		_ = b.persistConnectionSnapshot(expected)
	}
	return true
}

func (b *Bridge) clearConnectionProblem() {
	b.connectionPersistMu.Lock()
	defer b.connectionPersistMu.Unlock()
	if b.operatorRetiredClient.Load() == b.currentClient() {
		return
	}
	b.clearConnectionSnapshot(true)
}

func (b *Bridge) clearConnectionProblemIf(expected *ConnectionProblem) bool {
	b.connectionPersistMu.Lock()
	defer b.connectionPersistMu.Unlock()
	b.connectionMu.Lock()
	same := b.connectionProblem == expected
	b.connectionMu.Unlock()
	if !same {
		return false
	}
	b.clearConnectionSnapshot(false)
	return true
}

// Caller owns connectionPersistMu; immutable snapshots cannot change mid-write.
func (b *Bridge) clearConnectionSnapshot(keepOperatorIdle bool) {
	if b.StoreRoot != nil {
		if err := b.StoreRoot.Remove(connectionProblemFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			b.connectionMu.Lock()
			b.problemPersistenceFailed = true
			b.connectionMu.Unlock()
			b.Log.Errorf("Could not clear saved connection problem: %v", err)
			return
		}
	}
	b.connectionMu.Lock()
	b.connectionProblem = nil
	if !keepOperatorIdle || !b.operatorLogout.Load() {
		b.pairingState = ""
	}
	b.problemPersistenceFailed = false
	b.connectionChangedLocked()
	b.connectionMu.Unlock()
}

func (b *Bridge) setPairingState(state string) {
	b.connectionMu.Lock()
	if b.operatorLogout.Load() {
		b.connectionMu.Unlock()
		return // A cancelled pairing attempt cannot reopen operator idle.
	}
	b.pairingState = state
	b.connectionChangedLocked()
	b.connectionMu.Unlock()
	if state != "" {
		b.notifyConnection("pairing_required", state, true, false)
	}
}

// Every dial, including startup, must pass here. A blocked account stays alive
// serving health until shutdown; clearing its saved state is an operator action.
func (b *Bridge) waitConnectionAllowed() error {
	return b.waitConnectionAllowedContext(b.ctx)
}

func (b *Bridge) waitConnectionAllowedContext(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, state, changed := b.connectionWaitSnapshot()
		b.connectionMu.Lock()
		retry := b.problemPersistenceFailed && p.restrictsAccount() && b.StoreRoot != nil
		b.connectionMu.Unlock()
		if retry {
			b.persistConnectionProblemIf(p)
		}
		b.connectionMu.Lock()
		failed := b.problemPersistenceFailed && b.connectionProblem.restrictsAccount()
		b.connectionMu.Unlock()
		if failed {
			timer := time.NewTimer(time.Second)
			select {
			case <-changed:
				timer.Stop()
				continue
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				continue
			}
		}
		blocked := state != "" || p != nil && (p.Kind == "banned" || p.Kind == "locked" || p.Kind == "client_outdated")
		if blocked {
			if b.connectionBlocked != nil {
				b.connectionBlocked()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
				continue
			}
		}
		if p == nil || p.ExpiresAt == nil || !b.connectionNow().Before(*p.ExpiresAt) {
			return nil
		}
		wait := p.ExpiresAt.Sub(b.connectionNow())
		if b.problemWait != nil {
			if err := b.problemWait(wait); err != nil {
				return err
			}
		} else {
			timer := time.NewTimer(wait)
			select {
			case <-changed:
				timer.Stop()
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
}

func readConnectionProblem(root *os.Root) (*ConnectionProblem, error) {
	info, err := root.Lstat(connectionProblemFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("saved connection problem is not a regular file")
	}
	f, err := root.Open(connectionProblemFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("saved connection problem changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	var saved struct {
		ConnectionProblem
		RevokedDevice string `json:"revoked_device,omitempty"`
	}
	if len(data) > 4096 || json.Unmarshal(data, &saved) != nil || saved.Since.IsZero() {
		return nil, errors.New("invalid saved connection problem")
	}
	p := saved.ConnectionProblem
	p.RevokedDevice = saved.RevokedDevice
	if p.RevokedDevice != "" {
		jid, err := types.ParseJID(p.RevokedDevice)
		if err != nil || jid.IsEmpty() || jid.String() != p.RevokedDevice {
			return nil, errors.New("invalid saved revoked device")
		}
	}
	expected := classifyConnectionProblem(p.Code, p.TempBanReason, time.Hour, p.Since)
	if p.Kind != expected.Kind || p.Kind == "temporarily_banned" && p.ExpiresAt == nil {
		return nil, errors.New("invalid saved connection classification")
	}
	return clearOutdatedBuildProblem(root, &p, connectionBuildVersion())
}

func clearOutdatedBuildProblem(root *os.Root, p *ConnectionProblem, runningBuild string) (*ConnectionProblem, error) {
	if p.Kind == "client_outdated" && p.BuildVersion != runningBuild {
		if err := root.Remove(connectionProblemFile); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return p, nil
}

func writeConnectionProblem(root *os.Root, p *ConnectionProblem) error {
	part := connectionProblemFile + ".part"
	if err := root.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(part) }()
	writeErr := json.NewEncoder(f).Encode(struct {
		*ConnectionProblem
		RevokedDevice string `json:"revoked_device,omitempty"`
	}{p, p.RevokedDevice})
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return root.Rename(part, connectionProblemFile)
}
