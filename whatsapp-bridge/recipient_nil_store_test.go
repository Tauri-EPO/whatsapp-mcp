package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestMissingLIDBackendFallback(t *testing.T) {
	phone := types.NewJID("12025551234", types.DefaultUserServer)
	lid := types.NewJID("111222333444555", types.HiddenUserServer)
	for _, tc := range []struct {
		name   string
		client *whatsmeow.Client
	}{
		{"nil client", nil}, {"nil device", &whatsmeow.Client{}}, {"nil LID store", newTestClient(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("missing LID backend panicked: %v", value)
				}
			}()
			for _, raw := range []string{phone.User, phone.String()} {
				got, err := resolveRecipientJIDContext(context.Background(), tc.client, raw)
				if err != nil || got != phone {
					t.Fatalf("send target=%s error=%v, want phone=%s", got, err, phone)
				}
				if got := resolveMentionJIDs(tc.client, []string{raw, lid.String()}); !reflect.DeepEqual(got, []string{phone.String(), lid.String()}) {
					t.Fatalf("mentions=%v", got)
				}
				if got := resolveQuotedParticipantJID(tc.client, raw); got != phone.String() {
					t.Fatalf("quote=%s", got)
				}
			}
			if got := resolveUserJID(tc.client, lid, types.EmptyJID); got != lid {
				t.Fatalf("user fallback=%s", got)
			}
			if got := resolveLIDChat(tc.client, lid, types.EmptyJID, types.EmptyJID, false); got != lid {
				t.Fatalf("chat fallback=%s", got)
			}
			if got := storeAltJID(tc.client)(phone); !got.IsEmpty() {
				t.Fatalf("roster fallback=%s", got)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got, err := resolveRecipientJIDContext(ctx, tc.client, phone.User); !errors.Is(err, context.Canceled) || !got.IsEmpty() {
				t.Fatalf("canceled resolve=%s %v", got, err)
			}
		})
	}
}

func TestRESTRegistrationWithoutLIDBackend(t *testing.T) {
	for _, route := range []string{"/api/send", "/api/forward"} {
		t.Run(route, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("registration dereferenced missing LID backend: %v", value)
				}
			}()
			ask := registeredWithoutNinthDigit()
			b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
			b.Client.Store.LIDs = nil
			b.Store = seedEditStore(t)
			b.Policy = parseChatPolicy(efChat + "," + dialledNumber + "," + registeredNumber)
			payload := map[string]string{"recipient": dialledNumber, "message": "hello"}
			if route == "/api/forward" {
				payload = map[string]string{"chat_jid": efChat, "message_id": "THEIRS", "to_chat_jid": dialledNumber}
			}
			body, _ := json.Marshal(payload)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, route, string(body), sendRecipientToken))
			if rec.Code != http.StatusOK || len(*sent) != 1 || (*sent)[0] != registeredJID.String() || len(ask.calls) != 1 {
				t.Fatalf("status=%d body=%s sent=%v queries=%v", rec.Code, rec.Body.String(), *sent, ask.calls)
			}
		})
	}
}

func TestMissingLIDMentionFallback(t *testing.T) {
	phone := types.NewJID("12025551234", types.DefaultUserServer)
	lid := types.NewJID("111222333444555", types.HiddenUserServer)
	for index, client := range []*whatsmeow.Client{nil, {}, newTestClient(nil)} {
		t.Run(fmt.Sprintf("backend_%d", index), func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("mention dereferenced missing LID backend: %v", value)
				}
			}()
			got := resolveMentionJIDs(client, []string{phone.User, "1.2.3@s.whatsapp.net", phone.String(), lid.String()})
			want := []string{phone.String(), phone.String(), lid.String()}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("mentions=%v, want %v", got, want)
			}
		})
	}
}
