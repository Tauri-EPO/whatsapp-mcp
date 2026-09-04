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

func fakeGroup() *types.GroupInfo {
	g := &types.GroupInfo{
		JID:      types.JID{User: "120363000000000001", Server: types.GroupServer},
		OwnerJID: types.JID{User: "5511999999999", Server: types.DefaultUserServer},
	}
	g.Name = "Obra Rua A"
	g.Topic = "Combinados"
	g.Participants = []types.GroupParticipant{
		{JID: types.JID{User: "5511999999999", Server: types.DefaultUserServer}, IsSuperAdmin: true},
		{
			JID:         types.JID{User: "777", Server: types.HiddenUserServer},
			PhoneNumber: types.JID{User: "5511888888888", Server: types.DefaultUserServer},
			LID:         types.JID{User: "777", Server: types.HiddenUserServer},
			IsAdmin:     true,
		},
		{JID: types.JID{User: "888", Server: types.HiddenUserServer}, DisplayName: "anon"},
	}
	return g
}

func TestBuildGroupMembers(t *testing.T) {
	names := map[string]string{"5511999999999@s.whatsapp.net": "Enrico", "5511888888888@s.whatsapp.net": "Ana"}
	resp := buildGroupMembers(fakeGroup(), func(jid types.JID) string { return names[jid.String()] })

	if !resp.Success || resp.Name != "Obra Rua A" || resp.Topic != "Combinados" || resp.Participant != 3 {
		t.Fatalf("unexpected header: %+v", resp)
	}
	if resp.OwnerJID != "5511999999999@s.whatsapp.net" {
		t.Fatalf("owner = %q", resp.OwnerJID)
	}
	owner, ana, anon := resp.Members[0], resp.Members[1], resp.Members[2]
	if owner.PhoneNumber != "5511999999999" || owner.Name != "Enrico" || !owner.IsAdmin || !owner.IsSuperAdmin {
		t.Fatalf("owner = %+v", owner)
	}
	if ana.JID != "777@lid" || ana.PhoneNumber != "5511888888888" || ana.LID != "777@lid" || ana.Name != "Ana" || !ana.IsAdmin || ana.IsSuperAdmin {
		t.Fatalf("ana = %+v", ana)
	}
	if anon.PhoneNumber != "" || anon.LID != "888@lid" || anon.Name != "anon" || anon.IsAdmin {
		t.Fatalf("anon = %+v", anon)
	}
}

func TestHandleGroupMembers(t *testing.T) {
	fetch := func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		if jid.User == "404" {
			return nil, errors.New("not a member")
		}
		return fakeGroup(), nil
	}
	h := handleGroupMembers(fetch, nil, parseChatPolicy("*@g.us"))

	do := func(method, target, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		} else {
			req = httptest.NewRequest(method, target, nil)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	rec := do(http.MethodGet, "/api/group/members?jid=120363000000000001@g.us", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp GroupMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Members) != 3 {
		t.Fatalf("bad body: %v %s", err, rec.Body.String())
	}

	if rec := do(http.MethodPost, "/api/group/members", `{"group_jid":"120363000000000001@g.us"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/group/members", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing jid status = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/group/members?jid=5511999999999@s.whatsapp.net", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-group jid status = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/group/members?jid=404@g.us", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("fetch error status = %d", rec.Code)
	}

	// Allow-list applies to groups too.
	restricted := handleGroupMembers(fetch, nil, parseChatPolicy("5511999999999"))
	rec = httptest.NewRecorder()
	restricted(rec, httptest.NewRequest(http.MethodGet, "/api/group/members?jid=120363000000000001@g.us", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("policy status = %d", rec.Code)
	}
}
