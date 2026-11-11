package main

// POST /api/block {"jid": "...", "blocked": true|false} and GET /api/blocklist.
//
// Blocking is account-level, not a message: whatsmeow's UpdateBlocklist sends
// one blocklist IQ addressed by LID (it resolves a phone JID itself, asking the
// server when the LID map has no entry). Only user JIDs can be blocked; groups,
// broadcasts and newsletters are refused before anything is sent.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const blockDeadline = 20 * time.Second

type blocklistFunc func(context.Context) ([]types.BlocklistItem, error)
type updateBlocklistFunc func(context.Context, types.JID, events.BlocklistChangeAction) ([]types.BlocklistItem, error)

type blockDeps struct {
	policy    chatPolicyResolver
	connected func() bool
	update    updateBlocklistFunc
	list      blocklistFunc
}

type blocklistEntry struct {
	LID   string `json:"lid,omitempty"`
	Phone string `json:"phone_jid,omitempty"`
}

// blocklistEntries keeps the entries a restricted deployment may see: either
// address of the contact must be in the allow-list, like every other read.
func blocklistEntries(items []types.BlocklistItem, policy chatPolicy) []blocklistEntry {
	out := make([]blocklistEntry, 0, len(items))
	for _, item := range items {
		entry := blocklistEntry{}
		if !item.LID.IsEmpty() {
			entry.LID = item.LID.ToNonAD().String()
		}
		if !item.PN.IsEmpty() {
			entry.Phone = item.PN.ToNonAD().String()
		}
		if entry == (blocklistEntry{}) || (!policy.Allows(entry.LID) && !policy.Allows(entry.Phone)) {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LID+out[i].Phone < out[j].LID+out[j].Phone })
	return out
}

func handleBlock(deps blockDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policy := deps.policy.effective(r.Context())
		var req struct {
			JID     string `json:"jid"`
			Blocked *bool  `json:"blocked"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if req.Blocked == nil || strings.TrimSpace(req.JID) == "" {
			writeError(w, http.StatusBadRequest, "jid and blocked boolean are required")
			return
		}
		jid, ok := authorizeChat(w, policy, req.JID, true)
		if !ok {
			return
		}
		if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
			writeError(w, http.StatusBadRequest, "Only contacts (@s.whatsapp.net or @lid) can be blocked")
			return
		}
		if !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, notConnectedMessage)
			return
		}
		ctx, cancel := requestContext(r, blockDeadline)
		defer cancel()
		action := events.BlocklistChangeActionUnblock
		if *req.Blocked {
			action = events.BlocklistChangeActionBlock
		}
		list, err := deps.update(ctx, jid, action)
		if err != nil {
			// The IQ either never left (LID lookup failed) or got no answer;
			// the server applies a block idempotently, so a retry is harmless.
			writeErrorCode(w, http.StatusBadGateway, "bridge_unavailable", "Blocklist update failed; safe to retry: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "jid": jid.String(), "blocked": *req.Blocked, "blocklist": blocklistEntries(list, policy),
		})
	}
}

func handleBlocklist(deps blockDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policy := deps.policy.effective(r.Context())
		if !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, notConnectedMessage)
			return
		}
		ctx, cancel := requestContext(r, blockDeadline)
		defer cancel()
		list, err := deps.list(ctx)
		if err != nil {
			writeErrorCode(w, http.StatusBadGateway, "bridge_unavailable", "Cannot read the blocklist: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "blocklist": blocklistEntries(list, policy)})
	}
}
