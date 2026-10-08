package main

// QR pairing and first connection.
//
// A fresh store has no device ID: the bridge asks whatsmeow for a QR channel,
// connects, and redraws every rotated code until the phone scans one
// ("success"), the code sequence expires ("timeout") or whatsmeow reports an
// error. A paired store just connects. Each attempt has its own context and
// cancel; a timeout starts the next attempt immediately instead of waiting
// for the attempt deadline (issue #106).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// pairingClient is the slice of *whatsmeow.Client the pairing flow needs.
type pairingClient interface {
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	ConnectContext(context.Context) error
	Disconnect()
	SendPasskeyResponse(context.Context, *types.WebAuthnResponse) error
	SendPasskeyConfirmation(context.Context) error
}

type pairingOptions struct {
	attempts          int           // total attempts before giving up
	attemptTimeout    time.Duration // per-attempt deadline for the QR flow
	retryDelay        time.Duration // pause between attempts
	out               io.Writer     // where QR codes are drawn
	log               waLog.Logger
	beforeDial        func() error
	state             func(string)
	connectionContext context.Context // socket lifetime; cancelled after REST drains
}

var errPairingTimeout = errors.New("QR pairing timed out")
var errPairingOperator = errors.New("WhatsApp passkey step requires operator intervention; see docs/DOCKER.md Pairing")

// connectOrPair connects a paired client, or runs the QR pairing flow for an
// unpaired one, retrying up to opt.attempts times. Returns nil once
// connected and authenticated.
func connectOrPair(ctx context.Context, c pairingClient, paired bool, opt pairingOptions) error {
	var lastErr error
	for attempt := 1; attempt <= opt.attempts; attempt++ {
		if opt.beforeDial != nil {
			if err := opt.beforeDial(); err != nil {
				return err
			}
		}
		if attempt > 1 {
			select {
			case <-time.After(opt.retryDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		opt.log.Infof("Connection attempt %d/%d...", attempt, opt.attempts)

		if paired {
			if err := pairingConnect(ctx, c, opt); err != nil {
				lastErr = err
				opt.log.Errorf("Failed to connect (attempt %d): %v", attempt, err)
				continue
			}
			return nil
		}

		lastErr = pairOnce(ctx, c, opt, attempt)
		if lastErr == nil {
			return nil
		}
		c.Disconnect()
		if errors.Is(lastErr, errPairingOperator) {
			return lastErr
		}
	}
	return fmt.Errorf("could not connect after %d attempts: %w", opt.attempts, lastErr)
}

// pairOnce runs a single QR pairing attempt with its own deadline.
func pairOnce(parent context.Context, c pairingClient, opt pairingOptions, attempt int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	timer := time.NewTimer(opt.attemptTimeout)
	defer timer.Stop()

	qrChan, err := c.GetQRChannel(ctx)
	if err != nil {
		opt.log.Errorf("Failed to get QR channel: %v", err)
		return err
	}
	if err := pairingConnect(parent, c, opt); err != nil {
		opt.log.Errorf("Failed to connect (attempt %d): %v", attempt, err)
		return err
	}

	// whatsmeow rotates the code roughly every 20 seconds and the phone
	// rejects a scan of an expired one, so every "code" event is redrawn.
	codesShown := 0
	passkey := false
	setState := func(state string) {
		if opt.state != nil {
			opt.state(state)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if passkey {
				setState("passkey_failed")
				return errPairingOperator
			}
			opt.log.Errorf("Timeout waiting for QR code scan (attempt %d)", attempt)
			return errPairingTimeout
		case evt, ok := <-qrChan:
			if !ok {
				if passkey {
					setState("passkey_failed")
					return errPairingOperator
				}
				opt.log.Warnf("QR channel closed after %d code(s)", codesShown)
				return errPairingTimeout
			}
			switch {
			case evt.Event == whatsmeow.QRChannelEventPasskeyRequest:
				passkey = true
				setState("passkey_required")
				opt.log.Warnf("WhatsApp asked for a passkey check; this headless bridge cannot produce a WebAuthn assertion; see docs/DOCKER.md Pairing")
				wait := opt.attemptTimeout
				if evt.PasskeyRequest != nil && evt.PasskeyRequest.PublicKey != nil && evt.PasskeyRequest.PublicKey.Timeout > 0 {
					wait = min(time.Duration(evt.PasskeyRequest.PublicKey.Timeout), (5*time.Minute)/time.Millisecond) * time.Millisecond
				}
				timer.Reset(wait)
			case evt.Event == whatsmeow.QRChannelEventPasskeyResponse:
				passkey = true
				setState("passkey_confirm")
				opt.log.Warnf("WhatsApp passkey confirmation requires an operator to compare the code on the phone; see docs/DOCKER.md Pairing")
			case evt.Event == "code":
				if passkey {
					continue
				} // A scanned QR is no longer the pending credential.
				codesShown++
				printQRCode(opt.out, evt.Code, codesShown)
			case evt.Event == "success":
				setState("")
				return nil
			case evt.Event == "timeout":
				if passkey {
					setState("passkey_failed")
					return errPairingOperator
				}
				opt.log.Warnf("QR pairing timed out after %d code(s); starting over", codesShown)
				return errPairingTimeout
			case evt.Error != nil:
				if passkey {
					setState("passkey_failed")
					return errPairingOperator
				}
				opt.log.Errorf("QR pairing error (%s); see pairing diagnostics", evt.Event)
				return errors.New("WhatsApp QR pairing failed")
			default:
				opt.log.Warnf("QR pairing event: %s", evt.Event)
			}
		}
	}
}

// Interrupt startup dialing on a signal without cancelling an established
// socket before Shutdown has drained accepted REST requests.
func pairingConnect(ctx context.Context, c pairingClient, opt pairingOptions) error {
	lifecycle := opt.connectionContext
	if lifecycle == nil {
		lifecycle = ctx
	}
	dialCtx, cancel := context.WithCancel(lifecycle)
	stop := context.AfterFunc(ctx, cancel)
	if err := c.ConnectContext(dialCtx); err != nil {
		stop()
		cancel()
		return err
	}
	if !stop() {
		cancel()
		return ctx.Err()
	}
	return nil
}
