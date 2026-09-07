package main

// Operation-level access control, bridge side.
//
// WHATSAPP_ALLOWED_CHATS (chat_policy.go) restricts *which chats* the REST API
// may act on; WHATSAPP_READ_ONLY restricts *what it may do*. When set, every
// endpoint with an external side effect answers 403 with the shared JSON error
// shape: send, react, typing, mark-read, delete, edit, forward, the four group
// endpoints, media purge and the on-demand history request.
//
// The MCP server enforces the same variable on its tools (tool_policy.py) and
// hides them from tools/list, so a model never sees them. This second check is
// what still holds when the MCP server is bypassed, misconfigured or buggy —
// the same belt-and-braces split the chat allow-list uses.
//
// Reads stay open, including /api/download (it only fills the local media
// cache) and /api/group/members, /api/poll, /api/health, /api/ready.

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

const readOnlyEnv = "WHATSAPP_READ_ONLY"

var (
	readOnlyTrue  = []string{"1", "true", "yes", "on"}
	readOnlyFalse = []string{"0", "false", "no", "off"}
)

// readOnlyPolicy is the parsed switch. The zero value allows everything.
type readOnlyPolicy struct {
	enabled bool
}

// parseReadOnly is strict on purpose: a security switch must not fall back to
// "off" because it was spelled WHATSAPP_READ_ONLY=treu. main() refuses to start
// on the error rather than running wide open.
func parseReadOnly(raw string) (readOnlyPolicy, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return readOnlyPolicy{}, nil
	}
	for _, t := range readOnlyTrue {
		if v == t {
			return readOnlyPolicy{enabled: true}, nil
		}
	}
	for _, f := range readOnlyFalse {
		if v == f {
			return readOnlyPolicy{}, nil
		}
	}
	return readOnlyPolicy{}, fmt.Errorf("%s=%q is not a boolean; use one of %s", readOnlyEnv,
		raw, strings.Join(append(append([]string{}, readOnlyTrue...), readOnlyFalse...), ", "))
}

func loadReadOnlyPolicy() (readOnlyPolicy, error) {
	return parseReadOnly(os.Getenv(readOnlyEnv))
}

// Summary is a one-line description for the startup log.
func (p readOnlyPolicy) Summary() string {
	if !p.enabled {
		return readOnlyEnv + " unset: outbound endpoints enabled"
	}
	return readOnlyEnv + "=1: read-only, every mutating endpoint answers 403 " +
		"(send, react, typing, mark-read, delete, edit, forward, group/*, media/purge, history)"
}

// guard wraps a handler with a side effect: 403 while read-only, pass-through
// otherwise. Endpoints opt in explicitly in newRESTMux, so a new route is
// readable by default and has to be classified when it is added.
func (p readOnlyPolicy) guard(h http.HandlerFunc) http.HandlerFunc {
	if !p.enabled {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, r.URL.Path+" is disabled: "+readOnlyEnv+
			" is set, so this bridge may read WhatsApp but not act on it")
	}
}
