package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestForwardRegisteredRecipientAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, to, policy, wantTo, wantMessage string
		status, queries, downloads            int
		ask                                   *fakeIsOnWhatsApp
		media, offline, known                 bool
	}{
		{name: "text uses registered number", to: dialledNumber, policy: efChat + "," + dialledNumber + "," + registeredNumber, wantTo: registeredJID.String(), status: 200, queries: 1},
		{name: "media uses registered number", to: dialledNumber, policy: efChat + "," + dialledNumber + "," + registeredNumber, wantTo: registeredJID.String(), status: 200, queries: 1, media: true, downloads: 1},
		{name: "formatted spelling", to: "+55 (11) 98888-7777", policy: efChat + "," + dialledNumber + "," + registeredNumber, wantTo: registeredJID.String(), status: 200, queries: 1},
		{name: "full phone JID", to: dialledJID.String(), policy: efChat + "," + dialledNumber + "," + registeredNumber, wantTo: registeredJID.String(), status: 200, queries: 1},
		{name: "typed target refused before lookup", to: dialledNumber, policy: efChat + "," + registeredNumber, status: 403},
		{name: "registered target refused before media", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 403, queries: 1, media: true},
		{name: "not on WhatsApp", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 404, queries: 1, ask: &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{"+" + dialledNumber: {Query: "+" + dialledNumber, IsIn: false}}}, wantMessage: "not on WhatsApp"},
		{name: "no registration answer", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 502, queries: 1, ask: &fakeIsOnWhatsApp{}, wantMessage: "no answer"},
		{name: "lookup down with restricted policy", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 502, queries: 1, media: true, ask: &fakeIsOnWhatsApp{err: errors.New("lookup offline")}, wantMessage: "nothing was sent"},
		{name: "lookup down without policy retains fallback", to: dialledNumber, wantTo: dialledNumber, status: 200, queries: 1, ask: &fakeIsOnWhatsApp{err: errors.New("lookup offline")}},
		{name: "offline before media", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 500, offline: true, media: true},
		{name: "known phone does not query", to: dialledNumber, policy: efChat + "," + dialledNumber, wantTo: dialledJID.String(), status: 200, known: true},
		{name: "group stays group", to: "120363000000000001@g.us", policy: efChat + ",*@g.us", wantTo: "120363000000000001@g.us", status: 200},
		{name: "LID stays LID", to: registeredLID.String(), policy: efChat + "," + registeredLID.String(), wantTo: registeredLID.String(), status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := tc.ask
			if ask == nil {
				ask = registeredWithoutNinthDigit()
			}
			lids := &mockLIDStore{}
			if tc.known {
				lids.lidByPN = map[types.JID]types.JID{dialledJID: registeredLID}
			}
			b, _, sent := sendRecipientBridge(t, lids, ask)
			b.Store = seedEditStore(t)
			b.Policy = parseChatPolicy(tc.policy)
			b.Connected = func() bool { return !tc.offline }
			downloads := 0
			b.DownloadMedia = func(_ context.Context, id, chat string) (bool, string, string, string, error) {
				downloads++
				if id != "PIC" || chat != efChat {
					t.Fatalf("download id=%s chat=%s", id, chat)
				}
				return true, "image", "pic.jpg", "/store/pic.jpg", nil
			}
			id := "THEIRS"
			if tc.media {
				id = "PIC"
			}
			body, _ := json.Marshal(forwardRequest{ChatJID: efChat, MessageID: id, ToChatJID: tc.to})
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/forward", string(body), sendRecipientToken))
			if rec.Code != tc.status || len(ask.calls) != tc.queries || downloads != tc.downloads {
				t.Fatalf("status=%d body=%s queries=%v downloads=%d, want status=%d queries=%d downloads=%d", rec.Code, rec.Body.String(), ask.calls, downloads, tc.status, tc.queries, tc.downloads)
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Fatalf("response lacks %q: %s", tc.wantMessage, rec.Body.String())
			}
			if tc.status != 200 {
				if len(*sent) != 0 {
					t.Fatalf("refused forward sent to %v", *sent)
				}
				return
			}
			var response editForwardResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(*sent) != 1 || (*sent)[0] != tc.wantTo || response.ChatJID != tc.wantTo || !response.Success || response.MessageID == "" {
				t.Fatalf("sent=%v response=%+v, want registered target %s", *sent, response, tc.wantTo)
			}
		})
	}
}

func TestForwardMissingResolverFailsClosed(t *testing.T) {
	deps := forwardDeps{lookup: seedEditStore(t).messageContentLookup, send: func(context.Context, string, string, string, string, string, string, []string) (bool, string, sentMessage) {
		t.Fatal("missing resolver must not bypass registration")
		return false, "", sentMessage{}
	}}
	rec := httptest.NewRecorder()
	handleForwardMessage(deps, chatPolicy{})(rec, httptest.NewRequest(http.MethodPost, "/api/forward", strings.NewReader(`{"chat_jid":"`+efChat+`","message_id":"THEIRS","to_chat_jid":"5511999999999"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
