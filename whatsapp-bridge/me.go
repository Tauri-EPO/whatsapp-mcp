package main

// GET /api/me — which account this bridge is logged in as.
//
// An agent triaging group traffic has to be able to recognise itself: WhatsApp
// renders a mention as the mentioned account's LID ("@158883943301358"), which
// reads like a phone number and appears nowhere else in the archive, so
// "messages that mention me" was only answerable by pasting the owner's LID
// into a full-text search (issue #290).
//
// Deliberately not part of /api/health: that route is polled every few seconds
// by container tooling and its body ends up in logs and dashboards, while the
// owner's phone number is personal data. /api/me sits behind the same bearer
// token and Host allow-list as every other /api/* route (auth.go) and has no
// side effects, so it stays open in read-only mode like the other reads.

import (
	"context"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// MeResponse is the /api/me body. The LID fields are null on a session that
// has no LID (an old pairing, or one whatsmeow has not learned yet).
type MeResponse struct {
	PhoneJID string  `json:"phone_jid"`
	Phone    string  `json:"phone"`
	LIDJID   *string `json:"lid_jid"`
	LID      *string `json:"lid"`
}

// ownIdentity returns the account's own phone JID and LID. An empty phone JID
// means the bridge is not paired yet.
type ownIdentity func(ctx context.Context) (phone types.JID, lid types.JID)

// handleMe serves GET /api/me.
func handleMe(identity ownIdentity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		phone, lid := identity(r.Context())
		if phone.IsEmpty() {
			writeError(w, http.StatusServiceUnavailable, "WhatsApp account is not paired yet")
			return
		}
		resp := MeResponse{PhoneJID: phone.String(), Phone: phone.User}
		if !lid.IsEmpty() {
			jid, user := lid.String(), lid.User
			resp.LIDJID, resp.LID = &jid, &user
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// clientIdentity reads the identity off the whatsmeow store. Store.LID is set
// at pair time on current sessions; older ones only have the mapping, so fall
// back to the LID store rather than reporting no LID at all.
func clientIdentity(client *whatsmeow.Client) ownIdentity {
	return func(ctx context.Context) (types.JID, types.JID) {
		if client == nil || client.Store == nil || client.Store.ID == nil {
			return types.EmptyJID, types.EmptyJID
		}
		phone := client.Store.ID.ToNonAD()
		lid := client.Store.LID.ToNonAD()
		if lid.IsEmpty() && client.Store.LIDs != nil {
			if resolved, err := client.Store.LIDs.GetLIDForPN(ctx, phone); err == nil {
				lid = resolved.ToNonAD()
			}
		}
		return phone, lid
	}
}
