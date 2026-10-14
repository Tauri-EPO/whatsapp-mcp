package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func operatorChatsServer(t *testing.T, b *Bridge) *httptest.Server {
	t.Helper()
	// Use the production wiring, so a missing route cannot pass unnoticed.
	p := &operatorPairing{b: b}
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, p.routes(), b.Log))
	t.Cleanup(server.Close)
	return server
}

func operatorChatsRequest(t *testing.T, server *httptest.Server, query string) (int, []byte, []operatorChat) {
	t.Helper()
	req, err := http.NewRequest("GET", server.URL+"/operator/v1/chats"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("invalid operator response", err)
	}
	var body struct {
		Chats []operatorChat `json:"chats"`
	}
	if response.StatusCode == 200 && json.Unmarshal(data, &body) != nil {
		t.Fatal("invalid chat JSON")
	}
	return response.StatusCode, data, body.Chats
}

func TestOperatorChatsIgnoresBothAllowListsAndReturnsOnlyIdentity(t *testing.T) {
	for _, runtime := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime=%v", runtime), func(t *testing.T) {
			b := newSettingsBridge(t) // real SQLite archive on disk
			b.Policy = parseChatPolicy("5511888888888@s.whatsapp.net")
			var err error
			b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
				if name == chatPolicyEnv {
					return "5511888888888@s.whatsapp.net"
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			if runtime {
				err := b.patchRuntimeSettings(context.Background(), map[string]json.RawMessage{"chats.allowed": json.RawMessage(`["5511888888888@s.whatsapp.net"]`)}, []string{"chats.allowed"})
				if err != nil {
					t.Fatal(err)
				}
			}
			const outside = "5511999999999@s.whatsapp.net"
			if b.runtimeChatPolicy(b.Policy).Allows(outside) {
				t.Fatal("fixture did not enforce the allow-list")
			}
			stamp := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			if err := b.Store.StoreChat(outside, "Alice Archive", stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,content,media_type,filename,url) VALUES('private',?,'PRIVATE-MESSAGE-TEXT','image','PRIVATE-MEDIA-PATH','PRIVATE-MEDIA-URL')`, outside); err != nil {
				t.Fatal(err)
			}
			audit := &recordingLogger{}
			b.Log = audit
			server := operatorChatsServer(t, b)
			status, data, chats := operatorChatsRequest(t, server, "?query=alice%20archive")
			if status != 200 || len(chats) != 1 || chats[0].JID != outside || chats[0].Name != "Alice Archive" || chats[0].IsGroup || chats[0].LastMessageTime == nil || !chats[0].LastMessageTime.Equal(stamp) {
				t.Fatalf("status=%d chats=%+v", status, chats)
			}
			var projection struct {
				Chats []map[string]any `json:"chats"`
			}
			if err := json.Unmarshal(data, &projection); err != nil {
				t.Fatal(err)
			}
			for _, row := range projection.Chats {
				if len(row) != 4 {
					t.Fatal("response includes fields beyond chat identity")
				}
				for _, key := range []string{"jid", "name", "is_group", "last_message_time"} {
					if _, ok := row[key]; !ok {
						t.Fatal("missing identity field", key)
					}
				}
			}
			if strings.Contains(string(data), "PRIVATE-") {
				t.Fatal("message/media data leaked")
			}
			logs := audit.String()
			if !strings.Contains(logs, "GET /operator/v1/chats") || !strings.Contains(logs, "results=1") {
				t.Fatal("missing identity-only audit", logs)
			}
			for _, secret := range []string{"alice", "Alice", outside, "PRIVATE-", fakeOperatorToken} {
				if strings.Contains(logs, secret) {
					t.Fatal("query, name or data leaked into audit")
				}
			}
			var name, activity string
			if err := b.Store.db.QueryRow(`SELECT name,CAST(last_message_time AS TEXT) FROM chats WHERE jid=?`, outside).Scan(&name, &activity); err != nil || name != "Alice Archive" || activity != dbTime(stamp) {
				t.Fatal("search modified the archive", err)
			}
		})
	}
}

func TestOperatorChatsContactFallbackUnicodeDigitsGroupsAndOrder(t *testing.T) {
	b := newSettingsBridge(t)
	contactPath := filepath.Join(t.TempDir(), "contacts.db")
	db, err := sql.Open("sqlite", contactPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT);
		CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT);
		INSERT INTO whatsmeow_contacts VALUES('self','5511999999999@s.whatsapp.net','5511999999999','ÁLICE','Ignored','Ignored');
		INSERT INTO whatsmeow_lid_map VALUES('100000000000001','5511999999999');`); err != nil {
		t.Fatal(err)
	}
	b.Store.waDB, err = openWhatsmeowContactsDB(contactPath)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		jid, name string
		stamp     time.Time
	}{
		{"5511999999999@s.whatsapp.net", "5511999999999", stamp},
		{"100000000000001@lid", "", stamp},
		{"120363000000000001@g.us", "Álice Group", stamp.Add(time.Hour)},
		{"5511888888888@s.whatsapp.net", "Bob", time.Time{}},
	} {
		if err := b.Store.StoreChat(row.jid, row.name, row.stamp); err != nil {
			t.Fatal(err)
		}
	}
	server := operatorChatsServer(t, b)
	for _, tc := range []struct {
		query string
		count int
	}{
		{"áLiCe", 3}, {"+55 (11) 99999-9999", 2}, {"missing", 0}, {"%", 0}, {"_", 0}, {"s.whatsapp.net", 0},
	} {
		status, _, chats := operatorChatsRequest(t, server, "?query="+url.QueryEscape(tc.query))
		if status != 200 || len(chats) != tc.count {
			t.Fatalf("query=%q status=%d chats=%+v", tc.query, status, chats)
		}
	}
	for range 2 {
		status, _, chats := operatorChatsRequest(t, server, "")
		if status != 200 || len(chats) != 4 || !chats[0].IsGroup || chats[1].JID != "100000000000001@lid" || chats[2].Name != "ÁLICE" || chats[3].LastMessageTime != nil {
			t.Fatalf("unstable ordering/fallback: %+v", chats)
		}
	}
	if names, failures := b.Store.names.size(); names != 0 || failures != 0 {
		t.Fatal("search warmed the chat name cache")
	}
}

func TestOperatorChatsLimitAndReadFailures(t *testing.T) {
	b := newSettingsBridge(t)
	for i := range 60 {
		if err := b.Store.StoreChat(fmt.Sprintf("%d@s.whatsapp.net", i+1), "Archive", time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	server := operatorChatsServer(t, b)
	for _, tc := range []struct {
		query string
		count int
	}{
		{"", 20}, {"?limit=1", 1}, {"?limit=50", 50},
	} {
		status, _, chats := operatorChatsRequest(t, server, tc.query)
		if status != 200 || len(chats) != tc.count || chats[0].JID != "60@s.whatsapp.net" {
			t.Fatalf("limit=%s status=%d count=%d", tc.query, status, len(chats))
		}
	}
	for _, limit := range []string{"0", "-1", "51", "999999999999999999999", "invalid", "", "1&limit=2"} {
		if status, _, _ := operatorChatsRequest(t, server, "?limit="+limit); status != 400 {
			t.Fatalf("invalid limit accepted: %q status=%d", limit, status)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/operator/v1/chats", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	b.handleOperatorChats(w, req)
	if w.Code != 503 {
		t.Fatal("cancelled query did not fail")
	}
	if err := b.Store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := operatorChatsRequest(t, server, ""); status != 503 {
		t.Fatal("unavailable archive did not fail")
	}
}

func TestOperatorChatsLIDOnlyContactNames(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("mapped=%v", mapped), func(t *testing.T) {
			b := newSettingsBridge(t)
			path := filepath.Join(t.TempDir(), "contacts.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT,PRIMARY KEY(our_jid,their_jid));
				CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE NOT NULL);
				INSERT INTO whatsmeow_contacts VALUES('self','100000000000001@lid',NULL,'Alice',NULL,NULL);`); err != nil {
				t.Fatal(err)
			}
			if mapped {
				if _, err := db.Exec(`INSERT INTO whatsmeow_lid_map VALUES('100000000000001','5511999999999')`); err != nil {
					t.Fatal(err)
				}
			}
			b.Store.waDB, err = openWhatsmeowContactsDB(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Store.StoreChat("100000000000001@lid", "", time.Time{}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mapped {
				want = 2
				if err := b.Store.StoreChat("5511999999999@s.whatsapp.net", "5511999999999", time.Time{}); err != nil {
					t.Fatal(err)
				}
				// An exact placeholder must not hide a useful alternate name.
				if _, err := db.Exec(`INSERT INTO whatsmeow_contacts VALUES('self','5511999999999@s.whatsapp.net','5511999999999',NULL,NULL,NULL)`); err != nil {
					t.Fatal(err)
				}
			}
			status, _, chats := operatorChatsRequest(t, operatorChatsServer(t, b), "?query=alice")
			if status != 200 || len(chats) != want || chats[0].Name != "Alice" || chats[0].JID != "100000000000001@lid" {
				t.Fatalf("LID-only contact missing: status=%d chats=%+v", status, chats)
			}
		})
	}
}

func TestOperatorChatsLargeContactArchiveSearchWithinDeadline(t *testing.T) {
	b := newSettingsBridge(t)
	path := filepath.Join(t.TempDir(), "contacts.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT,PRIMARY KEY(our_jid,their_jid));
		CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	chats, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chats.Rollback() }()
	contacts, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contacts.Rollback() }()
	for i := range 20000 {
		jid := fmt.Sprintf("%d@s.whatsapp.net", i+1)
		name := "Alice"
		if i == 19999 {
			name = "Bob"
		}
		if _, err := chats.Exec(`INSERT INTO chats(jid,name) VALUES(?,'')`, jid); err != nil {
			t.Fatal(err)
		}
		if _, err := contacts.Exec(`INSERT INTO whatsmeow_contacts VALUES('self',?,?,NULL,NULL,NULL)`, jid, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := chats.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := contacts.Commit(); err != nil {
		t.Fatal(err)
	}
	b.Store.waDB, err = openWhatsmeowContactsDB(path)
	if err != nil {
		t.Fatal(err)
	}
	server := operatorChatsServer(t, b)
	status, _, found := operatorChatsRequest(t, server, "?query=missing")
	if status != 200 || len(found) != 0 {
		t.Fatalf("bounded search failed on 20000 contacts: status=%d count=%d", status, len(found))
	}
	status, _, found = operatorChatsRequest(t, server, "?query=bob&limit=1")
	if status != 200 || len(found) != 1 || found[0].JID != "20000@s.whatsapp.net" || found[0].Name != "Bob" {
		t.Fatalf("contact match outside first page missing: status=%d chats=%+v", status, found)
	}
}

func TestOperatorChatsNoAssistantMetricsOrMCPActivityAndDenyPaths(t *testing.T) {
	b := newSettingsBridge(t)
	if err := b.Store.StoreChat("5511999999999@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := b.renderMetrics()
	var forwarded atomic.Int64
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		writeJSON(w, 200, map[string]string{"last_mcp_call_at": "2026-10-09T00:00:00Z"})
	}))
	defer admin.Close()
	client := newMCPAdminClient()
	transport := client.Transport
	client.Transport = mcpTestTransport(func(r *http.Request) (*http.Response, error) {
		r.URL.Host = strings.TrimPrefix(admin.URL, "http://")
		return transport.RoundTrip(r)
	})
	routes := withMCPAdmin((&operatorPairing{b: b}).routes(), "fake-bridge-0123456789abcdef", client)
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, routes, b.Log))
	defer server.Close()
	for _, tc := range []struct {
		method, token, host, origin string
		status                      int
	}{
		{"GET", "", "", "", 401}, {"GET", "fake-bridge-0123456789abcdef", "", "", 401},
		{"GET", fakeOperatorToken, "evil.example.test", "", 403}, {"GET", fakeOperatorToken, "", "https://evil.example.test", 403},
		{"POST", fakeOperatorToken, "", "", 405}, {"GET", fakeOperatorToken, "", server.URL, 200},
	} {
		req, _ := http.NewRequest(tc.method, server.URL+"/operator/v1/chats?query=Alice", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("deny path status=%d want=%d", response.StatusCode, tc.status)
		}
	}
	if forwarded.Load() != 0 {
		t.Fatal("chat search called MCP and could change assistant activity")
	}
	// The full metric exposition is unchanged apart from elapsed wall time.
	withoutUptime := func(value string) string {
		var lines []string
		for _, line := range strings.Split(value, "\n") {
			if !strings.HasPrefix(line, "whatsapp_bridge_uptime_seconds ") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if withoutUptime(before) != withoutUptime(b.renderMetrics()) {
		t.Fatal("operator search changed bridge metrics")
	}
	for range 60 {
		status, _, _ := operatorChatsRequest(t, server, "")
		if status == 429 {
			return
		}
		if status != 200 {
			t.Fatal("unexpected rate limit status", status)
		}
	}
	t.Fatal("chat route bypassed operator token rate limit")
}
