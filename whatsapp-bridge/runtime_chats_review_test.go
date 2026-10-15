package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const reviewOtherGroup = "120363000000000002@g.us"

func reviewRequest(b *Bridge, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(method, path, body, sendRecipientToken))
	return rec
}

// The request is admitted with limits off; the PATCH lands while its body is
// read, and a busy writer leaves the reservation without a snapshot of its own.
func TestSendLimitEnabledMidSendIsNotSkipped(t *testing.T) {
	store, lock := lockedProductionStore(t)
	b := testBridge(t, nil, store, testLogger())
	b.RuntimeDefaults, _ = runtimeDefaults(func(string) string { return "" })
	var sends atomic.Int64
	fakeLimitedSender(b, &sends)
	release := func() {}
	req := seamRequest("POST", "/api/send", `{"recipient":"`+mgGroup+`","message":"fake text"}`, sendRecipientToken)
	req.Body = io.NopCloser(&runtimePatchBody{Reader: req.Body, change: func() {
		limitPatch(t, b, `{"send.rate_per_minute":1}`)
		release = lock()
	}})
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, req)
	release()
	if rec.Code != 503 || sends.Load() != 0 || b.sendCountSkipped.Load() != 0 {
		t.Fatalf("limit enabled mid-send was skipped: status=%d sends=%d skipped=%d", rec.Code, sends.Load(), b.sendCountSkipped.Load())
	}
}

func TestRuntimeChatPolicyReadFailureIsLoggedAndCounted(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	limitPatch(t, b, `{"chats.allowed":["5511999999999"]}`)
	log := &recordingLogger{}
	b.Log = log
	if _, err := b.Store.db.Exec("DROP TABLE runtime_settings"); err != nil {
		t.Fatal(err)
	}
	denied := func() {
		if b.Policy.once().effective(t.Context()).Allows(phonePN.String()) {
			t.Fatal("unreadable policy allowed a chat")
		}
	}
	for range 3 {
		denied()
	}
	b.policyFailLogged = time.Now().Add(-2 * time.Minute)
	denied()
	out := log.String()
	if strings.Count(out, "[ERROR] Runtime chat policy unreadable") != 2 || !strings.Contains(out, "unreadable 1 time(s)") || !strings.Contains(out, "unreadable 3 time(s)") || strings.Contains(out, "5511999999999") {
		t.Fatalf("read failures not reported once a minute with the count: %q", out)
	}
	if !strings.Contains(b.renderMetrics(), "whatsapp_bridge_chat_policy_read_failures_total 4") {
		t.Fatal("read failures not counted")
	}
}

func TestChatRefusalAndSummaryNameTheirOrigin(t *testing.T) {
	typing := `{"recipient":"5511888888888@s.whatsapp.net"}`
	for deploy, want := range map[string]string{"": "runtime chats.allowed", "*@g.us": "runtime chats.allowed within " + chatPolicyEnv + " (deploy)"} {
		b := newSettingsBridge(t)
		settingsChats(t, b, deploy)
		b.Connected = func() bool { return true }
		before, summary := reviewRequest(b, "POST", "/api/typing", typing), b.Policy.once().effective(t.Context()).Summary()
		if deploy == "" && (before.Code == 403 || summary != chatPolicyEnv+" unset: all chats allowed") {
			t.Fatalf("unset policy changed: %d %q", before.Code, summary)
		}
		if deploy != "" && (!strings.Contains(before.Body.String(), "is not in "+chatPolicyEnv+" (") || summary != chatPolicyEnv+": restricted to 0 chat(s) + *@g.us") {
			t.Fatalf("deploy-only wording changed: %s %q", before.Body.String(), summary)
		}
		limitPatch(t, b, `{"chats.allowed":["`+mgGroup+`","*@g.us"]}`)
		rec, summary := reviewRequest(b, "POST", "/api/typing", typing), b.Policy.once().effective(t.Context()).Summary()
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "is not in "+want+" (") {
			t.Fatalf("refusal blames the wrong list: %d %s", rec.Code, rec.Body.String())
		}
		if summary != want+": restricted to 1 chat(s) + 1 wildcard(s)" {
			t.Fatalf("summary=%q", summary)
		}
	}
}

func TestRuntimeChatsPatchRefusesEntriesThatMatchNothing(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	server := settingsServer(t, b)
	for _, entry := range []string{"+55 11 99999-9999", "a@b@c", "@", "*@", "@g.us", "5511999999999@", "*", "1.2@lid"} {
		body, _ := json.Marshal(map[string]any{"chats.allowed": []string{"5511999999999", entry}, "tools.deny": []string{"send_message"}})
		if status, _ := settingsRequest(t, server, "PATCH", string(body)); status != 400 {
			t.Fatalf("entry %q accepted: %d", entry, status)
		}
	}
	saved, err := b.settingsSnapshot(context.Background())
	if err != nil || saved.Version != 0 || saved.Settings["chats.allowed"].Source != "default" || !b.Policy.once().effective(t.Context()).Allows("5511888888888") {
		t.Fatalf("refused PATCH wrote something: %+v %v", saved.Settings["chats.allowed"], err)
	}
	patchAllowedChats(t, server, " 5511999999999 ", "5511888888888:12@s.whatsapp.net", "*@G.US", "status@broadcast", phoneLID.String())
}

func TestBackgroundConsumersSharePolicySnapshotAndRecheckRosterWrites(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(receiver.Close)
	b.Webhook.enabled, b.Webhook.url, b.Webhook.client = true, receiver.URL, receiver.Client()
	b.Connected = func() bool { return true }
	b.GroupRosterSync = groupRosterSyncInterval
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { return fakeGroup(), nil }
	for _, jid := range []string{mgGroup, reviewOtherGroup} {
		if err := b.Store.StoreChat(jid, "fake group", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	resolve := b.Policy.resolve
	b.Policy.resolve = func() chatPolicy { reads++; return resolve() }
	// One read for the pass, plus one fresh authorization check after each
	// of the two network fetches, before settings/participants are persisted.
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 2 || reads != 3 {
		t.Fatalf("roster pass: refreshed=%d policy reads=%d", n, reads)
	}
	reads = 0
	group, _ := types.ParseJID(mgGroup)
	b.handleMessage(buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "fake text"))
	if reads != 1 {
		t.Fatalf("one inbound group message read the policy %d times", reads)
	}
}

// Which routes skip the pinned snapshot is decided at registration: the guard
// itself pins whatever it wraps, whatever the path says.
func TestPolicyPinExemptionsLiveInTheRouteTable(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	if _, err := b.Store.db.Exec("DROP TABLE runtime_settings"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/me", "/api/health", "/api/ready"} {
		if rec := reviewRequest(b, "GET", path, ""); strings.Contains(rec.Body.String(), "settings_unavailable") {
			t.Fatalf("%s needs the settings table: %d", path, rec.Code)
		}
		rec := httptest.NewRecorder()
		b.runtimePolicyGuard(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })(rec, seamRequest("GET", path, "", sendRecipientToken))
		if rec.Code != 503 {
			t.Fatalf("the guard exempted %s by its path: %d", path, rec.Code)
		}
	}
	if rec := reviewRequest(b, "GET", "/api/poll?chat_jid="+mgGroup+"&message_id=B1", ""); rec.Code != 503 {
		t.Fatalf("pinned route answered without the settings table: %d", rec.Code)
	}
}

func TestDirectMessageWithoutWebhooksDoesNotResolveChatPolicy(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	b.Webhook.enabled = false
	b.RuntimeWebhook = nil
	reads := 0
	original := b.Policy.resolve
	b.Policy.resolve = func() chatPolicy { reads++; return original() }
	b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "fake text"))
	if reads != 0 {
		t.Fatalf("DM without webhooks read its chat policy %d times", reads)
	}
	var messages int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("DM was not persisted: messages=%d err=%v", messages, err)
	}
}
