package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func fixedIdentity(phone, lid types.JID) ownIdentity {
	return func(context.Context) (types.JID, types.JID) { return phone, lid }
}

func callMe(t *testing.T, identity ownIdentity) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handleMe(identity)(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	return rec
}

// The whole point of the endpoint: an agent can learn both spellings of the
// account it is driving, so a mention (rendered as the LID) is recognisable.
func TestHandleMeReportsPhoneAndLID(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("158883943301358", types.HiddenUserServer)

	rec := callMe(t, fixedIdentity(phone, lid))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PhoneJID != "5511999999999@s.whatsapp.net" || got.Phone != "5511999999999" {
		t.Errorf("phone fields = %q / %q", got.PhoneJID, got.Phone)
	}
	if got.LIDJID == nil || *got.LIDJID != "158883943301358@lid" {
		t.Errorf("lid_jid = %v", got.LIDJID)
	}
	if got.LID == nil || *got.LID != "158883943301358" {
		t.Errorf("lid = %v", got.LID)
	}
}

// A session without a LID answers with nulls rather than an empty string, so a
// caller can tell "no LID" from "the empty LID".
func TestHandleMeWithoutLID(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	rec := callMe(t, fixedIdentity(phone, types.EmptyJID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["lid_jid"] != nil || body["lid"] != nil {
		t.Errorf("expected null LID fields, got %v / %v", body["lid_jid"], body["lid"])
	}
}

// REST starts before pairing (AGENTS.md gotcha 14), so an unpaired bridge has
// no identity to report.
func TestHandleMeUnpaired(t *testing.T) {
	rec := callMe(t, fixedIdentity(types.EmptyJID, types.EmptyJID))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "bridge_unavailable" {
		t.Errorf("error code = %q", body.Error.Code)
	}
}

// The owner's number is personal data: /api/me is behind the same bearer token
// as every other /api/* route, and it never answers an unauthenticated caller.
func TestMeRequiresAuth(t *testing.T) {
	const token = "supersecrettoken1234567890abcdef"
	allowed, _ := buildHostAllowList(8080, defaultBridgeBind, "")
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	handler := withAuth(token, allowed, handleMe(fixedIdentity(phone, types.EmptyJID)))

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/me", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "5511999999999") {
		t.Fatalf("the refusal leaked the phone number: %s", body)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", rec.Code)
	}
}

// The route is registered, GET-only, and not on the health endpoints.
func TestMeRouteIsRegisteredAndGETOnly(t *testing.T) {
	const token = "supersecrettoken1234567890abcdef"
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	mux := b.newRESTMux(8080, token)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "http://127.0.0.1:8080/api/me", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		// Unpaired in the test bridge, so GET is 503 rather than 200; what
		// matters is that POST is refused as a method, not routed.
		want := http.StatusServiceUnavailable
		if method == http.MethodPost {
			want = http.StatusMethodNotAllowed
		}
		if rec.Code != want {
			t.Errorf("%s /api/me = %d, want %d (body %s)", method, rec.Code, want, rec.Body.String())
		}
	}

	// Health must stay free of identity: it is polled constantly and logged.
	health := b.healthStatus()
	for _, key := range []string{"phone", "phone_jid", "lid", "lid_jid"} {
		if _, ok := health[key]; ok {
			t.Errorf("/api/health carries %q; identity belongs on /api/me only", key)
		}
	}
}
