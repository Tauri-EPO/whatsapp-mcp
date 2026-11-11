package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type groupCreationClient interface {
	CreateGroup(context.Context, whatsmeow.ReqCreateGroup) (*types.GroupInfo, error)
	LinkGroup(context.Context, types.JID, types.JID) error
	GetSubGroups(context.Context, types.JID) ([]*types.GroupLinkTarget, error)
}

type groupCreationDeps struct {
	client    groupCreationClient
	connected func() bool
	policy    chatPolicyResolver
	twin      lidForPNFunc
	allowSend func(http.ResponseWriter, *http.Request, ...string) bool
}

func handleCreateGroup(d groupCreationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name         string   `json:"name"`
			Participants []string `json:"participants"`
			Community    bool     `json:"community"`
			ParentJID    string   `json:"parent_jid"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON body")
			return
		}
		policy := d.policy.effective(r.Context())
		if _, wildcard := policy.servers[types.GroupServer]; policy.restricted && !wildcard {
			writeError(w, 403, "creating a group requires *@g.us in "+policy.origin())
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || utf8.RuneCountInString(name) > 25 || (req.Community && req.ParentJID != "") || len(req.Participants) > 256 {
			writeError(w, 400, "name must be 1-25 characters; community and parent_jid are exclusive; maximum 256 participants")
			return
		}
		parent := types.EmptyJID
		if req.ParentJID != "" {
			var ok bool
			parent, ok = authorizeChat(w, policy, req.ParentJID, false)
			if !ok {
				return
			}
			if parent.Server != types.GroupServer {
				writeError(w, 400, "parent_jid must be a group JID")
				return
			}
		}
		participants := []types.JID{}
		if len(req.Participants) > 0 {
			var err error
			// Strict parsing also on unrestricted deployments: never drop @ parts.
			participants, err = parseParticipants(req.Participants, chatPolicy{restricted: true})
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
		}
		seen := map[types.JID]bool{}
		unique := []types.JID{}
		targets := []string{}
		for _, participant := range participants {
			if !policy.allowsIdentity(r.Context(), participant, d.twin) {
				writeError(w, 403, "participant is not in "+policy.origin())
				return
			}
			if !seen[participant] {
				seen[participant] = true
				unique = append(unique, participant)
				targets = append(targets, participant.String())
			}
		}
		if !d.connected() {
			writeError(w, 503, "WhatsApp client is not connected")
			return
		}
		ctx, cancel := requestContext(r, sendDeadline)
		defer cancel()
		if len(targets) > 0 && d.allowSend != nil && !d.allowSend(w, r, targets...) {
			return
		}
		create := whatsmeow.ReqCreateGroup{Name: name, Participants: unique}
		create.IsParent = req.Community
		if req.Community {
			create.DefaultMembershipApprovalMode = "request_required"
		}
		info, err := d.client.CreateGroup(ctx, create)
		if err != nil || info == nil || info.JID.IsEmpty() {
			writeError(w, 502, "group creation failed; delivery may be uncertain, do not blindly retry")
			return
		}
		result := map[string]any{"success": true, "group_jid": info.JID.String(), "community": req.Community}
		// A failed second step must preserve the created JID to prevent duplicates.
		if !parent.IsEmpty() {
			if err = d.client.LinkGroup(ctx, parent, info.JID); err != nil {
				result["link_error"] = "group created but linking failed; do not recreate the group"
			} else {
				result["parent_jid"] = parent.String()
			}
		}
		writeJSON(w, 200, result)
	}
}

func handleSubGroups(d groupCreationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policy := d.policy.effective(r.Context())
		jid, ok := authorizeChat(w, policy, r.URL.Query().Get("jid"), false)
		if !ok {
			return
		}
		if jid.Server != types.GroupServer {
			writeError(w, 400, "jid must be a community group JID")
			return
		}
		if !d.connected() {
			writeError(w, 503, "WhatsApp client is not connected")
			return
		}
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		groups, err := d.client.GetSubGroups(ctx, jid)
		if err != nil {
			writeError(w, 502, "could not list community groups")
			return
		}
		items := []map[string]any{}
		for _, group := range groups {
			if group != nil && policy.Allows(group.JID.String()) {
				items = append(items, map[string]any{"jid": group.JID.String(), "name": group.Name, "is_default": group.IsDefaultSubGroup})
			}
		}
		writeJSON(w, 200, map[string]any{"success": true, "chat_jid": jid.String(), "groups": items})
	}
}
