package main

// WHATSAPP_ALLOW_TOOLS / WHATSAPP_DENY_TOOLS on the bridge: the endpoints of
// tools that are not allowed answer 403, reads and allowed endpoints do not,
// read-only still wins, and an unknown name stops the process.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// toolPolicyRequest issues an authenticated request against a mux built from a
// bridge carrying the given policies.
func toolPolicyRequest(t *testing.T, readOnly bool, p toolPolicy, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	b := testBridge(nil, newTestMessageStore(t), testLogger())
	b.ReadOnly = readOnlyPolicy{enabled: readOnly}
	b.Tools = p
	b.MediaRoots = []string{t.TempDir()}
	mux := b.newRESTMux(8080, readOnlyTestToken)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func mustToolPolicy(t *testing.T, allowRaw, denyRaw string) toolPolicy {
	t.Helper()
	p, err := newToolPolicy(allowRaw, denyRaw)
	if err != nil {
		t.Fatalf("newToolPolicy(%q, %q): %v", allowRaw, denyRaw, err)
	}
	return p
}

// bodyFor returns the valid request body used for a mutating endpoint; the
// refusal must happen before parsing, connecting or touching the store.
func bodyFor(t *testing.T, path string) string {
	t.Helper()
	for _, tc := range readOnlyMutatingEndpoints {
		if tc.path == path {
			return tc.body
		}
	}
	t.Fatalf("no request body known for %s", path)
	return ""
}

func TestParseToolList(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"  ,  ", nil},
		{"send_message", []string{"send_message"}},
		{" send_message , list_chats ,", []string{"list_chats", "send_message"}},
	} {
		got := sortedNames(parseToolList(tc.raw))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("parseToolList(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// A typo must stop the bridge with the valid names, exactly like the MCP side.
func TestNewToolPolicyRejectsUnknownNames(t *testing.T) {
	for _, tc := range []struct {
		name, allow, deny, wantEnv, wantName string
	}{
		{"allow typo", "send_reaction,send_reactoin", "", allowToolsEnv, "send_reactoin"},
		{"deny typo", "", "delete_mesage", denyToolsEnv, "delete_mesage"},
		{"endpoint path instead of a tool", "/api/send", "", allowToolsEnv, "/api/send"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newToolPolicy(tc.allow, tc.deny)
			if err == nil {
				t.Fatalf("newToolPolicy(%q, %q) = nil error", tc.allow, tc.deny)
			}
			msg := err.Error()
			for _, want := range []string{tc.wantEnv, tc.wantName, "Valid names", "send_message", "list_chats"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

// Every name the MCP server may be given is accepted here, reads included: the
// same value is meant to be passed to both containers.
func TestNewToolPolicyAcceptsReadToolNames(t *testing.T) {
	allow := "list_chats,list_messages,search_contacts,get_message_context,send_reaction,mark_messages_read"
	p := mustToolPolicy(t, allow, "delete_message,leave_group")
	if !p.restricted() {
		t.Fatal("policy with both lists set reports unrestricted")
	}
	if !p.allowsTool("send_reaction") || p.allowsTool("send_message") || p.allowsTool("delete_message") {
		t.Errorf("allow/deny not applied: %v / %v", p.allow, p.deny)
	}
}

// Deny wins over allow, even when the same name is in both lists.
func TestDenyWinsOverAllow(t *testing.T) {
	p := mustToolPolicy(t, "send_reaction,send_message", "send_message")
	if !p.allowsTool("send_reaction") {
		t.Error("send_reaction should be allowed")
	}
	if p.allowsTool("send_message") {
		t.Error("send_message is in both lists; deny must win")
	}
}

// The allow/deny decision reaches the REST layer as a 403 in the shared JSON
// shape, and lets the allowed endpoint through.
func TestEndpointPolicyOverREST(t *testing.T) {
	// "read-only except reactions": every mutating endpoint refused but /api/react.
	allowReactions := mustToolPolicy(t, "list_messages,list_chats,send_reaction", "")
	denyDestructive := mustToolPolicy(t, "", "delete_message,leave_group,send_message,send_file,send_audio_message")

	for _, tc := range []struct {
		name    string
		policy  toolPolicy
		path    string
		wantFwd bool // true = must reach the handler (no policy 403)
	}{
		{"allow-list names the tool", allowReactions, "/api/react", true},
		{"allow-list omits the tool", allowReactions, "/api/send", false},
		{"allow-list omits group leave", allowReactions, "/api/group/leave", false},
		{"allow-list omits history", allowReactions, "/api/history", false},
		{"deny-list names the tool", denyDestructive, "/api/delete", false},
		{"deny-list covers every tool of the endpoint", denyDestructive, "/api/send", false},
		{"deny-list leaves the rest", denyDestructive, "/api/react", true},
		{"unrestricted", toolPolicy{}, "/api/send", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := toolPolicyRequest(t, false, tc.policy, tc.path, bodyFor(t, tc.path))
			refused := rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "is disabled")
			if tc.wantFwd {
				if refused {
					t.Fatalf("%s refused: %s", tc.path, rec.Body.String())
				}
				return
			}
			if !refused {
				t.Fatalf("%s: status = %d, want 403; body: %s", tc.path, rec.Code, rec.Body.String())
			}
			var body apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: body is not JSON: %v", tc.path, err)
			}
			if body.Success {
				t.Errorf("%s: success = true on a refusal", tc.path)
			}
			if body.Error.Code != "denied" {
				t.Errorf("%s: error.code = %q, want denied", tc.path, body.Error.Code)
			}
			if !strings.Contains(body.Message, tc.path) {
				t.Errorf("%s: message %q should name the endpoint", tc.path, body.Message)
			}
			if !strings.Contains(body.Message, allowToolsEnv) && !strings.Contains(body.Message, denyToolsEnv) {
				t.Errorf("%s: message %q should name the variable that blocked it", tc.path, body.Message)
			}
		})
	}
}

// One tool of a shared endpoint is enough to keep it open: the bridge is
// endpoint-granular, the MCP server is the finer filter.
func TestSharedEndpointStaysOpenWhileOneToolIsAllowed(t *testing.T) {
	p := mustToolPolicy(t, "", "send_message,send_file")
	rec := toolPolicyRequest(t, false, p, "/api/send", bodyFor(t, "/api/send"))
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "is disabled") {
		t.Fatalf("/api/send refused while send_audio_message is still allowed: %s", rec.Body.String())
	}
}

// Read-only is evaluated first and cannot be re-opened by an allow-list.
func TestReadOnlyWinsOverAllowList(t *testing.T) {
	p := mustToolPolicy(t, "send_message,send_file,send_audio_message,send_reaction", "")
	rec := toolPolicyRequest(t, true, p, "/api/send", bodyFor(t, "/api/send"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), readOnlyEnv) {
		t.Errorf("refusal %q should name %s", rec.Body.String(), readOnlyEnv)
	}
}

// Reads stay open under an allow-list that names none of them, exactly as they
// do in read-only mode.
func TestListsDoNotTouchReadEndpoints(t *testing.T) {
	p := mustToolPolicy(t, "send_reaction", "download_media,get_poll_results,list_group_members")
	for _, tc := range []struct{ path, body string }{
		{"/api/poll", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1"}`},
		{"/api/group/members", `{"group_jid":"120363000000000001@g.us"}`},
		{"/api/download", `{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"M1"}`},
	} {
		rec := toolPolicyRequest(t, false, p, tc.path, tc.body)
		if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "is disabled") {
			t.Errorf("%s refused, but reads are not covered by the lists: %s", tc.path, rec.Body.String())
		}
	}
}

// A mutating endpoint missing from endpointTools fails closed while a list is
// set: an unclassified side effect must not slip past.
func TestUnmappedPathFailsClosedOnlyWhenRestricted(t *testing.T) {
	p := mustToolPolicy(t, "send_reaction", "")
	if msg := p.refusal("/api/new-side-effect"); msg == "" {
		t.Error("unmapped path allowed while a list is set")
	} else if !strings.Contains(msg, allowToolsEnv) {
		t.Errorf("refusal %q should name %s", msg, allowToolsEnv)
	}
	if msg := (toolPolicy{}).refusal("/api/new-side-effect"); msg != "" {
		t.Errorf("unrestricted policy refused: %q", msg)
	}
}

// endpointTools is the translation table; if a mutating endpoint is added to
// newRESTMux without a row, the lists cannot classify it (and it fails closed).
func TestEndpointToolsCoversEveryMutatingEndpoint(t *testing.T) {
	for _, tc := range readOnlyMutatingEndpoints {
		if _, ok := endpointTools[tc.path]; !ok {
			t.Errorf("%s is mutating but has no endpointTools row", tc.path)
		}
	}
	if len(endpointTools) != len(readOnlyMutatingEndpoints) {
		t.Errorf("endpointTools has %d rows, read-only covers %d endpoints",
			len(endpointTools), len(readOnlyMutatingEndpoints))
	}
}

func TestToolPolicySummary(t *testing.T) {
	if s := (toolPolicy{}).Summary(); !strings.Contains(s, allowToolsEnv) || !strings.Contains(s, "unset") {
		t.Errorf("unrestricted summary = %q", s)
	}
	s := mustToolPolicy(t, "list_chats,send_reaction", "delete_message").Summary()
	for _, want := range []string{allowToolsEnv, denyToolsEnv, "send_reaction", "delete_message", "403", "/api/send"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q does not mention %q", s, want)
		}
	}
	if strings.Contains(s, "/api/react") {
		t.Errorf("summary %q lists /api/react, which the allow-list keeps open", s)
	}
}
