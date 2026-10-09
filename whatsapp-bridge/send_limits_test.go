package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"os"
	"path/filepath"
)

func limitPatch(t *testing.T, b *Bridge, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	b.handleRuntimeSettings(w, httptest.NewRequest("PATCH", "/operator/v1/settings", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("PATCH: %d %s", w.Code, w.Body.String())
	}
}

func limitSend(t *testing.T, b *Bridge, recipient string, dry bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/send", strings.NewReader(fmt.Sprintf(`{"recipient":%q,"message":"hello","dry_run":%v}`, recipient, dry)))
	r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	w := httptest.NewRecorder()
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, r)
	return w
}

func fakeLimitedSender(b *Bridge, calls *atomic.Int64) {
	b.Connected = func() bool { return true }
	b.Send = func(_ context.Context, recipient, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
		calls.Add(1)
		return true, "sent", sentMessage{ID: "fake-id", ChatJID: recipient, Timestamp: b.sendClock()}
	}
}

func TestSendLimitsFakeClockHTTPAndReset(t *testing.T) {
	for _, tc := range []struct {
		key, reason string
		value       int
		wait        time.Duration
		secondChat  bool
	}{
		{"rate_per_minute", "per_minute", 1, time.Minute, false},
		{"rate_per_day", "per_day", 1, 24 * time.Hour, false},
		{"new_chats_per_day", "new_chats_per_day", 1, 24 * time.Hour, true},
		{"min_interval_ms", "min_interval", 1500, 1500 * time.Millisecond, false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			b := newSettingsBridge(t)
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			b.sendNow = func() time.Time { return now }
			var calls atomic.Int64
			fakeLimitedSender(b, &calls)
			limitPatch(t, b, fmt.Sprintf(`{"send.%s":%d}`, tc.key, tc.value))
			if w := limitSend(t, b, "120363000000000001@g.us", false); w.Code != 200 {
				t.Fatalf("first: %d %s", w.Code, w.Body.String())
			}
			target := "120363000000000001@g.us"
			if tc.secondChat {
				target = "5511999999999@s.whatsapp.net"
				b.IsOnWhatsApp = func(_ context.Context, _ []string) ([]types.IsOnWhatsAppResponse, error) {
					return []types.IsOnWhatsAppResponse{{IsIn: true, JID: types.NewJID("5511999999999", types.DefaultUserServer)}}, nil
				}
			}
			w := limitSend(t, b, target, false)
			var refusal struct {
				Error, Limit string
				Retry        int `json:"retry_after_s"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &refusal)
			if w.Code != 429 || refusal.Error != "send_rate_limited" || refusal.Limit != tc.reason || fmt.Sprint(refusal.Retry) != w.Header().Get("Retry-After") || calls.Load() != 1 {
				t.Fatalf("refusal %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
			}
			u, err := b.sendUsageSnapshot(context.Background())
			if err != nil || u.Today != 1 || u.RefusalsToday != 1 {
				t.Fatalf("usage=%+v err=%v", u, err)
			}
			now = now.Add(tc.wait)
			if w := limitSend(t, b, target, false); w.Code != 200 {
				t.Fatalf("refill: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSendLimitsConcurrentRestartDryRunAndDeny(t *testing.T) {
	b := newSettingsBridge(t)
	var calls atomic.Int64
	fakeLimitedSender(b, &calls)
	limitPatch(t, b, `{"send.rate_per_day":5}`)
	if w := limitSend(t, b, "120363000000000001@g.us", true); w.Code != 200 || calls.Load() != 0 {
		t.Fatal("dry run sent")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			w := limitSend(t, b, "120363000000000001@g.us", false)
			if w.Code != 200 && w.Code != 429 {
				t.Errorf("concurrent=%d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	if calls.Load() != 5 {
		t.Fatalf("sends=%d", calls.Load())
	}
	u, err := b.sendUsageSnapshot(context.Background())
	if err != nil || u.Today != 5 || u.NewChatsToday != 1 || u.RefusalsToday != 15 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	fresh := testBridge(t, b.currentClient(), ms, testLogger())
	fresh.RuntimeDefaults = b.RuntimeDefaults
	fakeLimitedSender(fresh, &calls)
	if w := limitSend(t, fresh, "120363000000000001@g.us", false); w.Code != 429 {
		t.Fatal("restart reset daily budget")
	}
	limitPatch(t, b, `{"tools.deny":["send_message","send_file","send_audio_message"]}`)
	if w := limitSend(t, b, "120363000000000001@g.us", false); w.Code != 403 {
		t.Fatal("tool deny lost")
	}
	b.ReadOnly = readOnlyPolicy{enabled: true}
	if w := limitSend(t, b, "120363000000000001@g.us", false); w.Code != 403 {
		t.Fatal("read-only lost")
	}
	u, _ = b.sendUsageSnapshot(context.Background())
	if u.Today != 5 || u.RefusalsToday != 16 {
		t.Fatalf("403 consumed budget=%+v", u)
	}
	metrics := b.renderMetrics()
	for _, want := range []string{"whatsapp_bridge_send_today 5", "whatsapp_bridge_send_new_chats_today 1", `whatsapp_bridge_send_refusals_total{reason="per_day"} 16`} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestSendRuntimeAllKeysAtomicAndNull(t *testing.T) {
	b := newSettingsBridge(t)
	for _, key := range []string{"rate_per_minute", "rate_per_day", "new_chats_per_day", "min_interval_ms"} {
		limitPatch(t, b, fmt.Sprintf(`{"send.%s":9}`, key))
		snapshot, _ := b.settingsSnapshot(context.Background())
		if snapshot.Settings["send."+key].Source != "runtime" || snapshot.Settings["send."+key].Value != int64(9) {
			t.Fatal(key)
		}
		limitPatch(t, b, fmt.Sprintf(`{"send.%s":null}`, key))
		snapshot, _ = b.settingsSnapshot(context.Background())
		if snapshot.Settings["send."+key].Source != "default" || snapshot.Settings["send."+key].Value != int64(0) {
			t.Fatal("null", key)
		}
	}
	before, _ := b.settingsSnapshot(context.Background())
	for _, value := range []string{"-1", "true", "1.5", "2147483648", "\"9\""} {
		w := httptest.NewRecorder()
		b.handleRuntimeSettings(w, httptest.NewRequest(http.MethodPatch, "/operator/v1/settings", strings.NewReader(`{"send.rate_per_day":4,"send.rate_per_minute":`+value+`}`)))
		after, _ := b.settingsSnapshot(context.Background())
		if w.Code != 400 || after.Version != before.Version {
			t.Fatal("partial invalid patch")
		}
	}
}

func TestSendFirstContactCanonicalHistoryAndForward(t *testing.T) {
	b := newSettingsBridge(t)
	var calls atomic.Int64
	fakeLimitedSender(b, &calls)
	b.IsOnWhatsApp = func(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
		return []types.IsOnWhatsAppResponse{{Query: phones[0], IsIn: true, JID: registeredLID, PhoneNumber: registeredJID}}, nil
	}
	limitPatch(t, b, `{"send.new_chats_per_day":1}`)
	for _, recipient := range []string{dialledNumber, registeredNumber, dialledJID.String()} {
		if w := limitSend(t, b, recipient, false); w.Code != 200 {
			t.Fatalf("canonical=%d %s", w.Code, w.Body.String())
		}
	}
	u, _ := b.sendUsageSnapshot(context.Background())
	if u.Today != 3 || u.NewChatsToday != 1 {
		t.Fatalf("alias usage=%+v", u)
	}
	if err := b.Store.StoreChat(efChat, "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreMessage(storedMessage{ID: "old", ChatJID: efChat, Sender: "5511999999999", Content: "hello", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b.IsOnWhatsApp = func(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
		return []types.IsOnWhatsAppResponse{{Query: phones[0], IsIn: true, JID: types.NewJID("5511999999999", types.DefaultUserServer)}}, nil
	}
	if w := limitSend(t, b, efChat, false); w.Code != 200 {
		t.Fatal("existing inbound history consumed first contact", w.Body.String())
	}
	limitPatch(t, b, `{"send.rate_per_day":4}`)
	request := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/forward", strings.NewReader(`{"chat_jid":"`+efChat+`","message_id":"old","to_chat_jid":"120363000000000001@g.us"}`))
	request.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	w := httptest.NewRecorder()
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, request)
	if w.Code != 429 || calls.Load() != 4 {
		t.Fatalf("forward bypass: %d calls=%d", w.Code, calls.Load())
	}
}

func TestSendMediaActionsAndGroupAddShareBudget(t *testing.T) {
	b := newSettingsBridge(t)
	var calls atomic.Int64
	fakeLimitedSender(b, &calls)
	limitPatch(t, b, `{"send.rate_per_day":1}`)
	root := t.TempDir()
	b.MediaRoots = []string{root}
	for _, file := range []string{"fake.jpg", "fake.ogg"} {
		path := filepath.Join(root, file)
		if err := os.WriteFile(path, []byte("fake media"), 0600); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]string{"recipient": "120363000000000001@g.us", "media_path": path})
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/send", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		w := httptest.NewRecorder()
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, r)
		if file == "fake.jpg" && w.Code != 200 || file == "fake.ogg" && w.Code != 429 {
			t.Fatal("media bypass", w.Code)
		}
	}
	var reactions atomic.Int64
	b.sendMessage = func(_ context.Context, _ types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
		reactions.Add(1)
		return whatsmeow.SendResponse{}, nil
	}
	react := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/react", strings.NewReader(`{"recipient":"120363000000000001@g.us","message_id":"old","sender_jid":"5511999999999@s.whatsapp.net","emoji":""}`))
		r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, r)
		return w
	}
	if w := react(); w.Code != 200 || reactions.Load() != 1 {
		t.Fatal("default action counted")
	}
	b.SendIncludeActions = true
	if w := react(); w.Code != 429 || reactions.Load() != 1 {
		t.Fatal("opted action bypassed")
	}
	groupCalls := &groupCalls{}
	ops := fakeGroupOps(groupCalls, nil)
	ops.allowSend = b.allowSend
	w := httptest.NewRecorder()
	handleGroupParticipants(ops, b.Policy)(w, httptest.NewRequest("POST", "/api/group/participants", strings.NewReader(`{"group_jid":"120363000000000001@g.us","action":"add","participants":["5511999999999"]}`)))
	if w.Code != 429 || len(groupCalls.participants) != 0 {
		t.Fatal("group additions bypassed")
	}
	limitPatch(t, b, `{"send.rate_per_day":3}`)
	w = httptest.NewRecorder()
	handleGroupParticipants(ops, b.Policy)(w, httptest.NewRequest("POST", "/api/group/participants", strings.NewReader(`{"group_jid":"120363000000000001@g.us","action":"add","participants":["5511999999999","5511988887777"]}`)))
	if w.Code != 200 || len(groupCalls.participants) != 2 {
		t.Fatalf("group batch %d %s", w.Code, w.Body.String())
	}
	u, _ := b.sendUsageSnapshot(context.Background())
	if u.Today != 3 {
		t.Fatalf("group weight=%d", u.Today)
	}
}

func TestSendFirstContactBatchTwinsAndMissingBackend(t *testing.T) {
	b := newSettingsBridge(t)
	b.Client = newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{registeredLID: registeredJID}, lidByPN: map[types.JID]types.JID{registeredJID: registeredLID}})
	limitPatch(t, b, `{"send.new_chats_per_day":1}`)
	reason, _, err := b.reserveSend(context.Background(), []string{registeredJID.String(), registeredLID.String()})
	if err != nil || reason != "" {
		t.Fatalf("twins charged twice: %s %v", reason, err)
	}
	u, _ := b.sendUsageSnapshot(context.Background())
	if u.NewChatsToday != 1 || u.Today != 2 {
		t.Fatalf("twins=%+v", u)
	}
	b.Client = newTestClient(&mockLIDStore{})
	b.Client.Store.LIDs = nil
	reason, _, err = b.reserveSend(context.Background(), []string{registeredJID.String()})
	if err != nil || reason != "" {
		t.Fatalf("optional backend: %s %v", reason, err)
	}
}

func TestSendCountedReceiptRefusalPreservesPrefix(t *testing.T) {
	b := newSettingsBridge(t)
	b.SendIncludeActions = true
	limitPatch(t, b, `{"send.rate_per_day":1}`)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, b.Store, markReadDM, nil)
	for i := range markReadBatch + 5 {
		seedMarkReadMessage(t, b.Store, markReadDM, fmt.Sprintf("DM-%03d", i), "5511999999999", base.Add(time.Duration(i)*time.Second), false, nil)
	}
	rec := &markReadRecorder{}
	deps := newMarkReadDeps(t, b.Store, rec)
	deps.allowSend = b.allowSendAction
	w := postMarkRead(t, deps, map[string]any{"chat_jid": markReadDM})
	if w.Code != 429 || len(rec.allIDs()) != markReadBatch {
		t.Fatalf("partial receipts=%d %d", w.Code, len(rec.allIDs()))
	}
	if marker := chatReadMarker(t, b.Store, markReadDM); !marker.Equal(base.Add(time.Duration(markReadBatch-1) * time.Second)) {
		t.Fatalf("prefix lost: %v", marker)
	}
	limitPatch(t, b, `{"send.rate_per_day":2}`)
	w = postMarkRead(t, deps, map[string]any{"chat_jid": markReadDM})
	if w.Code != 200 || len(rec.allIDs()) != markReadBatch+5 {
		t.Fatal("resend or missing final batch")
	}
}

func TestSendBudgetDatabaseBusyRefusesBeforeNetwork(t *testing.T) {
	store, lock := lockedProductionStore(t)
	b := testBridge(t, nil, store, testLogger())
	var sends atomic.Int64
	fakeLimitedSender(b, &sends)
	release := lock()
	w := limitSend(t, b, "120363000000000001@g.us", false)
	release()
	if w.Code != 503 || sends.Load() != 0 || !strings.Contains(w.Body.String(), "send_usage_unavailable") {
		t.Fatalf("busy budget allowed network: %d %d %s", w.Code, sends.Load(), w.Body.String())
	}
	u, err := b.sendUsageSnapshot(context.Background())
	if err != nil || u.Today != 0 {
		t.Fatalf("failed reservation consumed count: %+v %v", u, err)
	}
}

func TestSendUsageDoesNotBlockHealthWithHeldPool(t *testing.T) {
	b := newSettingsBridge(t)
	b.Store.db.SetMaxOpenConns(1)
	conn, err := b.Store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	server := httptest.NewServer(b.newRESTMux(8080, readOnlyTestToken))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	r, _ := http.NewRequest("GET", server.URL+"/api/health", nil)
	r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	r.Host = "127.0.0.1:8080"
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatalf("health failed while usage pool held: %d", response.StatusCode)
	}
}

type budgetBarrierLIDs struct {
	*mockLIDStore
	entered, release chan struct{}
}

func (l *budgetBarrierLIDs) GetLIDForPN(ctx context.Context, _ types.JID) (types.JID, error) {
	close(l.entered)
	select {
	case <-l.release:
		return types.EmptyJID, nil
	case <-ctx.Done():
		return types.EmptyJID, ctx.Err()
	}
}

func TestSendReservationOwnsWALWriterBeforeReads(t *testing.T) {
	b := newSettingsBridge(t)
	if err := b.Store.StoreChat(phonePN.String(), "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	lids := &budgetBarrierLIDs{mockLIDStore: &mockLIDStore{}, entered: make(chan struct{}), release: make(chan struct{})}
	b.Client = newTestClient(lids)
	writer, err := b.Store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.ExecContext(context.Background(), "PRAGMA busy_timeout=5"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := b.reserveSend(ctx, []string{phonePN.String()}); done <- err }()
	select {
	case <-lids.entered:
	case <-ctx.Done():
		close(lids.release)
		t.Fatal("admission did not reach identity barrier")
	}
	_, competing := writer.ExecContext(ctx, "UPDATE chats SET name='Bob'")
	close(lids.release)
	if err := <-done; err != nil || !isBusyError(competing) {
		t.Fatalf("stale snapshot or missing writer ownership: admission=%v competing=%v", err, competing)
	}
	if _, err := writer.ExecContext(ctx, "UPDATE chats SET name='Bob'"); err != nil {
		t.Fatalf("reservation did not release WAL writer: %v", err)
	}
	u, err := b.sendUsageSnapshot(ctx)
	if err != nil || u.Today != 1 {
		t.Fatalf("atomic reservation=%+v %v", u, err)
	}
}

func TestSendUsageReadsWhileWALWriterHeld(t *testing.T) {
	b := newSettingsBridge(t)
	writer, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	h := newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{sendUsage: b.handleSendUsage}, b.Log)
	server := httptest.NewServer(h)
	defer func() { _ = writer.Rollback(); server.Close() }()
	client := server.Client()
	client.Timeout = time.Second
	r, _ := http.NewRequest("GET", server.URL+"/operator/v1/send/usage", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	r.Host = "127.0.0.1:8090"
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var usage sendUsage
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil || response.StatusCode != 200 || usage.Today != 0 {
		t.Fatalf("usage blocked or inconsistent: %d %+v %v", response.StatusCode, usage, err)
	}
}
