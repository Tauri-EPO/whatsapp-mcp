package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

func TestRecipientParserInterpretationContract(t *testing.T) {
	phone := types.NewJID("12025551234", types.DefaultUserServer)
	for _, tc := range []struct {
		name, raw string
		want      types.JID
		invalid   bool
	}{
		{"bare phone", phone.User, phone, false},
		{"full phone", phone.String(), phone, false},
		{"empty is not validated", "", types.NewJID("", types.DefaultUserServer), false},
		{"whitespace is literal", " \t", types.NewJID(" \t", types.DefaultUserServer), false},
		{"letters are literal", "Alice", types.NewJID("Alice", types.DefaultUserServer), false},
		{"length is not validated", "1234567890123456", types.NewJID("1234567890123456", types.DefaultUserServer), false},
		{"newline is literal", phone.User + "\n", types.NewJID(phone.User+"\n", types.DefaultUserServer), false},
		{"plus is literal", "+" + phone.User, types.NewJID("+"+phone.User, types.DefaultUserServer), false},
		{"full server case is literal", phone.User + "@S.WHATSAPP.NET", types.NewJID(phone.User, "S.WHATSAPP.NET"), false},
		{"extra at parts are discarded", phone.String() + "@g.us", phone, false},
		{"missing user is not validated", "@g.us", types.NewJID("", types.GroupServer), false},
		{"missing server is not validated", phone.User + "@", types.NewJID(phone.User, ""), false},
		{"group", "120363000000000001@g.us", types.NewJID("120363000000000001", types.GroupServer), false},
		{"LID", "111222333444555@lid", types.NewJID("111222333444555", types.HiddenUserServer), false},
		{"device is interpreted", phone.User + ":12@s.whatsapp.net", types.JID{User: phone.User, Server: types.DefaultUserServer, Device: 12}, false},
		{"unreadable device fails", "123:notadevice@s.whatsapp.net", types.EmptyJID, true},
		{"unreadable agent fails", "1.2.3@s.whatsapp.net", types.EmptyJID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRecipientJID(tc.raw)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("parse(%q)=%+v, %v; want %+v, error=%v", tc.raw, got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestMentionAndQuoteBoundaryNormalization(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := registeredLID
	client := newTestClient(&mockLIDStore{lidByPN: map[types.JID]types.JID{phone: lid}})
	for _, raw := range []string{phone.User, phone.String(), "+55 11 99999-9999", " \t" + phone.String() + "\n", phone.User + "@S.WHATSAPP.NET"} {
		t.Run(raw, func(t *testing.T) {
			if got := resolveMentionJIDs(client, []string{raw}); !reflect.DeepEqual(got, []string{phone.String(), lid.String()}) {
				t.Fatalf("wire mentions=%v", got)
			}
			if got := resolveQuotedParticipantJID(client, raw); got != lid.String() {
				t.Fatalf("wire quoted sender=%q", got)
			}
			participants, err := parseParticipants([]string{raw})
			if err != nil || !reflect.DeepEqual(participants, []types.JID{phone}) {
				t.Fatalf("participants=%v, error=%v", participants, err)
			}
		})
	}
	for _, raw := range []string{"", " \t", "Alice", "1234567890123456", "@s.whatsapp.net", phone.User + "@", phone.String() + "@g.us", "1.2.3@s.whatsapp.net"} {
		t.Run("drop/"+raw, func(t *testing.T) {
			if got := resolveMentionJIDs(client, []string{raw}); len(got) != 0 {
				t.Fatalf("invalid mention was emitted: %v", got)
			}
			if got := resolveQuotedParticipantJID(client, raw); got != "" {
				t.Fatalf("invalid quoted sender was emitted: %q", got)
			}
			if _, err := parseParticipants([]string{raw}); err == nil {
				t.Fatal("invalid participant was accepted")
			}
		})
	}
}

func TestMentionAndRecipientLookupKeepDeviceKey(t *testing.T) {
	phone := types.NewJID("12025551234", types.DefaultUserServer)
	device := phone
	device.Device = 12
	client := newTestClient(&mockLIDStore{lidByPN: map[types.JID]types.JID{device: registeredLID, phone: types.NewJID("111222333444555", types.HiddenUserServer)}})
	if got := resolveMentionJIDs(client, []string{device.String()}); !reflect.DeepEqual(got, []string{device.String(), registeredLID.String()}) {
		t.Fatalf("mention lookup normalized its established key: %v", got)
	}
	if got, err := resolveRecipientJIDContext(context.Background(), client, device.String()); err != nil || got != registeredLID {
		t.Fatalf("recipient lookup normalized its established key: %s, %v", got, err)
	}
}

func TestArchiveMissingLIDTwinUsesOnlyStoredChat(t *testing.T) {
	b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, &fakeIsOnWhatsApp{})
	b.Client.Store.LIDs = nil
	b.Connected = func() bool { return true }
	ts := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, chat string
		when     time.Time
	}{{"LID-SOURCE", registeredLID.String(), ts}, {"LATER-PHONE", efChat, ts.Add(time.Hour)}} {
		if err := b.Store.StoreMessage(storedMessage{
			ID:         row.id,
			ChatJID:    row.chat,
			Sender:     efChat,
			Content:    "hello",
			Timestamp:  row.when,
			IsFromMe:   true,
			FileLength: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	b.SendAppState = func(_ context.Context, patch appstate.PatchInfo) error {
		calls++
		key := patch.Mutations[0].Value.GetArchiveChatAction().GetMessageRange().GetMessages()[0].GetKey()
		if key.GetID() != "LID-SOURCE" || key.GetRemoteJID() != registeredLID.String() {
			t.Fatalf("missing twin added an alias: %v", key)
		}
		return nil
	}
	body, _ := json.Marshal(map[string]any{"chat_jid": registeredLID.String(), "archived": true})
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/chat/archive", string(body), sendRecipientToken))
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("archive status=%d body=%s sends=%d", rec.Code, rec.Body.String(), calls)
	}
}

func TestRESTUsesCanonicalAuthorizedChat(t *testing.T) {
	phone := types.NewJID(efChat[:strings.IndexByte(efChat, '@')], types.DefaultUserServer)
	for _, raw := range []string{strings.ToUpper(efChat), " \t" + efChat + "\n", efChat + "\n"} {
		for _, endpoint := range []string{"send", "typing", "forward", "download", "chat/archive", "poll"} {
			t.Run(endpoint+"/"+raw, func(t *testing.T) {
				b, _, _ := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{phone: registeredLID}}, &fakeIsOnWhatsApp{})
				b.Store = seedEditStore(t)
				b.Policy = parseChatPolicy(efChat)
				calls := 0
				assertChat := func(chat string) {
					calls++
					if chat != efChat {
						t.Fatalf("addressed %q instead of the canonical authorized chat", chat)
					}
				}
				b.Send = func(_ context.Context, chat, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
					assertChat(chat)
					return true, "sent", sentMessage{ID: "CANONICAL", ChatJID: chat}
				}
				b.chatPresence = func(_ context.Context, chat types.JID, _ types.ChatPresence, _ types.ChatPresenceMedia) error {
					assertChat(chat.String())
					return nil
				}
				b.DownloadMedia = func(_ context.Context, _, chat string) (bool, string, string, string, error) {
					assertChat(chat)
					return true, "image", "pic.jpg", "/store/pic.jpg", nil
				}
				b.SendAppState = func(_ context.Context, patch appstate.PatchInfo) error {
					calls++
					if got := patch.Mutations[0].Value.GetArchiveChatAction().GetMessageRange().GetMessages()[0].GetKey().GetRemoteJID(); got != registeredLID.String() {
						t.Fatalf("archive addressed %q instead of the known LID", got)
					}
					return nil
				}
				payload := map[string]any{"recipient": raw, "message": "hello", "is_typing": true}
				if endpoint == "forward" {
					payload = map[string]any{"chat_jid": raw, "message_id": "THEIRS", "to_chat_jid": raw}
				}
				if endpoint == "download" {
					payload = map[string]any{"chat_jid": raw, "message_id": "PIC"}
				}
				if endpoint == "chat/archive" {
					payload = map[string]any{"chat_jid": raw, "archived": true}
				}
				body, _ := json.Marshal(payload)
				rec := httptest.NewRecorder()
				method, path := http.MethodPost, "/api/"+endpoint
				if endpoint == "poll" {
					if err := b.Store.StorePoll("POLL1", efChat, &pollCreation{Question: "Choose", Options: []string{"A", "B"}, SelectableCount: 1}, time.Now()); err != nil {
						t.Fatal(err)
					}
					method, path = http.MethodGet, "/api/poll?message_id=POLL1&chat_jid="+url.QueryEscape(raw)
				}
				b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(method, path, string(body), sendRecipientToken))
				if endpoint == "poll" {
					var response PollResultsResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.ChatJID != efChat || !response.Success {
						t.Fatalf("poll status=%d response=%+v error=%v", rec.Code, response, err)
					}
					calls++
				}
				if rec.Code != 200 || calls != 1 {
					t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls)
				}
			})
		}
	}
}

func TestPurgeUsesCanonicalAuthorizedChat(t *testing.T) {
	for _, criteria := range []bool{false, true} {
		b, files := purgeFixture(t)
		b.Policy = parseChatPolicy(purgeChat)
		raw := " \t" + strings.ToUpper(purgeChat) + "\n"
		payload := map[string]any{"items": []PurgeItem{{MessageID: "OLDVID", ChatJID: raw}}, "dry_run": true}
		if criteria {
			payload = map[string]any{"chat_jid": raw, "dry_run": true}
		}
		body, _ := json.Marshal(payload)
		code, response := purgeCall(t, b, string(body))
		if code != 200 || len(response.Items) != 1 || response.Items[0].ChatJID != purgeChat || !response.Items[0].Purged || !fileExists(files["OLDVID"]) {
			t.Fatalf("criteria=%v status=%d response=%+v", criteria, code, response)
		}
	}
}

// Canonicalization must happen before registration, not only before Send: an
// uppercase phone server would otherwise skip the registered-number policy.
func TestCanonicalChatBeforeRegistration(t *testing.T) {
	for _, endpoint := range []string{"send", "forward"} {
		for _, allowed := range []bool{false, true} {
			for _, raw := range []string{strings.ToUpper(dialledJID.String()), " " + dialledJID.String() + "\n"} {
				b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
				b.Store = seedEditStore(t)
				b.Connected = func() bool { return true }
				ask := registeredWithoutNinthDigit()
				b.IsOnWhatsApp = ask.ask
				policy := efChat + "," + dialledNumber
				if allowed {
					policy += "," + registeredNumber
				}
				b.Policy = parseChatPolicy(policy)
				payload := map[string]any{"recipient": raw, "message": "hello"}
				if endpoint == "forward" {
					payload = map[string]any{"chat_jid": efChat, "message_id": "THEIRS", "to_chat_jid": raw}
				}
				body, _ := json.Marshal(payload)
				rec := httptest.NewRecorder()
				b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/"+endpoint, string(body), sendRecipientToken))
				wantStatus, wantSends := http.StatusForbidden, 0
				if allowed {
					wantStatus, wantSends = http.StatusOK, 1
				}
				if rec.Code != wantStatus || !reflect.DeepEqual(ask.calls, []string{"+" + dialledNumber}) || len(*sent) != wantSends || (allowed && (*sent)[0] != registeredJID.String()) {
					t.Fatalf("%s raw=%q allowed=%v status=%d queries=%v sent=%v body=%s", endpoint, raw, allowed, rec.Code, ask.calls, *sent, rec.Body.String())
				}
			}
		}
	}
}
