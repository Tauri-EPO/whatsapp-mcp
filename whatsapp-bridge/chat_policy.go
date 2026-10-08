package main

// Conversation allow-list, bridge side.
//
// WHATSAPP_ALLOWED_CHATS (comma-separated JIDs, bare phone numbers, or
// "*@g.us" / "*@s.whatsapp.net" wildcards) restricts which chats the REST API
// will act on. The MCP server enforces the same variable on its tools.
// Every chat-taking endpoint goes through authorizeChat before effects,
// including history and download. Unset means unrestricted, but malformed
// targets are still refused before parsing can discard part of their identity.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const chatPolicyEnv = "WHATSAPP_ALLOWED_CHATS"

// chatPolicy is the parsed allow-list. Restricted=false means "allow all".
type chatPolicy struct {
	restricted       bool
	exact            map[string]struct{}
	servers          map[string]struct{}
	invalidPositions []int
}

// normalizeChatEntry canonicalises an allow-list entry or a request target:
// bare number -> phone JID, device suffix dropped, server lower-cased.
func normalizeChatEntry(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Retain an invalid configuration entry so it cannot disappear into an
	// unrestricted policy, or be rewritten into a valid exact entry.
	if strings.Count(raw, "@") > 1 {
		return raw
	}
	if !strings.Contains(raw, "@") {
		return raw + "@" + types.DefaultUserServer
	}
	at := strings.LastIndex(raw, "@")
	user, server := raw[:at], strings.ToLower(raw[at+1:])
	if colon := strings.Index(user, ":"); colon >= 0 {
		user = user[:colon]
	}
	return user + "@" + server
}

func parseChatPolicy(raw string) chatPolicy {
	p := chatPolicy{exact: map[string]struct{}{}, servers: map[string]struct{}{}}
	for index, item := range strings.Split(raw, ",") {
		n := normalizeChatEntry(item)
		if n == "" {
			continue
		}
		if strings.Count(n, "@") > 1 {
			p.invalidPositions = append(p.invalidPositions, index+1)
		}
		at := strings.LastIndex(n, "@")
		if n[:at] == "*" {
			p.servers[n[at+1:]] = struct{}{}
		} else {
			p.exact[n] = struct{}{}
		}
	}
	p.restricted = len(p.exact) > 0 || len(p.servers) > 0
	return p
}

func loadChatPolicy() chatPolicy {
	return parseChatPolicy(os.Getenv(chatPolicyEnv))
}

// Allows reports whether the policy permits acting on target (a JID or a bare
// phone number as accepted by the REST API).
func (p chatPolicy) Allows(target string) bool {
	// whatsmeow parses the first two @-separated parts. A policy must never
	// authorize a different server from the one the client will address.
	if strings.Count(target, "@") > 1 {
		return false
	}
	if !p.restricted {
		return true
	}
	n := normalizeChatEntry(target)
	if n == "" {
		return false
	}
	if _, ok := p.exact[n]; ok {
		return true
	}
	_, ok := p.servers[n[strings.LastIndex(n, "@")+1:]]
	return ok
}

// authorizeChat is the one HTTP authorization boundary. Recipient endpoints
// permit bare digit-only phones; other endpoints require a full JID. Never
// parse first: ParseJID discards extra @ parts, and String hides an empty user.
func authorizeChat(w http.ResponseWriter, policy chatPolicy, raw string, allowPhone bool) (types.JID, bool) {
	if strings.Count(raw, "@") > 1 {
		writeError(w, http.StatusBadRequest, "malformed chat target: more than one '@'")
		return types.EmptyJID, false
	}
	if allowPhone && !strings.Contains(raw, "@") && !isPhoneDigits(raw) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a phone number: use the country code and ASCII digits, or a digits-only @s.whatsapp.net JID", raw))
		return types.EmptyJID, false
	}
	var jid types.JID
	var err error
	if allowPhone && !strings.Contains(raw, "@") && isPhoneDigits(raw) {
		jid, err = parseRecipientJID(raw)
	} else {
		jid, err = types.ParseJID(raw)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid chat JID: "+err.Error())
		return types.EmptyJID, false
	}
	if jid.User == "" || jid.Server == "" {
		writeError(w, http.StatusBadRequest, "malformed chat target: a user and server are required")
		return types.EmptyJID, false
	}
	if strings.ContainsAny(jid.User, ":.") {
		writeError(w, http.StatusBadRequest, "Invalid chat JID: malformed user")
		return types.EmptyJID, false
	}
	if rejectByChatPolicy(w, policy, jid.String()) {
		return types.EmptyJID, false
	}
	return jid, true
}

func (p chatPolicy) warnInvalidEntries(logger waLog.Logger) {
	if len(p.invalidPositions) > 0 {
		logger.Warnf("%s: malformed entries at positions %v are retained as refused literals", chatPolicyEnv, p.invalidPositions)
	}
}

// Summary is a one-line description for the startup log.
func (p chatPolicy) Summary() string {
	if !p.restricted {
		return chatPolicyEnv + " unset: all chats allowed"
	}
	n := len(p.exact)
	invalid := len(p.invalidPositions)
	for entry := range p.exact {
		if strings.Count(entry, "@") > 1 {
			n--
		}
	}
	parts := []string{}
	for s := range p.servers {
		parts = append(parts, "*@"+s)
	}
	desc := strings.Join(parts, ", ")
	if desc != "" {
		desc = " + " + desc
	}
	if invalid > 0 {
		desc += " + " + strconv.Itoa(invalid) + " invalid entry(s) refused"
	}
	return chatPolicyEnv + ": restricted to " + strconv.Itoa(n) + " chat(s)" + desc
}

// rejectByChatPolicy writes the 403 the outbound handlers share when target is
// outside the allow-list. Returns true when the request was rejected.
func rejectByChatPolicy(w http.ResponseWriter, policy chatPolicy, target string) bool {
	if policy.Allows(target) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": "chat " + target + " is not in " + chatPolicyEnv + " (this bridge is restricted to an allow-list of conversations)",
	})
	return true
}
