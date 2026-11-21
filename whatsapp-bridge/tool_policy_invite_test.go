package main

// Issue #851 split get_group_invite_link: the read keeps the old name and
// reset_group_invite_link revokes the link. Both reach /api/group/invite, so the
// bridge decides per request (the reset flag) and a list naming the old tool
// keeps exactly the power it had before the split.

import (
	"net/http"
	"strings"
	"testing"
)

func TestGroupInvitePolicyPerRequest(t *testing.T) {
	const (
		read  = `{"group_jid":"120363000000000001@g.us"}`
		reset = `{"group_jid":"120363000000000001@g.us","reset":true}`
	)
	for _, tc := range []struct {
		name         string
		allow, deny  string
		body         string
		wantRefusal  bool
		refusalNames string
	}{
		// Lists written before the split: the old name governs both requests.
		{"old deny refuses the read", "", "get_group_invite_link", read, true, "get_group_invite_link"},
		{"old deny refuses the reset", "", "get_group_invite_link", reset, true, "get_group_invite_link"},
		{"old allow permits the read", "get_group_invite_link", "", read, false, ""},
		{"old allow permits the reset", "get_group_invite_link", "", reset, false, ""},
		{"allow without either refuses the reset", "list_chats", "", reset, true, "get_group_invite_link"},
		// The new name on its own narrows only the reset.
		{"new deny keeps the read", "", "reset_group_invite_link", read, false, ""},
		{"new deny refuses the reset", "", "reset_group_invite_link", reset, true, "reset_group_invite_link"},
		{"new allow refuses the read", "reset_group_invite_link", "", read, true, "get_group_invite_link"},
		{"new allow permits the reset", "reset_group_invite_link", "", reset, false, ""},
		{"read allowed, reset denied", "get_group_invite_link", "reset_group_invite_link", reset, true, "reset_group_invite_link"},
		{"read allowed, reset denied keeps the read", "get_group_invite_link", "reset_group_invite_link", read, false, ""},
		{"spelling the flag differently is still a reset", "", "reset_group_invite_link",
			`{"group_jid":"120363000000000001@g.us","RESET":true} trailing`, true, "reset_group_invite_link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := toolPolicyRequest(t, false, mustToolPolicy(t, tc.allow, tc.deny), "/api/group/invite", tc.body)
			got := rec.Body.String()
			refused := rec.Code == http.StatusForbidden && strings.Contains(got, "is disabled")
			if refused != tc.wantRefusal {
				t.Fatalf("refused = %v, want %v (status %d): %s", refused, tc.wantRefusal, rec.Code, got)
			}
			if refused && !strings.Contains(got, tc.refusalNames) {
				t.Errorf("refusal %q should name %s", got, tc.refusalNames)
			}
			if !refused && strings.Contains(got, "invalid JSON body") {
				t.Errorf("the guard consumed the body the handler needs: %s", got)
			}
		})
	}
}

func TestGroupInviteResetToolNameIsKnown(t *testing.T) {
	if _, err := newToolPolicy("reset_group_invite_link", "reset_group_invite_link"); err != nil {
		t.Fatalf("reset_group_invite_link must be a valid list entry: %v", err)
	}
}

// Read-only still refuses both requests, as before the split.
func TestGroupInviteReadOnlyRefusesBoth(t *testing.T) {
	for _, body := range []string{`{"group_jid":"120363000000000001@g.us"}`, `{"group_jid":"120363000000000001@g.us","reset":true}`} {
		rec := toolPolicyRequest(t, true, mustToolPolicy(t, "get_group_invite_link", ""), "/api/group/invite", body)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), readOnlyEnv) {
			t.Fatalf("%s: status = %d, want read-only 403: %s", body, rec.Code, rec.Body.String())
		}
	}
}
