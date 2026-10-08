package main

import (
	"context"
	"time"
)

const connectionEventTimeout = 2 * time.Second

type connectionPayload struct {
	Type         string             `json:"type"`
	State        string             `json:"state"`
	Reason       string             `json:"reason"`
	At           time.Time          `json:"at"`
	Problem      *ConnectionProblem `json:"connection_problem,omitempty"`
	PairingState string             `json:"pairing_state,omitempty"`
}

// At most one disconnected timer is pending. Reconnection cancels it without
// posting another connected event when the receiver never saw the gap.
// Terminal events synchronously finish one bounded POST before process exit.
func (b *Bridge) notifyConnection(state, reason string, immediate, synchronous bool) {
	problem, pairing := b.connectionSnapshot()
	payload := connectionPayload{Type: "connection", State: state, Reason: reason, At: b.connectionNow().UTC(), Problem: problem, PairingState: pairing}
	b.connectionEventsMu.Lock()
	if b.connectionEventsClosing || b.ctx.Err() != nil {
		b.connectionEventsMu.Unlock()
		return
	}
	if state != "disconnected" || immediate {
		if b.connectionDisconnectCancel != nil {
			b.connectionDisconnectCancel()
			b.connectionDisconnectCancel = nil
		}
	} else if b.connectionDisconnectCancel != nil {
		b.connectionEventsMu.Unlock()
		return
	}
	key := state + ":" + reason
	if key == b.lastConnectionEvent {
		b.connectionEventsMu.Unlock()
		return
	}
	b.Log.Infof("WhatsApp connection state: %s (%s)", state, reason)
	if !b.ForwardConnection || !b.Webhook.Enabled() {
		b.lastConnectionEvent = key
		b.connectionEventsMu.Unlock()
		return
	}
	ctx := b.ctx
	if synchronous {
		ctx = context.WithoutCancel(ctx)
	}
	if immediate {
		b.lastConnectionEvent = key
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		b.connectionDisconnectCancel = cancel
	}
	b.connectionEvents.Add(1)
	debounce := b.ConnectionDebounce
	if debounce <= 0 {
		debounce = 5 * time.Second
	}
	b.connectionEventsMu.Unlock()
	post := func() {
		defer b.connectionEvents.Done()
		if !immediate {
			if b.connectionEventWait != nil {
				if !b.connectionEventWait(ctx, debounce) {
					return
				}
			} else {
				timer := time.NewTimer(debounce)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return
				}
			}
			b.connectionEventsMu.Lock()
			if ctx.Err() != nil {
				b.connectionEventsMu.Unlock()
				return
			}
			b.lastConnectionEvent = key
			b.connectionDisconnectCancel = nil
			b.connectionEventsMu.Unlock()
		}
		sendCtx, cancel := context.WithTimeout(ctx, connectionEventTimeout)
		defer cancel()
		b.Webhook.sendJSON(sendCtx, payload)
	}
	if synchronous {
		post()
	} else {
		go post()
	}
}

func (b *Bridge) stopConnectionEvents() {
	b.connectionEventsMu.Lock()
	b.connectionEventsClosing = true
	if b.connectionDisconnectCancel != nil {
		b.connectionDisconnectCancel()
		b.connectionDisconnectCancel = nil
	}
	b.connectionEventsMu.Unlock()
}
