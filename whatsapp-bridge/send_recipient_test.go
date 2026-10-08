package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// A number typed the way it is dialled, and the one WhatsApp has registered
// for it: the Brazilian mobile of issue #444, with and without the ninth digit.
const (
	dialledNumber    = "5511988887777"
	registeredNumber = "551188887777"
)

var (
	dialledJID    = types.NewJID(dialledNumber, types.DefaultUserServer)
	registeredJID = types.NewJID(registeredNumber, types.DefaultUserServer)
	registeredLID = types.NewJID("246813579024680", types.HiddenUserServer)
)

// fakeIsOnWhatsApp answers from a table keyed by the number as queried and
// records every call: a number the LID map knows must cause none. Like
// whatsmeow, it can return answers and an error together.
type fakeIsOnWhatsApp struct {
	answers map[string]types.IsOnWhatsAppResponse
	err     error
	calls   []string
	// within is how far away the deadline of the last call was (0 = none).
	within time.Duration
}

func (f *fakeIsOnWhatsApp) ask(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.calls = append(f.calls, phones...)
	if deadline, ok := ctx.Deadline(); ok {
		f.within = time.Until(deadline)
	}
	var out []types.IsOnWhatsAppResponse
	for _, phone := range phones {
		if answer, ok := f.answers[phone]; ok {
			out = append(out, answer)
		}
	}
	return out, f.err
}

// registeredWithoutNinthDigit is what WhatsApp answers for the dialled number:
// on WhatsApp, under a LID, with the registered number next to it.
func registeredWithoutNinthDigit() *fakeIsOnWhatsApp {
	return &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
		"+" + dialledNumber: {Query: "+" + dialledNumber, JID: registeredLID, PhoneNumber: registeredJID, IsIn: true},
	}}
}

func TestCanonicalRecipientJID(t *testing.T) {
	known := types.NewJID("5511999990000", types.DefaultUserServer)
	knownLID := types.NewJID("135792468013579", types.HiddenUserServer)
	group := types.NewJID("120363012345678901", types.GroupServer)
	lids := &mockLIDStore{lidByPN: map[types.JID]types.JID{known: knownLID}}
	lookupDown := errors.New("info query timed out")

	cases := []struct {
		name      string
		recipient string
		ask       *fakeIsOnWhatsApp
		want      types.JID
		wantErr   error // a sentinel, matched with errors.Is
		wantFail  bool  // any error at all (an unreadable recipient)
		wantCalls []string
	}{
		{
			name: "dialled with the ninth digit, registered without it", recipient: dialledNumber,
			ask: registeredWithoutNinthDigit(), want: registeredJID, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "the same as a full JID", recipient: dialledJID.String(),
			ask: registeredWithoutNinthDigit(), want: registeredJID, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "answer without LID addressing: the JID is the number", recipient: dialledNumber,
			ask: &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
				"+" + dialledNumber: {JID: registeredJID, IsIn: true},
			}},
			want: registeredJID, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "registered exactly as typed, new to the LID map", recipient: registeredNumber,
			ask: &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
				"+" + registeredNumber: {JID: registeredLID, PhoneNumber: registeredJID, IsIn: true},
			}},
			want: registeredJID, wantCalls: []string{"+" + registeredNumber},
		},
		{
			name: "on WhatsApp but the answer names only a LID: the number as typed", recipient: dialledNumber,
			ask: &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
				"+" + dialledNumber: {JID: registeredLID, IsIn: true},
			}},
			want: dialledJID, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "a number the LID map knows costs no call", recipient: known.User,
			ask: &fakeIsOnWhatsApp{}, want: known,
		},
		{
			name: "a group is untouched", recipient: group.String(),
			ask: &fakeIsOnWhatsApp{}, want: group,
		},
		{
			name: "an @lid recipient is untouched", recipient: registeredLID.String(),
			ask: &fakeIsOnWhatsApp{}, want: registeredLID,
		},
		{
			name: "not registered", recipient: dialledNumber,
			ask: &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
				"+" + dialledNumber: {JID: dialledJID, IsIn: false},
			}},
			wantErr: errNotOnWhatsApp, wantCalls: []string{"+" + dialledNumber},
		},
		{
			// A throttled query looks like this: it says nothing about the number.
			name: "no answer for the number is not a no", recipient: dialledNumber,
			ask: &fakeIsOnWhatsApp{}, wantErr: errRecipientLookup, wantCalls: []string{"+" + dialledNumber},
		},
		{
			// whatsmeow hands back the answers when only its own write of the
			// LID mapping failed.
			name: "an answer that came with an error is still the answer", recipient: dialledNumber,
			ask: func() *fakeIsOnWhatsApp {
				f := registeredWithoutNinthDigit()
				f.err = errors.New("failed to store LID mappings: database is locked")
				return f
			}(),
			want: registeredJID, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "the lookup itself failed", recipient: dialledNumber,
			ask: &fakeIsOnWhatsApp{err: lookupDown}, wantErr: errRecipientLookup, wantCalls: []string{"+" + dialledNumber},
		},
		{
			name: "malformed JID", recipient: "123:notadevice@s.whatsapp.net",
			ask: &fakeIsOnWhatsApp{}, wantFail: true,
		},
		{
			name: "not a phone number: nothing is asked", recipient: "+55 11 98888-7777",
			ask: &fakeIsOnWhatsApp{}, wantFail: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalRecipientJID(context.Background(), lids.GetLIDForPN, tc.ask.ask, tc.recipient)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantFail:
				if err == nil || errors.Is(err, errNotOnWhatsApp) || errors.Is(err, errRecipientLookup) {
					t.Fatalf("err = %v, want a recipient that cannot be read", err)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("JID = %s, want %s", got, tc.want)
			}
			if !reflect.DeepEqual(tc.ask.calls, tc.wantCalls) {
				t.Errorf("IsOnWhatsApp calls = %v, want %v", tc.ask.calls, tc.wantCalls)
			}
		})
	}

	// "Not on WhatsApp" and "could not ask" are two different answers.
	if errors.Is(errNotOnWhatsApp, errRecipientLookup) || errors.Is(errRecipientLookup, errNotOnWhatsApp) {
		t.Fatal("the two failures must stay distinguishable")
	}
}

const sendRecipientToken = "test-token-0123456789"

// sendRecipientBridge is a connected bridge whose send is a recorder: the
// tests below are about who a send is addressed to, never about sending.
func sendRecipientBridge(t *testing.T, lids *mockLIDStore, ask *fakeIsOnWhatsApp) (*Bridge, http.Handler, *[]string) {
	t.Helper()
	self := types.NewJID("5511900000000", types.DefaultUserServer)
	// b.Log is the recording logger: sendLog(b) reads it back.
	b := testBridge(t, newTestClientWithSelf(lids, self), newTestMessageStore(t), installRecordingLogger(t))
	b.Connected = func() bool { return true }
	b.IsOnWhatsApp = ask.ask
	var sentTo []string
	b.Send = func(_ context.Context, recipient, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
		sentTo = append(sentTo, recipient)
		return true, "Message sent to " + recipient, sentMessage{ID: "OUT1", ChatJID: recipient, Timestamp: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	}
	return b, b.newRESTMux(8080, sendRecipientToken), &sentTo
}

func sendLog(b *Bridge) string {
	return b.Log.(*recordingLogger).String()
}

func postSend(mux http.Handler, recipient string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"recipient": recipient, "message": "hi"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
	return rec
}

func TestSendGoesToTheRegisteredNumber(t *testing.T) {
	ask := registeredWithoutNinthDigit()
	b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, ask)

	rec := postSend(mux, dialledNumber)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if want := []string{registeredJID.String()}; !reflect.DeepEqual(*sentTo, want) {
		t.Fatalf("sent to %v, want %v", *sentTo, want)
	}
	var resp SendMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.ChatJID != registeredJID.String() {
		t.Errorf("response = %+v, want the registered chat JID", resp)
	}
	if want := []string{"+" + dialledNumber}; !reflect.DeepEqual(ask.calls, want) {
		t.Errorf("IsOnWhatsApp calls = %v, want %v", ask.calls, want)
	}
	// The question has a deadline of its own, well inside the send's.
	if ask.within <= 0 || ask.within > recipientLookupTimeout {
		t.Errorf("lookup deadline %s away, want within %s", ask.within, recipientLookupTimeout)
	}
	// And the redirect leaves a trace for whoever asks why the message
	// landed in another chat.
	if log := sendLog(b); !strings.Contains(log, dialledJID.String()+" is registered on WhatsApp as "+registeredJID.String()) {
		t.Errorf("no log line for the redirect:\n%s", log)
	}
}

// The dialled spelling must not open a second chat: the row lands in the chat
// the registered number already has. The fake send here is the real persist
// step of sendWhatsAppMessage, fed the recipient the handler handed over.
func TestSendToTheDialledSpellingLandsInTheExistingChat(t *testing.T) {
	b, mux, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
	ts := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if err := b.Store.StoreChat(registeredJID.String(), "Existing contact", ts.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	b.Send = func(_ context.Context, recipient, message, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
		storageJID, err := parseRecipientJID(recipient)
		if err != nil {
			t.Errorf("handler passed an unreadable recipient %q: %v", recipient, err)
		}
		sent := sentMessage{ID: "OUT9", Timestamp: ts}
		sent.ChatJID = persistOutbound(b.Client, b.Store, storageJID, sent, message, outboundMedia{}, "")
		return true, "sent", sent
	}

	if rec := postSend(mux, dialledNumber); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	rows, err := b.Store.db.Query("SELECT jid, name FROM chats")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	chats := map[string]string{}
	for rows.Next() {
		var jid, name string
		if err := rows.Scan(&jid, &name); err != nil {
			t.Fatal(err)
		}
		chats[jid] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{registeredJID.String(): "Existing contact"}; !reflect.DeepEqual(chats, want) {
		t.Fatalf("chats = %v, want only the existing one, name kept: %v", chats, want)
	}
	var chatJID string
	if err := b.Store.db.QueryRow("SELECT chat_jid FROM messages WHERE id = ?", "OUT9").Scan(&chatJID); err != nil {
		t.Fatal(err)
	}
	if chatJID != registeredJID.String() {
		t.Errorf("message stored under %s, want %s", chatJID, registeredJID)
	}
}

func decodeAPIError(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the JSON error shape: %v: %s", err, rec.Body.String())
	}
	return body
}

func TestSendRefusesANumberThatIsNotOnWhatsApp(t *testing.T) {
	ask := &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{
		"+" + dialledNumber: {JID: dialledJID, IsIn: false},
	}}
	// Listed or not, a number WhatsApp says has no account gets the same answer.
	for _, allowed := range []string{"", dialledNumber} {
		b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, ask)
		b.Policy = parseChatPolicy(allowed)
		rec := postSend(mux, dialledNumber)
		body := decodeAPIError(t, rec)
		if rec.Code != http.StatusNotFound || body.Error.Code != "not_found" {
			t.Fatalf("allow-list %q: status %d code %q, want 404 not_found: %s", allowed, rec.Code, body.Error.Code, rec.Body.String())
		}
		if !strings.Contains(body.Message, "is not on WhatsApp") || !strings.Contains(body.Message, dialledNumber) {
			t.Errorf("message does not say the number is not on WhatsApp: %q", body.Message)
		}
		if len(*sentTo) != 0 {
			t.Errorf("nothing may be sent, got %v", *sentTo)
		}
		if got := b.metrics.sendFailures.Load(); got != 1 {
			t.Errorf("send failures counted = %d, want 1", got)
		}
	}
}

// A question WhatsApp did not answer says nothing about the number, so it is
// never reported as "not on WhatsApp". What happens next depends on whether
// there is an allow-list to protect.
func TestSendWhenTheRegisteredNumberCannotBeChecked(t *testing.T) {
	unanswered := map[string]func() *fakeIsOnWhatsApp{
		"the lookup failed":     func() *fakeIsOnWhatsApp { return &fakeIsOnWhatsApp{err: errors.New("info query timed out")} },
		"the answer came empty": func() *fakeIsOnWhatsApp { return &fakeIsOnWhatsApp{} },
	}
	for name, newAsk := range unanswered {
		// With an allow-list the bridge cannot tell which number the message
		// would go to, so it refuses, even when both spellings are listed.
		t.Run(name+", allow-list set: refused", func(t *testing.T) {
			b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, newAsk())
			b.Policy = parseChatPolicy(dialledNumber + "," + registeredNumber)
			rec := postSend(mux, dialledNumber)
			body := decodeAPIError(t, rec)
			if rec.Code != http.StatusBadGateway || body.Error.Code != "bridge_unavailable" {
				t.Fatalf("status %d code %q, want 502 bridge_unavailable: %s", rec.Code, body.Error.Code, rec.Body.String())
			}
			if !strings.Contains(body.Message, "could not check the number with WhatsApp") || !strings.Contains(body.Message, "nothing was sent") {
				t.Errorf("message does not name the failed lookup: %q", body.Message)
			}
			if strings.Contains(body.Message, "is not on WhatsApp") {
				t.Errorf("an unanswered lookup must not read as an unregistered number: %q", body.Message)
			}
			if len(*sentTo) != 0 {
				t.Errorf("nothing may be sent, got %v", *sentTo)
			}
			if got := b.metrics.sendFailures.Load(); got != 1 {
				t.Errorf("send failures counted = %d, want 1", got)
			}
		})
		// Without one there is nothing to protect: the send goes out to the
		// number exactly as typed, as every send did before the lookup existed.
		t.Run(name+", no allow-list: sent as typed", func(t *testing.T) {
			b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, newAsk())
			if rec := postSend(mux, dialledNumber); rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if want := []string{dialledNumber}; !reflect.DeepEqual(*sentTo, want) {
				t.Errorf("sent to %v, want the recipient as typed %v", *sentTo, want)
			}
			if log := sendLog(b); !strings.Contains(log, "[WARN] could not check the number with WhatsApp") || !strings.Contains(log, "as typed") {
				t.Errorf("the fallback must be logged as a warning:\n%s", log)
			}
		})
	}
}

func TestSendRefusesARecipientItCannotRead(t *testing.T) {
	for _, recipient := range []string{"123:notadevice@s.whatsapp.net", "+55 11 98888-7777x"} {
		ask := &fakeIsOnWhatsApp{}
		_, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, ask)
		rec := postSend(mux, recipient)
		if body := decodeAPIError(t, rec); rec.Code != http.StatusBadRequest || body.Error.Code != "invalid_argument" {
			t.Errorf("%q: status %d code %q, want 400 invalid_argument", recipient, rec.Code, body.Error.Code)
		}
		if len(ask.calls) != 0 || len(*sentTo) != 0 {
			t.Errorf("%q: asked %v, sent %v; want neither", recipient, ask.calls, *sentTo)
		}
	}
}

// WHATSAPP_ALLOWED_CHATS covers the recipient as typed and the number it is
// registered under: listing one spelling must not open the door to the other.
func TestSendAllowListCoversTheRegisteredNumber(t *testing.T) {
	cases := []struct {
		name       string
		allowed    string
		wantStatus int
		wantAsked  bool   // was WhatsApp asked at all
		wantNamed  string // the JID the refusal names
	}{
		{
			name: "only the dialled spelling is listed: the registered number is refused", allowed: dialledNumber,
			wantStatus: http.StatusForbidden, wantAsked: true, wantNamed: registeredJID.String(),
		},
		{
			name: "only the registered number is listed: the typed one is refused before any lookup", allowed: registeredNumber,
			wantStatus: http.StatusForbidden, wantAsked: false, wantNamed: dialledNumber,
		},
		{
			name: "an unrelated chat and every group are listed", allowed: "5511977776666,*@g.us",
			wantStatus: http.StatusForbidden, wantAsked: false, wantNamed: dialledNumber,
		},
		{
			name: "the dialled spelling and every group: still not the registered number", allowed: dialledNumber + ",*@g.us",
			wantStatus: http.StatusForbidden, wantAsked: true, wantNamed: registeredJID.String(),
		},
		{
			name: "both spellings are listed", allowed: dialledNumber + "," + registeredJID.String(),
			wantStatus: http.StatusOK, wantAsked: true,
		},
		{
			name: "every personal chat is allowed", allowed: "*@s.whatsapp.net",
			wantStatus: http.StatusOK, wantAsked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ask := registeredWithoutNinthDigit()
			b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, ask)
			b.Policy = parseChatPolicy(tc.allowed)

			rec := postSend(mux, dialledNumber)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if asked := len(ask.calls) > 0; asked != tc.wantAsked {
				t.Errorf("IsOnWhatsApp calls = %v, want asked=%v", ask.calls, tc.wantAsked)
			}
			if tc.wantStatus == http.StatusOK {
				if want := []string{registeredJID.String()}; !reflect.DeepEqual(*sentTo, want) {
					t.Errorf("sent to %v, want %v", *sentTo, want)
				}
				return
			}
			if len(*sentTo) != 0 {
				t.Errorf("a refused recipient was sent to: %v", *sentTo)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			message, _ := body["message"].(string)
			if body["success"] != false || !strings.Contains(message, chatPolicyEnv) || !strings.Contains(message, "chat "+tc.wantNamed+" ") {
				t.Errorf("refusal = %v, want one naming %s and %s", body, tc.wantNamed, chatPolicyEnv)
			}
		})
	}
}

func TestSendAsksNothingForARecipientItAlreadyKnows(t *testing.T) {
	known := types.NewJID("5511999990000", types.DefaultUserServer)
	lids := &mockLIDStore{lidByPN: map[types.JID]types.JID{known: types.NewJID("135792468013579", types.HiddenUserServer)}}
	for _, recipient := range []string{known.User, known.String(), "120363012345678901@g.us", registeredLID.String()} {
		ask := &fakeIsOnWhatsApp{}
		_, mux, sentTo := sendRecipientBridge(t, lids, ask)
		if rec := postSend(mux, recipient); rec.Code != http.StatusOK {
			t.Fatalf("%q: status %d: %s", recipient, rec.Code, rec.Body.String())
		}
		if len(ask.calls) != 0 {
			t.Errorf("%q: IsOnWhatsApp calls = %v, want none", recipient, ask.calls)
		}
		want := recipient
		if !strings.Contains(recipient, "@") {
			want = recipient + "@" + types.DefaultUserServer
		}
		if !reflect.DeepEqual(*sentTo, []string{want}) {
			t.Errorf("%q: sent to %v, want %s unchanged", recipient, *sentTo, want)
		}
	}
}

// Offline, nothing can be asked, so nothing is handed to the send: a
// reconnect between the two must not let a recipient through unchecked.
func TestSendIsRefusedWhileDisconnected(t *testing.T) {
	ask := registeredWithoutNinthDigit()
	b, mux, sentTo := sendRecipientBridge(t, &mockLIDStore{}, ask)
	b.Connected = func() bool { return false }

	rec := postSend(mux, dialledNumber)
	if body := decodeAPIError(t, rec); rec.Code != http.StatusInternalServerError || body.Message != notConnectedMessage {
		t.Fatalf("status %d message %q, want 500 %q", rec.Code, body.Message, notConnectedMessage)
	}
	if len(ask.calls) != 0 || len(*sentTo) != 0 {
		t.Errorf("asked %v, sent %v; want neither while disconnected", ask.calls, *sentTo)
	}
	if got := b.metrics.sendFailures.Load(); got != 1 {
		t.Errorf("send failures counted = %d, want 1", got)
	}
}
