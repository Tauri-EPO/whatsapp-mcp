package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type fakeOperatorClient struct {
	items                                                  chan whatsmeow.QRChannelItem
	connects, disconnects, codes, responses, confirmations atomic.Int64
	codeStarted, codeRelease                               chan struct{}
	connectErr                                             error
	responseHook                                           func() error
}

func newFakeOperatorClient() *fakeOperatorClient {
	return &fakeOperatorClient{items: make(chan whatsmeow.QRChannelItem, 10)}
}

func TestOperatorPairCodeDoesNotOverwriteAsyncTransition(t *testing.T) {
	for _, transition := range []string{"passkey_required", "completing"} {
		t.Run(transition, func(t *testing.T) {
			f := newOperatorPairingFixture(t)
			f.client.codeStarted, f.client.codeRelease = make(chan struct{}), make(chan struct{})
			f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-QR", Timeout: time.Minute}
			state := f.stateHTTP(t, "awaiting_qr")
			type result struct {
				status int
				body   string
			}
			done := make(chan result, 1)
			go func() {
				status, body := f.request(t, "POST", "pairing/code", fakeOperatorToken, `{"phone":"5511999999999"}`)
				done <- result{status, body}
			}()
			select {
			case <-f.client.codeStarted:
			case <-time.After(time.Second):
				t.Fatal("SDK PairPhone was not called")
			}
			if transition == "passkey_required" {
				f.p.observe(state.Generation, whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Timeout: 60000}}})
			} else if !f.p.beginCompletion(f.client) {
				t.Fatal("completion was refused")
			}
			close(f.client.codeRelease)
			select {
			case result := <-done:
				if result.status != 409 || strings.Contains(result.body, "FAKE-PAIR-CODE") {
					t.Fatalf("stale code response: %+v", result)
				}
			case <-time.After(time.Second):
				t.Fatal("code response stalled")
			}
			current := f.p.snapshot()
			if current.State != transition || current.PairCode != nil {
				t.Fatalf("transition overwritten: %+v", current)
			}
			if transition == "completing" {
				f.p.connectionEvent(&events.PairError{})
			}
		})
	}
}
func (c *fakeOperatorClient) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return c.items, nil
}
func (c *fakeOperatorClient) ConnectContext(context.Context) error {
	c.connects.Add(1)
	return c.connectErr
}
func (c *fakeOperatorClient) Disconnect() { c.disconnects.Add(1) }
func (c *fakeOperatorClient) PairPhone(ctx context.Context, phone string, notify bool, clientType whatsmeow.PairClientType, name string) (string, error) {
	if phone != "5511999999999" || !notify || clientType != whatsmeow.PairClientChrome || name != "Chrome (Linux)" {
		panic("unexpected pairing-code parameters")
	}
	c.codes.Add(1)
	if c.codeStarted != nil {
		close(c.codeStarted)
		select {
		case <-c.codeRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "FAKE-PAIR-CODE", nil
}
func (c *fakeOperatorClient) SendPasskeyResponse(context.Context, *types.WebAuthnResponse) error {
	c.responses.Add(1)
	if c.responseHook != nil {
		return c.responseHook()
	}
	return nil
}
func (c *fakeOperatorClient) SendPasskeyConfirmation(context.Context) error {
	c.confirmations.Add(1)
	return nil
}

type operatorPairingFixture struct {
	p                 *operatorPairing
	b                 *Bridge
	server            *httptest.Server
	client            *fakeOperatorClient
	paired, connected atomic.Bool
	audit             bytes.Buffer
}

func newOperatorPairingFixture(t *testing.T) *operatorPairingFixture {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	f := &operatorPairingFixture{client: newFakeOperatorClient()}
	f.b = testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), newJSONLogger("pairing-test", "INFO", &f.audit))
	f.b.Connected = f.connected.Load
	f.p = newOperatorPairing(f.b.ctx, f.b, f.client, func() (operatorPairingClient, error) { return newFakeOperatorClient(), nil }, f.paired.Load, f.connected.Load, io.Discard, make(chan bool, 1))
	f.p.opt.attemptTimeout, f.p.opt.retryDelay = time.Second, time.Millisecond
	f.b.operatorPairing = f.p
	f.server = httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, f.p.routes(), f.b.Log))
	t.Cleanup(f.server.Close)
	f.p.start()
	return f
}
func (f *operatorPairingFixture) request(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+"/operator/v1/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}
func (f *operatorPairingFixture) wait(t *testing.T, predicate func(operatorPairingState) bool) operatorPairingState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := f.p.snapshot()
		if predicate(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pairing state did not reach expectation: %s", f.p.snapshot().State)
	return operatorPairingState{}
}
func (f *operatorPairingFixture) stateHTTP(t *testing.T, state string) operatorPairingState {
	t.Helper()
	f.wait(t, func(s operatorPairingState) bool { return s.State == state })
	status, body := f.request(t, "GET", "pairing", fakeOperatorToken, "")
	var snapshot operatorPairingState
	if status != 200 || json.Unmarshal([]byte(body), &snapshot) != nil || snapshot.State != state {
		t.Fatalf("pairing state HTTP=%d %s", status, body)
	}
	return snapshot
}

func TestOperatorPairingHTTPRotatesIssuesCodeAndInvalidatesOnPairing(t *testing.T) {
	f := newOperatorPairingFixture(t)
	for _, route := range []struct{ method, path, body string }{{"GET", "pairing", ""}, {"POST", "pairing/code", `{"phone":"5511999999999"}`}, {"POST", "pairing/restart", ""}} {
		if status, _ := f.request(t, route.method, route.path, "", route.body); status != 401 {
			t.Fatalf("unauthenticated pairing=%d", status)
		}
	}
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-FIRST-QR", Timeout: time.Minute}
	first := f.stateHTTP(t, "awaiting_qr")
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-SECOND-QR", Timeout: time.Minute}
	f.wait(t, func(s operatorPairingState) bool { return s.QR != nil && s.QR.Sequence == 2 })
	second := f.stateHTTP(t, "awaiting_qr")
	if first.QR.Payload == second.QR.Payload || !second.QR.ExpiresAt.After(time.Now()) {
		t.Fatal("rotated credential or actual expiry absent")
	}
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatal("restart cancelled an active QR attempt")
	}
	for index := 0; index < 4; index++ {
		status, body := f.request(t, "POST", "pairing/code", fakeOperatorToken, `{"phone":"5511999999999"}`)
		if index < 3 {
			if status != 200 || !strings.Contains(body, "FAKE-PAIR-CODE") {
				t.Fatalf("pairing code=%d %s", status, body)
			}
		} else if status != 429 {
			t.Fatalf("per-attempt limit=%d", status)
		}
	}
	if f.client.codes.Load() != 3 || f.client.connects.Load() != 1 {
		t.Fatal("HTTP code request started another connection or exceeded its limit")
	}
	if status, body := f.request(t, "GET", "health", fakeOperatorToken, ""); status != 200 || strings.Contains(body, "FAKE-") {
		t.Fatal("operator health exposed credentials")
	}
	f.paired.Store(true)
	f.client.items <- whatsmeow.QRChannelItem{Event: "success"}
	paired := f.stateHTTP(t, "paired")
	if paired.QR != nil || paired.PairCode != nil {
		t.Fatal("paired credentials were retained")
	}
	for _, path := range []string{"pairing/code", "pairing/restart"} {
		if status, _ := f.request(t, "POST", path, fakeOperatorToken, `{"phone":"5511999999999"}`); status != 409 {
			t.Fatalf("paired action=%d", status)
		}
	}
	f.connected.Store(true)
	f.p.connectionEvent(&events.Connected{})
	f.stateHTTP(t, "connected")
	f.b.cancel()
	<-f.p.done
	if strings.Contains(f.audit.String(), "FAKE-") || strings.Contains(f.audit.String(), "5511999999999") || strings.Contains(f.audit.String(), fakeOperatorToken) {
		t.Fatal("INFO pairing logs contain credentials")
	}
	dataMux := f.b.newRESTMux(8080, "fake-data-plane-token-0123456789abcdef")
	for _, path := range []string{"/api/health", "/metrics"} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil)
		req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
		out := httptest.NewRecorder()
		dataMux.ServeHTTP(out, req)
		if out.Code != 200 || strings.Contains(out.Body.String(), "FAKE-") {
			t.Fatal("data-plane status exposed pairing credentials")
		}
	}
}

func TestOperatorPairingHTTPExhaustionRestartAndCredentialExpiry(t *testing.T) {
	f := newOperatorPairingFixture(t)
	var factories atomic.Int64
	fresh := newFakeOperatorClient()
	f.p.factory = func() (operatorPairingClient, error) {
		if factories.Add(1) <= 2 {
			c := newFakeOperatorClient()
			c.items <- whatsmeow.QRChannelItem{Event: "timeout"}
			return c, nil
		}
		return fresh, nil
	}
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-EXPIRING-QR", Timeout: 20 * time.Millisecond}
	f.stateHTTP(t, "awaiting_qr")
	f.wait(t, func(s operatorPairingState) bool { return s.QR == nil })
	if status, _ := f.request(t, "POST", "pairing/code", fakeOperatorToken, `{"phone":"5511999999999"}`); status != 409 {
		t.Fatalf("expired QR action=%d", status)
	}
	f.client.items <- whatsmeow.QRChannelItem{Event: "timeout"}
	f.wait(t, func(s operatorPairingState) bool { return s.Attempt == 3 && s.State == "expired" })
	old := f.stateHTTP(t, "expired")
	if old.QR != nil || old.PairCode != nil {
		t.Fatal("exhausted attempt retained credentials")
	}
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 202 {
		t.Fatalf("restart=%d", status)
	}
	fresh.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-RESTART-QR", Timeout: time.Minute}
	current := f.stateHTTP(t, "awaiting_qr")
	if current.Generation <= old.Generation || current.Attempt != 1 || f.client.disconnects.Load() == 0 || fresh.connects.Load() != 1 {
		t.Fatal("restart reused old credentials/client or failed to reset attempts")
	}
}

func TestOperatorPairingHTTPRefusesBanBypassAndStatePersistenceFailure(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.b.recordConnectionProblem(402, 101, time.Hour)
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatal("restart bypassed temporary ban")
	}
	f.b.clearConnectionProblem()
	f.b.recordConnectionProblem(405, 0, 0)
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatal("restart bypassed outdated client")
	}
	f.b.clearConnectionProblem()
	statePath := filepath.Join(f.b.StoreRoot.Name(), connectionProblemFile)
	if err := os.Mkdir(statePath, storeDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "fake-child"), []byte("fake-state"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	f.b.recordConnectionProblem(403, 0, 0)
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 503 {
		t.Fatal("restart bypassed a saved-state deletion failure")
	}
}

func TestOperatorRestrictionClearingWakesAlreadyBlockedReconnect(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.recordConnectionProblem(403, 0, 0)
	blocked := make(chan struct{})
	var once atomic.Bool
	b.connectionBlocked = func() {
		if once.CompareAndSwap(false, true) {
			close(blocked)
		}
	}
	ctx, cancel := context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.waitConnectionAllowedContext(ctx) }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("reconnect gate did not park on the restriction")
	}
	b.clearConnectionProblem()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("restriction clearing left the gate blocked: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("restriction clearing did not wake the gate")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("gate was released only by its timeout")
	}
}

func TestOperatorPairedStartupFailureReachesGatedReconnectWithoutReplacingDevice(t *testing.T) {
	client := newFakeOperatorClient()
	client.connectErr = errors.New("fake temporary startup dial failure")
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511999999999", types.DefaultUserServer)), newTestMessageStore(t), testLogger())
	var connected atomic.Bool
	var retries, replacements atomic.Int64
	b.Connected = connected.Load
	b.Disconnect = client.Disconnect
	b.Connect = func() error { retries.Add(1); connected.Store(true); return nil }
	b.ReconnectInitialBackoff = time.Millisecond
	b.ReconnectMaxBackoff = time.Millisecond
	reconnect := make(chan bool, 1)
	p := newOperatorPairing(b.ctx, b, client, func() (operatorPairingClient, error) {
		replacements.Add(1)
		return nil, errors.New("paired device must be retained")
	}, b.isPaired, b.Connected, io.Discard, reconnect)
	b.operatorPairing = p
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
	t.Cleanup(server.Close)
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); b.reconnectLoop(reconnect) }()
	t.Cleanup(func() { b.cancel(); <-loopDone })
	p.start()
	deadline := time.Now().Add(2 * time.Second)
	for !connected.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !connected.Load() || retries.Load() != 1 || replacements.Load() != 0 || client.connects.Load() != 1 {
		t.Fatal("paired startup failure did not recover through the gated reconnect consumer")
	}
	req, _ := http.NewRequest("GET", server.URL+"/operator/v1/ready", nil)
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("paired startup recovery readiness=%d", response.StatusCode)
	}
}

func TestOperatorNormalUnlinkRetainsExitThree(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.operatorPairing = &operatorPairing{}
	exit := 0
	b.Exit = func(_ string, code int) { exit = code }
	b.handleEvent(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	if exit != exitCodeLoggedOut {
		t.Fatalf("normal unlink with operator enabled exit=%d, want 3", exit)
	}
}

func TestOperatorFinalDeviceSaveBlocksReplacementUntilTerminalSDKEvent(t *testing.T) {
	f := newOperatorPairingFixture(t)
	var replacements atomic.Int64
	f.p.factory = func() (operatorPairingClient, error) { replacements.Add(1); return newFakeOperatorClient(), nil }
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-COMMITTING-QR", Timeout: time.Minute}
	state := f.stateHTTP(t, "awaiting_qr")
	if !f.p.beginCompletion(f.client) {
		t.Fatal("active SDK pairing was not admitted")
	}
	f.p.observe(state.Generation, whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-LATE-QR", Timeout: time.Minute})
	state = f.stateHTTP(t, "completing")
	if state.QR != nil {
		t.Fatal("a completing device save exposed another QR")
	}
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatal("restart interrupted the SDK's final device save")
	}
	f.wait(t, func(s operatorPairingState) bool { return f.client.disconnects.Load() > 0 })
	if replacements.Load() != 0 {
		t.Fatal("attempt timeout replaced a device before its save completed")
	}
	f.paired.Store(true)
	f.p.connectionEvent(&events.PairSuccess{})
	f.stateHTTP(t, "paired")
	f.b.cancel()
	<-f.p.done
	if replacements.Load() != 0 {
		t.Fatal("a just-linked device was replaced after its terminal event")
	}
}

func fakeOperatorAssertion() types.WebAuthnResponse {
	hash := sha256.Sum256([]byte("whatsapp.com"))
	auth := append(hash[:], []byte{1, 0, 0, 0, 1}...)
	return types.WebAuthnResponse{ID: base64.RawURLEncoding.EncodeToString([]byte("fake-id")), RawID: []byte("fake-id"), Type: "public-key", Response: types.WebAuthnResponseData{ClientDataJSON: []byte(`{"type":"webauthn.get","challenge":"ZmFrZS1jaGFsbGVuZ2U","origin":"https://web.whatsapp.com"}`), AuthenticatorData: auth, Signature: []byte("fake-signature")}}
}

func TestOperatorPairingHTTPPasskeyChallengeAndManualConfirmation(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Challenge: []byte("fake-challenge"), RelyingPartID: "whatsapp.com", Timeout: 60000}}}
	state := f.stateHTTP(t, "passkey_required")
	assertion := fakeOperatorAssertion()
	encoded, _ := json.Marshal(map[string]any{"generation": state.Generation + 1, "assertion": assertion})
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 409 {
		t.Fatal("stale assertion accepted")
	}
	invalid := assertion
	invalid.Response.ClientDataJSON = []byte(`{"type":"webauthn.get","challenge":"d3Jvbmc","origin":"https://evil.example.test"}`)
	encoded, _ = json.Marshal(map[string]any{"generation": state.Generation, "assertion": invalid})
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 400 || f.client.responses.Load() != 0 {
		t.Fatal("mismatched assertion reached WhatsApp")
	}
	encoded, _ = json.Marshal(map[string]any{"generation": state.Generation, "assertion": assertion})
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 202 {
		t.Fatalf("assertion=%d", status)
	}
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 409 {
		t.Fatal("assertion replay reached WhatsApp")
	}
	f.client.items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyResponse, PasskeyConfirmation: &events.PairPasskeyConfirmation{Code: "FAKE-CONFIRMATION"}}
	f.stateHTTP(t, "passkey_confirm")
	encoded, _ = json.Marshal(map[string]any{"generation": state.Generation, "code": "WRONG"})
	if status, _ := f.request(t, "POST", "pairing/passkey/confirm", fakeOperatorToken, string(encoded)); status != 409 {
		t.Fatal("uncompared code confirmed")
	}
	encoded, _ = json.Marshal(map[string]any{"generation": state.Generation, "code": "FAKE-CONFIRMATION"})
	if status, _ := f.request(t, "POST", "pairing/passkey/confirm", fakeOperatorToken, string(encoded)); status != 202 {
		t.Fatalf("manual confirmation=%d", status)
	}
	if status, _ := f.request(t, "POST", "pairing/passkey/confirm", fakeOperatorToken, string(encoded)); status != 409 {
		t.Fatal("confirmation replay reached WhatsApp")
	}
	if f.client.responses.Load() != 1 || f.client.confirmations.Load() != 1 {
		t.Fatal("passkey action sent more than once")
	}
	f.b.cancel()
	<-f.p.done
	if strings.Contains(f.audit.String(), "fake-challenge") || strings.Contains(f.audit.String(), "FAKE-CONFIRMATION") || strings.Contains(f.audit.String(), "fake-signature") {
		t.Fatal("passkey credentials escaped to INFO logs")
	}
}

func TestOperatorPairingHTTPStrictBodyAndConcurrentRestart(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.codeStarted, f.client.codeRelease = make(chan struct{}), make(chan struct{})
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-QR", Timeout: time.Minute}
	f.stateHTTP(t, "awaiting_qr")
	for _, body := range []string{`{"phone":"5511999999999","extra":1}`, `{"phone":"5511999999999"} {}`, `{"phone":"5511999999999","phone":"0123456"}`, strings.Repeat("x", 65537)} {
		if status, _ := f.request(t, "POST", "pairing/code", fakeOperatorToken, body); status != 400 {
			t.Fatalf("invalid operator body=%d", status)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.request(t, "POST", "pairing/code", fakeOperatorToken, `{"phone":"5511999999999"}`)
	}()
	select {
	case <-f.client.codeStarted:
	case <-time.After(time.Second):
		t.Fatal("code request did not reach SDK")
	}
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatal("restart raced an in-flight SDK action")
	}
	close(f.client.codeRelease)
	<-done
}

func TestOperatorPasskeyFailureReasonSurvivesActorAndClearsOnRestart(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Timeout: 60000}}}
	f.stateHTTP(t, "passkey_required")
	f.client.items <- whatsmeow.QRChannelItem{Event: "error", Error: errors.New("fake verification rejected")}
	state := f.stateHTTP(t, "passkey_failed")
	if !strings.Contains(state.FailureReason, "fake verification rejected") || state.QR != nil || state.Passkey != nil {
		t.Fatal("private failure reason or credential cleanup missing")
	}
	f.p.mu.Lock()
	f.p.invalidateLocked("starting")
	reason := f.p.state.FailureReason
	f.p.mu.Unlock()
	if reason != "" {
		t.Fatal("new attempt retained old failure")
	}
}

func TestOperatorRestartPreservesBanArrivingAfterValidation(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.b.cancel()
	<-f.p.done
	f.b.problemNow = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	f.b.recordConnectionProblem(402, 101, time.Hour)
	f.b.problemNow = nil
	f.p.now = func() time.Time {
		f.b.recordConnectionProblem(402, 101, time.Hour)
		return time.Now()
	}
	if status, _ := f.request(t, "POST", "pairing/restart", fakeOperatorToken, ""); status != 409 {
		t.Fatalf("restart discarded a newer ban: status=%d", status)
	}
	problem, _ := f.b.connectionSnapshot()
	if problem == nil || problem.ExpiresAt == nil || !problem.ExpiresAt.After(time.Now()) {
		t.Fatal("new account restriction was lost")
	}
}

func TestOperatorPasskeySubmissionAdmitsSynchronousSDKCompletion(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Challenge: []byte("fake-challenge"), RelyingPartID: "whatsapp.com", Timeout: 60000}}}
	state := f.stateHTTP(t, "passkey_required")
	f.client.responseHook = func() error {
		if !f.p.beginCompletion(f.client) {
			return errors.New("SDK completion rejected during passkey submission")
		}
		f.paired.Store(true)
		f.p.connectionEvent(&events.PairSuccess{})
		return nil
	}
	encoded, _ := json.Marshal(map[string]any{"generation": state.Generation, "assertion": fakeOperatorAssertion()})
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 202 {
		t.Fatalf("SDK completion during submission: status=%d", status)
	}
	if state := f.p.snapshot(); state.State != "paired" || state.Passkey != nil {
		t.Fatal("submission overwrote completed pairing")
	}
}

func TestOperatorPasskeySubmissionFailureRestoresOnlyUnchangedChallenge(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.items <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Challenge: []byte("fake-challenge"), RelyingPartID: "whatsapp.com", Timeout: 60000}}}
	state := f.stateHTTP(t, "passkey_required")
	f.client.responseHook = func() error { return errors.New("fake rejected submission") }
	encoded, _ := json.Marshal(map[string]any{"generation": state.Generation, "assertion": fakeOperatorAssertion()})
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 502 {
		t.Fatalf("failed submission: status=%d", status)
	}
	if state := f.p.snapshot(); state.State != "passkey_required" || state.Passkey == nil {
		t.Fatal("unchanged challenge cannot be retried")
	}
	f.client.responseHook = func() error {
		f.p.observe(state.Generation, whatsmeow.QRChannelItem{Event: "error", Error: errors.New("fake asynchronous refusal")})
		return errors.New("fake rejected submission")
	}
	if status, _ := f.request(t, "POST", "pairing/passkey/response", fakeOperatorToken, string(encoded)); status != 502 {
		t.Fatalf("asynchronously failed submission: status=%d", status)
	}
	if state := f.p.snapshot(); state.State != "passkey_failed" || state.Passkey != nil {
		t.Fatal("rollback revived an asynchronously refused challenge")
	}
}
