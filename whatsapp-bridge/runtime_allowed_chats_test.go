package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func settingsChats(t *testing.T, b *Bridge, deploy string) {
	t.Helper()
	var err error
	b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
		if name == chatPolicyEnv {
			return deploy
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Policy = b.runtimeChatPolicy(parseChatPolicy(deploy))
}

func TestRuntimeChatsHTTPFloorEmptyClearRestartAndPrivateAudit(t *testing.T) {
	const alice = "5511999999999@s.whatsapp.net"
	const bob = "5511888888888@s.whatsapp.net"
	for _, tc := range []struct {
		deploy, raw string
		want        []string
	}{
		{"", `[" 5511999999999 "]`, []string{alice}},
		{alice, `["*@s.whatsapp.net"]`, []string{alice}},
		{"*@g.us", `["5511999999999","120363000000000001@G.US"]`, []string{mgGroup}},
		{alice, `["5511888888888"]`, []string{}},
		{alice, `["*@g.us"]`, []string{}},
		{"*@g.us", `["*@g.us"]`, []string{"*@g.us"}},
	} {
		t.Run(tc.deploy+tc.raw, func(t *testing.T) {
			b := newSettingsBridge(t)
			settingsChats(t, b, tc.deploy)
			log := &recordingLogger{}
			b.Log = log
			server := settingsServer(t, b)
			code, initial := settingsRequest(t, server, "GET", "")
			if code != 200 {
				t.Fatal(code)
			}
			code, result := settingsRequest(t, server, "PATCH", `{"chats.allowed":`+tc.raw+`}`)
			if code != 200 {
				t.Fatal(code)
			}
			want, _ := json.Marshal(tc.want)
			got, _ := json.Marshal(result.Settings["chats.allowed"].Value)
			if string(got) != string(want) || result.Settings["chats.allowed"].Source != "runtime" {
				t.Fatalf("effective=%s", got)
			}
			for _, invalid := range []string{`[]`, `"5511999999999"`, `[""]`, `[" "]`, `[1]`, `["5511999999999,5511888888888"]`} {
				status, _ := settingsRequest(t, server, "PATCH", `{"tools.deny":["send_message"],"chats.allowed":`+invalid+`}`)
				if status != 400 {
					t.Fatalf("invalid=%s status=%d", invalid, status)
				}
			}
			saved, err := b.settingsSnapshot(context.Background())
			if err != nil || saved.Version != result.Version {
				t.Fatalf("non-atomic validation: %v", err)
			}
			reopened := testBridge(t, b.Client, b.Store, testLogger())
			settingsChats(t, reopened, tc.deploy)
			snapshot, err := reopened.settingsSnapshot(context.Background())
			if err != nil || !reflect.DeepEqual(snapshot.Settings["chats.allowed"], saved.Settings["chats.allowed"]) {
				t.Fatal("restart lost override")
			}
			if len(tc.want) == 0 && (reopened.Policy.Allows(alice) || reopened.Policy.Allows(bob)) {
				t.Fatal("empty intersection widened policy")
			}
			if status, cleared := settingsRequest(t, server, "PATCH", `{"chats.allowed":null}`); status != 200 || !reflect.DeepEqual(cleared.Settings["chats.allowed"], initial.Settings["chats.allowed"]) {
				t.Fatal("clear did not restore deployment")
			}
			if !strings.Contains(log.String(), "key=chats.allowed source=") {
				t.Fatal("audit missing")
			}
			for _, private := range []string{alice, bob, mgGroup, "5511999999999", "5511888888888"} {
				if strings.Contains(log.String(), private) {
					t.Fatal("chat entry leaked in audit")
				}
			}
		})
	}
}

func TestRuntimeChatsCapturedRESTHandlersAndParticipantOperations(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	b.Connected = func() bool { return true }
	mux := b.newRESTMux(8080, sendRecipientToken) // Construct before PATCH.
	server := settingsServer(t, b)
	if status, _ := settingsRequest(t, server, "PATCH", `{"chats.allowed":["5511999999999"]}`); status != 200 {
		t.Fatal(status)
	}
	const bob = "5511888888888@s.whatsapp.net"
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/send", `{"recipient":"` + bob + `","message":"fake text"}`},
		{"POST", "/api/typing", `{"recipient":"` + bob + `"}`},
		{"POST", "/api/react", `{"recipient":"` + bob + `","message_id":"B1","emoji":""}`},
		{"POST", "/api/mark-read", `{"chat_jid":"` + bob + `","message_ids":["B1"]}`},
		{"POST", "/api/edit", `{"chat_jid":"` + bob + `","message_id":"B1","text":"fake text"}`},
		{"POST", "/api/forward", `{"chat_jid":"` + bob + `","message_id":"B1","to_chat_jid":"5511999999999@s.whatsapp.net"}`},
		{"POST", "/api/delete", `{"chat_jid":"` + bob + `","message_id":"B1"}`},
		{"POST", "/api/history", `{"chat_jid":"` + bob + `"}`},
		{"POST", "/api/download", `{"chat_jid":"` + bob + `","message_id":"B1"}`},
		{"POST", "/api/media/purge", `{"chat_jid":"` + bob + `"}`},
		{"POST", "/api/chat/archive", `{"chat_jid":"` + bob + `","archived":true}`},
		{"POST", "/api/chat/label", `{"chat_jid":"` + bob + `","label_id":"1","labeled":true}`},
		{"GET", "/api/media/blob?chat_jid=" + bob + "&message_id=B1", ""},
		{"GET", "/api/media/cache?chat_jid=" + bob + "&message_id=B1", ""},
		{"GET", "/api/labels?chat_jid=" + bob, ""},
		{"GET", "/api/poll?chat_jid=" + bob + "&message_id=B1", ""},
		{"GET", "/api/group/members?jid=" + mgGroup, ""},
		{"POST", "/api/group/participants", `{"group_jid":"` + mgGroup + `","participants":["5511888888888"],"action":"add"}`},
		{"POST", "/api/group/subject", `{"group_jid":"` + mgGroup + `","name":"fake group"}`},
		{"POST", "/api/group/invite", `{"group_jid":"` + mgGroup + `"}`},
		{"POST", "/api/group/leave", `{"group_jid":"` + mgGroup + `"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, seamRequest(tc.method, tc.path, tc.body, sendRecipientToken))
			if rec.Code != 403 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	// The group remains allowed while its participants are narrowed dynamically.
	calls := &groupCalls{}
	handler := handleGroupParticipants(fakeGroupOps(calls, nil), b.Policy)
	for _, action := range []string{"add", "promote", "remove", "demote"} {
		if status, _ := settingsRequest(t, server, "PATCH", `{"chats.allowed":["`+mgGroup+`","5511999999999"]}`); status != 200 {
			t.Fatal(status)
		}
		code, _ := groupPost(t, handler, `{"group_jid":"`+mgGroup+`","participants":["5511888888888"],"action":"`+action+`"}`)
		want := 200
		if action == "add" || action == "promote" {
			want = 403
		}
		if code != want {
			t.Fatalf("%s=%d", action, code)
		}
	}
}

func TestRuntimeChatsNextMessageBothWebhookSenders(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	settingsChats(t, b, "")
	configureWebhookHTTP(t, server, destination)
	legacy := make(chan string, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		legacy <- string(data)
		w.WriteHeader(204)
	}))
	t.Cleanup(receiver.Close)
	b.Webhook.enabled, b.Webhook.url, b.Webhook.client = true, receiver.URL, receiver.Client()
	send := func(chat types.JID) {
		b.handleMessage(buildTextMessage(chat, chat, types.EmptyJID, types.EmptyJID, false, "fake text"))
	}
	delivered := func(chat string) {
		r := receiveWebhook(t, receipts)
		var event runtimeWebhookEvent
		if err := json.Unmarshal(r.body, &event); err != nil || event.Chat != chat {
			t.Fatal("wrong runtime source forwarded")
		}
		select {
		case body := <-legacy:
			var payload WebhookPayload
			if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.ChatJID != chat {
				t.Fatal("wrong legacy source forwarded")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("legacy sender did not deliver")
		}
	}
	send(phonePN)
	delivered(phonePN.String())
	if code, _ := settingsRequest(t, server, "PATCH", `{"chats.allowed":["120363000000000001@g.us"]}`); code != 200 {
		t.Fatal(code)
	}
	send(phonePN) // Denied source is stored but forwarded to neither sender.
	group := types.NewJID("120363000000000001", types.GroupServer)
	send(group)
	delivered(group.String())
	select {
	case <-legacy:
		t.Fatal("denied legacy delivery")
	case <-receipts:
		t.Fatal("denied runtime delivery")
	default:
	}
	if code, _ := settingsRequest(t, server, "PATCH", `{"chats.allowed":null}`); code != 200 {
		t.Fatal(code)
	}
	send(phonePN)
	delivered(phonePN.String())
}

func TestRuntimeChatsCorruptRowAndReadFailureRefuse(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	for _, raw := range []string{"invalid-json", "[]", `[""]`} {
		if _, err := b.Store.db.Exec("INSERT OR REPLACE INTO runtime_settings VALUES ('chats.allowed',?,'2026-10-09',1)", raw); err != nil {
			t.Fatal(err)
		}
		if b.Policy.Allows(phonePN.String()) {
			t.Fatal("corrupt override allowed chat")
		}
	}
	if _, err := b.Store.db.Exec("DROP TABLE runtime_settings"); err != nil {
		t.Fatal(err)
	}
	if b.Policy.Allows(phonePN.String()) {
		t.Fatal("missing table lifted restriction")
	}
}

func patchAllowedChats(t *testing.T, server *httptest.Server, entries ...string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"chats.allowed": entries})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := settingsRequest(t, server, "PATCH", string(body)); status != 200 {
		t.Fatal(status)
	}
}

type runtimePatchBody struct {
	io.Reader
	change func()
}

func (body *runtimePatchBody) Read(buf []byte) (int, error) {
	if body.change != nil {
		change := body.change
		body.change = nil
		change()
	}
	return body.Reader.Read(buf)
}

func TestRuntimeChatsRequestPinsTypedRegisteredAndForwardTargets(t *testing.T) {
	for _, mode := range []string{"send", "forward"} {
		for _, phase := range []string{"body", "lookup"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				ask := registeredWithoutNinthDigit()
				b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
				settingsChats(t, b, "")
				server := settingsServer(t, b)
				patchAllowedChats(t, server, mgGroup, dialledNumber)
				changed := false
				change := func() {
					if !changed {
						changed = true
						patchAllowedChats(t, server, mgGroup, registeredNumber)
					}
				}
				b.IsOnWhatsApp = func(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
					if phase == "lookup" {
						change()
					}
					return ask.ask(ctx, phones)
				}
				if err := b.Store.StoreMessage(storedMessage{ID: "FORWARD1", ChatJID: mgGroup, Sender: phonePN.String(), Content: "fake text", Timestamp: time.Now()}); err != nil {
					t.Fatal(err)
				}
				mux := b.newRESTMux(8080, sendRecipientToken)
				path, body := "/api/send", `{"recipient":"`+dialledNumber+`","message":"fake text"}`
				if mode == "forward" {
					path, body = "/api/forward", `{"chat_jid":"`+mgGroup+`","message_id":"FORWARD1","to_chat_jid":"`+dialledNumber+`"}`
				}
				request := func() *httptest.ResponseRecorder {
					req := seamRequest("POST", path, body, sendRecipientToken)
					if phase == "body" {
						req.Body = io.NopCloser(&runtimePatchBody{Reader: req.Body, change: change})
					}
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, req)
					return rec
				}
				// PATCH occurs after admission, either on body read or in network lookup.
				// Neither version permits both typed and registered spellings.
				for range 2 {
					rec := request()
					if rec.Code != 403 || len(*sent) != 0 {
						t.Fatalf("mixed policy authorized %s: status=%d sends=%d", mode, rec.Code, len(*sent))
					}
				}
				if !changed {
					t.Fatal("PATCH did not occur inside the request")
				}
				patchAllowedChats(t, server, mgGroup, dialledNumber, registeredNumber)
				rec := request()
				if rec.Code != 200 || len(*sent) != 1 || (*sent)[0] != registeredJID.String() {
					t.Fatalf("fresh request did not refresh: status=%d sends=%v", rec.Code, *sent)
				}
			})
		}
	}
}

func TestRuntimeChatsParticipantBatchCannotCombineVersions(t *testing.T) {
	b := newSettingsBridge(t)
	settingsChats(t, b, "")
	server := settingsServer(t, b)
	patchAllowedChats(t, server, mgGroup, phoneLID.String())
	calls := &groupCalls{}
	ops := fakeGroupOps(calls, nil)
	changed := false
	const bob = "5511888888888@s.whatsapp.net"
	ops.twin = func(_ context.Context, jid types.JID) (types.JID, error) {
		if jid == phoneLID {
			if !changed {
				changed = true
				patchAllowedChats(t, server, mgGroup, bob)
			}
			return phonePN, nil
		}
		return types.EmptyJID, nil
	}
	handler := b.runtimePolicyGuard(handleGroupParticipants(ops, b.Policy))
	body := `{"group_jid":"` + mgGroup + `","participants":["` + phonePN.String() + `","` + bob + `"],"action":"add"}`
	code, _ := groupPost(t, handler, body)
	if code != 403 || !changed || len(calls.participants) != 0 {
		t.Fatalf("mixed batch: status=%d changed=%t effects=%d", code, changed, len(calls.participants))
	}
	patchAllowedChats(t, server, mgGroup, phonePN.String(), bob)
	code, _ = groupPost(t, handler, body)
	if code != 200 || len(calls.participants) != 2 {
		t.Fatalf("fresh batch did not refresh: status=%d effects=%d", code, len(calls.participants))
	}
}

func TestRuntimeChatsToolAndChatGuardsShareAtomicSnapshot(t *testing.T) {
	b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
	settingsChats(t, b, "")
	server := settingsServer(t, b)
	patch := func(chats []string, denied []string) {
		body, err := json.Marshal(map[string]any{"chats.allowed": chats, "tools.deny": denied})
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := settingsRequest(t, server, "PATCH", string(body)); status != 200 {
			t.Fatal(status)
		}
	}
	const otherGroup = "120363000000000002@g.us"
	patch([]string{mgGroup}, endpointTools["/api/send"])
	changed := false
	send := b.runtimeToolGuard(b.handleSend(b.MediaRoots))
	handler := b.runtimePolicyGuard(func(w http.ResponseWriter, r *http.Request) {
		if !changed {
			changed = true
			patch([]string{otherGroup}, []string{})
		}
		send(w, r)
	})
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler(rec, seamRequest("POST", "/api/send", `{"recipient":"`+mgGroup+`","message":"fake text"}`, sendRecipientToken))
		return rec
	}
	for range 2 {
		rec := request()
		if rec.Code != 403 || len(*sent) != 0 {
			t.Fatalf("combined permissions: status=%d sends=%d", rec.Code, len(*sent))
		}
	}
	patch([]string{mgGroup}, []string{})
	rec := request()
	if rec.Code != 200 || len(*sent) != 1 {
		t.Fatalf("joint permission did not refresh: status=%d sends=%d", rec.Code, len(*sent))
	}
}
