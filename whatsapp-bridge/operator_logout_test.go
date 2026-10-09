package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestOperatorLogoutExitProcessHelper(t *testing.T) {
	if os.Getenv("WAMCP_TEST_OPERATOR_EXIT") == "" {
		return
	}
	b := newSettingsBridge(t)
	b.Log = waLog.Stdout("Proof", "INFO", false)
	b.logoutClient = func(context.Context) error { return nil }
	b.wipeSession = func(context.Context) error { return nil }
	b.Exit = func(_ string, code int) { os.Exit(code) }
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
	fmt.Println(server.URL)
	<-b.ctx.Done()
}

func TestOperatorLogoutExitFlushesCompleteHTTPResponseThenExitsThree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOperatorLogoutExitProcessHelper$") //nolint:gosec // Runs this test binary with a fixed helper selector; no user input.
	cmd.Env = append(os.Environ(), "WAMCP_TEST_OPERATOR_EXIT=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	var url string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "http://127.0.0.1:") {
			url = scanner.Text()
			break
		}
	}
	if url == "" {
		t.Fatal("helper listener did not start")
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/operator/v1/logout", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), `"local_session_wiped":true`) {
		t.Fatalf("exit truncated response: status=%d err=%v body=%s", response.StatusCode, err, body)
	}
	var audit strings.Builder
	for scanner.Scan() {
		audit.WriteString(scanner.Text() + "\n")
	}
	if strings.Count(audit.String(), "Operator POST /operator/v1/logout peer=") != 1 || !strings.Contains(audit.String(), "outcome=200") {
		t.Fatal("process exited before one successful operator audit line")
	}
	if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("observed exit=%v err=%v", cmd.ProcessState, err)
	}
}

func TestNewOperatorRoutesDenyDataCredentialsHostAndOrigin(t *testing.T) {
	b := newSettingsBridge(t)
	var calls atomic.Int64
	routes := operatorRoutes{settings: func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}, logout: func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]bool{"ok": true})
	}}
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, routes, b.Log))
	defer server.Close()
	for _, route := range []struct{ method, path string }{{"GET", "settings"}, {"PATCH", "settings"}, {"POST", "logout"}} {
		for _, tc := range []struct {
			token, host, origin string
			status              int
		}{
			{"", "", "", 401}, {readOnlyTestToken, "", "", 401},
			{fakeOperatorToken, "evil.example.test", "", 403}, {fakeOperatorToken, "", "https://evil.example.test", 403},
		} {
			req, _ := http.NewRequest(route.method, server.URL+"/operator/v1/"+route.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("%s %s=%d", route.method, route.path, response.StatusCode)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("deny path reached a mutation")
	}
	data := b.newRESTMux(8080, readOnlyTestToken)
	for _, path := range []string{"/operator/v1/settings", "/operator/v1/logout", "/api/logout"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080"+path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		w := httptest.NewRecorder()
		data.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("operator route leaked onto REST: %d", w.Code)
		}
	}
}

func TestOperatorLogoutExitIdleAndServerFailure(t *testing.T) {
	for _, after := range []string{"exit", "idle", ""} {
		for _, offline := range []bool{false, true} {
			t.Run(after+"/"+map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
				b := newSettingsBridge(t)
				b.ReadOnly = readOnlyPolicy{enabled: true}
				client := newFakeOperatorClient()
				var paired atomic.Bool
				paired.Store(true)
				var logoutCalls, wipeCalls, exitCode atomic.Int64
				b.logoutClient = func(ctx context.Context) error {
					logoutCalls.Add(1)
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
						t.Error("unlink has no bounded deadline")
					}
					if offline {
						return errors.New("fake offline")
					}
					return nil
				}
				b.wipeSession = func(ctx context.Context) error {
					wipeCalls.Add(1)
					if ctx.Err() != nil {
						t.Error("wipe reused canceled context")
					}
					paired.Store(false)
					return nil
				}
				b.Exit = func(_ string, code int) { exitCode.Store(int64(code)) }
				p := newOperatorPairing(b.ctx, b, client, func() (operatorPairingClient, error) { return newFakeOperatorClient(), nil }, paired.Load, b.Connected, io.Discard, make(chan bool, 1))
				b.operatorPairing = p
				server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
				defer server.Close()
				b.ForwardConnection = true
				events := make(chan string, 1)
				webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					events <- string(body)
					w.WriteHeader(200)
				}))
				defer webhook.Close()
				b.Webhook = &webhookSender{client: webhook.Client(), url: webhook.URL, enabled: true}
				req, _ := http.NewRequest("POST", server.URL+"/operator/v1/logout", strings.NewReader(`{"after":"`+after+`"}`))
				req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != 200 || !strings.Contains(string(body), `"local_session_wiped":true`) || !strings.Contains(string(body), `"server_unlinked":`+map[bool]string{false: "true", true: "false"}[offline]) {
					t.Fatalf("logout=%d %s", response.StatusCode, body)
				}
				if logoutCalls.Load() != 1 || wipeCalls.Load() != 1 {
					t.Fatal("logout/wipe not called once")
				}
				select {
				case event := <-events:
					if !strings.Contains(event, `"reason":"operator"`) || !strings.Contains(event, `"state":"logged_out"`) {
						t.Fatal(event)
					}
				case <-time.After(time.Second):
					t.Fatal("connection event missing")
				}
				if after == "idle" {
					if exitCode.Load() != 0 || p.snapshot().State != "logged_out_by_operator" || client.connects.Load() != 0 || client.codes.Load() != 0 {
						t.Fatal("idle started pairing or exited")
					}
					if err := b.restoreOperatorIdle(); err != nil {
						t.Fatal(err)
					}
					p.start() // Even a late startup may not run the QR path.
				} else {
					deadline := time.After(time.Second)
					for exitCode.Load() == 0 {
						select {
						case <-deadline:
							t.Fatal("exit was not observed")
						default:
							time.Sleep(time.Millisecond)
						}
					}
					if exitCode.Load() != 3 {
						t.Fatal("wrong logout exit code")
					}
				}
				w := httptest.NewRecorder()
				p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{}`)))
				if w.Code != 409 || logoutCalls.Load() != 1 {
					t.Fatal("repeated logout was not 409")
				}
			})
		}
	}
}

func TestOperatorLogoutOfflineWipesRealSessionAndIdleSurvivesRestart(t *testing.T) {
	b := newSettingsBridge(t)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, messagesPoolConns)
	b.sessionDB = db
	container := sqlstore.NewWithDB(db, "sqlite", testLogger())
	defer func() { _ = container.Close() }()
	if err := container.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	device := container.NewDevice()
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := [][]byte{append([]byte(nil), device.NoiseKey.Priv[:]...), append([]byte(nil), device.IdentityKey.Priv[:]...)}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := device.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	var before []byte
	for _, path := range []string{whatsmeowDBPath(), whatsmeowDBPath() + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, data...)
	}
	for _, key := range keys {
		if !bytes.Contains(before, key) {
			t.Fatal("fixture did not write private key bytes to SQLite")
		}
	}
	client := newRuntimeClient(device, testLogger())
	b.Client = client
	p := newOperatorPairing(b.ctx, b, client, func() (operatorPairingClient, error) { return newFakeOperatorClient(), nil }, b.isPaired, b.Connected, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	w := httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"server_unlinked":false`) {
		t.Fatalf("offline=%d %s", w.Code, w.Body.String())
	}
	devices, err := container.GetAllDevices(context.Background())
	if err != nil || len(devices) != 0 || device.ID != nil {
		t.Fatal("real SDK session survived wipe")
	}
	for _, path := range []string{whatsmeowDBPath(), whatsmeowDBPath() + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, key := range keys {
			if bytes.Contains(data, key) {
				t.Fatal("private key bytes survived offline local cleanup")
			}
		}
	}
	fresh := testBridge(t, newTestClient(&mockLIDStore{}), b.Store, testLogger())
	fake := newFakeOperatorClient()
	fresh.operatorPairing = newOperatorPairing(fresh.ctx, fresh, fake, func() (operatorPairingClient, error) { return newFakeOperatorClient(), nil }, func() bool { return false }, fresh.Connected, io.Discard, make(chan bool, 1))
	if err := fresh.restoreOperatorIdle(); err != nil || fresh.operatorPairing.snapshot().State != "logged_out_by_operator" {
		t.Fatal("durable idle not restored")
	}
	pair := fresh.operatorPairing
	pair.start()
	// The startup controller can briefly own action while noticing durable idle.
	// Retry only its documented busy response, never a semantic restart refusal.
	restartDeadline := time.Now().Add(time.Second)
	for {
		w = httptest.NewRecorder()
		pair.restart(w, httptest.NewRequest("POST", "/operator/v1/pairing/restart", nil))
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"pairing_busy"`) || time.Now().After(restartDeadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if w.Code != 202 {
		t.Fatalf("restart=%d %s", w.Code, w.Body.String())
	}
	fake.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-QR", Timeout: time.Minute}
	deadline := time.Now().Add(time.Second)
	for pair.snapshot().State != "awaiting_qr" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pair.snapshot().State != "awaiting_qr" {
		t.Fatal("explicit restart did not start QR")
	}
}

func TestOperatorLogoutLocalFailureAndCanceledCaller(t *testing.T) {
	for _, failWipe := range []bool{false, true} {
		b := newSettingsBridge(t)
		b.logoutClient = func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Error("caller canceled unlink cleanup")
			}
			return errors.New("fake offline")
		}
		var wipes int
		b.wipeSession = func(ctx context.Context) error {
			wipes++
			if ctx.Err() != nil {
				t.Error("wipe canceled")
			}
			if failWipe {
				return errors.New("fake storage failure")
			}
			return nil
		}
		b.Exit = func(string, int) { t.Fatal("idle logout exited") }
		p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Caller cancellation must not cancel either persistence or local cleanup.
		b.Exit = func(string, int) {}
		w := httptest.NewRecorder()
		p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{}`)).WithContext(ctx))
		want := 200
		if failWipe {
			restart := httptest.NewRecorder()
			p.restart(restart, httptest.NewRequest("POST", "/operator/v1/pairing/restart", nil))
			if restart.Code != 409 || !strings.Contains(restart.Body.String(), "local_session_not_wiped") {
				t.Fatal("restart admitted before local wipe succeeded")
			}
			want = 500
		}
		if w.Code != want || wipes != 1 || (failWipe && !strings.Contains(w.Body.String(), `"local_session_wiped":false`)) {
			t.Fatalf("cleanup=%d %s", w.Code, w.Body.String())
		}
		if failWipe {
			var saved bool
			if err := b.Store.db.QueryRow("SELECT logged_out FROM operator_state WHERE id=1").Scan(&saved); err != nil || !saved {
				t.Fatal("failed cleanup was not durably parked")
			}
			failWipe = false
			w = httptest.NewRecorder()
			p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
			if w.Code != 200 || wipes != 2 {
				t.Fatalf("local cleanup could not be retried: %d %s", w.Code, w.Body.String())
			}
		}
	}
}

type logoutBlockingLIDs struct {
	mockLIDStore
	entered, release chan struct{}
}

func (l *logoutBlockingLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	close(l.entered)
	<-l.release
	return types.EmptyJID, nil
}

func TestOperatorLogoutDrainsRealRESTIdentityAndRetiredCallbacks(t *testing.T) {
	b := newSettingsBridge(t)
	lids := &logoutBlockingLIDs{entered: make(chan struct{}), release: make(chan struct{})}
	client := newTestClientWithSelf(&mockLIDStore{}, types.NewJID("5511999999999", types.DefaultUserServer))
	client.Store.LIDs = lids
	b.Client = client
	b.installClient(client, true, make(chan bool, 1))
	var released atomic.Bool
	b.logoutClient = func(context.Context) error { return errors.New("fake offline") }
	b.wipeSession = func(context.Context) error {
		if !released.Load() {
			t.Error("local wipe overlapped admitted REST identity read")
		}
		client.Store.ID = nil
		return nil
	}
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, b.isPaired, b.Connected, io.Discard, make(chan bool, 1))
	data := httptest.NewServer(b.runtimeRESTHandler(8080, readOnlyTestToken))
	defer data.Close()
	readDone := make(chan int, 1)
	go func() {
		r, _ := http.NewRequest("GET", data.URL+"/api/me", nil)
		r.Host = "127.0.0.1:8080"
		r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		resp, err := data.Client().Do(r)
		if err != nil {
			readDone <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		readDone <- resp.StatusCode
	}()
	select {
	case <-lids.entered:
	case <-time.After(time.Second):
		t.Fatal("REST did not reach identity lookup")
	}
	logoutDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
		logoutDone <- w.Code
	}()
	deadline := time.Now().Add(time.Second)
	for !b.operatorLogout.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !b.operatorLogout.Load() {
		t.Fatal("logout never retired the client")
	}
	eventDone := make(chan struct{})
	go func() { b.handleClientEvent(client, &events.LoggedOut{}, make(chan bool, 1)); close(eventDone) }()
	select {
	case <-eventDone:
	case <-time.After(time.Second):
		t.Fatal("retired SDK callback waited for logout gate")
	}
	released.Store(true)
	close(lids.release)
	if status := <-readDone; status != 200 {
		t.Fatalf("admitted identity read=%d", status)
	}
	select {
	case status := <-logoutDone:
		if status != 200 {
			t.Fatalf("logout=%d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not drain")
	}
}

func TestOperatorIdleWithoutListenerStillBlocksDial(t *testing.T) {
	b := newSettingsBridge(t)
	audit := &recordingLogger{}
	b.Log = audit
	if _, err := b.Store.db.Exec("INSERT INTO operator_state(id,logged_out) VALUES (1,1)"); err != nil {
		t.Fatal(err)
	}
	if b.operatorPairing != nil {
		t.Fatal("test must exercise listener disabled")
	}
	if err := b.restoreOperatorIdle(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.waitConnectionAllowedContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("durable logout allowed dial: %v", err)
	}
	if !b.operatorLogout.Load() || b.pairingState != "logged_out_by_operator" {
		t.Fatal("idle state lost without listener")
	}
	if strings.Count(audit.String(), "[WARN]") != 1 || !strings.Contains(audit.String(), "WHATSAPP_OPERATOR_BIND") || !strings.Contains(audit.String(), "/operator/v1/pairing/restart") {
		t.Fatal("disabled listener did not log the idle recovery instruction once")
	}
}

func TestOperatorLogoutRejectsInvalidAndUnpairedBeforeSDKAndReportsRemoteSuccess(t *testing.T) {
	b := newSettingsBridge(t)
	var calls int
	b.logoutClient = func(context.Context) error {
		calls++
		return errors.New("error deleting data from store: fake storage failure")
	}
	b.wipeSession = func(context.Context) error { return nil }
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
	for _, body := range []string{`{"after":"other"}`, `{"unknown":true}`, `{} {}`} {
		w := httptest.NewRecorder()
		p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(body)))
		if w.Code != 400 || calls != 0 {
			t.Fatalf("invalid body reached SDK: %d", w.Code)
		}
	}
	p.paired = func() bool { return false }
	w := httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{}`)))
	if w.Code != 409 || calls != 0 {
		t.Fatal("unpaired logout reached SDK")
	}
	p.paired = func() bool { return true }
	w = httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"server_unlinked":true`) || !strings.Contains(w.Body.String(), `"local_session_wiped":true`) {
		t.Fatalf("successful remote unlink followed by local retry=%d %s", w.Code, w.Body.String())
	}
}

func TestOperatorLogoutReassertsIdleAfterAdmittedConnectionCallback(t *testing.T) {
	b := newSettingsBridge(t)
	b.logoutClient = func(context.Context) error { return errors.New("fake offline") }
	b.wipeSession = func(context.Context) error { return nil }
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	// Pause an admitted callback at the same read gate used by event dispatch.
	b.clientGate.RLock()
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
		done <- w.Code
	}()
	deadline := time.Now().Add(time.Second)
	parked := func() bool {
		_, state := b.connectionSnapshot()
		return b.operatorLogout.Load() && state == "logged_out_by_operator" && p.snapshot().State == state
	}
	for !parked() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !parked() {
		b.clientGate.RUnlock()
		t.Fatal("logout never retired client")
	}
	b.handleEvent(&events.Connected{}, make(chan bool, 1))
	p.connectionEvent(&events.Connected{})
	b.clientGate.RUnlock()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("logout=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not drain callback")
	}
	b.setPairingState("") // A cancelled old QR completion is also ignored.
	_, state := b.connectionSnapshot()
	if state != "logged_out_by_operator" || p.snapshot().State != state {
		t.Fatal("late completion reopened operator idle")
	}
}
