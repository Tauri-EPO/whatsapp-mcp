package main

// Group management endpoints (issue #120), all outbound and therefore behind
// the chat allow-list:
//
//   POST /api/group/participants {"group_jid", "action": add|remove|promote|demote, "participants": [...]}
//   POST /api/group/subject      {"group_jid", "name"?, "description"?}
//   POST /api/group/invite       {"group_jid", "reset": bool}   → {"link"}
//   POST /api/group/leave        {"group_jid"}
//
// The whatsmeow calls are injected as functions so the handlers are testable
// without a live connection (same pattern as group_members.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// groupOps is the slice of the whatsmeow client the handlers need.
type groupOps struct {
	updateParticipants func(ctx context.Context, jid types.JID, participants []types.JID, action whatsmeow.ParticipantChange) ([]types.GroupParticipant, error)
	setName            func(ctx context.Context, jid types.JID, name string) error
	setDescription     func(ctx context.Context, jid types.JID, description string) error
	inviteLink         func(ctx context.Context, jid types.JID, reset bool) (string, error)
	leave              func(ctx context.Context, jid types.JID) error
}

// liveGroupOps binds groupOps to a whatsmeow client, refusing when offline.
func liveGroupOps(client *whatsmeow.Client) groupOps {
	online := func() error {
		if client == nil || !client.IsConnected() {
			return errors.New("WhatsApp client is not connected")
		}
		return nil
	}
	return groupOps{
		updateParticipants: func(ctx context.Context, jid types.JID, p []types.JID, action whatsmeow.ParticipantChange) ([]types.GroupParticipant, error) {
			if err := online(); err != nil {
				return nil, err
			}
			return client.UpdateGroupParticipants(ctx, jid, p, action)
		},
		setName: func(ctx context.Context, jid types.JID, name string) error {
			if err := online(); err != nil {
				return err
			}
			return client.SetGroupName(ctx, jid, name)
		},
		setDescription: func(ctx context.Context, jid types.JID, description string) error {
			if err := online(); err != nil {
				return err
			}
			return client.SetGroupDescription(ctx, jid, description)
		},
		inviteLink: func(ctx context.Context, jid types.JID, reset bool) (string, error) {
			if err := online(); err != nil {
				return "", err
			}
			return client.GetGroupInviteLink(ctx, jid, reset)
		},
		leave: func(ctx context.Context, jid types.JID) error {
			if err := online(); err != nil {
				return err
			}
			return client.LeaveGroup(ctx, jid)
		},
	}
}

type groupRequest struct {
	GroupJID     string   `json:"group_jid"`
	Action       string   `json:"action,omitempty"`
	Participants []string `json:"participants,omitempty"`
	Name         *string  `json:"name,omitempty"`
	Description  *string  `json:"description,omitempty"`
	Reset        bool     `json:"reset,omitempty"`
}

type groupResponse struct {
	Success      bool          `json:"success"`
	Message      string        `json:"message,omitempty"`
	GroupJID     string        `json:"group_jid,omitempty"`
	Link         string        `json:"link,omitempty"`
	Participants []GroupMember `json:"participants,omitempty"`
}

func writeGroupError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(groupResponse{Success: false, Message: msg})
}

// parseGroupRequest decodes the body and validates the group JID against the policy.
func parseGroupRequest(w http.ResponseWriter, r *http.Request, policy chatPolicy) (groupRequest, types.JID, bool) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		writeGroupError(w, http.StatusMethodNotAllowed, "method not allowed")
		return groupRequest{}, types.EmptyJID, false
	}
	var req groupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGroupError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return groupRequest{}, types.EmptyJID, false
	}
	raw := strings.TrimSpace(req.GroupJID)
	jid, err := types.ParseJID(raw)
	if raw == "" || err != nil || jid.Server != types.GroupServer {
		writeGroupError(w, http.StatusBadRequest, "group_jid must be a group JID (…@g.us)")
		return groupRequest{}, types.EmptyJID, false
	}
	if rejectByChatPolicy(w, policy, jid.String()) {
		return groupRequest{}, types.EmptyJID, false
	}
	return req, jid, true
}

// parseParticipants accepts phone numbers or JIDs.
func parseParticipants(raw []string) ([]types.JID, error) {
	out := make([]types.JID, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "@") {
			item = strings.TrimPrefix(item, "+") + "@" + types.DefaultUserServer
		}
		jid, err := types.ParseJID(item)
		if err != nil || jid.User == "" || strings.ContainsAny(jid.User, " @") ||
			(jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
			return nil, errors.New("invalid participant " + item + " (use a phone number or a user JID)")
		}
		out = append(out, jid)
	}
	if len(out) == 0 {
		return nil, errors.New("participants must list at least one phone number or JID")
	}
	return out, nil
}

func handleGroupParticipants(ops groupOps, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, jid, ok := parseGroupRequest(w, r, policy)
		if !ok {
			return
		}
		action := whatsmeow.ParticipantChange(strings.ToLower(strings.TrimSpace(req.Action)))
		switch action {
		case whatsmeow.ParticipantChangeAdd, whatsmeow.ParticipantChangeRemove, whatsmeow.ParticipantChangePromote, whatsmeow.ParticipantChangeDemote:
		default:
			writeGroupError(w, http.StatusBadRequest, "action must be one of add, remove, promote, demote")
			return
		}
		participants, err := parseParticipants(req.Participants)
		if err != nil {
			writeGroupError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := ops.updateParticipants(r.Context(), jid, participants, action)
		if err != nil {
			writeGroupError(w, http.StatusBadGateway, "group update failed: "+err.Error())
			return
		}
		resp := groupResponse{Success: true, GroupJID: jid.String(), Message: string(action) + " applied"}
		for _, p := range result {
			m := GroupMember{JID: p.JID.ToNonAD().String(), IsAdmin: p.IsAdmin, IsSuperAdmin: p.IsSuperAdmin}
			if !p.PhoneNumber.IsEmpty() {
				m.PhoneNumber = p.PhoneNumber.ToNonAD().User
			}
			if !p.LID.IsEmpty() {
				m.LID = p.LID.ToNonAD().String()
			}
			resp.Participants = append(resp.Participants, m)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func handleGroupSubject(ops groupOps, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, jid, ok := parseGroupRequest(w, r, policy)
		if !ok {
			return
		}
		if req.Name == nil && req.Description == nil {
			writeGroupError(w, http.StatusBadRequest, "provide name and/or description")
			return
		}
		var changed []string
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" {
				writeGroupError(w, http.StatusBadRequest, "name must not be empty")
				return
			}
			if err := ops.setName(r.Context(), jid, name); err != nil {
				writeGroupError(w, http.StatusBadGateway, "set name failed: "+err.Error())
				return
			}
			changed = append(changed, "name")
		}
		if req.Description != nil {
			if err := ops.setDescription(r.Context(), jid, strings.TrimSpace(*req.Description)); err != nil {
				writeGroupError(w, http.StatusBadGateway, "set description failed: "+err.Error())
				return
			}
			changed = append(changed, "description")
		}
		_ = json.NewEncoder(w).Encode(groupResponse{Success: true, GroupJID: jid.String(), Message: "updated " + strings.Join(changed, " and ")})
	}
}

func handleGroupInvite(ops groupOps, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, jid, ok := parseGroupRequest(w, r, policy)
		if !ok {
			return
		}
		link, err := ops.inviteLink(r.Context(), jid, req.Reset)
		if err != nil {
			writeGroupError(w, http.StatusBadGateway, "invite link failed: "+err.Error())
			return
		}
		msg := "invite link"
		if req.Reset {
			msg = "invite link reset (the previous link no longer works)"
		}
		_ = json.NewEncoder(w).Encode(groupResponse{Success: true, GroupJID: jid.String(), Link: link, Message: msg})
	}
}

func handleGroupLeave(ops groupOps, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, jid, ok := parseGroupRequest(w, r, policy)
		if !ok {
			return
		}
		if err := ops.leave(r.Context(), jid); err != nil {
			writeGroupError(w, http.StatusBadGateway, "leave failed: "+err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(groupResponse{Success: true, GroupJID: jid.String(), Message: "left the group"})
	}
}

// registerGroupManagement wires the four endpoints.
func registerGroupManagement(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, ops groupOps, policy chatPolicy) {
	mux.HandleFunc("/api/group/participants", auth(handleGroupParticipants(ops, policy)))
	mux.HandleFunc("/api/group/subject", auth(handleGroupSubject(ops, policy)))
	mux.HandleFunc("/api/group/invite", auth(handleGroupInvite(ops, policy)))
	mux.HandleFunc("/api/group/leave", auth(handleGroupLeave(ops, policy)))
}
