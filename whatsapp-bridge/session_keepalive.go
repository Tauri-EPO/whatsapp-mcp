package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Session keepalive: tell WhatsApp this linked device is in use (issue #576).
//
// WhatsApp logs a linked device out when it has not been "opened" for about a
// month, and the phone warns a day before ("open WhatsApp on this device to
// keep it connected"). Being connected does not count: a bridge that
// reconnected several times a day for four weeks still showed its pairing time
// as its last connection. What was seen to move that date, on a paired device
// with the phone's Linked devices screen open:
//
//   - a new login (restart): no;
//   - the unified_session notice alone: no;
//   - presence "available", a few seconds, presence "unavailable": yes.
//
// The bridge otherwise never sends presence, on purpose: an available device
// shows the account as online and takes notifications away from the phone. So
// this is a short blip on a timer, not a state.

// sessionKeepaliveEnv sets how often the blip is sent, in hours.
const sessionKeepaliveEnv = "WHATSAPP_SESSION_KEEPALIVE_HOURS"

const sessionKeepaliveFile = ".session-keepalive"

const (
	// sessionKeepaliveInterval: the default for sessionKeepaliveEnv. Twice a
	// day is far inside a limit counted in weeks and survives a bridge that is
	// down for a night.
	sessionKeepaliveInterval = 12 * time.Hour
	// sessionKeepaliveMaxHours: a longer interval would leave fewer than four
	// blips inside the month WhatsApp allows, so it is refused at startup
	// rather than accepted and found out thirty days later.
	sessionKeepaliveMaxHours = 7 * 24
	// defaultSessionKeepaliveSettle: how long the session must have been
	// ready (connected and logged in) before a blip.
	defaultSessionKeepaliveSettle = time.Minute
	// defaultSessionKeepalivePoll: the cadence of the loop. Everything else is
	// compared against the wall clock at this cadence, so a host that was
	// suspended for a day blips when it wakes instead of counting twelve
	// hours of uptime first.
	defaultSessionKeepalivePoll = 15 * time.Second
	// defaultSessionKeepaliveRetry: how soon to try again when "available"
	// could not be sent, and how long a missing push name is waited out
	// before it is reported.
	defaultSessionKeepaliveRetry = 5 * time.Minute
	// defaultSessionPresenceHold: how long the device stays "available".
	defaultSessionPresenceHold = 5 * time.Second
	// sessionPresenceTimeout bounds each presence send: a half-dead socket
	// must fail the blip promptly, and the "unavailable" at shutdown has to
	// fit inside the bridge's shutdown budget.
	sessionPresenceTimeout = 3 * time.Second
)

// resolveSessionKeepalive parses sessionKeepaliveEnv (see resolveHoursEnv).
func resolveSessionKeepalive(value string) (time.Duration, error) {
	return resolveHoursEnv(sessionKeepaliveEnv, value, sessionKeepaliveInterval, sessionKeepaliveMaxHours)
}

// sessionKeepaliveSummary renders the setting for the startup log.
func sessionKeepaliveSummary(interval time.Duration) string {
	if interval <= 0 {
		return "off (WhatsApp logs a linked device out about a month after it was last opened)"
	}
	return everyHoursSummary(interval)
}

// presenceSender is the part of the WhatsApp client the keepalive needs.
type presenceSender func(ctx context.Context, state types.Presence) error

// startSessionKeepalive runs the loop in a goroutine Shutdown waits for.
// SessionKeepalive <= 0 disables it.
func (b *Bridge) startSessionKeepalive() {
	if b.SessionKeepalive <= 0 {
		return
	}
	b.keepaliveLoop.Add(1)
	go func() {
		defer b.keepaliveLoop.Done()
		b.runSessionKeepalive()
	}()
}

// runSessionKeepalive wakes every SessionKeepalivePoll until b.ctx is
// cancelled (Shutdown) and sends the blip when all of this holds, by the wall
// clock: the session has been ready for SessionKeepaliveSettle, the last blip
// is at least SessionKeepalive old, and a failed attempt is at least
// SessionKeepaliveRetry old.
//
// "Ready" is connected and logged in: while the bridge shows its QR code the
// socket is up and there is no session to keep. Whatever happens after
// "available" was attempted, "unavailable" follows: after the hold, again if
// that send fails, right away if "available" itself failed (whatsmeow
// switches to active delivery receipts before it writes the frame), and on
// the way out when the bridge is shutting down.
func (b *Bridge) runSessionKeepalive() {
	send := b.sessionPresence
	if send == nil {
		if b.Client == nil {
			return
		}
		send = b.Client.SendPresence
	}
	ready := b.sessionReady
	if ready == nil {
		ready = func() bool { return b.Client != nil && b.Client.IsConnected() && b.Client.IsLoggedIn() }
	}
	clock := b.sessionNow
	if clock == nil {
		clock = time.Now
	}
	lastBlip := readSessionBlip(b.StoreRoot)
	// A clock corrected after the previous process ran must not suppress
	// keepalives until an erroneously future date.
	if lastBlip.After(clock().Round(0)) {
		lastBlip = time.Time{}
	}

	var (
		readySince   time.Time // zero while the session is not ready
		notBefore    time.Time // earliest next attempt after a failed one
		noNameSince  time.Time // zero unless the push name is still missing
		noNameWarned bool
		pendingOff   bool // "available" went out and "unavailable" has not yet
	)
	for {
		if !b.sleep(b.SessionKeepalivePoll) {
			if pendingOff {
				if err := markUnavailable(b.ctx, send); err != nil {
					b.Log.Warnf("Session keepalive: shutting down with the device still marked available: %v", err)
				}
			}
			return
		}
		if pendingOff {
			if err := markUnavailable(b.ctx, send); err != nil {
				b.Log.Warnf("Session keepalive: the device is still marked available, trying again: %v", err)
				continue
			}
			pendingOff = false
			continue
		}

		// Round(0) drops the monotonic reading: these comparisons must see
		// the time a suspended host slept through.
		now := clock().Round(0)
		if !ready() {
			readySince = time.Time{}
			continue
		}
		if readySince.IsZero() {
			readySince = now
		}
		if now.Sub(readySince) < b.SessionKeepaliveSettle ||
			(!lastBlip.IsZero() && now.Sub(lastBlip) < b.SessionKeepalive) ||
			now.Before(notBefore) {
			continue
		}

		err := sendPresence(b.ctx, send, types.PresenceAvailable)
		if errors.Is(err, whatsmeow.ErrNoPushName) {
			// Logged in, but the app-state sync that carries the push name
			// has not arrived. whatsmeow refuses before it changes anything,
			// so there is nothing to undo; say so once if it lasts.
			if noNameSince.IsZero() {
				noNameSince = now
			}
			if !noNameWarned && now.Sub(noNameSince) >= b.SessionKeepaliveRetry {
				noNameWarned = true
				b.Log.Warnf("Session keepalive: WhatsApp has not sent this account's push name yet, so the device cannot be marked as in use; still waiting")
			}
			continue
		}
		noNameSince, noNameWarned = time.Time{}, false
		if err != nil {
			// The frame may or may not have gone out, and whatsmeow already
			// counts the device as active: take that back either way.
			offErr := markUnavailable(b.ctx, send)
			// Not the error's identity: a socket torn down mid-write surfaces
			// as "context canceled" too, and the loop has to outlive that.
			if b.ctx.Err() != nil {
				return
			}
			b.Log.Warnf("Session keepalive: could not tell WhatsApp this device is in use: %v", err)
			if offErr != nil {
				b.Log.Debugf("Session keepalive: the unavailable that follows a failed blip failed too: %v", offErr)
			}
			notBefore = now.Add(b.SessionKeepaliveRetry)
			continue
		}

		lastBlip = now
		b.metrics.sessionKeepalives.Add(1)
		b.Log.Infof("Session keepalive: told WhatsApp this linked device is in use")
		stopping := !b.sleep(b.SessionPresenceHold)
		offErr := markUnavailable(b.ctx, send)
		// Disk I/O must not delay the hold or the unavailable send. Remember
		// the successful available even if unavailable will need a retry.
		if err := writeSessionBlip(b.StoreRoot, lastBlip); err != nil {
			b.Log.Warnf("Session keepalive: could not remember the last blip: %v", err)
		}
		if offErr != nil {
			if stopping {
				b.Log.Warnf("Session keepalive: shutting down with the device still marked available: %v", offErr)
				return
			}
			b.Log.Warnf("Session keepalive: the device is still marked available, trying again: %v", offErr)
			pendingOff = true
		}
		if stopping {
			return
		}
	}
}

// readSessionBlip treats missing, unreadable or corrupt state as never. Only a
// regular file opened through the store root is read; links are not state.
func readSessionBlip(root *os.Root) time.Time {
	if root == nil {
		return time.Time{}
	}
	seen, err := root.Lstat(sessionKeepaliveFile)
	if err != nil || !seen.Mode().IsRegular() {
		return time.Time{}
	}
	f, err := root.Open(sessionKeepaliveFile)
	if err != nil {
		return time.Time{}
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !os.SameFile(seen, opened) {
		return time.Time{}
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(data) > 64 {
		return time.Time{}
	}
	stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}
	}
	return stamp
}

// writeSessionBlip replaces one timestamp atomically with a fresh owner-only
// file. A stale temporary name is unlinked, never opened through, and rename
// replaces a link under the final name without touching its target.
func writeSessionBlip(root *os.Root, stamp time.Time) error {
	if root == nil {
		return errors.New("store directory unavailable")
	}
	part := sessionKeepaliveFile + ".part"
	if err := root.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(part) }()
	_, writeErr := f.WriteString(stamp.UTC().Format(time.RFC3339Nano) + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(part, sessionKeepaliveFile)
}

// sendPresence sends one presence under a bounded context.
func sendPresence(ctx context.Context, send presenceSender, state types.Presence) error {
	sendCtx, cancel := context.WithTimeout(ctx, sessionPresenceTimeout)
	defer cancel()
	return send(sendCtx, state)
}

// markUnavailable sends presence "unavailable" under a context that survives
// the cancellation of ctx: it is the half of the blip that must not be skipped.
func markUnavailable(ctx context.Context, send presenceSender) error {
	return sendPresence(context.WithoutCancel(ctx), send, types.PresenceUnavailable)
}
