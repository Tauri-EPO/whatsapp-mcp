package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestPinnedLibraryServerFailuresReachDiagnostics(t *testing.T) {
	for _, code := range []string{"500", "503"} {
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
		b.Client.Log = connectionProblemLogger{Logger: b.Log, bridge: b}
		b.Client.DangerousInternals().HandleConnectFailure(b.ctx, &waBinary.Node{Tag: "failure", Attrs: waBinary.Attrs{"reason": code, "message": "fake diagnostic"}}) //nolint:staticcheck // Test the actual pinned protocol handler; production uses only the public Logger seam.
		p, _ := b.connectionSnapshot()
		if p == nil || p.Kind != "server_error" {
			t.Fatalf("protocol failure %s not captured: %+v", code, p)
		}
		if strings.Contains(b.renderMetrics(), "fake diagnostic") {
			t.Fatal("server message leaked to metrics")
		}
	}
}

func TestStalledKeepaliveAndLoginHandshakeUseGatedReconnect(t *testing.T) {
	for _, event := range []any{&events.KeepAliveTimeout{LastSuccess: time.Now().Add(-4 * time.Minute)}, &events.ManualLoginReconnect{}} {
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
		var connected atomic.Bool
		connected.Store(true)
		b.Connected = connected.Load
		b.Disconnect = func() { connected.Store(false) }
		called := make(chan struct{}, 1)
		b.Connect = func() error { called <- struct{}{}; return nil }
		b.ReconnectInitialBackoff = time.Millisecond
		ch := make(chan bool, 1)
		b.handleEvent(event, ch)
		go b.reconnectLoop(ch)
		select {
		case <-called:
		case <-time.After(5 * time.Second):
			t.Fatal("stalled connection never redialled")
		}
		b.cancel()
	}
}

func TestConnectionProblemsReachHTTPAndMetrics(t *testing.T) {
	for _, tc := range []struct {
		code int
		kind string
	}{{401, "unlinked"}, {402, "temporarily_banned"}, {403, "locked"}, {405, "client_outdated"}, {406, "banned"}, {503, "server_error"}, {409, "other"}} {
		t.Run(tc.kind, func(t *testing.T) {
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			b.Exit = func(string, int) {}
			ch := make(chan bool, 1)
			b.handleEvent(&events.ConnectFailure{Reason: events.ConnectFailureReason(tc.code)}, ch)
			r := httptest.NewRecorder()
			b.handleHealth()(r, httptest.NewRequest("GET", "/api/health", nil))
			var body struct {
				Problem ConnectionProblem `json:"connection_problem"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if r.Code != 200 || body.Problem.Kind != tc.kind || body.Problem.Code != tc.code {
				t.Fatalf("health=%s", r.Body)
			}
			if !strings.Contains(b.renderMetrics(), "whatsapp_bridge_connection_problem{kind=\""+tc.kind+"\"} 1") {
				t.Fatal("missing active gauge")
			}
		})
	}
}

func TestTemporaryBanSurvivesRestartAndWaitsBeforeDial(t *testing.T) {
	for _, reason := range []events.TempBanReason{101, 102, 103, 104, 106, 999} {
		t.Run(reason.String(), func(t *testing.T) {
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = root.Close() })
			b.StoreRoot = root
			now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
			b.problemNow = func() time.Time { return now }
			ch := make(chan bool, 1)
			b.handleEvent(&events.TemporaryBan{Code: reason, Expire: 24 * time.Hour}, ch)
			p, err := readConnectionProblem(root)
			if err != nil || p == nil || p.TempBanReason != int(reason) || !p.ExpiresAt.Equal(now.Add(24*time.Hour)) {
				t.Fatalf("restored=%+v err=%v", p, err)
			}
			b.connectionProblem = p // fresh process loads the same file before any dial
			waits, dials := 0, 0
			b.problemWait = func(d time.Duration) error {
				waits++
				if d != 24*time.Hour || dials != 0 {
					t.Fatalf("wait=%v dials=%d", d, dials)
				}
				now = now.Add(d)
				return nil
			}
			c := &fakePairingClient{}
			opt := fastOpts(&bytes.Buffer{})
			opt.beforeDial = func() error {
				err := b.waitConnectionAllowed()
				if err == nil {
					dials++
				}
				return err
			}
			if err := connectOrPair(b.ctx, c, true, opt); err != nil {
				t.Fatal(err)
			}
			if waits != 1 || dials != 1 || c.connects != 1 {
				t.Fatalf("waits=%d dials=%d connects=%d", waits, dials, c.connects)
			}
			b.handleEvent(&events.Connected{}, ch)
			if p, _ := b.connectionSnapshot(); p != nil {
				t.Fatal("successful connection did not clear problem")
			}
			if _, err := root.Lstat(connectionProblemFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("saved ban not cleared")
			}
		})
	}
}

func TestBlockedLogoutPersistsAndNeverDrawsQR(t *testing.T) {
	for _, code := range []events.ConnectFailureReason{403, 406} {
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		b.StoreRoot = root
		b.Exit = func(string, int) { t.Fatal("blocked logout must stay alive") }
		b.handleEvent(&events.LoggedOut{OnConnect: true, Reason: code}, make(chan bool, 1))
		b.connectionProblem, err = readConnectionProblem(root)
		if err != nil {
			t.Fatal(err)
		}
		c := &fakePairingClient{}
		out := &bytes.Buffer{}
		opt := fastOpts(out)
		opt.beforeDial = b.waitConnectionAllowed
		b.cancel()
		if err := connectOrPair(b.ctx, c, false, opt); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		if c.connects != 0 || c.qrCalls != 0 || out.Len() != 0 {
			t.Fatal("blocked account reached QR or network")
		}
	}
}

func TestSavedProblemRefusesCorruptionAndSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.WriteFile(dir+"/"+connectionProblemFile, []byte("broken"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := readConnectionProblem(root); err == nil {
		t.Fatal("corruption accepted")
	}
	if err := root.Remove(connectionProblemFile); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("other", connectionProblemFile); err != nil {
		t.Skip("symlink requires privileges on Windows")
	}
	if _, err := readConnectionProblem(root); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestProblemPersistenceFailureKeepsTransientReconnectAvailable(t *testing.T) {
	for _, code := range []int{503, 409, 401, 402, 403, 405, 406} {
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		b.StoreRoot = root
		if err := root.Mkdir(connectionProblemFile+".part", storeDirMode); err != nil {
			t.Fatal(err)
		}
		f, err := root.Create(connectionProblemFile + ".part/occupied")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		b.recordConnectionProblem(code, 0, time.Hour)
		r := httptest.NewRecorder()
		b.handleHealth()(r, httptest.NewRequest("GET", "/api/health", nil))
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"connection_problem_persistence_failed":true`) {
			t.Fatalf("health=%s", r.Body)
		}
		ctx, cancel := context.WithTimeout(b.ctx, 20*time.Millisecond)
		err = b.waitConnectionAllowedContext(ctx)
		cancel()
		p, _ := b.connectionSnapshot()
		if p.restrictsAccount() {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("code=%d dial not blocked: %v", code, err)
			}
			if err := root.Remove(connectionProblemFile + ".part/occupied"); err != nil {
				t.Fatal(err)
			}
			// The next gate pass retries the actual filesystem write.
			ctx, cancel = context.WithTimeout(b.ctx, 20*time.Millisecond)
			_ = b.waitConnectionAllowedContext(ctx)
			cancel()
			if saved, err := readConnectionProblem(root); err != nil || saved == nil || saved.Code != code {
				t.Fatalf("retry saved=%+v err=%v", saved, err)
			}
		} else if err != nil {
			t.Fatalf("transient code=%d blocked: %v", code, err)
		}
	}
}

func TestTransientProblemCannotEraseRestriction(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.recordConnectionProblem(406, 0, 0)
	b.recordConnectionProblem(503, 0, 0)
	p, _ := b.connectionSnapshot()
	if p.Kind != "banned" {
		t.Fatalf("restriction replaced: %+v", p)
	}
}

func TestOutdatedBuildIsRetriedOnlyAfterBuildChanges(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	p := classifyConnectionProblem(405, 0, 0, time.Now())
	if p.BuildVersion == "" {
		t.Fatal("outdated problem has no build identity")
	}
	if err := writeConnectionProblem(root, p); err != nil {
		t.Fatal(err)
	}
	if saved, err := readConnectionProblem(root); err != nil || saved == nil {
		t.Fatalf("same build forgotten: %+v %v", saved, err)
	}
	p.BuildVersion = "fake-previous-build"
	if err := writeConnectionProblem(root, p); err != nil {
		t.Fatal(err)
	}
	if saved, err := readConnectionProblem(root); err != nil || saved != nil {
		t.Fatalf("new build blocked: %+v %v", saved, err)
	}
	if _, err := root.Lstat(connectionProblemFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outdated file not cleared")
	}
	p = classifyConnectionProblem(406, 0, 0, time.Now())
	p.BuildVersion = "fake-previous-build"
	if err := writeConnectionProblem(root, p); err != nil {
		t.Fatal(err)
	}
	if saved, err := readConnectionProblem(root); err != nil || saved == nil {
		t.Fatalf("account ban erased by upgrade: %+v %v", saved, err)
	}
}

func TestPasskeyFlowIsVisibleWithoutLeakingOptions(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	states := []string{}
	opt := fastOpts(&bytes.Buffer{})
	opt.state = func(s string) { states = append(states, s); b.setPairingState(s) }
	c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{{
		{Event: whatsmeow.QRChannelEventPasskeyRequest, PasskeyRequest: &events.PairPasskeyRequest{PublicKey: &types.WebAuthnPublicKey{Timeout: 1000, RelyingPartID: "whatsapp.com"}}},
		{Event: "code", Code: "FAKE-STALE-QR"},
		{Event: whatsmeow.QRChannelEventPasskeyResponse, PasskeyConfirmation: &events.PairPasskeyConfirmation{Code: "FAKE-CODE"}},
		{Event: "success"},
	}}}
	if err := connectOrPair(b.ctx, c, false, opt); err != nil {
		t.Fatal(err)
	}
	if strings.Join(states, ",") != "passkey_required,passkey_confirm," {
		t.Fatalf("states=%v", states)
	}
	b.setPairingState("passkey_required")
	r := httptest.NewRecorder()
	b.handleHealth()(r, httptest.NewRequest("GET", "/api/health", nil))
	if !strings.Contains(r.Body.String(), "passkey_required") || strings.Contains(r.Body.String(), "whatsapp.com") || strings.Contains(r.Body.String(), "FAKE-CODE") {
		t.Fatalf("unsafe health %s", r.Body)
	}
}

func TestPasskeyFailureDoesNotStartAnotherQRSequence(t *testing.T) {
	for _, terminal := range []whatsmeow.QRChannelItem{{Event: "timeout"}, {Event: "error", Error: errors.New("secret assertion")}} {
		c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{{{Event: whatsmeow.QRChannelEventPasskeyRequest}, terminal}}}
		err := connectOrPair(context.Background(), c, false, fastOpts(&bytes.Buffer{}))
		if !errors.Is(err, errPairingOperator) || c.qrCalls != 1 || strings.Contains(err.Error(), "secret assertion") {
			t.Fatalf("err=%v qrCalls=%d", err, c.qrCalls)
		}
	}
}
