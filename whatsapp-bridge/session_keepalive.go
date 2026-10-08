package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	// sessionKeepaliveStartDelay: let the connection settle first.
	sessionKeepaliveStartDelay = time.Minute
	// sessionKeepaliveRetryDelay: how soon to try again when the socket was
	// down or the send failed.
	sessionKeepaliveRetryDelay = 5 * time.Minute
	// sessionPresenceHold: how long the device stays "available" each time.
	sessionPresenceHold = 5 * time.Second
)

// resolveSessionKeepalive parses sessionKeepaliveEnv. Zero disables the
// keepalive; a negative or non-numeric value is an error so main() fails fast
// rather than silently running with a default the operator did not write.
func resolveSessionKeepalive(value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return sessionKeepaliveInterval, nil
	}
	hours, err := strconv.Atoi(v)
	if err != nil || hours < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of hours (0 disables)", sessionKeepaliveEnv, value)
	}
	return time.Duration(hours) * time.Hour, nil
}

// sessionKeepaliveSummary renders the setting for the startup log.
func sessionKeepaliveSummary(interval time.Duration) string {
	if interval <= 0 {
		return "off (WhatsApp logs a linked device out about a month after it was last opened)"
	}
	return fmt.Sprintf("every %d h", int(interval.Hours()))
}

// presenceSender is the part of the WhatsApp client the keepalive needs.
type presenceSender func(ctx context.Context, state types.Presence) error

// signalSessionInUse marks the device available, holds, and marks it
// unavailable again. The second step is attempted even when the context was
// cancelled during the hold, with a context of its own: a bridge shutting
// down must not leave the account showing as online.
func signalSessionInUse(ctx context.Context, send presenceSender, hold time.Duration) error {
	if err := send(ctx, types.PresenceAvailable); err != nil {
		return err
	}
	select {
	case <-time.After(hold):
	case <-ctx.Done():
	}
	offCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return send(offCtx, types.PresenceUnavailable)
}

// runSessionKeepalive sends the blip sessionKeepaliveStartDelay after startup
// and then every SessionKeepalive until b.ctx is cancelled (Shutdown).
// SessionKeepalive <= 0 disables it. While the socket is down, or when a send
// fails, it looks again after sessionKeepaliveRetryDelay instead of waiting a
// whole interval.
func (b *Bridge) runSessionKeepalive() {
	if b.SessionKeepalive <= 0 {
		return
	}
	send := b.sessionPresence
	if send == nil {
		if b.Client == nil {
			return
		}
		send = b.Client.SendPresence
	}
	timing := b.sessionKeepaliveTiming
	if timing == (sessionKeepaliveTiming{}) {
		timing = sessionKeepaliveTiming{start: sessionKeepaliveStartDelay, retry: sessionKeepaliveRetryDelay, hold: sessionPresenceHold}
	}
	delay := timing.start
	for {
		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return
		}
		if b.Connected != nil && !b.Connected() {
			delay = timing.retry
			continue
		}
		if err := signalSessionInUse(b.ctx, send, timing.hold); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			b.Log.Warnf("Session keepalive: could not tell WhatsApp this device is in use: %v", err)
			delay = timing.retry
			continue
		}
		b.metrics.sessionKeepalives.Add(1)
		b.Log.Infof("Session keepalive: told WhatsApp this linked device is in use")
		delay = b.SessionKeepalive
	}
}

// sessionKeepaliveTiming holds the three waits of the loop; the zero value
// means the defaults above. Tests set short ones on their own Bridge.
type sessionKeepaliveTiming struct {
	start, retry, hold time.Duration
}
