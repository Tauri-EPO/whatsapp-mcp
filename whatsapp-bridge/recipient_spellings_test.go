package main

import (
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
	var cases []struct{ Raw, Normalized string }
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if got := normalizePhoneRecipient(tc.Raw); got != tc.Normalized {
			t.Errorf("normalize %q = %q, want %q", tc.Raw, got, tc.Normalized)
		}
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
