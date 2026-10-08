package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestForwardRegisteredRecipientAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, to, policy, wantTo, wantMessage, sourceID string
		status, queries, downloads                      int
		ask                                             *fakeIsOnWhatsApp
		media, offline, known                           bool
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
		{name: "lookup down without policy retains fallback", to: dialledNumber, wantTo: dialledJID.String(), status: 200, queries: 1, ask: &fakeIsOnWhatsApp{err: errors.New("lookup offline")}},
		{name: "offline before media", to: dialledNumber, policy: efChat + "," + dialledNumber, status: 500, offline: true, media: true},
		{name: "known phone does not query", to: dialledNumber, policy: efChat + "," + dialledNumber, wantTo: dialledJID.String(), status: 200, known: true},
		{name: "group stays group", to: "120363000000000001@g.us", policy: efChat + ",*@g.us", wantTo: "120363000000000001@g.us", status: 200},
		{name: "LID stays LID", to: registeredLID.String(), policy: efChat + "," + registeredLID.String(), wantTo: registeredLID.String(), status: 200},
		{name: "missing source before lookup", to: dialledNumber, policy: efChat + "," + dialledNumber + "," + registeredNumber, sourceID: "NOPE", status: 404},
		{name: "poll vote before lookup", to: dialledNumber, policy: efChat + "," + dialledNumber + "," + registeredNumber, sourceID: "VOTE", status: 400},
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
			if tc.sourceID != "" {
				id = tc.sourceID
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

func TestForwardSharesRegistrationAndSendBudget(t *testing.T) {
	for _, id := range []string{"THEIRS", "PIC"} {
		t.Run(id, func(t *testing.T) {
			var budget context.Context
			var deadline time.Time
			deps := forwardDeps{
				lookup: seedEditStore(t).messageContentLookup,
				resolveRecipient: func(ctx context.Context, _ http.ResponseWriter, to string) (string, bool) {
					budget = ctx
					var ok bool
					deadline, ok = ctx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > sendDeadline {
						t.Fatalf("resolver has no bounded send budget: %v, %v", deadline, ok)
					}
					return to, true
				},
				download: func(ctx context.Context, _, _ string) (bool, string, string, string, error) {
					mediaDeadline, ok := ctx.Deadline()
					if !ok || mediaDeadline.After(deadline) || time.Until(mediaDeadline) > downloadDeadline {
						t.Fatalf("download exceeds shared budget: %v, %v", mediaDeadline, ok)
					}
					return true, "image", "pic.jpg", "/store/pic.jpg", nil
				},
				send: func(ctx context.Context, to, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
					if ctx != budget {
						t.Fatal("send started a new budget after registration")
					}
					return true, "sent", sentMessage{ID: "FORWARDED", ChatJID: to}
				},
			}
			body := `{"chat_jid":"` + efChat + `","message_id":"` + id + `","to_chat_jid":"` + dialledNumber + `"}`
			if code, response := efPost(t, handleForwardMessage(deps, chatPolicy{}), body); code != 200 || !response.Success {
				t.Fatalf("status=%d response=%+v", code, response)
			}
		})
	}
}

func TestRecipientFailureCountsOnlySendEndpoint(t *testing.T) {
	for _, endpoint := range []string{"send", "forward"} {
		for _, failure := range []string{"offline", "unregistered", "lookup unavailable"} {
			t.Run(endpoint+"/"+failure, func(t *testing.T) {
				ask := registeredWithoutNinthDigit()
				status := http.StatusInternalServerError
				switch failure {
				case "unregistered":
					ask = &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{"+" + dialledNumber: {IsIn: false}}}
					status = http.StatusNotFound
				case "lookup unavailable":
					ask = &fakeIsOnWhatsApp{err: errors.New("lookup unavailable")}
					status = http.StatusBadGateway
				}
				b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
				b.Store = seedEditStore(t)
				b.Policy = parseChatPolicy(efChat + "," + dialledNumber + "," + registeredNumber)
				b.Connected = func() bool { return failure != "offline" }
				b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
					t.Fatal("recipient refusal must precede downloading")
					return false, "", "", "", nil
				}
				body := `{"recipient":"` + dialledNumber + `","message":"hi"}`
				if endpoint == "forward" {
					body = `{"chat_jid":"` + efChat + `","message_id":"PIC","to_chat_jid":"` + dialledNumber + `"}`
				}
				mux := b.newRESTMux(8080, sendRecipientToken)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, seamRequest(http.MethodPost, "/api/"+endpoint, body, sendRecipientToken))
				if rec.Code != status || len(*sent) != 0 {
					t.Fatalf("status=%d body=%s sends=%v", rec.Code, rec.Body.String(), *sent)
				}
				want := int64(0)
				if endpoint == "send" {
					want = 1
				}
				metrics := httptest.NewRecorder()
				mux.ServeHTTP(metrics, seamRequest(http.MethodGet, "/metrics", "", sendRecipientToken))
				line := "whatsapp_bridge_send_failures_total " + fmt.Sprint(want) + "\n"
				if b.metrics.sendFailures.Load() != want || metrics.Code != 200 || !strings.Contains(metrics.Body.String(), line) {
					t.Fatalf("want send failures=%d, metrics status=%d body=%s", want, metrics.Code, metrics.Body.String())
				}
			})
		}
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
