package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestClampHistoryCount(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to default", 0, defaultHistoryCount},
		{"negative falls back to default", -5, defaultHistoryCount},
		{"in range is preserved", 25, 25},
		{"max is preserved", maxHistoryCount, maxHistoryCount},
		{"above max is clamped", maxHistoryCount + 1, maxHistoryCount},
		{"absurd value is clamped", 1 << 20, maxHistoryCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampHistoryCount(tc.in); got != tc.want {
				t.Fatalf("clampHistoryCount(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestHistoryRepairRequest(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	other := "5511888888888@s.whatsapp.net"
	ts := time.Date(2026, 1, 1, 12, 0, 0, 123000000, time.UTC)
	for _, tc := range []struct {
		name, requestChat, storedChat, target, wantID string
		wantFromMe                                    bool
		wantTime                                      time.Time
		status                                        int
	}{
		{"default backfill unchanged", phone.String(), phone.String(), "BACKFILL", "M1", false, ts.Add(-time.Minute), 200},
		{"successor with tied timestamp", phone.String(), phone.String(), "M2", "M3", true, ts, 200},
		{"successor with later timestamp", phone.String(), phone.String(), "M3", "M4", false, ts.Add(time.Minute), 200},
		{"newest timestamp fallback", phone.String(), phone.String(), "M4", "", false, ts.Add(time.Minute + time.Second), 200},
		{"LID resolves phone storage", lid.String(), phone.String(), "M2", "M3", true, ts, 200},
		{"phone resolves LID storage", phone.String(), lid.String(), "M2", "M3", true, ts, 200},
		{"canonical spelling", " 5511999999999@S.WHATSAPP.NET ", phone.String(), "M2", "M3", true, ts, 200},
		{"unknown id", phone.String(), phone.String(), "MISSING", "", false, time.Time{}, 400},
		{"another chat id", phone.String(), phone.String(), "OTHER", "", false, time.Time{}, 400},
		{"empty id", phone.String(), phone.String(), "", "", false, time.Time{}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMessageStore(t)
			for _, msg := range []storedMessage{
				{ID: "M4", ChatJID: tc.storedChat, Timestamp: ts.Add(time.Minute)},
				{ID: "M3", ChatJID: tc.storedChat, Timestamp: ts, IsFromMe: true},
				{ID: "M2", ChatJID: tc.storedChat, Timestamp: ts},
				{ID: "M1", ChatJID: tc.storedChat, Timestamp: ts.Add(-time.Minute)},
				{ID: "M2", ChatJID: other, Timestamp: ts.Add(-time.Hour)},
				{ID: "OTHER", ChatJID: other, Timestamp: ts},
			} {
				msg.Content = "hello"
				if err := store.StoreMessage(msg); err != nil {
					t.Fatal(err)
				}
			}
			client := newTestClientWithSelf(&mockLIDStore{
				lidByPN: map[types.JID]types.JID{phone: lid},
				pnByLID: map[types.JID]types.JID{lid: phone},
			}, phone)
			var sent *waE2E.Message
			mux := http.NewServeMux()
			registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc { return h },
				client, func() bool { return true }, store, chatPolicy{}, func(_ context.Context, msg *waE2E.Message) error {
					sent = msg
					return nil
				})
			var beforeID *string
			if tc.target != "BACKFILL" {
				beforeID = &tc.target
			}
			body, err := json.Marshal(HistoryRequest{ChatJID: tc.requestChat, Count: 7, BeforeMessageID: beforeID})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(string(body))))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status != http.StatusOK {
				var failure apiError
				if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if sent != nil || failure.Error.Code != "invalid_argument" || !strings.Contains(failure.Message, "in this chat") {
					t.Fatalf("invalid target sent a request or lost its error: sent=%v failure=%+v", sent, failure)
				}
				return
			}
			anchor := sent.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
			requestedChat, err := canonicalChatJID(tc.requestChat, false)
			if err != nil {
				t.Fatal(err)
			}
			if anchor == nil || anchor.GetChatJID() != requestedChat.String() || anchor.GetOldestMsgID() != tc.wantID ||
				anchor.GetOldestMsgFromMe() != tc.wantFromMe || anchor.GetOldestMsgTimestampMS() != tc.wantTime.Unix() ||
				anchor.GetOnDemandMsgCount() != 7 {
				t.Fatalf("built request = %v; want chat=%s id=%q fromMe=%v timestamp=%d count=7",
					anchor, requestedChat, tc.wantID, tc.wantFromMe, tc.wantTime.Unix())
			}
		})
	}
}

func TestHistoryRepairDenied(t *testing.T) {
	mux := http.NewServeMux()
	registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, nil,
		func() bool { t.Fatal("denied request checked connection"); return true }, nil,
		parseChatPolicy("5511888888888@s.whatsapp.net"), func(context.Context, *waE2E.Message) error {
			t.Fatal("denied request sent to phone")
			return nil
		})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(
		`{"chat_jid":"5511999999999@s.whatsapp.net","before_message_id":"M2"}`)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "WHATSAPP_ALLOWED_CHATS") {
		t.Fatalf("denial = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnchorTime(t *testing.T) {
	want := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

	t.Run("time.Time passes through", func(t *testing.T) {
		if got := anchorTime(want); !got.Equal(want) {
			t.Fatalf("anchorTime(time.Time) = %v, want %v", got, want)
		}
	})

	t.Run("go String() format is parsed", func(t *testing.T) {
		if got := anchorTime("2025-03-04 05:06:07 +0000 UTC"); !got.Equal(want) {
			t.Fatalf("anchorTime(string) = %v, want %v", got, want)
		}
	})

	t.Run("rfc3339 string is parsed", func(t *testing.T) {
		if got := anchorTime("2025-03-04T05:06:07Z"); !got.Equal(want) {
			t.Fatalf("anchorTime(rfc3339) = %v, want %v", got, want)
		}
	})

	t.Run("byte slice is parsed", func(t *testing.T) {
		if got := anchorTime([]byte("2025-03-04T05:06:07Z")); !got.Equal(want) {
			t.Fatalf("anchorTime([]byte) = %v, want %v", got, want)
		}
	})

	// These two are the cross-driver case the string fallback exists for:
	// a messages.db written by cgo go-sqlite3, read by a pure-Go build. Both
	// failed before the format list was extended.
	offsetWant := time.Date(2026, 8, 1, 20, 0, 0, 123000000, time.UTC)

	t.Run("cgo space-separated format is parsed", func(t *testing.T) {
		if got := anchorTime("2026-08-01 14:00:00.123-06:00"); !got.Equal(offsetWant) {
			t.Fatalf("anchorTime(cgo space) = %v, want %v", got, offsetWant)
		}
	})

	t.Run("cgo T-separated format is parsed", func(t *testing.T) {
		if got := anchorTime("2026-08-01T14:00:00.123-06:00"); !got.Equal(offsetWant) {
			t.Fatalf("anchorTime(cgo T) = %v, want %v", got, offsetWant)
		}
	})

	t.Run("unparseable value yields zero time", func(t *testing.T) {
		if got := anchorTime("not a timestamp"); !got.IsZero() {
			t.Fatalf("anchorTime(garbage) = %v, want zero time", got)
		}
	})

	t.Run("unexpected type yields zero time", func(t *testing.T) {
		if got := anchorTime(42); !got.IsZero() {
			t.Fatalf("anchorTime(int) = %v, want zero time", got)
		}
	})
}

// TestHistoryAnchorInfo asserts the four fields whatsmeow's
// BuildHistorySyncRequest actually reads are carried through. The request's
// wire message has no field for the anchor's sender or a group flag, so those
// are intentionally not set and not tested.
func TestHistoryAnchorInfo(t *testing.T) {
	chat := types.JID{User: "15552223333", Server: types.DefaultUserServer}
	ts := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

	got := historyAnchorInfo(chat, "MSGID", true, ts)

	if got.Chat != chat {
		t.Errorf("Chat = %v, want %v", got.Chat, chat)
	}
	if !got.IsFromMe {
		t.Error("IsFromMe = false, want true")
	}
	if got.ID != "MSGID" {
		t.Errorf("ID = %q, want %q", got.ID, "MSGID")
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, ts)
	}
}

// TestHistoryEndpointValidation exercises the handler's own request validation.
// The real auth wrapper is covered by auth_test.go, so an identity wrapper is
// used here. A nil client is sufficient: each of these paths returns at or
// before the connection check, which reports offline.
func TestHistoryEndpointValidation(t *testing.T) {
	identity := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mux := http.NewServeMux()
	offline := func() bool { return false }
	registerHistoryEndpoint(mux, identity, nil, offline, nil, chatPolicy{})

	cases := []struct {
		name       string
		method     string
		body       string
		wantStatus int
	}{
		{"wrong method", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"invalid json", http.MethodPost, "{not json", http.StatusBadRequest},
		{"missing chat_jid", http.MethodPost, `{"count":10}`, http.StatusBadRequest},
		{"not connected", http.MethodPost, `{"chat_jid":"15552223333@s.whatsapp.net"}`, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, "/api/history", body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
