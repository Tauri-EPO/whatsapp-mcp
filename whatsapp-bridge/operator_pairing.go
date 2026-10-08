package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type operatorPairingClient interface {
	pairingClient
}

type operatorQR struct {
	Payload   string    `json:"payload"`
	Sequence  int       `json:"sequence"`
	ExpiresAt time.Time `json:"expires_at"`
}

type operatorPairingState struct {
	State            string                   `json:"state"`
	Attempt          int                      `json:"attempt"`
	Attempts         int                      `json:"attempts"`
	Generation       uint64                   `json:"generation"`
	QR               *operatorQR              `json:"qr"`
	PairCode         *operatorCodeExpiry      `json:"pair_code"`
	Passkey          *types.WebAuthnPublicKey `json:"passkey,omitempty"`
	ConfirmationCode string                   `json:"confirmation_code,omitempty"`
	StepExpiresAt    *time.Time               `json:"step_expires_at,omitempty"`
}
type operatorCodeExpiry struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// A single owner drives the QR channel. HTTP actions reuse that live attempt;
// the action mutex serializes SDK mutations with cancellation/client handoff.
type operatorPairing struct {
	mu             sync.Mutex
	action         sync.Mutex
	b              *Bridge
	state          operatorPairingState
	client         operatorPairingClient
	factory        func() (operatorPairingClient, error)
	paired         func() bool
	connected      func() bool
	ctx            context.Context
	cancelAttempt  context.CancelFunc
	attemptContext context.Context
	kick           chan struct{}
	reconnect      chan bool
	done           chan struct{}
	started        atomic.Bool
	codeCalls      int
	sequence       int
	completing     bool
	completionDone chan struct{}
	out            io.Writer
	opt            pairingOptions
	now            func() time.Time
}

func newOperatorPairing(ctx context.Context, b *Bridge, client operatorPairingClient, factory func() (operatorPairingClient, error), paired, connected func() bool, out io.Writer, reconnect chan bool) *operatorPairing {
	return &operatorPairing{b: b, client: client, factory: factory, paired: paired, connected: connected, ctx: ctx, kick: make(chan struct{}, 1), done: make(chan struct{}), out: out, now: time.Now,
		state: operatorPairingState{State: "starting", Attempts: 3}, opt: pairingOptions{attemptTimeout: 5 * time.Minute, retryDelay: 5 * time.Second}, reconnect: reconnect}
}

func (p *operatorPairing) invalidateLocked(state string) {
	p.state.State = state
	p.state.QR, p.state.PairCode, p.state.Passkey = nil, nil, nil
	p.state.ConfirmationCode, p.state.StepExpiresAt = "", nil
}

func (p *operatorPairing) observe(generation uint64, evt whatsmeow.QRChannelItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.state.Generation {
		return
	}
	now := p.now().UTC()
	switch evt.Event {
	case whatsmeow.QRChannelEventCode:
		if p.completing || p.state.State == "passkey_required" || p.state.State == "passkey_submitted" || p.state.State == "passkey_confirm" {
			return
		}
		p.sequence++
		p.state.QR = &operatorQR{Payload: evt.Code, Sequence: p.sequence, ExpiresAt: now.Add(evt.Timeout)}
		if p.state.PairCode == nil {
			p.state.State = "awaiting_qr"
		}
	case whatsmeow.QRChannelEventPasskeyRequest:
		p.invalidateLocked("passkey_required")
		if evt.PasskeyRequest != nil {
			p.state.Passkey = evt.PasskeyRequest.PublicKey
		}
		wait := p.opt.attemptTimeout
		if p.state.Passkey != nil && p.state.Passkey.Timeout > 0 {
			wait = min(time.Duration(p.state.Passkey.Timeout), (5*time.Minute)/time.Millisecond) * time.Millisecond
		}
		expires := now.Add(wait)
		p.state.StepExpiresAt = &expires
	case whatsmeow.QRChannelEventPasskeyResponse:
		// SkipHandoffUX is confirmed by GetQRChannel itself; never submit twice.
		if evt.PasskeyConfirmation == nil || evt.PasskeyConfirmation.SkipHandoffUX {
			return
		}
		p.state.State, p.state.Passkey = "passkey_confirm", nil
		p.state.ConfirmationCode = evt.PasskeyConfirmation.Code
	case "success":
		p.invalidateLocked("paired")
	default:
		if evt.Event == "timeout" || evt.Error != nil {
			p.invalidateLocked("expired")
		}
	}
}

func (p *operatorPairing) connectionEvent(evt interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch evt.(type) {
	case *events.Connected:
		p.invalidateLocked("connected")
	case *events.PairSuccess:
		p.invalidateLocked("paired")
		p.completeLocked()
	case *events.PairError:
		p.invalidateLocked("expired")
		p.completeLocked()
	case *events.LoggedOut:
		p.invalidateLocked("logged_out")
		p.state.Generation++
		if p.cancelAttempt != nil {
			p.cancelAttempt()
		}
	}
}

// The pinned SDK saves the device asynchronously after PrePairCallback. Keep
// that admission closed outside a live attempt, and wait for its terminal event
// before a handoff so a just-linked device is never replaced by NewDevice.
func (p *operatorPairing) beginCompletion(client pairingClient) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil || p.client != client || p.completing || p.paired() {
		return false
	}
	switch p.state.State {
	case "awaiting_qr", "code_issued", "passkey_submitted", "passkey_confirm":
	default:
		return false
	}
	p.completing = true
	p.completionDone = make(chan struct{})
	p.invalidateLocked("completing")
	return true
}

func (p *operatorPairing) completeLocked() {
	if p.completing {
		p.completing = false
		close(p.completionDone)
	}
}

func (p *operatorPairing) waitCompletion() bool {
	p.mu.Lock()
	var done <-chan struct{}
	if p.completing {
		done = p.completionDone
	}
	p.mu.Unlock()
	if done == nil {
		return p.ctx.Err() == nil
	}
	select {
	case <-done:
		return true
	case <-p.ctx.Done():
		return false
	}
}

func (p *operatorPairing) run() {
	defer close(p.done)
	first := true
	for {
		for attempt := 1; attempt <= p.state.Attempts; attempt++ {
			if p.ctx.Err() != nil {
				return
			}
			p.action.Lock()
			if !p.waitCompletion() {
				p.action.Unlock()
				return
			}
			if !first && p.paired() {
				p.action.Unlock()
				break
			}
			if !first {
				p.client.Disconnect()
				client, err := p.factory()
				if err != nil {
					p.b.Log.Errorf("Operator pairing client could not be initialized")
					p.mu.Lock()
					p.invalidateLocked("expired")
					p.mu.Unlock()
					p.action.Unlock()
					break
				}
				p.client = client
			}
			first = false
			ctx, cancel := context.WithCancel(p.ctx)
			p.mu.Lock()
			p.state.Generation++
			generation := p.state.Generation
			p.state.Attempt, p.codeCalls, p.sequence = attempt, 0, 0
			p.invalidateLocked("starting")
			p.attemptContext, p.cancelAttempt = ctx, cancel
			client := p.client
			p.mu.Unlock()
			p.action.Unlock()
			err := connectOrPair(ctx, client, p.paired(), pairingOptions{
				attempts: 1, attemptTimeout: p.opt.attemptTimeout, log: p.b.Log, out: p.out, connectionContext: p.b.ctx,
				beforeDial: func() error { return p.b.waitConnectionAllowedContext(ctx) },
				observe:    func(evt whatsmeow.QRChannelItem) { p.observe(generation, evt) },
				state:      func(state string) { p.b.setPairingState(state) },
			})
			cancel()
			if p.ctx.Err() != nil {
				return
			}
			if err == nil {
				break
			}
			p.action.Lock()
			client.Disconnect()
			completed := p.waitCompletion()
			p.action.Unlock()
			if !completed {
				return
			}
			p.mu.Lock()
			cancelled := generation != p.state.Generation
			if !cancelled {
				if errors.Is(err, errPairingOperator) {
					p.invalidateLocked("passkey_failed")
				} else {
					p.invalidateLocked("expired")
				}
			}
			p.mu.Unlock()
			if cancelled || errors.Is(err, errPairingOperator) {
				break
			}
			problem, _ := p.b.connectionSnapshot()
			if p.paired() {
				if !problem.restrictsAccount() {
					p.b.scheduleReconnect(p.reconnect)
				}
				break // Retain the paired device; only the gated consumer may redial.
			}
			if problem.restrictsAccount() {
				break
			}
			if attempt < p.state.Attempts {
				timer := time.NewTimer(p.opt.retryDelay)
				select {
				case <-timer.C:
				case <-p.ctx.Done():
					timer.Stop()
					return
				case <-p.kick:
					timer.Stop()
					attempt = 0
				}
			}
		}
		select {
		case <-p.ctx.Done():
			return
		case <-p.kick:
		}
	}
}

func (p *operatorPairing) start() { p.started.Store(true); go p.run() }

func (p *operatorPairing) snapshot() operatorPairingState {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state
	if state.QR != nil && !p.now().Before(state.QR.ExpiresAt) {
		state.QR = nil
	}
	if state.PairCode != nil && !p.now().Before(state.PairCode.ExpiresAt) {
		state.PairCode = nil
	}
	if state.StepExpiresAt != nil && !p.now().Before(*state.StepExpiresAt) {
		state.Passkey, state.ConfirmationCode = nil, ""
	}
	if p.paired() {
		state.State, state.QR, state.PairCode, state.Passkey, state.ConfirmationCode = "paired", nil, nil, nil, ""
		if p.connected() {
			state.State = "connected"
		}
	}
	return state
}

func operatorDecode(w http.ResponseWriter, r *http.Request, body any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		writeErrorCode(w, 400, "invalid_request", "Invalid operator request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeErrorCode(w, 400, "invalid_request", "Expected one JSON object")
		return false
	}
	return true
}

func (p *operatorPairing) actionContext(r *http.Request) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	p.mu.Lock()
	attempt := p.attemptContext
	p.mu.Unlock()
	if attempt == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(attempt, cancel)
	return ctx, func() { stop(); cancel() }
}

func (p *operatorPairing) code(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !operatorDecode(w, r, &body) {
		return
	}
	if len(body.Phone) < 7 || len(body.Phone) > 15 || body.Phone[0] == '0' || strings.Trim(body.Phone, "0123456789") != "" {
		writeErrorCode(w, 400, "invalid_request", "phone must be 7-15 international digits")
		return
	}
	if !p.action.TryLock() {
		writeErrorCode(w, 409, "pairing_busy", "Another pairing action is running")
		return
	}
	defer p.action.Unlock()
	p.mu.Lock()
	if p.paired() || (p.state.State != "awaiting_qr" && p.state.State != "code_issued") || p.state.QR == nil || !p.now().Before(p.state.QR.ExpiresAt) {
		p.mu.Unlock()
		writeErrorCode(w, 409, "pairing_unavailable", "An unpaired client with a current QR is required")
		return
	}
	if p.codeCalls >= 3 {
		p.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		writeErrorCode(w, 429, "rate_limited", "Pairing-code limit reached for this attempt")
		return
	}
	p.codeCalls++
	client, generation, expires := p.client, p.state.Generation, p.state.QR.ExpiresAt
	p.mu.Unlock()
	ctx, cancel := p.actionContext(r)
	defer cancel()
	code, err := client.PairPhone(ctx, body.Phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		writeErrorCode(w, 502, "pairing_failed", "WhatsApp refused the pairing code")
		return
	}
	p.mu.Lock()
	valid := generation == p.state.Generation && !p.paired() && p.now().Before(expires)
	if valid {
		p.state.State = "code_issued"
		p.state.PairCode = &operatorCodeExpiry{ExpiresAt: expires}
	}
	p.mu.Unlock()
	if !valid {
		writeErrorCode(w, 409, "pairing_expired", "Pairing attempt changed or expired")
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expires_at": expires, "generation": generation})
}

func (p *operatorPairing) restart(w http.ResponseWriter, _ *http.Request) {
	if !p.action.TryLock() {
		writeErrorCode(w, 409, "pairing_busy", "Another pairing action is running")
		return
	}
	defer p.action.Unlock()
	if p.paired() {
		writeErrorCode(w, 409, "already_paired", "Unlink on the phone before starting another pairing")
		return
	}
	problem, _ := p.b.connectionSnapshot()
	if problem != nil && problem.Kind == "temporarily_banned" && problem.ExpiresAt != nil && p.now().Before(*problem.ExpiresAt) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(problem.ExpiresAt.Sub(p.now()).Seconds()))))
		writeErrorCode(w, 409, "temporarily_banned", "Wait until the WhatsApp restriction expires")
		return
	}
	if problem != nil && problem.Kind == "client_outdated" {
		writeErrorCode(w, 409, "client_outdated", "Upgrade this bridge before retrying")
		return
	}
	p.mu.Lock()
	active := p.completing || (p.state.State != "expired" && p.state.State != "passkey_failed" && p.state.State != "logged_out" && !problem.restrictsAccount())
	p.mu.Unlock()
	if active {
		writeErrorCode(w, 409, "pairing_active", "Wait for the current attempt to finish before restarting")
		return
	}
	p.b.clearConnectionProblem()
	p.b.connectionMu.Lock()
	failed := p.b.problemPersistenceFailed
	p.b.connectionMu.Unlock()
	if failed {
		writeErrorCode(w, 503, "state_persistence_failed", "Saved connection restriction could not be cleared")
		return
	}
	p.mu.Lock()
	p.state.Generation++
	p.invalidateLocked("starting")
	if p.cancelAttempt != nil {
		p.cancelAttempt()
	}
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
	writeJSON(w, 202, map[string]string{"state": "starting"})
}

func validPasskeyResponse(response *types.WebAuthnResponse, request *types.WebAuthnPublicKey) bool {
	if request == nil || request.RelyingPartID != "whatsapp.com" || response.Type != "public-key" || len(response.RawID) == 0 || response.ID != base64.RawURLEncoding.EncodeToString(response.RawID) || len(response.Response.AuthenticatorData) < 37 || len(response.Response.Signature) == 0 {
		return false
	}
	var data struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if json.Unmarshal(response.Response.ClientDataJSON, &data) != nil || data.Type != "webauthn.get" || data.CrossOrigin || data.Challenge != base64.RawURLEncoding.EncodeToString(request.Challenge) {
		return false
	}
	origin, err := url.Parse(data.Origin)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Port() != "" || (origin.Hostname() != request.RelyingPartID && !strings.HasSuffix(origin.Hostname(), "."+request.RelyingPartID)) {
		return false
	}
	hash := sha256.Sum256([]byte(request.RelyingPartID))
	return subtle.ConstantTimeCompare(hash[:], response.Response.AuthenticatorData[:32]) == 1
}

func (p *operatorPairing) passkeyResponse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Generation uint64                 `json:"generation"`
		Assertion  types.WebAuthnResponse `json:"assertion"`
	}
	if !operatorDecode(w, r, &body) {
		return
	}
	if !p.action.TryLock() {
		writeErrorCode(w, 409, "pairing_busy", "Another pairing action is running")
		return
	}
	defer p.action.Unlock()
	p.mu.Lock()
	valid := !p.paired() && p.state.State == "passkey_required" && body.Generation == p.state.Generation && p.state.StepExpiresAt != nil && p.now().Before(*p.state.StepExpiresAt)
	client, request := p.client, p.state.Passkey
	p.mu.Unlock()
	if !valid {
		writeErrorCode(w, 409, "passkey_unavailable", "No matching active passkey challenge")
		return
	}
	if !validPasskeyResponse(&body.Assertion, request) {
		writeErrorCode(w, 400, "invalid_assertion", "Assertion does not match the WhatsApp challenge and relying party")
		return
	}
	ctx, cancel := p.actionContext(r)
	defer cancel()
	if err := client.SendPasskeyResponse(ctx, &body.Assertion); err != nil {
		writeErrorCode(w, 502, "passkey_failed", "WhatsApp refused the assertion")
		return
	}
	p.mu.Lock()
	if p.state.Generation == body.Generation && p.state.State == "passkey_required" {
		p.state.State, p.state.Passkey = "passkey_submitted", nil
	}
	p.mu.Unlock()
	writeJSON(w, 202, map[string]string{"state": "passkey_submitted"})
}

func (p *operatorPairing) passkeyConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Generation uint64 `json:"generation"`
		Code       string `json:"code"`
	}
	if !operatorDecode(w, r, &body) {
		return
	}
	if !p.action.TryLock() {
		writeErrorCode(w, 409, "pairing_busy", "Another pairing action is running")
		return
	}
	defer p.action.Unlock()
	p.mu.Lock()
	valid := !p.paired() && p.state.State == "passkey_confirm" && body.Generation == p.state.Generation && p.state.ConfirmationCode != "" && subtle.ConstantTimeCompare([]byte(body.Code), []byte(p.state.ConfirmationCode)) == 1 && p.state.StepExpiresAt != nil && p.now().Before(*p.state.StepExpiresAt)
	client := p.client
	p.mu.Unlock()
	if !valid {
		writeErrorCode(w, 409, "passkey_unavailable", "Confirm the matching code on the phone during this attempt")
		return
	}
	ctx, cancel := p.actionContext(r)
	defer cancel()
	if err := client.SendPasskeyConfirmation(ctx); err != nil {
		writeErrorCode(w, 502, "passkey_failed", "WhatsApp refused the confirmation")
		return
	}
	p.mu.Lock()
	if p.state.Generation == body.Generation && p.state.State == "passkey_confirm" {
		p.state.State, p.state.ConfirmationCode = "passkey_submitted", ""
	}
	p.mu.Unlock()
	writeJSON(w, 202, map[string]string{"state": "passkey_submitted"})
}

func (p *operatorPairing) routes() operatorRoutes {
	return operatorRoutes{health: p.b.handleHealth(), ready: p.b.handleReady(), pairing: func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, p.snapshot()) }, code: p.code, restart: p.restart, passkeyResponse: p.passkeyResponse, passkeyConfirm: p.passkeyConfirm}
}
