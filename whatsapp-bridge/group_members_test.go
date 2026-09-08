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
	resp := buildGroupMembers(fakeGroup(), func(jid types.JID) string { return names[jid.String()] }, nil)

	if !resp.Success || resp.Name != "Obra Rua A" || resp.Topic != "Combinados" || resp.Participant != 3 {
		t.Fatalf("unexpected header: %+v", resp)
	}
	if resp.OwnerJID != "5511999999999@s.whatsapp.net" {
		t.Fatalf("owner = %q", resp.OwnerJID)
	}
	if resp.Owner == nil || resp.Owner.JID != "5511999999999@s.whatsapp.net" ||
		resp.Owner.PhoneNumber != "5511999999999" || resp.Owner.Name != "Enrico" {
		t.Fatalf("owner block = %+v", resp.Owner)
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

// A modern group hands back a bare LID as the owner (issue #396): it has to
// come out dual-addressed like a participant, from the roster when the owner is
// still in the group and from the LID map otherwise.
func TestBuildGroupOwner(t *testing.T) {
	lid := func(user string) types.JID { return types.JID{User: user, Server: types.HiddenUserServer} }
	phone := func(user string) types.JID { return types.JID{User: user, Server: types.DefaultUserServer} }
	names := map[string]string{
		"5511999999999@s.whatsapp.net": "Enrico",
		"5511888888888@s.whatsapp.net": "Ana",
		"5511777777777@s.whatsapp.net": "Bruno",
	}
	nameOf := func(jid types.JID) string { return names[jid.String()] }
	alts := map[string]types.JID{
		"999@lid":                      phone("5511777777777"),
		"998@lid":                      phone("5511666666666"),
		"5511999999999@s.whatsapp.net": lid("111"),
	}
	altOf := func(jid types.JID) types.JID { return alts[jid.String()] }

	cases := []struct {
		name          string
		owner         types.JID
		wantJID       string
		wantPhone     string
		wantLID       string
		wantName      string
		wantAltLookup int
	}{
		{
			name: "LID of a participant resolves from the roster",
			// Ana is in the group with both forms, so no store lookup is needed.
			owner: lid("777"), wantJID: "5511888888888@s.whatsapp.net",
			wantPhone: "5511888888888", wantLID: "777@lid", wantName: "Ana",
		},
		{
			name:  "LID of an owner who left resolves from the map",
			owner: lid("999"), wantJID: "5511777777777@s.whatsapp.net",
			wantPhone: "5511777777777", wantLID: "999@lid", wantName: "Bruno", wantAltLookup: 1,
		},
		{
			name:  "unknown LID stays a bare LID",
			owner: lid("42"), wantJID: "42@lid", wantLID: "42@lid", wantAltLookup: 1,
		},
		{
			name:  "phone owner gains the LID from the map",
			owner: phone("5511999999999"), wantJID: "5511999999999@s.whatsapp.net",
			wantPhone: "5511999999999", wantLID: "111@lid", wantName: "Enrico", wantAltLookup: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			info := fakeGroup()
			info.OwnerJID = tc.owner
			resp := buildGroupMembers(info, nameOf, func(jid types.JID) types.JID {
				lookups++
				return alts[jid.String()]
			})
			owner := resp.Owner
			if owner == nil {
				t.Fatalf("no owner block")
			}
			if owner.JID != tc.wantJID || owner.PhoneNumber != tc.wantPhone || owner.LID != tc.wantLID || owner.Name != tc.wantName {
				t.Fatalf("owner = %+v", owner)
			}
			if resp.OwnerJID != tc.wantJID {
				t.Fatalf("owner_jid = %q, want %q", resp.OwnerJID, tc.wantJID)
			}
			if lookups != tc.wantAltLookup {
				t.Fatalf("LID map lookups = %d, want %d", lookups, tc.wantAltLookup)
			}
		})
	}

	// No owner (whatsmeow leaves it empty for a group we did not fetch fully)
	// and no LID map: neither may invent a block or panic.
	info := fakeGroup()
	info.OwnerJID = types.EmptyJID
	if resp := buildGroupMembers(info, nameOf, nil); resp.Owner != nil || resp.OwnerJID != "" {
		t.Fatalf("empty owner = %+v / %q", resp.Owner, resp.OwnerJID)
	}
	info.OwnerJID = lid("42")
	if resp := buildGroupMembers(info, nameOf, nil); resp.Owner == nil || resp.Owner.JID != "42@lid" {
		t.Fatalf("owner without a resolver = %+v", resp.Owner)
	}

	// The owner is reported as a LID while the roster carries that person under
	// a phone JID only: the display name sits on that row and nobody in the
	// contact store knows the number, so only a second look at the roster —
	// after the map has translated the LID — finds it.
	info = fakeGroup()
	info.OwnerJID = lid("998")
	info.Participants = append(info.Participants, types.GroupParticipant{
		JID:         phone("5511666666666"),
		DisplayName: "Bruno B.",
	})
	owner := buildGroupMembers(info, nameOf, altOf).Owner
	if owner == nil || owner.JID != "5511666666666@s.whatsapp.net" || owner.LID != "998@lid" || owner.Name != "Bruno B." {
		t.Fatalf("owner known only to the roster = %+v", owner)
	}
}

func TestHandleGroupMembers(t *testing.T) {
	fetch := func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		if jid.User == "404" {
			return nil, errors.New("not a member")
		}
		return fakeGroup(), nil
	}
	h := handleGroupMembers(fetch, nil, nil, parseChatPolicy("*@g.us"), nil)

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
	if resp.Owner == nil || resp.Owner.JID != resp.OwnerJID {
		t.Fatalf("owner block = %+v, owner_jid = %q", resp.Owner, resp.OwnerJID)
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
	restricted := handleGroupMembers(fetch, nil, nil, parseChatPolicy("5511999999999"), nil)
	rec = httptest.NewRecorder()
	restricted(rec, httptest.NewRequest(http.MethodGet, "/api/group/members?jid=120363000000000001@g.us", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("policy status = %d", rec.Code)
	}
}
