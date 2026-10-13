package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func restrictionHealth(t *testing.T, b *Bridge, kind, status string) {
	t.Helper()
	server := httptest.NewServer(b.runtimeRESTHandler(8080, "fake-data-plane-token-0123456789abcdef"))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/api/health", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer fake-data-plane-token-0123456789abcdef")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Status  string            `json:"status"`
		Paired  bool              `json:"paired"`
		Problem ConnectionProblem `json:"connection_problem"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || body.Status != status || body.Paired || body.Problem.Kind != kind || body.Problem.UnlinkedAt == nil || body.Problem.RevokedDevice != "" {
		t.Fatalf("restriction/unlink health lost reporting: %+v", body)
	}
}

func TestUnlinkRestrictionOrdersFenceSessionAndRetainBanGate(t *testing.T) {
	for _, after := range []bool{false, true} {
		order := "restriction_then_unlink"
		if after {
			order = "unlink_then_restriction"
		}
		for _, tc := range []struct {
			name    string
			code    int
			expired bool
			status  string
		}{
			{"expired_temporary", 402, true, "pairing_required"},
			{"active_temporary", 402, false, "temporarily_banned"},
			{"locked", 403, false, "blocked"},
			{"banned", 406, false, "blocked"},
			{"client_outdated", 405, false, "blocked"},
		} {
			for _, onConnect := range []bool{false, true} {
				source := "device_removed"
				if onConnect {
					source = "connect_401"
				}
				t.Run(order+"/"+tc.name+"/"+source, func(t *testing.T) {
					b, container := revokedSessionFixture(t)
					b.Exit = func(string, int) { t.Error("unlink exited") }
					now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
					b.problemNow = func() time.Time { return now }
					id := b.currentClient().Store.ID.String()
					reconnect := make(chan bool, 1)
					record := func() *ConnectionProblem { return b.recordConnectionProblem(tc.code, 101, time.Second) }
					var restriction *ConnectionProblem
					if !after {
						restriction = record()
						if tc.expired {
							now = now.Add(2 * time.Second)
						}
					}
					evt := &events.LoggedOut{OnConnect: onConnect, Reason: events.ConnectFailureLoggedOut}
					if !onConnect {
						evt.Reason = 0
					}
					b.handleClientEvent(b.currentClient(), evt, reconnect)
					if after {
						restriction = record()
						if tc.expired {
							now = now.Add(2 * time.Second)
						}
					}
					saved, err := readConnectionProblem(b.StoreRoot)
					if err != nil || saved == nil || !saved.isUnlinked() || saved.UnlinkedAt == nil || saved.RevokedDevice != id || saved.SessionRemoved || saved.Kind != restriction.Kind || saved.Code != restriction.Code || !saved.Since.Equal(restriction.Since) {
						t.Fatalf("restriction suppressed or erased unlink: saved=%+v original=%+v err=%v", saved, restriction, err)
					}
					if tc.code == 402 && (saved.TempBanReason != 101 || saved.ExpiresAt == nil || !saved.ExpiresAt.Equal(*restriction.ExpiresAt)) {
						t.Fatal("temporary-ban details changed")
					}
					if count := strings.Count(b.renderMetrics(), `whatsapp_bridge_connection_problem{kind="unlinked"} 1`); count != 1 {
						t.Fatalf("unlink gauge count=%d", count)
					}
					if !strings.Contains(b.renderMetrics(), `whatsapp_bridge_connection_problem{kind="`+saved.Kind+`"} 1`) {
						t.Fatal("ban gauge lost")
					}
					if tc.code == 402 && !strings.Contains(b.renderMetrics(), `whatsapp_bridge_temp_bans_total{reason="101"} 1`) {
						t.Fatal("unlink duplicated temporary-ban counter")
					}
					restrictionHealth(t, b, saved.Kind, tc.status)
					placeholder, pending, err := initialSessionDevice(b.ctx, container, saved)
					if err != nil || !pending || placeholder.ID != nil {
						t.Fatalf("startup exposed revoked keys: pending=%t device=%+v err=%v", pending, placeholder, err)
					}
					b.installClient(newRuntimeClient(placeholder, testLogger()), false, reconnect)
					client, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)()
					if err != nil || client == nil || b.currentClient().Store.ID != nil {
						t.Fatalf("restricted cleanup did not create unpaired client: %v", err)
					}
					devices, err := container.GetAllDevices(b.ctx)
					if err != nil || len(devices) != 0 {
						t.Fatalf("revoked session survived: %d %v", len(devices), err)
					}
					saved, err = readConnectionProblem(b.StoreRoot)
					if err != nil || saved == nil || !saved.SessionRemoved || saved.Kind != restriction.Kind || saved.RevokedDevice != id {
						t.Fatalf("cleanup lost durable restriction or completion: %+v %v", saved, err)
					}
					b.connectionProblem = saved // Restart reopens both durable states.
					b.setPairingState("")       // Production controller does this after the factory.
					ctx, cancel := context.WithTimeout(b.ctx, 40*time.Millisecond)
					defer cancel()
					fake := newFakeOperatorClient()
					fake.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-RESTRICTED-RECOVERY-QR", Timeout: time.Minute}
					fake.items <- whatsmeow.QRChannelItem{Event: "success"}
					err = connectOrPair(ctx, fake, b.isPaired(), pairingOptions{attempts: 1, attemptTimeout: time.Second, out: io.Discard, log: testLogger(), beforeDial: func() error { return b.waitConnectionAllowedContext(ctx) }})
					if tc.expired {
						if err != nil || fake.connects.Load() != 1 {
							t.Fatalf("expired ban did not admit fresh pairing: %v", err)
						}
					} else if !errors.Is(err, context.DeadlineExceeded) || fake.connects.Load() != 0 {
						t.Fatalf("active ban gate lost: connects=%d err=%v", fake.connects.Load(), err)
					}
				})
			}
		}
	}
}

func TestLaterRestrictionKeepsCompletedAndLegacyUnlinkMarkers(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_pending", true: "completed"}[complete], func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
			if complete {
				device, err := b.unlinkedDevice(b.ctx, container)
				if err != nil {
					t.Fatal(err)
				}
				before, _ := b.connectionSnapshot()
				jid, err := types.ParseJID(before.RevokedDevice)
				if err != nil {
					t.Fatal(err)
				}
				device.ID = &jid // New keys can reuse the revoked device identity.
				device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
				if err := device.Save(b.ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				old, _ := b.connectionSnapshot()
				legacy := *old
				legacy.UnlinkedAt = nil
				b.connectionProblem = &legacy
			}
			before, _ := b.connectionSnapshot()
			b.recordConnectionProblem(402, 101, time.Hour)
			saved, err := readConnectionProblem(b.StoreRoot)
			if err != nil || saved == nil || saved.Kind != "temporarily_banned" || !saved.isUnlinked() || saved.UnlinkedAt == nil || saved.SessionRemoved != complete || saved.RevokedDevice != before.RevokedDevice {
				t.Fatalf("later ban lost revocation state: %+v %v", saved, err)
			}
			if _, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), make(chan bool, 1))(); err != nil {
				t.Fatal(err)
			}
			devices, err := container.GetAllDevices(b.ctx)
			want := 0
			if complete {
				want = 1
			}
			if err != nil || len(devices) != want {
				t.Fatalf("late ban left revoked rows: %d %v", len(devices), err)
			}
			if complete {
				b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
				saved, err := readConnectionProblem(b.StoreRoot)
				if err != nil || saved == nil || saved.SessionRemoved || saved.Kind != "temporarily_banned" {
					t.Fatalf("new unlink under a ban reused old completion: %+v %v", saved, err)
				}
				if _, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), make(chan bool, 1))(); err != nil {
					t.Fatal(err)
				}
				devices, err = container.GetAllDevices(b.ctx)
				if err != nil || len(devices) != 0 {
					t.Fatal("newly revoked replacement survived its ban")
				}
			}
		})
	}
}

func TestOutdatedBuildUpgradeKeepsPendingUnlinkFence(t *testing.T) {
	b, container := revokedSessionFixture(t)
	b.recordConnectionProblem(405, 0, 0)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	prior, _ := b.connectionSnapshot()
	saved, err := clearOutdatedBuildProblem(b.StoreRoot, prior, "fake-new-build")
	if err != nil || saved == nil || saved.Kind != "unlinked" || !saved.isUnlinked() || saved.RevokedDevice != prior.RevokedDevice || saved.SessionRemoved {
		t.Fatalf("upgrade discarded unlink fence: %+v %v", saved, err)
	}
	device, pending, err := initialSessionDevice(b.ctx, container, saved)
	if err != nil || !pending || device.ID != nil {
		t.Fatal("upgrade admitted revoked keys")
	}
	b.connectionProblem = saved
	if _, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), make(chan bool, 1))(); err != nil {
		t.Fatal(err)
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 0 {
		t.Fatal("upgrade left revoked row")
	}
}

func TestOperatorResolvedRestrictionKeepsRevocationAndAdmitsFreshPairing(t *testing.T) {
	for _, code := range []int{403, 406} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			b, container := revokedSessionFixture(t)
			b.recordConnectionProblem(code, 0, 0)
			reconnect := make(chan bool, 1)
			b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, reconnect)
			prior, _ := b.connectionSnapshot()
			p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, b.isPaired, b.Connected, io.Discard, reconnect)
			b.operatorPairing = p
			p.state.State = "pairing_required"
			server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
			defer server.Close()
			req, _ := http.NewRequest("POST", server.URL+"/operator/v1/pairing/restart", nil)
			req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != 202 {
				t.Fatalf("resolved restriction restart=%d", response.StatusCode)
			}
			saved, err := readConnectionProblem(b.StoreRoot)
			if err != nil || saved == nil || saved.Kind != "unlinked" || saved.Code != 401 || saved.RevokedDevice != prior.RevokedDevice || saved.SessionRemoved || !saved.UnlinkedAt.Equal(*prior.UnlinkedAt) {
				t.Fatalf("acknowledgement lost fence or kept restriction: %+v %v", saved, err)
			}
			if _, err := b.pairingClientFactory(b.ctx, container, historyLimits{}, false, testLogger(), reconnect)(); err != nil {
				t.Fatal(err)
			}
			b.setPairingState("")
			ctx, cancel := context.WithTimeout(b.ctx, time.Second)
			defer cancel()
			if err := b.waitConnectionAllowedContext(ctx); err != nil {
				t.Fatalf("resolved restriction still blocked fresh client: %v", err)
			}
			devices, err := container.GetAllDevices(b.ctx)
			if err != nil || len(devices) != 0 || b.currentClient().Store.ID != nil {
				t.Fatal("resolved restart admitted revoked keys")
			}
		})
	}
}

func TestSharedControllerResumesAfterTemporaryBanDuringFinalQRAttempt(t *testing.T) {
	b := newSettingsBridge(t)
	reconnect := make(chan bool, 1)
	initial := &unlinkOnConnectClient{fakeOperatorClient: newFakeOperatorClient()}
	initial.items <- whatsmeow.QRChannelItem{Event: "timeout"}
	initial.emit = func() { b.handleEvent(&events.TemporaryBan{Code: 101, Expire: 80 * time.Millisecond}, reconnect) }
	called := make(chan time.Time, 1)
	fresh := &unlinkOnConnectClient{fakeOperatorClient: newFakeOperatorClient()}
	fresh.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-AFTER-BAN-QR", Timeout: time.Minute}
	fresh.items <- whatsmeow.QRChannelItem{Event: "success"}
	fresh.emit = func() {
		problem, _ := b.connectionSnapshot()
		at := time.Now()
		if problem == nil || problem.ExpiresAt == nil || at.Before(*problem.ExpiresAt) {
			t.Error("temporary ban did not block the fresh dial until expiry")
		}
		called <- at
	}
	p := newOperatorPairing(b.ctx, b, initial, func() (operatorPairingClient, error) { return fresh, nil }, func() bool { return false }, func() bool { return false }, io.Discard, reconnect)
	b.operatorPairing = p
	p.state.Attempts = 1
	p.opt.attemptTimeout = time.Second
	p.opt.retryDelay = time.Millisecond
	p.start()
	defer func() { b.cancel(); <-p.done }()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("temporary-ban expiry never resumed pairing without operator restart")
	}
	if initial.connects.Load() != 1 || fresh.connects.Load() != 1 {
		t.Fatal("temporary ban replayed the old attempt")
	}
}

func TestUnlinkDeleteMustVerifyRevokedRowActuallyDisappeared(t *testing.T) {
	b, container := revokedSessionFixture(t)
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fake_ignore_delete BEFORE DELETE ON whatsmeow_device BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if device, err := b.unlinkedDevice(b.ctx, container); err == nil || device != nil {
		t.Fatal("SDK delete returned nil without removing row, but cleanup admitted it")
	}
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil || saved.SessionRemoved {
		t.Fatal("failed row verification published completion")
	}
	if _, err := db.Exec("DROP TRIGGER fake_ignore_delete"); err != nil {
		t.Fatal(err)
	}
	if device, err := b.unlinkedDevice(b.ctx, container); err != nil || device.ID != nil {
		t.Fatalf("verification retry=%+v %v", device, err)
	}
}
