package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

type groupCalls struct {
	participants []types.JID
	action       whatsmeow.ParticipantChange
	name, desc   string
	reset        *bool
	left         bool
}

func fakeGroupOps(calls *groupCalls, fail error) groupOps {
	return groupOps{
		updateParticipants: func(_ context.Context, _ types.JID, p []types.JID, a whatsmeow.ParticipantChange) ([]types.GroupParticipant, error) {
			calls.participants, calls.action = p, a
			if fail != nil {
				return nil, fail
			}
			out := make([]types.GroupParticipant, 0, len(p))
			for _, j := range p {
				out = append(out, types.GroupParticipant{JID: j, PhoneNumber: j, IsAdmin: a == whatsmeow.ParticipantChangePromote})
			}
			return out, nil
		},
		setName: func(_ context.Context, _ types.JID, n string) error { calls.name = n; return fail },
		getInfo: func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
			return &types.GroupInfo{JID: jid}, fail
		},
		setTopic: func(_ context.Context, _ types.JID, topic groupTopicUpdate) error {
			calls.desc = topic.Description
			return fail
		},
		inviteLink: func(_ context.Context, _ types.JID, reset bool) (string, error) {
			calls.reset = &reset
			if fail != nil {
				return "", fail
			}
			return "https://chat.whatsapp.com/ABC", nil
		},
		leave: func(_ context.Context, _ types.JID) error { calls.left = true; return fail },
	}
}

func groupPost(t *testing.T, h http.HandlerFunc, body string) (int, groupResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/group/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var resp groupResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

const mgGroup = "120363000000000001@g.us"

func multipleLIDPolicyClient(t *testing.T) (*whatsmeow.Client, types.JID, types.JID) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "whatsapp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	pn := types.NewJID("5511999999999", types.DefaultUserServer)
	other := types.NewJID("100000000000008", types.HiddenUserServer)
	if _, err = db.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES (?, ?), (?, ?)", phoneLID.User, pn.User, other.User, pn.User); err != nil {
		t.Fatal(err)
	}
	container := sqlstore.NewWithDB(db, "sqlite", testLogger())
	if err = container.LIDMap.FillCache(ctx); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(container.LIDMap)
	selected, err := lookupAltJID(ctx, client, pn)
	if err != nil || selected.IsEmpty() {
		t.Fatalf("SDK-selected LID: %s %v", selected, err)
	}
	if selected == other {
		other = phoneLID
	}
	mapped, err := lookupAltJID(ctx, client, other)
	if err != nil || mapped != pn || other == selected {
		t.Fatalf("non-selected LID has no reverse mapping: %s %v", mapped, err)
	}
	return client, pn, other
}

func TestGroupParticipantsMultipleKnownLIDs(t *testing.T) {
	client, pn, allowed := multipleLIDPolicyClient(t)
	selected, _ := lookupAltJID(context.Background(), client, pn)
	for _, action := range []string{"add", "promote"} {
		for _, participant := range []string{pn.User, "551199999999", selected.String()} {
			t.Run(action+"/"+participant, func(t *testing.T) {
				calls := &groupCalls{}
				ops := fakeGroupOps(calls, nil)
				ops.twin = liveGroupOps(client, func() bool { return true }).twin
				body := `{"group_jid":"` + mgGroup + `","action":"` + action + `","participants":["` + participant + `"]}`
				code, _ := groupPost(t, handleGroupParticipants(ops, parseChatPolicy(mgGroup+","+allowed.String())), body)
				if code != http.StatusOK || len(calls.participants) != 1 {
					t.Fatalf("known non-selected LID refused: %d %+v", code, calls)
				}
				calls.participants = nil
				code, _ = groupPost(t, handleGroupParticipants(ops, parseChatPolicy(mgGroup+",5511888888888")), body)
				if code != http.StatusForbidden || len(calls.participants) != 0 {
					t.Fatalf("outside identity accepted: %d %+v", code, calls)
				}
			})
		}
	}
}

func TestGroupParticipantsAllowList(t *testing.T) {
	for _, action := range []string{"add", "remove", "promote", "demote"} {
		t.Run(action, func(t *testing.T) {
			calls := &groupCalls{}
			h := handleGroupParticipants(fakeGroupOps(calls, nil), parseChatPolicy(mgGroup+",5511999999999"))
			code, _ := groupPost(t, h, `{"group_jid":"`+mgGroup+`","action":"`+action+`","participants":["5511999999999","5511888888888"]}`)
			if action == "add" || action == "promote" {
				if code != http.StatusForbidden || len(calls.participants) != 0 {
					t.Fatalf("mixed batch: status %d, effects %+v", code, calls)
				}
			} else if code != http.StatusOK || len(calls.participants) != 2 {
				t.Fatalf("access reduction: status %d, effects %+v", code, calls)
			}
			code, resp := groupPost(t, h, `{"group_jid":"`+mgGroup+`","action":"`+action+`","participants":["+55 (11) 99999-9999","551199999999"]}`)
			if code != http.StatusOK || !resp.Success || len(calls.participants) != 2 {
				t.Fatalf("allowed batch: status %d, response %+v, effects %+v", code, resp, calls)
			}
		})
	}
}

func TestGroupParticipantsAccessReductionWithoutPhone(t *testing.T) {
	for _, action := range []string{"remove", "demote"} {
		t.Run(action, func(t *testing.T) {
			calls := &groupCalls{}
			ops := fakeGroupOps(calls, nil)
			ops.twin = func(context.Context, types.JID) (types.JID, error) {
				t.Fatal("access reduction must not require a phone/LID lookup")
				return types.EmptyJID, nil
			}
			code, resp := groupPost(t, handleGroupParticipants(ops, parseChatPolicy(mgGroup)), `{"group_jid":"`+mgGroup+`","action":"`+action+`","participants":["`+phoneLID.String()+`"]}`)
			if code != http.StatusOK || !resp.Success || len(calls.participants) != 1 || calls.participants[0] != phoneLID {
				t.Fatalf("unmapped LID access reduction: %d %+v %+v", code, resp, calls)
			}
			calls.participants = nil
			code, _ = groupPost(t, handleGroupParticipants(ops, parseChatPolicy(phonePN.String())), `{"group_jid":"`+mgGroup+`","action":"`+action+`","participants":["`+phoneLID.String()+`"]}`)
			if code != http.StatusForbidden || len(calls.participants) != 0 {
				t.Fatalf("denied group access reduction: %d %+v", code, calls)
			}
		})
	}
}

func TestGroupParticipantsUnrestrictedOversizedMessage(t *testing.T) {
	calls := &groupCalls{}
	item := strings.Repeat("1", 16)
	code, resp := groupPost(t, handleGroupParticipants(fakeGroupOps(calls, nil), chatPolicy{}), `{"group_jid":"`+mgGroup+`","action":"add","participants":["`+item+`"]}`)
	want := "invalid participant " + item + " (use a phone number or a user JID)"
	if code != http.StatusBadRequest || resp.Message != want || len(calls.participants) != 0 {
		t.Fatalf("unrestricted oversized participant: %d %+v %+v", code, resp, calls)
	}
}

func TestGroupParticipantsIdentityTwins(t *testing.T) {
	for _, tc := range []struct {
		policy, participant string
		allowed             bool
	}{
		{phonePN.String(), phoneLID.String(), true},
		{phoneLID.String(), phonePN.String(), true},
		{"551199999999", "5511999999999", true},
		{"551133334444", "5511999999999", false},
		{mgGroup, phoneLID.String(), false},
	} {
		t.Run(tc.policy+"/"+tc.participant, func(t *testing.T) {
			calls := &groupCalls{}
			ops := fakeGroupOps(calls, nil)
			client := newTestClient(&mockLIDStore{lidByPN: map[types.JID]types.JID{phonePN: phoneLID}, pnByLID: map[types.JID]types.JID{phoneLID: phonePN}})
			ops.twin = liveGroupOps(client, func() bool { return true }).twin
			code, _ := groupPost(t, handleGroupParticipants(ops, parseChatPolicy(mgGroup+","+tc.policy)), `{"group_jid":"`+mgGroup+`","action":"add","participants":["`+tc.participant+`"]}`)
			if tc.allowed && (code != http.StatusOK || len(calls.participants) != 1) {
				t.Fatalf("allowed identity: %d %+v", code, calls)
			}
			if !tc.allowed && (code != http.StatusForbidden || len(calls.participants) != 0) {
				t.Fatalf("denied identity: %d %+v", code, calls)
			}
		})
	}
}

func TestGroupParticipantsAddAndPromote(t *testing.T) {
	calls := &groupCalls{}
	h := handleGroupParticipants(fakeGroupOps(calls, nil), chatPolicy{})
	code, resp := groupPost(t, h, `{"group_jid":"`+mgGroup+`","action":"add","participants":["+5511999999999","5511888888888@s.whatsapp.net"]}`)
	if code != http.StatusOK || !resp.Success || len(resp.Participants) != 2 {
		t.Fatalf("add: %d %+v", code, resp)
	}
	if calls.action != whatsmeow.ParticipantChangeAdd || calls.participants[0].User != "5511999999999" || calls.participants[0].Server != types.DefaultUserServer {
		t.Fatalf("calls = %+v", calls)
	}
	code, resp = groupPost(t, h, `{"group_jid":"`+mgGroup+`","action":"promote","participants":["5511999999999"]}`)
	if code != http.StatusOK || !resp.Participants[0].IsAdmin {
		t.Fatalf("promote: %d %+v", code, resp)
	}
}

func TestGroupParticipantsValidation(t *testing.T) {
	h := handleGroupParticipants(fakeGroupOps(&groupCalls{}, nil), chatPolicy{})
	for _, body := range []string{
		`{"group_jid":"5511999999999@s.whatsapp.net","action":"add","participants":["x"]}`, // not a group
		`{"group_jid":"` + mgGroup + `","action":"kick","participants":["5511999999999"]}`, // bad action
		`{"group_jid":"` + mgGroup + `","action":"add","participants":[]}`,                 // nobody
		`{"group_jid":"` + mgGroup + `","action":"add","participants":["not a jid@@"]}`,    // bad jid
		`not json`,
	} {
		if code, _ := groupPost(t, h, body); code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", body, code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/group/participants", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET → %d", rec.Code)
	}
}

func TestGroupParticipantsUnrestrictedParsingUnchanged(t *testing.T) {
	for _, entry := range []string{"", mgGroup + ",5511999999999"} {
		calls := &groupCalls{}
		code, _ := groupPost(t, handleGroupParticipants(fakeGroupOps(calls, nil), parseChatPolicy(entry)), `{"group_jid":"`+mgGroup+`","action":"add","participants":["5511999999999:1@s.whatsapp.net"]}`)
		if entry == "" && (code != http.StatusOK || len(calls.participants) != 1 || calls.participants[0].Device != 1) {
			t.Fatalf("unrestricted parsing changed: %d %+v", code, calls)
		}
		if entry != "" && (code != http.StatusBadRequest || len(calls.participants) != 0) {
			t.Fatalf("restricted malformed participant reached WhatsApp: %d %+v", code, calls)
		}
	}
}

func TestGroupEndpointsRespectPolicyAndBridgeErrors(t *testing.T) {
	policy := parseChatPolicy("5511999999999")
	h := handleGroupLeave(fakeGroupOps(&groupCalls{}, nil), policy)
	if code, _ := groupPost(t, h, `{"group_jid":"`+mgGroup+`"}`); code != http.StatusForbidden {
		t.Fatalf("policy: %d, want 403", code)
	}
	h = handleGroupInvite(fakeGroupOps(&groupCalls{}, errors.New("not connected")), chatPolicy{})
	if code, resp := groupPost(t, h, `{"group_jid":"`+mgGroup+`"}`); code != http.StatusBadGateway || !strings.Contains(resp.Message, "not connected") {
		t.Fatalf("bridge error: %d %+v", code, resp)
	}
}

func TestGroupSubjectInviteLeave(t *testing.T) {
	calls := &groupCalls{}
	ops := fakeGroupOps(calls, nil)
	code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}, nil), `{"group_jid":"`+mgGroup+`","name":" Família ","description":"regras"}`)
	if code != http.StatusOK || calls.name != "Família" || calls.desc != "regras" || !strings.Contains(resp.Message, "name and description") {
		t.Fatalf("subject: %d %+v calls=%+v", code, resp, calls)
	}
	if code, _ := groupPost(t, handleGroupSubject(ops, chatPolicy{}, nil), `{"group_jid":"`+mgGroup+`"}`); code != http.StatusBadRequest {
		t.Fatalf("subject without fields → %d", code)
	}
	code, resp = groupPost(t, handleGroupInvite(ops, chatPolicy{}), `{"group_jid":"`+mgGroup+`","reset":true}`)
	if code != http.StatusOK || resp.Link == "" || calls.reset == nil || !*calls.reset {
		t.Fatalf("invite: %d %+v", code, resp)
	}
	code, resp = groupPost(t, handleGroupLeave(ops, chatPolicy{}), `{"group_jid":"`+mgGroup+`"}`)
	if code != http.StatusOK || !calls.left || !resp.Success {
		t.Fatalf("leave: %d %+v", code, resp)
	}
}
