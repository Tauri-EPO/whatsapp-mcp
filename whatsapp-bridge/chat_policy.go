package main

// Conversation allow-list, bridge side.
//
// WHATSAPP_ALLOWED_CHATS (comma-separated JIDs, bare phone numbers, or
// "*@g.us" / "*@s.whatsapp.net" wildcards) restricts which chats the REST API
// will act on. The MCP server enforces the same variable on its tools; the
// bridge repeats the check on every endpoint with a side effect (send, react,
// mark-read, typing) so a bug or a bypass on the MCP side still cannot reach
// a chat you did not enable. Unset means unrestricted.

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

const chatPolicyEnv = "WHATSAPP_ALLOWED_CHATS"

// chatPolicy is the parsed allow-list. Restricted=false means "allow all".
type chatPolicy struct {
	restricted bool
	exact      map[string]struct{}
	servers    map[string]struct{}
}

// normalizeChatEntry canonicalises an allow-list entry or a request target:
// bare number -> phone JID, device suffix dropped, server lower-cased.
func normalizeChatEntry(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
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
	for _, item := range strings.Split(raw, ",") {
		n := normalizeChatEntry(item)
		if n == "" {
			continue
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

// Summary is a one-line description for the startup log.
func (p chatPolicy) Summary() string {
	if !p.restricted {
		return chatPolicyEnv + " unset: all chats allowed"
	}
	n := len(p.exact)
	parts := []string{}
	for s := range p.servers {
		parts = append(parts, "*@"+s)
	}
	desc := strings.Join(parts, ", ")
	if desc != "" {
		desc = " + " + desc
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
