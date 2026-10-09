package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestReconnectQueuedWriterAndConcurrentLogoutComplete(t *testing.T) {
	b := newSettingsBridge(t)
	b.bindRuntimeClient()
	entered, proceed := make(chan struct{}), make(chan struct{})
	b.Connected = func() bool { close(entered); <-proceed; return false }
	b.ReconnectInitialBackoff = time.Millisecond
	reconnect := make(chan bool, 1)
	done := make(chan struct{})
	go func() { b.reconnectLoop(reconnect); close(done) }()
	defer b.cancel()
	reconnect <- true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not acquire its outer read lock")
	}
	writerDone := make(chan struct{})
	go func() { b.clientGate.Lock(); b.clientGate.Unlock(); close(writerDone) }()
	deadline := time.Now().Add(time.Second)
	for b.clientGate.TryRLock() {
		b.clientGate.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("exclusive writer was not queued")
		}
		time.Sleep(time.Millisecond)
	}
	b.logoutClient = func(context.Context) error { return nil }
	b.wipeSession = func(context.Context) error { return nil }
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, func() bool { return false }, io.Discard, reconnect)
	logoutDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
		logoutDone <- w
	}()
	for !b.operatorLogout.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !b.operatorLogout.Load() {
		t.Fatal("logout did not retire the reconnecting client")
	}
	close(proceed)
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("nested reconnect read lock deadlocked with the queued writer")
	}
	select {
	case w := <-logoutDone:
		if w.Code != 200 {
			t.Fatalf("logout=%d %s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent logout did not complete")
	}
	b.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not stop")
	}
}

func TestOperatorLogoutDrainsDetachedDownloadAfterHTTPWaiterCancels(t *testing.T) {
	b := newSettingsBridge(t)
	b.Connected = func() bool { return true }
	seedMediaRowIn(t, b.Store, mediaTestChat, "DETACHED")
	entered, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		b.mediaTransfers.wait()
	})
	b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
		close(entered)
		<-release
		finished.Store(true)
		return 0, errors.New("fake transfer complete")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/download", strings.NewReader(`{"message_id":"DETACHED","chat_jid":"5511999999999@s.whatsapp.net"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	handler := b.runtimeRESTHandler(8080, readOnlyTestToken)
	waiterDone := make(chan struct{})
	go func() { handler.ServeHTTP(httptest.NewRecorder(), r); close(waiterDone) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("transfer did not start")
	}
	cancel()
	select {
	case <-waiterDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP waiter did not cancel")
	}
	b.logoutDrainTimeout = 30 * time.Millisecond
	b.logoutClient = func(context.Context) error { return nil }
	b.wipeSession = func(context.Context) error {
		if !finished.Load() {
			t.Error("wipe overlapped detached transfer")
		}
		return nil
	}
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
	w := httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "client_busy") {
		t.Fatalf("detached drain=%d %s", w.Code, w.Body.String())
	}
	close(release)
	b.mediaTransfers.wait()
	w = httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	if w.Code != 200 {
		t.Fatalf("retry=%d %s", w.Code, w.Body.String())
	}
}

func TestDetachedDownloadDoesNotWaitBehindQueuedClientWriter(t *testing.T) {
	b := newSettingsBridge(t)
	seedMediaRowIn(t, b.Store, mediaTestChat, "QUEUED")
	b.clientGate.RLock()
	done := make(chan struct{})
	go func() { b.clientGate.Lock(); b.clientGate.Unlock(); close(done) }()
	deadline := time.Now().Add(time.Second)
	for b.clientGate.TryRLock() {
		b.clientGate.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("writer did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	result := startDownload(context.Background(), b, "QUEUED", mediaTestChat)
	select {
	case res := <-result:
		if res.err == nil || !strings.Contains(res.err.Error(), "busy") {
			t.Fatalf("queued writer result=%+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("detached transfer nested read gate deadlocked")
	}
	b.clientGate.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writer did not finish")
	}
}

func TestOperatorLogoutBoundsDrainAndCanRetryWithoutRestart(t *testing.T) {
	b := newSettingsBridge(t)
	b.logoutDrainTimeout = 30 * time.Millisecond
	var unlinks, wipes int
	b.logoutClient = func(context.Context) error { unlinks++; return nil }
	b.wipeSession = func(context.Context) error { wipes++; return nil }
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, b.Connected, io.Discard, make(chan bool, 1))
	b.clientGate.RLock()
	start := time.Now()
	w := httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	b.clientGate.RUnlock()
	if w.Code != 503 || !strings.Contains(w.Body.String(), "client_busy") || time.Since(start) > time.Second || unlinks != 0 || wipes != 0 {
		t.Fatalf("unbounded/admitted busy logout: %d %s", w.Code, w.Body.String())
	}
	if !b.operatorLogout.Load() {
		t.Fatal("busy logout did not remain parked")
	}
	restart := httptest.NewRecorder()
	p.restart(restart, httptest.NewRequest("POST", "/operator/v1/pairing/restart", nil))
	if restart.Code != 409 || !strings.Contains(restart.Body.String(), "local_session_not_wiped") {
		t.Fatal("busy cleanup allowed restart")
	}
	w = httptest.NewRecorder()
	p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
	if w.Code != 200 || unlinks != 1 || wipes != 1 {
		t.Fatalf("retry=%d %s", w.Code, w.Body.String())
	}
}

func TestOperatorIdleRestoresIncompleteCleanupWithoutPairedClient(t *testing.T) {
	b := newSettingsBridge(t)
	if _, err := b.Store.db.Exec("INSERT INTO operator_state(id,logged_out,local_session_wiped) VALUES (1,1,0)"); err != nil {
		t.Fatal(err)
	}
	p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return false }, b.Connected, io.Discard, make(chan bool, 1))
	b.operatorPairing = p
	if err := b.restoreOperatorIdle(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.restart(w, httptest.NewRequest("POST", "/operator/v1/pairing/restart", nil))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "local_session_not_wiped") {
		t.Fatal("restart forgot unfinished durable cleanup")
	}
}

type backgroundLogoutLIDs struct {
	mockLIDStore
	entered, release chan struct{}
	done             *atomic.Bool
	once             sync.Once
}

func (l *backgroundLogoutLIDs) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	l.once.Do(func() { close(l.entered); <-l.release; l.done.Store(true) })
	return types.EmptyJID, nil
}

func TestOperatorLogoutDrainsBackgroundSessionConsumers(t *testing.T) {
	for _, kind := range []string{"history", "media", "labels"} {
		t.Run(kind, func(t *testing.T) {
			b := newSettingsBridge(t)
			entered, release, backgroundDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var identityFinished atomic.Bool
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				b.cancel()
			})
			jid := types.NewJID("5511999999999", types.DefaultUserServer)
			b.Client.Store.ID = &jid
			switch kind {
			case "history":
				b.Client = newTestClient(&backgroundLogoutLIDs{entered: entered, release: release, done: &identityFinished})
				b.Client.Store.ID = &jid
				data, err := proto.Marshal(shareHistoryFixture(1).Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, _ := encryptedShareServer(t, b, compressShare(t, data), "")
				go func() {
					b.runHistoryShare(b.ctx, historyShareJob{bundle: bundle, chat: "120363000000000001@g.us"})
					close(backgroundDone)
				}()
			case "media":
				b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
					close(entered)
					<-release
					if b.currentClient().Store.ID == nil {
						t.Error("media observed deletion while admitted")
					}
					identityFinished.Store(true)
					return false, "", "", "", nil
				}
				go func() { b.runAutoDownload(b.ctx, mediaJob{}); close(backgroundDone) }()
			case "labels":
				b.Connected = func() bool { return true }
				b.LabelResync = func(context.Context) error {
					close(entered)
					<-release
					if b.currentClient().Store.ID == nil {
						t.Error("label sync observed deletion while admitted")
					}
					identityFinished.Store(true)
					return nil
				}
				b.startLabelSync()
				go func() { b.labelSyncWait.Wait(); close(backgroundDone) }()
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("background consumer did not reach session lookup")
			}
			b.logoutClient = func(context.Context) error { return nil }
			b.wipeSession = func(context.Context) error {
				if !identityFinished.Load() {
					t.Error("logout deletion overlapped background session consumer")
				}
				b.currentClient().Store.ID = nil
				return nil
			}
			p := newOperatorPairing(b.ctx, b, newFakeOperatorClient(), nil, func() bool { return true }, func() bool { return false }, io.Discard, make(chan bool, 1))
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				p.logout(w, httptest.NewRequest("POST", "/operator/v1/logout", strings.NewReader(`{"after":"idle"}`)))
				result <- w
			}()
			select {
			case <-result:
				t.Fatal("logout skipped active background drain")
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			select {
			case <-backgroundDone:
			case <-time.After(time.Second):
				t.Fatal("background consumer did not drain")
			}
			select {
			case w := <-result:
				if w.Code != 200 {
					t.Fatalf("logout=%d %s", w.Code, w.Body.String())
				}
			case <-time.After(time.Second):
				t.Fatal("logout did not finish")
			}
		})
	}
}
