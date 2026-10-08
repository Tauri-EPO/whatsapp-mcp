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
	var previous <-chan struct{}
	var delivered chan struct{}
	if synchronous {
		ctx = context.WithoutCancel(ctx)
	}
	if immediate {
		b.lastConnectionEvent = key
		previous, delivered = b.queueConnectionDeliveryLocked()
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
			previous, delivered = b.queueConnectionDeliveryLocked()
			b.connectionEventsMu.Unlock()
		}
		defer b.finishConnectionDelivery(previous, delivered)
		// Reserving under the transition lock preserves order even when a
		// debounce expires just before reconnect. Terminal events share the
		// queue, with their entire wait and POST bounded by the exit deadline.
		if synchronous {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, connectionEventTimeout)
			defer cancel()
		}
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
				return
			}
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

func (b *Bridge) queueConnectionDeliveryLocked() (<-chan struct{}, chan struct{}) {
	previous := b.connectionDeliveryDone
	done := make(chan struct{})
	b.connectionDeliveryDone = done
	return previous, done
}

func (b *Bridge) finishConnectionDelivery(previous <-chan struct{}, done chan struct{}) {
	if previous != nil {
		select {
		case <-previous:
		default:
			// A terminal deadline may expire while waiting for an older POST.
			// Keep its place in the queue until that predecessor finishes.
			b.connectionEvents.Add(1)
			go func() {
				defer b.connectionEvents.Done()
				<-previous
				close(done)
			}()
			return
		}
	}
	close(done)
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
