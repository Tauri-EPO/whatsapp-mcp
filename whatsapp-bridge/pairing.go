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
	waLog "go.mau.fi/whatsmeow/util/log"
)

// pairingClient is the slice of *whatsmeow.Client the pairing flow needs.
type pairingClient interface {
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	Connect() error
	Disconnect()
}

type pairingOptions struct {
	attempts       int           // total attempts before giving up
	attemptTimeout time.Duration // per-attempt deadline for the QR flow
	retryDelay     time.Duration // pause between attempts
	out            io.Writer     // where QR codes are drawn
	log            waLog.Logger
}

var errPairingTimeout = errors.New("QR pairing timed out")

// connectOrPair connects a paired client, or runs the QR pairing flow for an
// unpaired one, retrying up to opt.attempts times. Returns nil once
// connected and authenticated.
func connectOrPair(ctx context.Context, c pairingClient, paired bool, opt pairingOptions) error {
	var lastErr error
	for attempt := 1; attempt <= opt.attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(opt.retryDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		opt.log.Infof("Connection attempt %d/%d...", attempt, opt.attempts)

		if paired {
			if err := c.Connect(); err != nil {
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
	}
	return fmt.Errorf("could not connect after %d attempts: %w", opt.attempts, lastErr)
}

// pairOnce runs a single QR pairing attempt with its own deadline.
func pairOnce(parent context.Context, c pairingClient, opt pairingOptions, attempt int) error {
	ctx, cancel := context.WithTimeout(parent, opt.attemptTimeout)
	defer cancel()

	qrChan, err := c.GetQRChannel(ctx)
	if err != nil {
		opt.log.Errorf("Failed to get QR channel: %v", err)
		return err
	}
	if err := c.Connect(); err != nil {
		opt.log.Errorf("Failed to connect (attempt %d): %v", attempt, err)
		return err
	}

	// whatsmeow rotates the code roughly every 20 seconds and the phone
	// rejects a scan of an expired one, so every "code" event is redrawn.
	codesShown := 0
	for {
		select {
		case <-ctx.Done():
			opt.log.Errorf("Timeout waiting for QR code scan (attempt %d)", attempt)
			return errPairingTimeout
		case evt, ok := <-qrChan:
			if !ok {
				opt.log.Warnf("QR channel closed after %d code(s)", codesShown)
				return errPairingTimeout
			}
			switch {
			case evt.Event == "code":
				codesShown++
				printQRCode(opt.out, evt.Code, codesShown)
			case evt.Event == "success":
				return nil
			case evt.Event == "timeout":
				opt.log.Warnf("QR pairing timed out after %d code(s); starting over", codesShown)
				return errPairingTimeout
			case evt.Error != nil:
				opt.log.Errorf("QR pairing error (%s): %v", evt.Event, evt.Error)
				return evt.Error
			default:
				opt.log.Warnf("QR pairing event: %s", evt.Event)
			}
		}
	}
}
