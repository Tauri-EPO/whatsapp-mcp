package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func revokedSessionFixture(t *testing.T) (*Bridge, *sqlstore.Container) {
	t.Helper()
	b := newSettingsBridge(t)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	container := sqlstore.NewWithDB(db, "sqlite", testLogger())
	t.Cleanup(func() { _ = container.Close() })
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
	b.Client = newRuntimeClient(device, testLogger())
	b.Client.SetWebsocketHTTPClient(&http.Client{Transport: mcpTestTransport(func(*http.Request) (*http.Response, error) {
		t.Error("revoked client reached the network transport")
		return nil, errors.New("fake network refusal")
	})})
	b.bindRuntimeClient()
	b.installClient(b.Client, true, make(chan bool, 1))
	return b, container
}

func unlinkHealth(t *testing.T, b *Bridge) {
	t.Helper()
	server := httptest.NewServer(b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef"))
	defer server.Close()
	for _, path := range []string{"health", "ready"} {
		req, _ := http.NewRequest("GET", server.URL+"/api/"+path, nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(response.Body).Decode(&body)
		_ = response.Body.Close()
		want := 200
		if path == "ready" {
			want = 503
		}
		if err != nil || response.StatusCode != want || body["paired"] != false || body["connected"] != false || body["status"] != "pairing_required" {
			t.Fatalf("%s status=%d body=%v err=%v", path, response.StatusCode, body, err)
		}
	}
}

func TestUnlinkFencesNextStartup(t *testing.T) {
	for _, onConnect := range []bool{false, true} {
		name := "device_removed"
		if onConnect {
			name = "connect_failure_401"
		}
		t.Run(name, func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			log := &recordingLogger{}
			b.Log = log
			b.Exit = func(string, int) { t.Error("unlink exited") }
			old := b.currentClient()
			reconnect := make(chan bool, 1)
			event := &events.LoggedOut{OnConnect: onConnect, Reason: events.ConnectFailureLoggedOut}
			b.handleClientEvent(old, event, reconnect)
			unlinkHealth(t, b)
			saved, err := readConnectionProblem(b.StoreRoot)
			if err != nil || saved == nil || saved.Kind != "unlinked" {
				t.Fatalf("unlink fence=%+v err=%v", saved, err)
			}
			// Simulate a process stopping before the asynchronous SDK delete runs.
			devices, err := container.GetAllDevices(context.Background())
			if err != nil || len(devices) != 1 {
				t.Fatalf("delayed SDK fixture devices=%d err=%v", len(devices), err)
			}
			b.handleClientEvent(old, event, reconnect)
			b.handleClientEvent(old, &events.Connected{}, reconnect)
			b.handleClientEvent(old, &events.Disconnected{}, reconnect)
			old.Log.Warnf("Got %d/%s connect failure, assuming automatic reconnect will handle it", 503, "service unavailable")
			after, _ := b.connectionSnapshot()
			if after.Since != saved.Since || len(reconnect) != 0 || strings.Count(log.String(), "WhatsApp connection problem: unlinked") != 1 {
				t.Fatal("retired callbacks duplicated or erased unlink")
			}
			if err := b.Connect(); err == nil {
				t.Fatal("revoked client was admitted to Connect")
			}
			metrics := b.renderMetrics()
			if strings.Count(metrics, `whatsapp_bridge_connection_problem{kind="unlinked"} 1`) != 1 {
				t.Fatalf("unlink gauge=%s", metrics)
			}
			// A new startup reads the actual disk fence and the actual SQLite store.
			reopenedDB, err := openSessionDB()
			if err != nil {
				t.Fatal(err)
			}
			restarted := sqlstore.NewWithDB(reopenedDB, "sqlite", testLogger())
			defer func() { _ = restarted.Close() }()
			device, _, err := startupDevice(context.Background(), restarted, b.StoreRoot, saved)
			if err != nil || device.ID != nil {
				t.Fatalf("startup reused revoked session: device=%+v err=%v", device, err)
			}
			devices, err = container.GetAllDevices(context.Background())
			if err != nil || len(devices) != 0 {
				t.Fatalf("revoked rows survive restart: count=%d err=%v", len(devices), err)
			}
			b.installClient(newRuntimeClient(device, testLogger()), false, reconnect)
			unlinkHealth(t, b)
			fake := newFakeOperatorClient()
			fake.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-NEW-QR", Timeout: time.Minute}
			fake.items <- whatsmeow.QRChannelItem{Event: "success"}
			var qr bool
			err = connectOrPair(b.ctx, fake, b.isPaired(), pairingOptions{attempts: 1, attemptTimeout: time.Second, out: io.Discard, log: b.Log, observe: func(item whatsmeow.QRChannelItem) {
				if item.Event == "code" {
					qr = true
				}
			}})
			if err != nil || !qr || fake.connects.Load() != 1 {
				t.Fatalf("fresh pairing qr=%t connects=%d err=%v", qr, fake.connects.Load(), err)
			}
		})
	}
}

type unlinkOnConnectClient struct {
	*fakeOperatorClient
	emit func()
}

func (c *unlinkOnConnectClient) ConnectContext(ctx context.Context) error {
	c.connects.Add(1)
	c.emit()
	return ctx.Err()
}

func TestUnlinkControllerReplacesRevokedClientAndStaysAlive(t *testing.T) {
	b, container := revokedSessionFixture(t)
	b.Exit = func(string, int) { t.Error("controller exited after unlink") }
	old := b.currentClient()
	reconnect := make(chan bool, 1)
	initial := &unlinkOnConnectClient{fakeOperatorClient: newFakeOperatorClient()}
	fresh := newFakeOperatorClient()
	fresh.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-REPLACEMENT-QR", Timeout: time.Minute}
	var replacements atomic.Int64
	p := newOperatorPairing(b.ctx, b, initial, func() (operatorPairingClient, error) {
		device, err := b.unlinkedDevice(b.ctx, container)
		if err != nil {
			return nil, err
		}
		b.installClient(newRuntimeClient(device, testLogger()), false, reconnect)
		replacements.Add(1)
		return fresh, nil
	}, b.isPaired, b.Connected, io.Discard, reconnect)
	b.operatorPairing = p
	p.opt.attemptTimeout = 50 * time.Millisecond
	p.state.Attempts = 1
	initial.emit = func() {
		b.handleClientEvent(old, &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	}
	p.start()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if state := p.snapshot(); state.QR != nil && state.QR.Payload == "FAKE-REPLACEMENT-QR" {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("no replacement QR: %+v", p.snapshot())
		}
	}
	unlinkHealth(t, b)
	if initial.connects.Load() != 1 || fresh.connects.Load() != 1 || replacements.Load() != 1 {
		t.Fatalf("old=%d fresh=%d replacements=%d", initial.connects.Load(), fresh.connects.Load(), replacements.Load())
	}
	// Exhausting the replacement QR sequence must leave the process/controller up.
	for p.snapshot().State != "expired" {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("replacement attempt did not expire")
		}
	}
	unlinkHealth(t, b)
	select {
	case <-p.done:
		t.Fatal("controller stopped after unlink/QR expiry")
	default:
	}
	b.cancel()
	<-p.done
}

func TestUnlinkDeleteFailureKeepsStartupAndRestartFenced(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	fake := newFakeOperatorClient()
	p := newOperatorPairing(b.ctx, b, fake, nil, b.isPaired, b.Connected, io.Discard, reconnect)
	b.operatorPairing = p
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER refuse_delete BEFORE DELETE ON whatsmeow_device BEGIN SELECT RAISE(FAIL,'fake deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	problem, _ := b.connectionSnapshot()
	if _, _, err := startupDevice(b.ctx, container, b.StoreRoot, problem); err == nil {
		t.Fatal("failed delete admitted startup")
	}
	w := httptest.NewRecorder()
	p.restart(w, httptest.NewRequest("POST", "/operator/v1/pairing/restart", nil))
	if w.Code != 202 {
		t.Fatalf("explicit retry refused: %d %s", w.Code, w.Body.String())
	}
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil || saved.Kind != "unlinked" {
		t.Fatalf("restart cleared unlink fence before delete: %+v err=%v", saved, err)
	}
	if _, _, err := startupDevice(b.ctx, container, b.StoreRoot, saved); err == nil {
		t.Fatal("restart admitted failed delete")
	}
	unlinkHealth(t, b)
}

func TestUnlinkPinnedSDKProtocolDispatchSurvivesFailedDelete(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "connect_failure_401"
		if stream {
			name = "device_removed"
		}
		t.Run(name, func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			b.Exit = func(string, int) { t.Error("protocol unlink exited") }
			db, err := openSessionDB()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`CREATE TRIGGER refuse_sdk_delete BEFORE DELETE ON whatsmeow_device BEGIN SELECT RAISE(FAIL,'fake SDK deletion failure'); END`); err != nil {
				t.Fatal(err)
			}
			seen := make(chan struct{}, 1)
			client := b.currentClient()
			client.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.LoggedOut); ok {
					seen <- struct{}{}
				}
			})
			if stream {
				client.DangerousInternals().HandleStreamError(b.ctx, &waBinary.Node{Tag: "stream:error", Attrs: waBinary.Attrs{"code": "401"}, Content: []waBinary.Node{{Tag: "conflict", Attrs: waBinary.Attrs{"type": "device_removed"}}}}) //nolint:staticcheck // Exercises the pinned SDK's real dispatch/delete order with a fake SQLite session, no network.
			} else {
				client.DangerousInternals().HandleConnectFailure(b.ctx, &waBinary.Node{Tag: "failure", Attrs: waBinary.Attrs{"reason": "401"}}) //nolint:staticcheck // Exercises the pinned SDK's real dispatch/delete order with a fake SQLite session, no network.
			}
			select {
			case <-seen:
			case <-time.After(5 * time.Second):
				t.Fatal("SDK did not dispatch LoggedOut")
			}
			unlinkHealth(t, b)
			saved, err := readConnectionProblem(b.StoreRoot)
			if err != nil || saved == nil || saved.Kind != "unlinked" {
				t.Fatalf("protocol fence=%+v err=%v", saved, err)
			}
			if _, err := db.Exec("DROP TRIGGER refuse_sdk_delete"); err != nil {
				t.Fatal(err)
			}
			device, _, err := startupDevice(b.ctx, container, b.StoreRoot, saved)
			if err != nil || device.ID != nil {
				t.Fatalf("protocol restart reused old session: %+v %v", device, err)
			}
		})
	}
}

func TestRestrictedLogoutStaysBlockedWhileAlive(t *testing.T) {
	for _, code := range []events.ConnectFailureReason{403, 406} {
		t.Run(code.String(), func(t *testing.T) {
			b, _ := revokedSessionFixture(t)
			b.Exit = func(string, int) { t.Error("account restriction exited") }
			b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: code}, make(chan bool, 1))
			ctx, cancel := context.WithCancel(b.ctx)
			defer cancel()
			blocked := make(chan struct{})
			var once sync.Once
			b.connectionBlocked = func() { once.Do(func() { close(blocked) }) }
			fake := newFakeOperatorClient()
			done := make(chan error, 1)
			go func() {
				done <- connectOrPair(ctx, fake, false, pairingOptions{attempts: 1, attemptTimeout: time.Second, out: io.Discard, log: b.Log, beforeDial: func() error { return b.waitConnectionAllowedContext(ctx) }})
			}()
			select {
			case <-blocked:
			case err := <-done:
				t.Fatalf("account restriction admitted dial: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("restriction did not park")
			}
			if b.healthStatus()["status"] != "blocked" || fake.connects.Load() != 0 {
				t.Fatalf("blocked health=%v connects=%d", b.healthStatus(), fake.connects.Load())
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("restriction shutdown=%v", err)
			}
		})
	}
}

func TestUnlinkStartupPendingControllerAllowsOperatorRetry(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER refuse_retry_delete BEFORE DELETE ON whatsmeow_device BEGIN SELECT RAISE(FAIL,'fake pending deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	// This is the new, unpaired placeholder used by startup after failed cleanup.
	b.installClient(newRuntimeClient(container.NewDevice(), testLogger()), false, reconnect)
	initial, fresh := newFakeOperatorClient(), newFakeOperatorClient()
	fresh.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-RETRY-QR", Timeout: time.Minute}
	var replacements atomic.Int64
	p := newOperatorPairing(b.ctx, b, initial, func() (operatorPairingClient, error) {
		replacements.Add(1)
		device, err := b.unlinkedDevice(b.ctx, container)
		if err != nil {
			return nil, err
		}
		b.installClient(newRuntimeClient(device, testLogger()), false, reconnect)
		return fresh, nil
	}, b.isPaired, b.Connected, io.Discard, reconnect)
	b.operatorPairing = p
	p.state.Attempts = 1
	p.start()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	wait := func(state string) {
		t.Helper()
		for p.snapshot().State != state {
			select {
			case <-ticker.C:
			case <-deadline:
				t.Fatalf("controller state=%s want=%s", p.snapshot().State, state)
			}
		}
	}
	wait("pairing_required")
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
	defer server.Close()
	retry := func() {
		t.Helper()
		req, _ := http.NewRequest("POST", server.URL+"/operator/v1/pairing/restart", nil)
		req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 202 {
			t.Fatalf("pending startup retry=%d", response.StatusCode)
		}
	}
	retry()
	wait("expired")
	if initial.connects.Load() != 0 || fresh.connects.Load() != 0 || replacements.Load() != 1 {
		t.Fatal("failed pending deletion reached a dial")
	}
	if _, err := db.Exec("DROP TRIGGER refuse_retry_delete"); err != nil {
		t.Fatal(err)
	}
	retry()
	wait("awaiting_qr")
	if initial.connects.Load() != 0 || fresh.connects.Load() != 1 || replacements.Load() != 2 || p.snapshot().QR == nil {
		t.Fatal("operator could not recover the parked startup")
	}
	unlinkHealth(t, b)
	b.cancel()
	<-p.done
}

func TestUnlinkReplacementSurvivesRestartBeforeConnected(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	replacement, err := b.unlinkedDevice(b.ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	jid.Device = 42
	replacement.ID = &jid
	replacement.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := replacement.Save(b.ctx); err != nil {
		t.Fatal(err)
	}
	client := newRuntimeClient(replacement, testLogger())
	b.installClient(client, true, reconnect)
	b.handleClientEvent(client, &events.PairSuccess{ID: jid}, reconnect)
	if !b.isPaired() {
		t.Fatal("replacement PairSuccess did not pair")
	}
	// Connected has not occurred. Reopen the actual marker and session database.
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil || saved.Kind != "unlinked" {
		t.Fatalf("saved diagnostics=%+v err=%v", saved, err)
	}
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	restarted := sqlstore.NewWithDB(db, "sqlite", testLogger())
	defer func() { _ = restarted.Close() }()
	restored, _, err := startupDevice(b.ctx, restarted, b.StoreRoot, saved)
	if err != nil || restored.ID == nil || *restored.ID != jid || !bytes.Equal(restored.NoiseKey.Priv[:], replacement.NoiseKey.Priv[:]) {
		t.Fatalf("restart erased or changed valid replacement: ID=%v sameID=%t sameNoise=%t originalID=%#v restoredID=%#v err=%v", restored.ID, restored.ID != nil && *restored.ID == jid, bytes.Equal(restored.NoiseKey.Priv[:], replacement.NoiseKey.Priv[:]), jid, restored.ID, err)
	}
	devices, err := restarted.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("replacement rows=%d err=%v", len(devices), err)
	}
}

func TestUnlinkCleanupPublicationFailureDoesNotAdmitPairing(t *testing.T) {
	b, container := revokedSessionFixture(t)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	if err := b.StoreRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if device, err := b.unlinkedDevice(b.ctx, container); err == nil || device != nil {
		t.Fatal("failed completion publication admitted a new client")
	}
	if b.healthStatus()["connection_problem_persistence_failed"] != true {
		t.Fatal("completion publication failure was not diagnosed")
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 0 {
		t.Fatalf("cleanup fixture failed: %d %v", len(devices), err)
	}
	root, err := os.OpenRoot(storeDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	saved, err := readConnectionProblem(root)
	if err != nil || saved == nil || saved.SessionRemoved {
		t.Fatalf("failed publication changed fence: %+v %v", saved, err)
	}
	device, _, err := startupDevice(b.ctx, container, root, saved)
	if err != nil || device.ID != nil {
		t.Fatalf("retry could not recover empty revoked store: %+v %v", device, err)
	}
}

func TestUnlinkNewReplacementRevocationRequiresCleanupAgain(t *testing.T) {
	b, container := revokedSessionFixture(t)
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	b.problemNow = func() time.Time { return now }
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	replacement, err := b.unlinkedDevice(b.ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	jid.Device = 43
	replacement.ID = &jid
	replacement.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := replacement.Save(b.ctx); err != nil {
		t.Fatal(err)
	}
	client := newRuntimeClient(replacement, testLogger())
	b.installClient(client, true, reconnect)
	b.handleClientEvent(client, &events.PairSuccess{ID: jid}, reconnect)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER refuse_second_unlink BEFORE DELETE ON whatsmeow_device BEGIN SELECT RAISE(FAIL,'fake second unlink failure'); END`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	b.handleClientEvent(client, &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil {
		t.Fatalf("new unlink not persisted: %+v %v", saved, err)
	}
	if device, _, err := startupDevice(b.ctx, container, b.StoreRoot, saved); err == nil || device != nil {
		t.Fatal("newly revoked replacement was admitted after restart")
	}
	if !saved.Since.Equal(now) {
		t.Fatal("new unlink reused the prior transition")
	}
	if _, err := db.Exec("DROP TRIGGER refuse_second_unlink"); err != nil {
		t.Fatal(err)
	}
	device, _, err := startupDevice(b.ctx, container, b.StoreRoot, saved)
	if err != nil || device.ID != nil {
		t.Fatalf("second unlink could not recover: %+v %v", device, err)
	}
}

func TestUnlinkPersistenceRepairRecoversExplicitRetryAndCompletion(t *testing.T) {
	for _, operator := range []bool{true, false} {
		name := "completion"
		if operator {
			name = "operator_retry"
		}
		t.Run(name, func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			part := connectionProblemFile + ".part"
			if err := b.StoreRoot.Mkdir(part, storeDirMode); err != nil {
				t.Fatal(err)
			}
			if err := b.StoreRoot.WriteFile(part+"/block", []byte("fake obstruction"), storeFileMode); err != nil {
				t.Fatal(err)
			}
			b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
			if b.healthStatus()["connection_problem_persistence_failed"] != true {
				t.Fatal("fixture did not fail persistence")
			}
			if err := b.StoreRoot.Remove(part + "/block"); err != nil {
				t.Fatal(err)
			}
			if err := b.StoreRoot.Remove(part); err != nil {
				t.Fatal(err)
			}
			if operator {
				p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, b.isPaired, b.Connected, io.Discard, make(chan bool, 1))
				b.operatorPairing = p
				p.state.State = "expired"
				server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
				defer server.Close()
				req, _ := http.NewRequest("POST", server.URL+"/operator/v1/pairing/restart", nil)
				req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != 202 {
					t.Fatalf("repaired persistence retry=%d", response.StatusCode)
				}
				saved, err := readConnectionProblem(b.StoreRoot)
				if err != nil || saved == nil || saved.Kind != "unlinked" || saved.SessionRemoved {
					t.Fatalf("explicit retry lost pending fence: %+v %v", saved, err)
				}
			}
			if device, err := b.unlinkedDevice(b.ctx, container); err != nil || device.ID != nil {
				t.Fatalf("repaired completion=%+v %v", device, err)
			}
			if b.healthStatus()["connection_problem_persistence_failed"] != false {
				t.Fatal("successful persistence repair remained failed")
			}
			saved, err := readConnectionProblem(b.StoreRoot)
			if err != nil || saved == nil || !saved.SessionRemoved {
				t.Fatalf("repaired completion not durable: %+v %v", saved, err)
			}
		})
	}
}
