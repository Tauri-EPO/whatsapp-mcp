package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestChatPolicyRefusesAmbiguousJIDs(t *testing.T) {
	for _, raw := range []string{"", "*@g.us", "*@s.whatsapp.net", "5511888888888:1@lid@s.whatsapp.net", "*@g.us@s.whatsapp.net"} {
		policy := parseChatPolicy(raw)
		for _, target := range []string{"5511888888888@s.whatsapp.net@g.us", "120363000000000002@g.us@s.whatsapp.net"} {
			if policy.Allows(target) {
				t.Fatalf("policy %q allowed %q", raw, target)
			}
		}
		if raw != "" && !policy.restricted {
			t.Fatalf("invalid entries made %q unrestricted", raw)
		}
	}
	if parseChatPolicy("5511888888888:1@lid@s.whatsapp.net").Allows("5511888888888@s.whatsapp.net") {
		t.Fatal("invalid configuration gained a valid alias")
	}
}

func TestInvalidEntryWarningDoesNotExposeValue(t *testing.T) {
	raw := "5511888888888:1@lid@s.whatsapp.net"
	policy := parseChatPolicy("," + raw + "," + raw)
	logger := installRecordingLogger(t)
	policy.warnInvalidEntries(logger)
	log := logger.String()
	if strings.Count(log, "malformed entries") != 1 || !strings.Contains(log, "[2 3]") || strings.Contains(log, raw) || !strings.Contains(policy.Summary(), "restricted to 0 chat(s)") || !strings.Contains(policy.Summary(), "2 invalid entry(s)") {
		t.Fatalf("warning=%q summary=%q", log, policy.Summary())
	}
}

func TestHistoryAndDownloadRefuseWellFormedDisallowedChat(t *testing.T) {
	for _, path := range []string{"/api/history", "/api/download"} {
		b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, &fakeIsOnWhatsApp{})
		b.Policy = parseChatPolicy("*@g.us")
		b.Connected = func() bool { t.Fatal("connection before policy denial"); return false }
		b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
			t.Fatal("download before policy denial")
			return false, "", "", "", nil
		}
		rec := httptest.NewRecorder()
		body := `{"chat_jid":"5511888888888@s.whatsapp.net","message_id":"MSG1","count":1}`
		b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, path, body, sendRecipientToken))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRESTAmbiguousChatDeniedBeforeEffects(t *testing.T) {
	for _, allow := range []string{"", "*@g.us", "*@s.whatsapp.net", efChat + ",120363000000000001@g.us"} {
		t.Run(allow, func(t *testing.T) {
			ask := &fakeIsOnWhatsApp{}
			b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
			b.Policy = parseChatPolicy(allow)
			b.Connected = func() bool { t.Fatal("connection touched before denial"); return false }
			b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
				t.Fatal("download touched before denial")
				return false, "", "", "", nil
			}
			mux := b.newRESTMux(8080, sendRecipientToken)
			source := "120363000000000001@g.us"
			if allow == "*@s.whatsapp.net" || strings.Contains(allow, efChat) {
				source = efChat
			}
			var targets []struct {
				raw    string
				status int
			}
			for _, raw := range []string{"5511888888888@s.whatsapp.net@g.us", "120363000000000002@g.us@s.whatsapp.net", "120363000000000002@g.us@g.us", "5511888888888@s.whatsapp.net@s.whatsapp.net", "@g.us", "@lid", "@broadcast", "@newsletter", "@s.whatsapp.net", "g.us", "lid", "broadcast", "newsletter", "s.whatsapp.net", "5511888888888@", "5511999999999:1.2@s.whatsapp.net", "5511999999999.1.2@s.whatsapp.net", "5511999999999:3@s.whatsapp.net", "5511999999999.0@s.whatsapp.net", "5511999999999.0:1@s.whatsapp.net", "5511999999999.256@s.whatsapp.net", "111222333444555:3@lid", "111222333444555.0@lid", strings.Replace("120363000000000001@g.us", "@", ":3@", 1), strings.Replace("120363000000000001@g.us", "@", ".0@", 1)} {
				targets = append(targets, struct {
					raw    string
					status int
				}{raw, http.StatusBadRequest})
			}
			for _, outside := range []string{"5511888888888@s.whatsapp.net", "120363000000000002@g.us"} {
				if allow != "" && !b.Policy.Allows(outside) {
					targets = append(targets, struct {
						raw    string
						status int
					}{outside, http.StatusForbidden})
				}
			}
			for _, targetCase := range targets {
				target := targetCase.raw
				for _, route := range []struct{ path, field, extra string }{
					{"/api/send", "recipient", `,"message":"hello"`},
					{"/api/react", "recipient", `,"message_id":"MSG1","emoji":"","from_me":true`},
					{"/api/typing", "recipient", `,"is_typing":true`},
					{"/api/mark-read", "chat_jid", `,"message_ids":["MSG1"],"sender_jid":"5511999999999@s.whatsapp.net"`},
					{"/api/mark-read", "chat_jid", `,"up_to":"2026-10-07T12:00:00Z"`},
					{"/api/edit", "chat_jid", `,"message_id":"MSG1","text":"edited"`},
					{"/api/forward", "chat_jid", `,"message_id":"MSG1","to_chat_jid":"` + source + `"`},
					{"/api/forward", "to_chat_jid", `,"message_id":"MSG1","chat_jid":"` + source + `"`},
					{"/api/delete", "chat_jid", `,"message_id":"MSG1","for_everyone":false`},
					{"/api/delete", "chat_jid", `,"message_id":"MSG1","for_everyone":true`},
					{"/api/group/participants", "group_jid", `,"action":"add","participants":["5511999999999"]`},
					{"/api/group/subject", "group_jid", `,"name":"Example"`},
					{"/api/group/invite", "group_jid", `,"reset":false`},
					{"/api/group/leave", "group_jid", ""},
					{"/api/group/members", "group_jid", ""},
					{"/api/group/members?jid=", "group_jid", ""},
					{"/api/chat/archive", "chat_jid", `,"archived":true`},
					{"/api/download", "chat_jid", `,"message_id":"MSG1"`},
					{"/api/history", "chat_jid", `,"count":1`},
					{"/api/media/purge", "chat_jid", `,"dry_run":false`},
					{"/api/poll", "chat_jid", ""},
				} {
					body := `{"` + route.field + `":"` + target + `"` + route.extra + `}`
					method, path := http.MethodPost, route.path
					if strings.HasSuffix(path, "?jid=") {
						method = http.MethodGet
						path += url.QueryEscape(target)
					}
					if path == "/api/poll" {
						method = http.MethodGet
						path += "?message_id=MSG1&chat_jid=" + url.QueryEscape(target)
					}
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, seamRequest(method, path, body, sendRecipientToken))
					if rec.Code != targetCase.status {
						t.Fatalf("%s/%s policy=%q target=%q: %d %s", route.path, route.field, allow, target, rec.Code, rec.Body.String())
					}
					if targetCase.status == http.StatusBadRequest && decodeAPIError(t, rec).Error.Code != "invalid_argument" {
						t.Fatal("malformed target must be invalid_argument")
					}
					// A well-formed allowed target must pass authorization, even
					// when later validation or the disconnected client refuses it.
					b.Connected = func() bool { return false }
					valid := source
					if route.field == "group_jid" {
						valid = "120363000000000001@g.us"
					}
					for _, valid := range []string{valid, strings.ToUpper(valid), valid + "\n", " \t" + valid + "\n"} {
						b.Connected = func() bool { return false }
						encoded, _ := json.Marshal(valid)
						control := `{"` + route.field + `":` + string(encoded) + route.extra + `}`
						controlPath := route.path
						if strings.HasSuffix(controlPath, "?jid=") {
							controlPath += url.QueryEscape(valid)
						}
						if route.path == "/api/poll" {
							controlPath += "?message_id=MSG1&chat_jid=" + url.QueryEscape(valid)
						}
						positive := httptest.NewRecorder()
						self := b.Client.Store.ID
						if route.path == "/api/react" {
							// A permitted reaction reaches the pairing check; it does
							// not need the mock client's uninitialized network internals.
							b.Client.Store.ID = nil
						}
						controlMux := mux
						if route.field == "group_jid" && allow == "*@s.whatsapp.net" {
							b.Policy = parseChatPolicy("*@g.us")
							controlMux = b.newRESTMux(8080, sendRecipientToken)
						}
						controlMux.ServeHTTP(positive, seamRequest(method, controlPath, control, sendRecipientToken))
						b.Client.Store.ID = self
						b.Policy = parseChatPolicy(allow)
						if positive.Code == http.StatusForbidden || positive.Code == http.StatusBadRequest {
							t.Fatalf("allowed control refused: %s/%s policy=%q body=%s", route.path, route.field, allow, positive.Body.String())
						}
						b.Connected = func() bool { t.Fatal("connection touched before denial"); return false }
					}
				}
			}
			if len(ask.calls) != 0 || len(*sent) != 0 {
				t.Fatalf("effects: queries=%v sent=%v", ask.calls, *sent)
			}
		})
	}
}

// Observe the real parser behind a forwarded destination, not just its raw spelling.
func TestForwardAmbiguousDestinationNeverReachesSend(t *testing.T) {
	for _, allow := range []string{"*@g.us", "*@s.whatsapp.net"} {
		t.Run(allow, func(t *testing.T) {
			known, _ := types.ParseJID(efChat)
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{known: registeredLID}}, &fakeIsOnWhatsApp{})
			b.Policy = parseChatPolicy(allow)
			source, target := "120363000000000001@g.us", "5511888888888@s.whatsapp.net@g.us"
			if allow == "*@s.whatsapp.net" {
				source, target = efChat, "120363000000000002@g.us@s.whatsapp.net"
			}
			if err := b.Store.StoreMessage(storedMessage{
				ID:         "SOURCE1",
				ChatJID:    source,
				Sender:     "5511999999999",
				Content:    "hello",
				Timestamp:  time.Now(),
				FileLength: 0,
			}); err != nil {
				t.Fatal(err)
			}
			var addressed []types.JID
			b.Send = func(_ context.Context, to, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
				jid, err := parseRecipientJID(to)
				if err != nil {
					t.Fatal(err)
				}
				addressed = append(addressed, jid)
				return true, "sent", sentMessage{ID: "OUT1", ChatJID: jid.String()}
			}
			body, _ := json.Marshal(map[string]string{"chat_jid": source, "message_id": "SOURCE1", "to_chat_jid": target})
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/forward", string(body), sendRecipientToken))
			if rec.Code != http.StatusBadRequest || len(addressed) != 0 {
				t.Fatalf("status=%d body=%s actual addressed JIDs=%v", rec.Code, strings.TrimSpace(rec.Body.String()), addressed)
			}
			body, _ = json.Marshal(map[string]string{"chat_jid": source, "message_id": "SOURCE1", "to_chat_jid": source})
			rec = httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/forward", string(body), sendRecipientToken))
			if rec.Code != http.StatusOK || len(addressed) != 1 || addressed[0].String() != source {
				t.Fatalf("well-formed destination: status=%d body=%s addressed=%v", rec.Code, rec.Body.String(), addressed)
			}
		})
	}
}

func TestPurgeAmbiguousItemDeniedBeforeStoreLookup(t *testing.T) {
	for _, allow := range []string{"", "*@g.us", "*@s.whatsapp.net"} {
		b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, &fakeIsOnWhatsApp{})
		b.Policy = parseChatPolicy(allow)
		mux := b.newRESTMux(8080, sendRecipientToken)
		// A policy rejection must not need the store or probe the file cache.
		b.Store = nil
		for _, target := range []string{"5511888888888@s.whatsapp.net@g.us", "120363000000000002@g.us@s.whatsapp.net"} {
			body := `{"items":[{"message_id":"MSG1","chat_jid":"` + target + `"}],"dry_run":false}`
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, seamRequest(http.MethodPost, "/api/media/purge", body, sendRecipientToken))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "malformed chat target") {
				t.Fatalf("policy=%q target=%q status=%d body=%s", allow, target, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestTypingNormalizesBeforeAuthorization(t *testing.T) {
	for _, tc := range []struct {
		policy string
		status int
	}{
		{"5511999999999", http.StatusInternalServerError},
		{"*@g.us", http.StatusForbidden},
	} {
		b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, &fakeIsOnWhatsApp{})
		b.Policy = parseChatPolicy(tc.policy)
		rec := httptest.NewRecorder()
		b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/typing", `{"recipient":"+55 11 99999-9999","is_typing":true}`, sendRecipientToken))
		if rec.Code != tc.status {
			t.Fatalf("policy=%q status=%d body=%s", tc.policy, rec.Code, rec.Body.String())
		}
		if tc.status == http.StatusInternalServerError && !strings.Contains(rec.Body.String(), "websocket not connected") {
			t.Fatalf("permitted target did not reach presence client: %s", rec.Body.String())
		}
	}
}

func TestSendRefusesDeviceIdentityBeforeEffects(t *testing.T) {
	phone := types.JID{User: "5511999999999", Server: types.DefaultUserServer}
	for _, raw := range []string{"5511999999999.0@s.whatsapp.net", "5511999999999.0:1@s.whatsapp.net", "5511999999999.256@s.whatsapp.net"} {
		ask := &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
			"+" + phone.User: {PhoneNumber: phone, JID: registeredLID, IsIn: true},
		}}
		b, _, sent := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{phone: registeredLID}}, ask)
		b.Policy = parseChatPolicy(phone.String())
		rec := postSend(b.newRESTMux(8080, sendRecipientToken), raw)
		if rec.Code != http.StatusBadRequest || len(*sent) != 0 || len(ask.calls) != 0 {
			t.Fatalf("raw=%q status=%d body=%s sent=%v queries=%v", raw, rec.Code, rec.Body.String(), *sent, ask.calls)
		}
	}
}
