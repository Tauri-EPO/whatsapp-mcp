package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type fakeCreationClient struct {
	created      []whatsmeow.ReqCreateGroup
	participants []types.GroupParticipant
	groups       []*types.GroupLinkTarget
}

func (f *fakeCreationClient) CreateGroup(_ context.Context, req whatsmeow.ReqCreateGroup) (*types.GroupInfo, error) {
	f.created = append(f.created, req)
	return &types.GroupInfo{JID: types.NewJID("120363000000000002", types.GroupServer), Participants: f.participants}, nil
}
func (f *fakeCreationClient) GetSubGroups(context.Context, types.JID) ([]*types.GroupLinkTarget, error) {
	return f.groups, nil
}

func TestCreateGroupAuthorizationAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name, policy, body string
		status             int
	}{
		{"no wildcard", "5511999999999", `{"name":"Alice","participants":["5511999999999"]}`, 403},
		{"exact group insufficient", "120363000000000002@g.us,5511999999999", `{"name":"Alice","participants":["5511999999999"]}`, 403},
		{"outside participant", "*@g.us,5511999999999", `{"name":"Alice","participants":["5511999999999","5511888888888"]}`, 403},
		{"malformed participant", "", `{"name":"Alice","participants":["5511999999999@s.whatsapp.net@lid"]}`, 400},
		{"device participant", "", `{"name":"Alice","participants":["5511999999999:2@s.whatsapp.net"]}`, 400},
		{"not a parent group", "", `{"name":"Alice","parent_jid":"5511999999999@s.whatsapp.net"}`, 400},
		{"community and parent", "", `{"name":"Alice","community":true,"parent_jid":"120363000000000001@g.us"}`, 400},
		{"empty name", "", `{"name":" "}`, 400},
		{"too long name", "", `{"name":"abcdefghijklmnopqrstuvwxyz"}`, 400},
		{"allowed", "*@g.us,5511999999999", `{"name":"Alice","participants":["5511999999999","5511999999999"],"parent_jid":"120363000000000001@g.us"}`, 200},
		{"community", "", `{"name":"Alice","community":true}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeCreationClient{}
			var targets []string
			d := groupCreationDeps{client: client, connected: func() bool { return true }, policy: parseChatPolicy(tc.policy), allowSend: func(_ http.ResponseWriter, _ *http.Request, p ...string) bool { targets = p; return true }}
			w := httptest.NewRecorder()
			handleCreateGroup(d)(w, httptest.NewRequest("POST", "/api/group/create", strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.status != 200 {
				if len(client.created)+len(targets) != 0 {
					t.Fatal("effect before denial")
				}
				return
			}
			if len(client.created) != 1 {
				t.Fatal(client.created)
			}
			if tc.name == "allowed" {
				if len(targets) != 1 || targets[0] != "5511999999999@s.whatsapp.net" || len(client.created[0].Participants) != 1 || client.created[0].LinkedParentJID.String() != "120363000000000001@g.us" {
					t.Fatalf("targets=%v create=%+v", targets, client.created)
				}
			} else if !client.created[0].IsParent || client.created[0].DefaultMembershipApprovalMode != "" {
				t.Fatal(client.created[0])
			}
		})
	}
}

func TestCreateGroupLimitsAndParticipantResults(t *testing.T) {
	client := &fakeCreationClient{participants: []types.GroupParticipant{
		{JID: types.NewJID("5511999999999", types.DefaultUserServer), Error: 403, AddRequest: &types.GroupParticipantAddRequest{Code: "fake-invite"}},
		{JID: types.NewJID("5511888888888", types.DefaultUserServer), Error: 406},
		{JID: types.NewJID("100000000000001", types.HiddenUserServer), AddRequest: &types.GroupParticipantAddRequest{Code: "fake-second"}},
		{JID: types.NewJID("100000000000002", types.HiddenUserServer)},
	}}
	d := groupCreationDeps{client: client, connected: func() bool { return true }, policy: chatPolicy{}}
	d.allowSend = func(w http.ResponseWriter, _ *http.Request, _ ...string) bool {
		writeError(w, 429, "budget")
		return false
	}
	w := httptest.NewRecorder()
	handleCreateGroup(d)(w, httptest.NewRequest("POST", "/api/group/create", strings.NewReader(`{"name":"Alice","participants":["5511999999999"]}`)))
	if w.Code != 429 || len(client.created) != 0 {
		t.Fatal(w.Code, client.created)
	}
	d.allowSend = nil
	w = httptest.NewRecorder()
	handleCreateGroup(d)(w, httptest.NewRequest("POST", "/api/group/create", strings.NewReader(`{"name":"Alice","parent_jid":"120363000000000001@g.us"}`)))
	var result struct {
		GroupJID  string `json:"group_jid"`
		ParentJID string `json:"parent_jid"`
		NotAdded  []struct {
			JID        string `json:"jid"`
			Error      int    `json:"error"`
			InviteCode string `json:"invite_code"`
		} `json:"not_added"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.GroupJID != "120363000000000002@g.us" || result.ParentJID != "120363000000000001@g.us" {
		t.Fatal(result)
	}
	failures := result.NotAdded
	if len(failures) != 3 {
		t.Fatal(failures)
	}
	if failures[0].JID != "5511999999999@s.whatsapp.net" || failures[0].InviteCode != "fake-invite" || failures[0].Error != 403 || failures[1].JID != "5511888888888@s.whatsapp.net" || failures[1].Error != 406 || failures[1].InviteCode != "" || failures[2].Error != 0 || failures[2].InviteCode != "fake-second" {
		t.Fatal(failures)
	}
}

func TestSubGroupsFilter(t *testing.T) {
	parent := types.NewJID("120363000000000001", types.GroupServer)
	child := types.NewJID("120363000000000002", types.GroupServer)
	client := &fakeCreationClient{groups: []*types.GroupLinkTarget{{JID: parent}, {JID: child}}}
	d := groupCreationDeps{client: client, connected: func() bool { return true }, policy: parseChatPolicy(parent.String())}
	w := httptest.NewRecorder()
	handleSubGroups(d)(w, httptest.NewRequest("GET", "/api/group/subgroups?jid="+parent.String(), nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), child.String()) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	handleSubGroups(d)(w, httptest.NewRequest("GET", "/api/group/subgroups?jid="+child.String(), nil))
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestCreateGroupDurableFirstContactBudget(t *testing.T) {
	b := newSettingsBridge(t)
	limitPatch(t, b, `{"send.new_chats_per_day":1}`)
	client := &fakeCreationClient{}
	d := groupCreationDeps{client: client, connected: func() bool { return true }, policy: chatPolicy{}, allowSend: b.allowGroupCreation}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleCreateGroup(d)(w, httptest.NewRequest("POST", "/api/group/create", strings.NewReader(body)))
		return w
	}
	w := post(`{"name":"Alice","participants":["5511999999999"]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	usage, err := b.sendUsageSnapshot(context.Background())
	if err != nil || usage.Today != 2 || usage.NewChatsToday != 1 {
		t.Fatal(usage, err)
	}
	w = post(`{"name":"Bob","participants":["5511888888888"]}`)
	if w.Code != 429 || len(client.created) != 1 {
		t.Fatal(w.Code, w.Body.String(), client.created)
	}
}

func TestCreateEmptyGroupConsumesMinuteBudget(t *testing.T) {
	t.Setenv("WHATSAPP_SEND_RATE_PER_MINUTE", "1")
	b := newSettingsBridge(t)
	var err error
	b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
		if name == sendRateMinuteEnv {
			return os.Getenv(name)
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeCreationClient{}
	d := groupCreationDeps{client: client, connected: func() bool { return true }, policy: chatPolicy{}, allowSend: b.allowGroupCreation}
	for i, want := range []int{200, 429} {
		w := httptest.NewRecorder()
		handleCreateGroup(d)(w, httptest.NewRequest("POST", "/api/group/create", strings.NewReader(`{"name":"Alice"}`)))
		if w.Code != want {
			t.Fatalf("call %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	usage, err := b.sendUsageSnapshot(context.Background())
	if err != nil || usage.Today != 1 || usage.NewChatsToday != 0 || len(client.created) != 1 {
		t.Fatal(usage, err, client.created)
	}
}
