package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func recoveryHealth(t *testing.T, b *Bridge) {
	t.Helper()
	server := httptest.NewServer(b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef"))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, _ := http.NewRequest("GET", server.URL+"/api/health", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
	started := time.Now()
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("health blocked by cleanup: %v", err)
	}
	var body map[string]any
	err = json.NewDecoder(response.Body).Decode(&body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || body["paired"] != false || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("slow cleanup health=%v err=%v", body, err)
	}
	if body["pairing_action"] == nil {
		t.Fatal("cleanup health omitted recovery guidance")
	}
}

func TestProductionFactorySlowCleanupKeepsStartupAndRuntimeHealthy(t *testing.T) {
	for _, startup := range []bool{true, false} {
		name := "runtime"
		if startup {
			name = "startup"
		}
		t.Run(name, func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			reconnect := make(chan bool, 1)
			b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: !startup, Reason: events.ConnectFailureLoggedOut}, reconnect)
			if startup {
				saved, err := readConnectionProblem(b.StoreRoot)
				if err != nil {
					t.Fatal(err)
				}
				device, pending, err := initialSessionDevice(b.ctx, container, saved)
				if err != nil || !pending || device.ID != nil {
					t.Fatal("startup exposed revoked device")
				}
				b.installClient(newRuntimeClient(device, testLogger()), false, reconnect)
				b.operatorRetiredClient.Store(b.currentClient())
			}
			started := make(chan struct{})
			var once sync.Once
			b.sessionCleanup = func(ctx context.Context, c *sqlstore.Container, p *ConnectionProblem) (*store.Device, *ConnectionProblem, error) {
				once.Do(func() { close(started) })
				if _, short := ctx.Deadline(); short {
					t.Error("production cleanup has an attempt deadline")
				}
				select {
				case <-time.After(6 * time.Second):
					return prepareSessionDevice(ctx, c, p)
				case <-ctx.Done():
					return nil, p, ctx.Err()
				}
			}
			ctx, cancel := context.WithCancel(b.ctx)
			defer cancel()
			factory := b.pairingClientFactory(ctx, container, historyLimits{}, false, testLogger(), reconnect)
			done := make(chan error, 1)
			go func() { _, err := factory(); done <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("production factory skipped session cleanup")
			}
			recoveryHealth(t, b)
			select {
			case err := <-done:
				t.Fatalf("cleanup exited before slow cascade completed: %v", err)
			case <-time.After(5500 * time.Millisecond):
			}
			recoveryHealth(t, b)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("slow cascade never recovered")
			}
			devices, err := container.GetAllDevices(b.ctx)
			if err != nil || len(devices) != 0 || b.currentClient().Store.ID != nil {
				t.Fatal("production factory reused revoked credentials")
			}
		})
	}
}

func TestProductionFactoryRetriesCleanupInsteadOfExpiring(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	var calls atomic.Int64
	b.cleanupRetryDelay = time.Millisecond
	b.sessionCleanup = func(ctx context.Context, c *sqlstore.Container, p *ConnectionProblem) (*store.Device, *ConnectionProblem, error) {
		if calls.Add(1) < 3 {
			return nil, p, errors.New("fake SDK writer is still busy")
		}
		return prepareSessionDevice(ctx, c, p)
	}
	factory := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)
	client, err := factory()
	if err != nil || client == nil || calls.Load() != 3 {
		t.Fatalf("cleanup did not retry: calls=%d err=%v", calls.Load(), err)
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 0 || b.isPaired() {
		t.Fatal("cleanup retry did not create a fresh client")
	}
}

func TestProductionFactorySurvivesRealSDKWriterContention(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE whatsmeow_device SET push_name=push_name"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	b.cleanupRetryDelay = time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := b.pairingClientFactory(ctx, container, historyLimits{}, false, testLogger(), reconnect)()
		done <- err
	}()
	recoveryHealth(t, b)
	select {
	case err := <-done:
		t.Fatalf("SDK writer contention parked recovery: %v", err)
	case <-time.After(6 * time.Second):
	}
	recoveryHealth(t, b)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer released but recovery did not resume")
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 0 {
		t.Fatal("revoked row survived real contention")
	}
}

func TestCleanupFsyncDoesNotBlockHealth(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	b.problemWrite = func(root *os.Root, p *ConnectionProblem) error {
		close(started)
		<-release
		return writeConnectionProblem(root, p)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)()
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("completion write was not reached")
	}
	recoveryHealth(t, b)
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRevokedMarkerDoesNotDeleteAnotherRestoredDevice(t *testing.T) {
	b, container := revokedSessionFixture(t)
	reconnect := make(chan bool, 1)
	revoked := b.currentClient().Store.ID.String()
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
	device, err := b.unlinkedDevice(b.ctx, container)
	if err != nil {
		t.Fatal(err)
	}
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	jid.Device = 44
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(b.ctx); err != nil {
		t.Fatal(err)
	}
	problem := classifyConnectionProblem(401, 0, 0, time.Now())
	problem.RevokedDevice = revoked
	if err := writeConnectionProblem(b.StoreRoot, problem); err != nil {
		t.Fatal(err)
	}
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	b.connectionProblem = saved
	restored, _, err := startupDevice(b.ctx, container, b.StoreRoot, saved)
	if err != nil || restored.ID == nil || *restored.ID != jid {
		t.Fatal("marker for A deleted restored B")
	}
	client, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)()
	if err != nil || client == nil || !b.isPaired() {
		t.Fatal("production factory did not retain restored B for connection")
	}
	var dials atomic.Int64
	b.currentClient().SetWebsocketHTTPClient(&http.Client{Transport: mcpTestTransport(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, errors.New("fake restored-device transport refusal")
	})})
	b.setPairingState("") // The shared controller clears this after the factory.
	ctx, cancel := context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	err = connectOrPair(ctx, client, b.isPaired(), pairingOptions{attempts: 1, log: testLogger(), beforeDial: func() error {
		return b.waitConnectionAllowedContext(ctx)
	}})
	if err == nil || dials.Load() != 1 || b.currentClient().Store.ID == nil || *b.currentClient().Store.ID != jid {
		t.Fatalf("restored B did not reach Connect with its own identity: dials=%d err=%v", dials.Load(), err)
	}
	data, err := json.Marshal(b.healthStatus())
	if err != nil || bytes.Contains(data, []byte(revoked)) || bytes.Contains(data, []byte(jid.String())) {
		t.Fatal("private marker identity escaped through health")
	}
}

func TestExpiredPairingHealthExplainsHowToRestart(t *testing.T) {
	b := newSettingsBridge(t)
	b.operatorPairing = newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, b.isPaired, b.Connected, nil, nil)
	b.operatorPairing.state.State = "expired"
	body := b.healthStatus()
	if body["paired"] != false || body["operator_pairing_state"] != "expired" || body["pairing_action"] != "Restart the bridge or POST /operator/v1/pairing/restart for a new QR" {
		t.Fatalf("expired pairing health omitted usable action: %v", body)
	}
	b.recordConnectionProblem(406, 0, 0)
	if b.healthStatus()["pairing_action"] != nil {
		t.Fatal("ban health advised requesting a QR")
	}
}
