package main

import (
	"context"
	"errors"
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
	// ready (connected and logged in) before the first blip.
	defaultSessionKeepaliveSettle = time.Minute
	// defaultSessionKeepalivePoll: how often readiness is looked at while the
	// bridge is pairing or the socket is down, and how soon a pending
	// "unavailable" is tried again.
	defaultSessionKeepalivePoll = 15 * time.Second
	// defaultSessionKeepaliveRetry: how soon to try again when "available"
	// could not be sent.
	defaultSessionKeepaliveRetry = 5 * time.Minute
	// defaultSessionPresenceHold: how long the device stays "available".
	defaultSessionPresenceHold = 5 * time.Second
	// sessionUnavailableTimeout bounds the "unavailable" send, which runs under
	// a context of its own so that a shutdown cannot skip it.
	sessionUnavailableTimeout = 10 * time.Second
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
	b.sessionKeepalive.Add(1)
	go func() {
		defer b.sessionKeepalive.Done()
		b.runSessionKeepalive()
	}()
}

// runSessionKeepalive sends the blip once the session has been ready for
// SessionKeepaliveSettle, and then every SessionKeepalive, until b.ctx is
// cancelled (Shutdown).
//
// "Ready" is connected and logged in: while the bridge shows its QR code the
// socket is up and there is no session to keep. A send that fails is tried
// again after SessionKeepaliveRetry instead of a whole interval. Once
// "available" went out, "unavailable" must follow: it is retried on its own,
// and sent on the way out when the bridge shuts down in between, so the
// account is never left showing as online.
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

	settled := false    // the session has been ready for SessionKeepaliveSettle
	pendingOff := false // "available" went out and "unavailable" has not yet
	delay := b.SessionKeepalivePoll
	for {
		if !b.keepaliveWait(delay) {
			if pendingOff {
				if err := markUnavailable(b.ctx, send); err != nil {
					b.Log.Warnf("Session keepalive: shutting down with the device still marked available: %v", err)
				}
			}
			return
		}
		if !ready() {
			settled = false
			delay = b.SessionKeepalivePoll
			continue
		}
		if !settled {
			settled = true
			delay = b.SessionKeepaliveSettle
			continue
		}

		if !pendingOff {
			if err := send(b.ctx, types.PresenceAvailable); err != nil {
				// Not the error's identity: a socket torn down mid-write
				// surfaces as "context canceled" too, and the loop has to
				// outlive that.
				if b.ctx.Err() != nil {
					return
				}
				if errors.Is(err, whatsmeow.ErrNoPushName) {
					// Logged in, but the app-state sync that carries the
					// push name has not arrived yet.
					b.Log.Debugf("Session keepalive: no push name yet, waiting")
					delay = b.SessionKeepalivePoll
					continue
				}
				b.Log.Warnf("Session keepalive: could not tell WhatsApp this device is in use: %v", err)
				delay = b.SessionKeepaliveRetry
				continue
			}
			b.metrics.sessionKeepalives.Add(1)
			b.Log.Infof("Session keepalive: told WhatsApp this linked device is in use")
			pendingOff = true
			delay = b.SessionPresenceHold
			continue
		}

		if err := markUnavailable(b.ctx, send); err != nil {
			b.Log.Warnf("Session keepalive: the device is still marked available, trying again: %v", err)
			delay = b.SessionKeepalivePoll
			continue
		}
		pendingOff = false
		delay = b.SessionKeepalive
	}
}

// keepaliveWait sleeps for d and reports false when the bridge is shutting down.
func (b *Bridge) keepaliveWait(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-b.ctx.Done():
		return false
	}
}

// markUnavailable sends presence "unavailable" under a context that survives
// the cancellation of ctx: it is the half of the blip that must not be skipped.
func markUnavailable(ctx context.Context, send presenceSender) error {
	offCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionUnavailableTimeout)
	defer cancel()
	return send(offCtx, types.PresenceUnavailable)
}
