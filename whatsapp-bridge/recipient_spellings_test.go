package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestRecipientSpellingContract(t *testing.T) {
	data, err := os.ReadFile("testdata/recipient_spellings.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Raw, Normalized string
		Invalid         bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		got, err := normalizePhoneRecipient(tc.Raw)
		if (err != nil) != tc.Invalid || (!tc.Invalid && got != tc.Normalized) {
			t.Errorf("normalize %q = %q, %v; want %q invalid=%v", tc.Raw, got, err, tc.Normalized, tc.Invalid)
		}
	}
}

func TestFormattedForwardPolicyBeforeSend(t *testing.T) {
	const source = "120363000000000001@g.us"
	for _, entry := range []string{"5511999999999", "5511888888888", "+55 11 99999-9999"} {
		lookedUp, sent := 0, 0
		deps := forwardDeps{
			resolveRecipient: forwardRecipientAsTyped,
			lookup: func(_, _ string) (string, string, bool, error) {
				lookedUp++
				return "hello", "", true, nil
			},
			send: func(_ context.Context, to, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
				sent++
				if to != "5511999999999@s.whatsapp.net" {
					t.Fatalf("forward target %q differs from the checked number", to)
				}
				return true, "sent", sentMessage{}
			},
		}
		h := handleForwardMessage(deps, parseChatPolicy(source+","+entry))
		code, _ := efPost(t, h, `{"chat_jid":"`+source+`","message_id":"MSG1","to_chat_jid":"+55 11 99999-9999"}`)
		if entry == "5511999999999" {
			if code != http.StatusOK || sent != 1 || lookedUp != 1 {
				t.Fatalf("allowed forward: status=%d lookup=%d send=%d", code, lookedUp, sent)
			}
		} else if code != http.StatusForbidden || sent != 0 || lookedUp != 0 {
			t.Fatalf("denied forward: status=%d lookup=%d send=%d", code, lookedUp, sent)
		}
	}
}

func TestForwardTrimsDestinationBeforePolicy(t *testing.T) {
	const source = "120363000000000001@g.us"
	for _, tc := range []struct{ raw, want string }{
		{" " + source + " ", source},
		{"5511999999999\n", "5511999999999@s.whatsapp.net"},
	} {
		var addressed string
		deps := forwardDeps{
			resolveRecipient: forwardRecipientAsTyped,
			lookup:           func(_, _ string) (string, string, bool, error) { return "hello", "", true, nil },
			send: func(_ context.Context, to, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
				addressed = to
				return true, "sent", sentMessage{ChatJID: to}
			},
		}
		h := handleForwardMessage(deps, parseChatPolicy(source+",5511999999999"))
		body, _ := json.Marshal(forwardRequest{ChatJID: source, MessageID: "MSG1", ToChatJID: tc.raw})
		code, resp := efPost(t, h, string(body))
		if code != http.StatusOK || addressed != tc.want || resp.ChatJID != tc.want {
			t.Fatalf("forward %q: status=%d addressed=%q response=%+v, want %q", tc.raw, code, addressed, resp, tc.want)
		}
	}
}

func TestInvalidLongRecipientBeforeEffects(t *testing.T) {
	ask := &fakeIsOnWhatsApp{}
	b, mux, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
	b.Policy = parseChatPolicy("")
	if rec := postSend(mux, "12025551234-1612345678"); rec.Code != http.StatusBadRequest || len(*sent) != 0 || len(ask.calls) != 0 {
		t.Fatalf("long send: status=%d sent=%v lookup=%v", rec.Code, *sent, ask.calls)
	}
	h := handleForwardMessage(forwardDeps{}, parseChatPolicy(""))
	code, _ := efPost(t, h, `{"chat_jid":"120363000000000001@g.us","message_id":"MSG1","to_chat_jid":"12025551234-1612345678"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("long forward: status=%d", code)
	}
}

func TestFormattedAllowListEntryDoesNotGainAnAlias(t *testing.T) {
	ask := &fakeIsOnWhatsApp{}
	b, mux, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
	b.Policy = parseChatPolicy("+55 11 99999-9999")
	if rec := postSend(mux, "+55 11 99999-9999"); rec.Code != http.StatusForbidden || len(*sent) != 0 || len(ask.calls) != 0 {
		t.Fatalf("literal configuration: status=%d sent=%v lookup=%v", rec.Code, *sent, ask.calls)
	}
}

func TestFormattedSendPolicyBeforeLookup(t *testing.T) {
	const number = "5511999999999"
	jid := types.NewJID(number, types.DefaultUserServer)
	for _, spelling := range []string{number, "+55 (11) 99999-9999", "55.11.99999.9999", "\u200e+55 11 99999\u20119999\u200f"} {
		for _, allowed := range []bool{false, true} {
			ask := &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
				"+" + number: {JID: jid, IsIn: true},
			}}
			b, mux, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
			b.Policy = parseChatPolicy("5511888888888")
			if allowed {
				b.Policy = parseChatPolicy(number)
			}
			rec := postSend(mux, spelling)
			if allowed {
				if rec.Code != http.StatusOK || len(*sent) != 1 || (*sent)[0] != jid.String() || len(ask.calls) != 1 || ask.calls[0] != "+"+number {
					t.Fatalf("allowed %q: code=%d sent=%v lookup=%v", spelling, rec.Code, *sent, ask.calls)
				}
			} else if rec.Code != http.StatusForbidden || len(*sent) != 0 || len(ask.calls) != 0 {
				t.Fatalf("denied %q: code=%d sent=%v lookup=%v", spelling, rec.Code, *sent, ask.calls)
			}
		}
	}
}
