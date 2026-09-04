package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
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
		setName:        func(_ context.Context, _ types.JID, n string) error { calls.name = n; return fail },
		setDescription: func(_ context.Context, _ types.JID, d string) error { calls.desc = d; return fail },
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
	code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}), `{"group_jid":"`+mgGroup+`","name":" Família ","description":"regras"}`)
	if code != http.StatusOK || calls.name != "Família" || calls.desc != "regras" || !strings.Contains(resp.Message, "name and description") {
		t.Fatalf("subject: %d %+v calls=%+v", code, resp, calls)
	}
	if code, _ := groupPost(t, handleGroupSubject(ops, chatPolicy{}), `{"group_jid":"`+mgGroup+`"}`); code != http.StatusBadRequest {
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
