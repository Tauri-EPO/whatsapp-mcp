package main

// GET/POST /api/group/members — list the participants of a group.
//
// whatsmeow's GetGroupInfo returns each participant with both address forms
// (phone JID and LID) plus admin flags; this endpoint flattens that into a
// JSON list the MCP server can hand to an agent, resolving names from the
// contact store the same way message senders are resolved.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type GroupMember struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	LID          string `json:"lid,omitempty"`
	Name         string `json:"name,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

type GroupMembersResponse struct {
	Success     bool          `json:"success"`
	Message     string        `json:"message,omitempty"`
	GroupJID    string        `json:"group_jid,omitempty"`
	Name        string        `json:"name,omitempty"`
	Topic       string        `json:"topic,omitempty"`
	OwnerJID    string        `json:"owner_jid,omitempty"`
	Participant int           `json:"participant_count"`
	Members     []GroupMember `json:"members"`
}

// groupInfoFetcher abstracts the whatsmeow call so the handler can be tested
// without a live connection.
type groupInfoFetcher func(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

// contactNameResolver returns a display name for a phone JID, or "".
type contactNameResolver func(jid types.JID) string

// buildGroupMembers converts whatsmeow's GroupInfo into the API shape.
func buildGroupMembers(info *types.GroupInfo, nameOf contactNameResolver) GroupMembersResponse {
	resp := GroupMembersResponse{
		Success:  true,
		GroupJID: info.JID.String(),
		Name:     info.Name,
		Topic:    info.Topic,
		Members:  make([]GroupMember, 0, len(info.Participants)),
	}
	if !info.OwnerJID.IsEmpty() {
		resp.OwnerJID = info.OwnerJID.ToNonAD().String()
	}
	for _, p := range info.Participants {
		m := GroupMember{
			JID:          p.JID.ToNonAD().String(),
			IsAdmin:      p.IsAdmin || p.IsSuperAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
			Name:         strings.TrimSpace(p.DisplayName),
		}
		phone := p.PhoneNumber
		if phone.IsEmpty() && p.JID.Server == types.DefaultUserServer {
			phone = p.JID
		}
		if !phone.IsEmpty() {
			m.PhoneNumber = phone.ToNonAD().User
			if m.Name == "" && nameOf != nil {
				m.Name = nameOf(phone.ToNonAD())
			}
		}
		lid := p.LID
		if lid.IsEmpty() && p.JID.Server == types.HiddenUserServer {
			lid = p.JID
		}
		if !lid.IsEmpty() {
			m.LID = lid.ToNonAD().String()
		}
		resp.Members = append(resp.Members, m)
	}
	resp.Participant = len(resp.Members)
	return resp
}

// storeContactName looks a phone JID up in whatsmeow's contact store.
func storeContactName(client *whatsmeow.Client) contactNameResolver {
	return func(jid types.JID) string {
		if client == nil || client.Store == nil || client.Store.Contacts == nil {
			return ""
		}
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err != nil || !contact.Found {
			return ""
		}
		for _, candidate := range []string{contact.FullName, contact.FirstName, contact.PushName, contact.BusinessName} {
			if name := strings.TrimSpace(candidate); name != "" {
				return name
			}
		}
		return ""
	}
}

// handleGroupMembers serves /api/group/members. The group JID comes from the
// `jid` query parameter (GET) or a JSON body {"group_jid": ...} (POST).
func handleGroupMembers(fetch groupInfoFetcher, nameOf contactNameResolver, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw := strings.TrimSpace(r.URL.Query().Get("jid"))
		if raw == "" && r.Method == http.MethodPost {
			var body struct {
				GroupJID string `json:"group_jid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw = strings.TrimSpace(body.GroupJID)
		}
		if raw == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(GroupMembersResponse{Success: false, Message: "group JID is required (?jid=...@g.us)"})
			return
		}
		jid, err := types.ParseJID(raw)
		if err != nil || jid.Server != types.GroupServer {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(GroupMembersResponse{Success: false, Message: "not a group JID: " + raw})
			return
		}
		if rejectByChatPolicy(w, policy, jid.String()) {
			return
		}
		info, err := fetch(r.Context(), jid)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(GroupMembersResponse{Success: false, Message: "failed to fetch group info: " + err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(buildGroupMembers(info, nameOf))
	}
}
