package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestTypingUsesSharedRecipientSpelling(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		invalid   bool
	}{
		{"12025551234", "12025551234@s.whatsapp.net", false},
		{"12025551234@s.whatsapp.net", "12025551234@s.whatsapp.net", false},
		{"111222333444555@lid", "111222333444555@lid", false},
		{"120363000000000001@g.us", "120363000000000001@g.us", false},
		{"1.2.3@s.whatsapp.net", "", true},
	} {
		for _, typing := range []bool{false, true} {
			t.Run(tc.raw, func(t *testing.T) {
				b := testBridge(t, nil, newTestMessageStore(t), testLogger())
				calls := 0
				b.chatPresence = func(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
					calls++
					wantState := types.ChatPresencePaused
					if typing {
						wantState = types.ChatPresenceComposing
					}
					if tc.invalid || jid.String() != tc.want || state != wantState || media != types.ChatPresenceMediaText {
						t.Fatalf("presence=%s %s %s, want target=%s", jid, state, media, tc.want)
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("missing action deadline")
					}
					return nil
				}
				body, _ := json.Marshal(map[string]any{"recipient": tc.raw, "is_typing": typing})
				rec := httptest.NewRecorder()
				b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/typing", string(body), sendRecipientToken))
				var response struct {
					Success bool `json:"success"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if tc.invalid {
					if calls != 0 || response.Success {
						t.Fatalf("bad JID sent: %s calls=%d", rec.Body.String(), calls)
					}
				} else if rec.Code != http.StatusOK || !response.Success || calls != 1 {
					t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls)
				}
			})
		}
	}
}
