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
	"time"

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

// GroupOwner is the group's creator addressed the way a participant is (issue
// #396): whatsmeow hands back a single JID, which for a modern group is a bare
// LID, and an agent then has no way of telling which member row owns the group
// without matching LIDs by hand.
type GroupOwner struct {
	JID         string `json:"jid"`
	PhoneNumber string `json:"phone_number,omitempty"`
	LID         string `json:"lid,omitempty"`
	Name        string `json:"name,omitempty"`
}

type GroupMembersResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message,omitempty"`
	GroupJID string `json:"group_jid,omitempty"`
	Name     string `json:"name,omitempty"`
	Topic    string `json:"topic,omitempty"`
	// OwnerJID is Owner.JID, kept for callers written before the block existed:
	// the phone JID when the map knows it, the LID otherwise.
	OwnerJID    string        `json:"owner_jid,omitempty"`
	Owner       *GroupOwner   `json:"owner,omitempty"`
	Participant int           `json:"participant_count"`
	Members     []GroupMember `json:"members"`
	// FetchedAt is when this roster came off the network (canonical UTC, see
	// store_time.go). It is also the freshness stamp written to group_members,
	// so a caller can tell how old the cached membership behind
	// get_contact_chats is.
	FetchedAt string `json:"fetched_at,omitempty"`
}

// groupInfoFetcher abstracts the whatsmeow call so the handler can be tested
// without a live connection.
type groupInfoFetcher func(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

// contactNameResolver returns a display name for a phone JID, or "".
type contactNameResolver func(jid types.JID) string

// altJIDResolver returns the other address form of a user JID (the phone JID of
// a LID, the LID of a phone JID), or an empty JID when the map knows neither.
type altJIDResolver func(jid types.JID) types.JID

// buildGroupMembers converts whatsmeow's GroupInfo into the API shape.
func buildGroupMembers(info *types.GroupInfo, nameOf contactNameResolver, altOf altJIDResolver) GroupMembersResponse {
	resp := GroupMembersResponse{
		Success:  true,
		GroupJID: info.JID.String(),
		Name:     info.Name,
		Topic:    info.Topic,
		Members:  make([]GroupMember, 0, len(info.Participants)),
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
	if owner := buildGroupOwner(info.OwnerJID, resp.Members, nameOf, altOf); owner != nil {
		resp.Owner, resp.OwnerJID = owner, owner.JID
	}
	return resp
}

// buildGroupOwner dual-addresses the group's creator. The roster we just built
// already carries both forms of every participant and the owner is normally one
// of them, so that is the first place to look; only an owner who has left the
// group (or a roster whatsmeow gave one form of) reaches the LID map.
func buildGroupOwner(owner types.JID, members []GroupMember, nameOf contactNameResolver, altOf altJIDResolver) *GroupOwner {
	owner = owner.ToNonAD()
	if owner.IsEmpty() {
		return nil
	}
	out := &GroupOwner{}
	switch owner.Server {
	case types.HiddenUserServer:
		out.LID = owner.String()
	case types.DefaultUserServer:
		out.PhoneNumber = owner.User
	default:
		// Neither address form: hand it back as it came rather than guess.
		out.JID = owner.String()
		return out
	}
	// Fill whatever the roster knows about the address forms we hold so far.
	fromRoster := func() {
		for _, m := range members {
			if (out.LID != "" && m.LID == out.LID) || (out.PhoneNumber != "" && m.PhoneNumber == out.PhoneNumber) {
				if out.LID == "" {
					out.LID = m.LID
				}
				if out.PhoneNumber == "" {
					out.PhoneNumber = m.PhoneNumber
				}
				if out.Name == "" {
					out.Name = m.Name
				}
				return
			}
		}
	}
	fromRoster()
	if (out.PhoneNumber == "" || out.LID == "") && altOf != nil {
		switch alt := altOf(owner).ToNonAD(); alt.Server {
		case types.DefaultUserServer:
			if out.PhoneNumber == "" {
				out.PhoneNumber = alt.User
			}
		case types.HiddenUserServer:
			if out.LID == "" {
				out.LID = alt.String()
			}
		}
		// Second pass: the roster may hold the owner under the form the map
		// just supplied (a participant whatsmeow gave only a phone JID for
		// cannot match a LID owner until the LID has been translated), and with
		// it the display name that would otherwise be lost.
		fromRoster()
	}
	if out.PhoneNumber != "" {
		phone := types.NewJID(out.PhoneNumber, types.DefaultUserServer)
		out.JID = phone.String()
		if out.Name == "" && nameOf != nil {
			out.Name = nameOf(phone)
		}
	} else {
		out.JID = out.LID
	}
	return out
}

// storeAltJID resolves a user JID to its other address form through whatsmeow's
// LID map (whatsapp.db), the same store resolveUserJID falls back to.
func storeAltJID(client *whatsmeow.Client) altJIDResolver {
	return func(jid types.JID) types.JID {
		if client == nil || client.Store == nil || client.Store.LIDs == nil {
			return types.EmptyJID
		}
		alt, err := client.Store.GetAltJID(context.Background(), jid)
		if err != nil {
			// Degraded, not empty: without this line a failing map read looks
			// exactly like "the map does not know this LID" (issue #396).
			bridgeLog.Warnf("LID map lookup for %s failed: %v", jid, err)
			return types.EmptyJID
		}
		return alt
	}
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

// rosterRecorder caches a roster we just fetched (group_members_store.go).
// nil = do not cache, which is what the tests that only check the JSON use.
type rosterRecorder func(groupJID string, members []GroupMember, now time.Time)

// handleGroupMembers serves /api/group/members. The group JID comes from the
// `jid` query parameter (GET) or a JSON body {"group_jid": ...} (POST).
//
// Every successful fetch also refreshes the cached roster: this endpoint is
// the one place a full participant list arrives, and paying for the write here
// is what lets get_contact_chats answer "which groups is this person in?"
// without a live round trip per group (issue #288). The write is a local cache
// with no WhatsApp side effect, so the route stays a read for read-only mode —
// like /api/download, which caches media the same way.
func handleGroupMembers(fetch groupInfoFetcher, nameOf contactNameResolver, altOf altJIDResolver, policy chatPolicy, record rosterRecorder) http.HandlerFunc {
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
		// Read before the request: this is the stamp the cached roster is swept
		// with, and a member who joins while the fetch is in flight must not be
		// deleted by a snapshot taken before they arrived.
		at := time.Now()
		info, err := fetch(r.Context(), jid)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(GroupMembersResponse{Success: false, Message: "failed to fetch group info: " + err.Error()})
			return
		}
		resp := buildGroupMembers(info, nameOf, altOf)
		resp.FetchedAt = dbTime(at)
		if record != nil {
			// ToNonAD: ParseJID accepts a device suffix, and a roster cached
			// under "…:1@g.us" would never match chats.jid again.
			record(jid.ToNonAD().String(), resp.Members, at)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
