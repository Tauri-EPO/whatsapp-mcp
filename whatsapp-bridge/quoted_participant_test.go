package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestResolveQuotedParticipantJID(t *testing.T) {
	phone := types.JID{User: "15551234567", Server: types.DefaultUserServer}
	lid := types.JID{User: "98765432109876", Server: types.HiddenUserServer}
	withLID := newTestClient(&mockLIDStore{lidByPN: map[types.JID]types.JID{phone: lid}})
	withoutLID := newTestClient(&mockLIDStore{})

	cases := []struct {
		name   string
		client *whatsmeow.Client
		raw    string
		want   string
	}{
		{"empty stays empty", withLID, "", ""},
		{"whitespace stays empty", withLID, "   ", ""},
		{"bare number gets server", withoutLID, "15551234567", "15551234567@s.whatsapp.net"},
		{"bare number upgraded to cached LID", withLID, "15551234567", lid.String()},
		{"phone JID upgraded to cached LID", withLID, "15551234567@s.whatsapp.net", lid.String()},
		{"phone JID without LID unchanged", withoutLID, "15551234567@s.whatsapp.net", "15551234567@s.whatsapp.net"},
		{"device suffix dropped before LID lookup", withLID, "15551234567:12@s.whatsapp.net", lid.String()},
		{"LID passes through", withLID, "5555@lid", "5555@lid"},
		{"group JID passes through", withLID, "120363000000000001@g.us", "120363000000000001@g.us"},
		{"unparseable returned as-is", withLID, "1.2.3@s.whatsapp.net", "1.2.3@s.whatsapp.net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveQuotedParticipantJID(tc.client, tc.raw)
			if got != tc.want {
				t.Fatalf("resolveQuotedParticipantJID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	t.Run("nil client still normalises", func(t *testing.T) {
		if got := resolveQuotedParticipantJID(nil, "15551234567"); got != "15551234567@s.whatsapp.net" {
			t.Fatalf("got %q", got)
		}
	})
}
