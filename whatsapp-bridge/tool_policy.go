package main

// Per-tool allow/deny, bridge side.
//
// WHATSAPP_ALLOW_TOOLS / WHATSAPP_DENY_TOOLS name MCP *tool* names, not
// endpoints, so one value can be written once and handed to both containers
// meaning the same thing (compose passes both variables to both services). The
// MCP server hides the blocked tools from tools/list (tool_policy.py); the
// bridge repeats the decision on the REST endpoints those tools call, so a
// direct REST caller, a bypass or a bug on the MCP side still cannot reach
// WhatsApp — the same belt-and-braces split WHATSAPP_READ_ONLY uses.
//
// The translation is endpointTools below: endpoint -> the tools that reach it.
// It is endpoint-granular, and three tools share /api/send, so an endpoint
// stays open while *any* tool that uses it is allowed; the MCP server is the
// finer filter. Tool names the bridge has nothing to enforce for (reads served
// from messages.db, the read endpoints that stay open in read-only mode, notes
// written locally) are still valid entries — unenforcedTools — because the
// list is written once for both sides.
//
// Order: WHATSAPP_READ_ONLY is evaluated first (read_only.go) and wins, then
// deny, then allow. Every filter only removes capability, so an allow-list can
// never re-open a read-only bridge. Unknown names stop the bridge at startup
// with the list of valid ones, exactly as they stop the MCP server: a typo in
// an allow-list must not silently widen it.

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const (
	allowToolsEnv = "WHATSAPP_ALLOW_TOOLS"
	denyToolsEnv  = "WHATSAPP_DENY_TOOLS"
)

// endpointTools maps every mutating REST endpoint to the MCP tools that call
// it (whatsapp-mcp-server/whatsapp.py). Keys must stay in step with the
// endpoints registered through `mutate` in newRESTMux — TestEndpointToolsCoversEveryMutatingEndpoint
// fails otherwise — and the tool names with the MCP server's, which
// tests/test_bridge_tool_policy.py checks from the Python side.
var endpointTools = map[string][]string{
	"/api/send":               {"send_message", "send_file", "send_audio_message"},
	"/api/react":              {"send_reaction"},
	"/api/typing":             {"send_typing"},
	"/api/mark-read":          {"mark_messages_read"},
	"/api/delete":             {"delete_message"},
	"/api/edit":               {"edit_message"},
	"/api/forward":            {"forward_message"},
	"/api/group/participants": {"manage_group_participants"},
	"/api/group/subject":      {"update_group"},
	"/api/group/invite":       {"get_group_invite_link"},
	"/api/group/leave":        {"leave_group"},
	"/api/media/purge":        {"purge_media"},
	"/api/history":            {"request_history"},
}

// unenforcedTools are the remaining MCP tool names: reads (including the ones
// served by /api/group/members, /api/poll and /api/download, which stay open
// here just as they do in read-only mode) and writes that never leave the host.
// Accepted in both lists, enforced by the MCP server only.
var unenforcedTools = []string{
	"annotate",
	"annotate_media",
	"bridge_status",
	"compact",
	"coverage",
	"download_media",
	"export_messages",
	"get_chat",
	"get_contact",
	"get_contact_chats",
	"get_direct_chat_by_contact",
	"get_last_interaction",
	"get_media_notes",
	"get_media_stats",
	"get_message_context",
	"get_notes",
	"get_poll_results",
	"list_chats",
	"list_group_members",
	"list_media",
	"list_messages",
	"list_unanswered",
	"list_unread",
	"message_stats",
	"search_contacts",
	"search_media_notes",
	"search_notes",
	"transcribe_audio",
}

// toolPolicy is the parsed pair of lists. The zero value allows everything.
type toolPolicy struct {
	allow map[string]bool
	deny  map[string]bool
}

// knownTools is every name the two lists accept.
func knownTools() map[string]bool {
	known := make(map[string]bool, len(unenforcedTools)+len(endpointTools))
	for _, name := range unenforcedTools {
		known[name] = true
	}
	for _, tools := range endpointTools {
		for _, name := range tools {
			known[name] = true
		}
	}
	return known
}

func parseToolList(raw string) map[string]bool {
	names := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			names[item] = true
		}
	}
	return names
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// newToolPolicy parses both lists and rejects names that are not tools.
func newToolPolicy(allowRaw, denyRaw string) (toolPolicy, error) {
	p := toolPolicy{allow: parseToolList(allowRaw), deny: parseToolList(denyRaw)}
	known := knownTools()
	for _, list := range []struct {
		env   string
		names map[string]bool
	}{{allowToolsEnv, p.allow}, {denyToolsEnv, p.deny}} {
		var unknown []string
		for _, name := range sortedNames(list.names) {
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			return toolPolicy{}, fmt.Errorf("%s lists unknown tool(s): %s. Valid names: %s",
				list.env, strings.Join(unknown, ", "), strings.Join(sortedNames(known), ", "))
		}
	}
	return p, nil
}

func loadToolPolicy() (toolPolicy, error) {
	return newToolPolicy(os.Getenv(allowToolsEnv), os.Getenv(denyToolsEnv))
}

// restricted reports whether either list is set (unset = allow everything).
func (p toolPolicy) restricted() bool {
	return len(p.allow) > 0 || len(p.deny) > 0
}

// allowsTool applies deny-then-allow to one tool name.
func (p toolPolicy) allowsTool(name string) bool {
	if p.deny[name] {
		return false
	}
	return len(p.allow) == 0 || p.allow[name]
}

// refusal returns the 403 message for a path, or "" when the path may proceed.
func (p toolPolicy) refusal(path string) string {
	if !p.restricted() {
		return ""
	}
	tools, mapped := endpointTools[path]
	if !mapped {
		// A mutating endpoint that nobody added to endpointTools. Fail closed:
		// an unclassified side effect must not slip past a list that is set.
		return path + " is disabled: it maps to no MCP tool name, so " + allowToolsEnv +
			" / " + denyToolsEnv + " cannot classify it"
	}
	reasons := make([]string, 0, len(tools))
	for _, name := range tools {
		if p.allowsTool(name) {
			return ""
		}
		env := allowToolsEnv + " is set and does not list it"
		if p.deny[name] {
			env = denyToolsEnv + " lists it"
		}
		reasons = append(reasons, name+" ("+env+")")
	}
	return path + " is disabled: every tool that uses it is blocked: " + strings.Join(reasons, "; ")
}

// guard wraps a mutating handler: 403 with the shared JSON error shape when no
// tool that reaches this endpoint is allowed, pass-through otherwise. Applied
// inside readOnlyPolicy.guard, so read-only answers first when both apply.
func (p toolPolicy) guard(h http.HandlerFunc) http.HandlerFunc {
	if !p.restricted() {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if msg := p.refusal(r.URL.Path); msg != "" {
			writeError(w, http.StatusForbidden, msg)
			return
		}
		h(w, r)
	}
}

// Summary is a one-line description for the startup log.
func (p toolPolicy) Summary() string {
	if !p.restricted() {
		return allowToolsEnv + " / " + denyToolsEnv + " unset: every endpoint allowed by the other layers stays open"
	}
	parts := []string{}
	if len(p.allow) > 0 {
		parts = append(parts, allowToolsEnv+": "+strings.Join(sortedNames(p.allow), ", "))
	}
	if len(p.deny) > 0 {
		parts = append(parts, denyToolsEnv+": "+strings.Join(sortedNames(p.deny), ", "))
	}
	var blocked []string
	for path := range endpointTools {
		if p.refusal(path) != "" {
			blocked = append(blocked, path)
		}
	}
	sort.Strings(blocked)
	if len(blocked) == 0 {
		return "Endpoint policy — " + strings.Join(parts, "; ") + "; every mutating endpoint still enabled"
	}
	return fmt.Sprintf("Endpoint policy — %s; %d endpoint(s) answer 403 (%s)",
		strings.Join(parts, "; "), len(blocked), strings.Join(blocked, ", "))
}
