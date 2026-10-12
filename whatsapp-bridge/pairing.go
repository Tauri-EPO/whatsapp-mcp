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
	"os"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type sessionCleanupFunc func(context.Context, *sqlstore.Container, *ConnectionProblem) (*store.Device, *ConnectionProblem, error)

var errSessionCleanupPublication = errors.New("session cleanup completion could not be saved")

// SDK Device.Delete mutates its object. Reload an independent object and remove
// only the identity the unlink named; old markers without an identity use the
// legacy single-account store cleanup. The caller's context is shutdown only.
func prepareSessionDevice(ctx context.Context, container *sqlstore.Container, problem *ConnectionProblem) (*store.Device, *ConnectionProblem, error) {
	if problem.isUnlinked() && !problem.SessionRemoved {
		devices, err := container.GetAllDevices(ctx)
		if err != nil {
			return nil, problem, err
		}
		revoked := func(device *store.Device) bool {
			return problem.RevokedDevice == "" || device.ID.String() == problem.RevokedDevice
		}
		for _, device := range devices {
			if revoked(device) {
				if err := device.Delete(ctx); err != nil {
					return nil, problem, fmt.Errorf("remove revoked session: %w", err)
				}
			}
		}
		devices, err = container.GetAllDevices(ctx)
		if err != nil {
			return nil, problem, err
		}
		for _, device := range devices {
			if revoked(device) {
				return nil, problem, errors.New("revoked session still present")
			}
		}
		next := *problem
		next.SessionRemoved = true
		device, err := container.GetFirstDevice(ctx)
		return device, &next, err
	}
	device, err := container.GetFirstDevice(ctx)
	return device, problem, err
}

// Startup/test wrapper; the production startup defers cleanup to the factory
// until after REST serves health. Completion is durable before pairing begins.
func startupDevice(ctx context.Context, container *sqlstore.Container, root *os.Root, problem *ConnectionProblem) (*store.Device, *ConnectionProblem, error) {
	device, next, err := prepareSessionDevice(ctx, container, problem)
	if err != nil {
		return nil, problem, err
	}
	if next != problem {
		if err := writeConnectionProblem(root, next); err != nil {
			return nil, problem, errors.Join(errSessionCleanupPublication, err)
		}
	}
	return device, next, nil
}

func initialSessionDevice(ctx context.Context, container *sqlstore.Container, problem *ConnectionProblem) (*store.Device, bool, error) {
	if problem.isUnlinked() && !problem.SessionRemoved {
		return container.NewDevice(), true, nil
	}
	device, err := container.GetFirstDevice(ctx)
	return device, false, err
}

// SQL and disk work do not hold the health mutex. The publication mutex orders
// marker writes, and only the snapshot cleaned here may publish completion.
func (b *Bridge) unlinkedDevice(ctx context.Context, container *sqlstore.Container) (*store.Device, error) {
	expected, _ := b.connectionSnapshot()
	if !expected.isUnlinked() {
		return nil, errors.New("unlink state changed")
	}
	cleanup := b.sessionCleanup
	if cleanup == nil {
		cleanup = prepareSessionDevice
	}
	device, next, err := cleanup(ctx, container, expected)
	if err != nil {
		return nil, err
	}
	b.connectionPersistMu.Lock()
	defer b.connectionPersistMu.Unlock()
	current, _ := b.connectionSnapshot()
	if current != expected {
		return nil, errors.New("unlink state changed")
	}
	if err := b.persistConnectionSnapshot(next); err != nil {
		return nil, errors.Join(errSessionCleanupPublication, err)
	}
	b.connectionMu.Lock()
	b.connectionProblem = next
	b.connectionChangedLocked()
	b.connectionMu.Unlock()
	return device, nil
}

// Both startup and post-unlink recovery use this exact factory. SQLite may be
// busy with the SDK's cascade, or storage may need operator repair; keep health
// alive and retry without consuming a QR attempt or reusing revoked keys.
func (b *Bridge) pairingClientFactory(ctx context.Context, container *sqlstore.Container, history historyLimits, fullHistory bool, logger waLog.Logger, reconnect chan bool) func() (operatorPairingClient, error) {
	return func() (operatorPairingClient, error) {
		device := container.NewDevice()
		problem, _ := b.connectionSnapshot()
		if problem.isUnlinked() {
			warned := false
			for {
				var err error
				device, err = b.unlinkedDevice(ctx, container)
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				current, _ := b.connectionSnapshot()
				if !current.isUnlinked() {
					return nil, err // Preserve a newer restriction or explicit reset.
				}
				b.setPairingState("pairing_required")
				if !warned {
					b.Log.Warnf("Revoked session cleanup pending; retrying until shutdown: %v", err)
					warned = true
				}
				delay := b.cleanupRetryDelay
				if delay <= 0 {
					delay = time.Second
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
			}
		}
		history.apply(store.DeviceProps, device.ID != nil, fullHistory)
		fresh := newRuntimeClient(device, logger)
		b.installClient(fresh, device.ID != nil, reconnect)
		return fresh, nil
	}
}

// pairingClient is the slice of *whatsmeow.Client the pairing flow needs.
type pairingClient interface {
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	ConnectContext(context.Context) error
	Disconnect()
	SendPasskeyResponse(context.Context, *types.WebAuthnResponse) error
	SendPasskeyConfirmation(context.Context) error
	PairPhone(context.Context, string, bool, whatsmeow.PairClientType, string) (string, error)
}

type pairingOptions struct {
	attempts          int           // total attempts before giving up
	attemptTimeout    time.Duration // per-attempt deadline for the QR flow
	retryDelay        time.Duration // pause between attempts
	out               io.Writer     // where QR codes are drawn
	log               waLog.Logger
	beforeDial        func() error
	state             func(string)
	observe           func(whatsmeow.QRChannelItem)
	connectionContext context.Context // socket lifetime; cancelled after REST drains
}

var errPairingTimeout = errors.New("QR pairing timed out")
var errPairingTransport = errors.New("pairing transport failed")
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
				lastErr = errors.Join(errPairingTransport, err)
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
		return errors.Join(errPairingTransport, err)
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
			if opt.observe != nil {
				opt.observe(evt)
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
				opt.log.Errorf("QR pairing error (%s): %v", evt.Event, evt.Error)
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
