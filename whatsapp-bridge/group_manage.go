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
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// groupOps is the slice of the whatsmeow client the handlers need.
type groupOps struct {
	allowSend          func(http.ResponseWriter, *http.Request, ...string) bool
	twin               lidForPNFunc
	updateParticipants func(ctx context.Context, jid types.JID, participants []types.JID, action whatsmeow.ParticipantChange) ([]types.GroupParticipant, error)
	setName            func(ctx context.Context, jid types.JID, name string) error
	getInfo            groupInfoFetcher
	setTopic           func(ctx context.Context, jid types.JID, topic groupTopicUpdate) error
	inviteLink         func(ctx context.Context, jid types.JID, reset bool) (string, error)
	leave              func(ctx context.Context, jid types.JID) error
}

// groupTopicUpdate names the adjacent SDK strings so the description cannot
// silently become the previous ID (which would clear the description).
type groupTopicUpdate struct {
	PreviousID  string
	Description string
}

// groupManagementClient keeps the live adapter testable at the SDK boundary.
// *whatsmeow.Client implements it; tests observe the actual argument placement.
type groupManagementClient interface {
	UpdateGroupParticipants(context.Context, types.JID, []types.JID, whatsmeow.ParticipantChange) ([]types.GroupParticipant, error)
	SetGroupName(context.Context, types.JID, string) error
	GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error)
	SetGroupTopic(context.Context, types.JID, string, string, string) error
	GetGroupInviteLink(context.Context, types.JID, bool) (string, error)
	LeaveGroup(context.Context, types.JID) error
}

// liveGroupOps binds groupOps to a whatsmeow client, refusing when offline
// (connected is Bridge.Connected, so tests can flip it).
func liveGroupOps(client groupManagementClient, connected func() bool) groupOps {
	// Preserve the nil-client guard when a typed SDK pointer enters the interface.
	if sdk, ok := client.(*whatsmeow.Client); ok && sdk == nil {
		client = nil
	}
	online := func() error {
		if client == nil || !connected() {
			return errors.New("WhatsApp client is not connected")
		}
		return nil
	}
	return groupOps{
		twin: func(ctx context.Context, jid types.JID) (types.JID, error) {
			sdk, _ := client.(*whatsmeow.Client)
			return lookupAltJID(ctx, sdk, jid)
		},
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
		getInfo: func(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
			if err := online(); err != nil {
				return nil, err
			}
			return client.GetGroupInfo(ctx, jid)
		},
		setTopic: func(ctx context.Context, jid types.JID, topic groupTopicUpdate) error {
			if err := online(); err != nil {
				return err
			}
			// Empty new ID asks the SDK to generate one. When PreviousID is
			// empty (no existing topic), the pinned SDK still refetches group
			// info; pass the real ID rather than inventing a protocol sentinel.
			return client.SetGroupTopic(ctx, jid, topic.PreviousID, "", topic.Description)
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
	Changed      []string      `json:"changed,omitempty"`
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
	if raw == "" {
		writeGroupError(w, http.StatusBadRequest, "group_jid is required")
		return groupRequest{}, types.EmptyJID, false
	}
	jid, ok := authorizeChat(w, policy, raw, false)
	if !ok {
		return groupRequest{}, types.EmptyJID, false
	}
	if jid.Server != types.GroupServer {
		writeGroupError(w, http.StatusBadRequest, "group_jid must be a group JID (…@g.us)")
		return groupRequest{}, types.EmptyJID, false
	}
	return req, jid, true
}

// parseParticipants accepts phone numbers or JIDs.
func parseParticipants(raw []string, policies ...chatPolicy) ([]types.JID, error) {
	out := make([]types.JID, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		normalized, err := normalizePhoneRecipient(item)
		if err != nil {
			if len(policies) == 0 || !policies[0].restricted {
				return nil, errors.New("invalid participant " + item + " (use a phone number or a user JID)")
			}
			return nil, err
		}
		jid, err := normalizedUserJID(normalized)
		if err == nil && len(policies) > 0 && policies[0].restricted {
			// Restricted operations validate the complete envelope before
			// checking identity; unrestricted calls retain their old parsing.
			jid, err = canonicalChatJID(normalized, true)
		}
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
		participants, err := parseParticipants(req.Participants, policy)
		if err != nil {
			writeGroupError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Adding/promoting expands access: authorize the whole batch first.
		// Removing/demoting reduces access and must work for outside or
		// unmapped LID members without granting their direct chats access.
		if action == whatsmeow.ParticipantChangeAdd || action == whatsmeow.ParticipantChangePromote {
			for _, participant := range participants {
				if !policy.allowsIdentity(r.Context(), participant, ops.twin) {
					writeGroupError(w, http.StatusForbidden, "participant "+participant.String()+" is not in "+chatPolicyEnv)
					return
				}
			}
		}
		if action == whatsmeow.ParticipantChangeAdd && ops.allowSend != nil {
			targets := make([]string, 0, len(participants))
			for _, participant := range participants {
				targets = append(targets, participant.String())
			}
			if !ops.allowSend(w, r, targets...) {
				return
			}
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

func handleGroupSubject(ops groupOps, policy chatPolicy, record rosterRecorder) http.HandlerFunc {
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
		failDescription := func(msg string) {
			if len(changed) > 0 {
				// _bridge_json preserves this message in the MCP error envelope.
				msg = "group was renamed; " + msg + "; retry only description"
			}
			writeJSON(w, http.StatusBadGateway, groupResponse{Success: false, Message: msg, Changed: changed})
		}
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
			// The sweep stamp must predate the fetch, preserving joins that
			// arrive while the roster snapshot is in flight.
			at := time.Now()
			info, err := ops.getInfo(r.Context(), jid)
			if err != nil || info == nil {
				msg := "could not read group info"
				if err != nil {
					msg += ": " + err.Error()
				}
				failDescription(msg)
				return
			}
			if record != nil {
				record(jid.String(), buildGroupMembers(info, nil, nil).Members, at)
			}
			topic := groupTopicUpdate{PreviousID: info.TopicID, Description: strings.TrimSpace(*req.Description)}
			if err := ops.setTopic(r.Context(), jid, topic); err != nil {
				failDescription("set description failed: " + err.Error())
				return
			}
			changed = append(changed, "description")
		}
		_ = json.NewEncoder(w).Encode(groupResponse{Success: true, GroupJID: jid.String(), Message: "updated " + strings.Join(changed, " and "), Changed: changed})
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
func registerGroupManagement(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, ops groupOps, policy chatPolicy, record rosterRecorder) {
	mux.HandleFunc("/api/group/participants", auth(handleGroupParticipants(ops, policy)))
	mux.HandleFunc("/api/group/subject", auth(handleGroupSubject(ops, policy, record)))
	mux.HandleFunc("/api/group/invite", auth(handleGroupInvite(ops, policy)))
	mux.HandleFunc("/api/group/leave", auth(handleGroupLeave(ops, policy)))
}
