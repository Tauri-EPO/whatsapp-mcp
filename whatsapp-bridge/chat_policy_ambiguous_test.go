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

func TestRESTAmbiguousChatDeniedBeforeEffects(t *testing.T) {
	for _, allow := range []string{"", "*@g.us", "*@s.whatsapp.net"} {
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
			if allow == "*@s.whatsapp.net" {
				source = efChat
			}
			for _, target := range []string{"5511888888888@s.whatsapp.net@g.us", "120363000000000002@g.us@s.whatsapp.net", "120363000000000002@g.us@g.us", "5511888888888@s.whatsapp.net@s.whatsapp.net"} {
				for _, route := range []struct{ path, field, extra string }{
					{"/api/send", "recipient", `,"message":"hello"`},
					{"/api/react", "recipient", `,"message_id":"MSG1","emoji":"","from_me":true`},
					{"/api/typing", "recipient", `,"is_typing":true`},
					{"/api/mark-read", "chat_jid", `,"message_ids":["MSG1"]`},
					{"/api/edit", "chat_jid", `,"message_id":"MSG1","text":"edited"`},
					{"/api/forward", "chat_jid", `,"message_id":"MSG1","to_chat_jid":"` + source + `"`},
					{"/api/forward", "to_chat_jid", `,"message_id":"MSG1","chat_jid":"` + source + `"`},
					{"/api/delete", "chat_jid", `,"message_id":"MSG1","for_everyone":false`},
					{"/api/group/participants", "group_jid", `,"action":"add","participants":["5511999999999"]`},
					{"/api/group/subject", "group_jid", `,"name":"Example"`},
					{"/api/group/invite", "group_jid", `,"reset":false`},
					{"/api/group/leave", "group_jid", ""},
					{"/api/group/members", "group_jid", ""},
					{"/api/chat/archive", "chat_jid", `,"archived":true`},
					{"/api/download", "chat_jid", `,"message_id":"MSG1"`},
					{"/api/history", "chat_jid", `,"count":1`},
					{"/api/media/purge", "chat_jid", `,"dry_run":false`},
					{"/api/poll", "chat_jid", ""},
				} {
					body := `{"` + route.field + `":"` + target + `"` + route.extra + `}`
					method, path := http.MethodPost, route.path
					if path == "/api/poll" {
						method = http.MethodGet
						path += "?message_id=MSG1&chat_jid=" + url.QueryEscape(target)
					}
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, seamRequest(method, path, body, sendRecipientToken))
					if rec.Code != http.StatusForbidden {
						t.Fatalf("%s/%s policy=%q target=%q: %d %s", route.path, route.field, allow, target, rec.Code, rec.Body.String())
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
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, &fakeIsOnWhatsApp{})
			b.Policy = parseChatPolicy(allow)
			source, target := "120363000000000001@g.us", "5511888888888@s.whatsapp.net@g.us"
			if allow == "*@s.whatsapp.net" {
				source, target = efChat, "120363000000000002@g.us@s.whatsapp.net"
			}
			if err := b.Store.StoreMessage("SOURCE1", source, "5511999999999", "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""); err != nil {
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
			if rec.Code != http.StatusForbidden || len(addressed) != 0 {
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
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "chat not in "+chatPolicyEnv) {
				t.Fatalf("policy=%q target=%q status=%d body=%s", allow, target, rec.Code, rec.Body.String())
			}
		}
	}
}
