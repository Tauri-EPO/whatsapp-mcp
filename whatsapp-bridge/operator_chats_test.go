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

	"go.mau.fi/whatsmeow/types"
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

func TestOperatorChatsIgnoresRuntimeAllowListWithinDeployList(t *testing.T) {
	for _, runtime := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime=%v", runtime), func(t *testing.T) {
			b := newSettingsBridge(t) // real SQLite archive on disk
			const deploy = "5511888888888@s.whatsapp.net,5511999999999@s.whatsapp.net"
			var err error
			b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
				if name == chatPolicyEnv {
					return deploy
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			b.Policy = b.runtimeChatPolicy(parseChatPolicy(deploy)) // as wired in production
			if runtime {
				err := b.patchRuntimeSettings(context.Background(), map[string]json.RawMessage{"chats.allowed": json.RawMessage(`["5511888888888@s.whatsapp.net"]`)}, []string{"chats.allowed"})
				if err != nil {
					t.Fatal(err)
				}
			}
			const outside = "5511999999999@s.whatsapp.net"
			if b.Policy.Allows(outside) != !runtime {
				t.Fatal("fixture did not enforce the runtime allow-list")
			}
			stamp := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			if err := b.Store.StoreChat(outside, "Alice Archive", stamp); err != nil {
				t.Fatal(err)
			}
			// Outside the deploy list: runtime could never allow it, so never shown.
			if err := b.Store.StoreChat("5511777777777@s.whatsapp.net", "Alice Elsewhere", stamp); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,content,media_type,filename,url) VALUES('private',?,'PRIVATE-MESSAGE-TEXT','image','PRIVATE-MEDIA-PATH','PRIVATE-MEDIA-URL')`, outside); err != nil {
				t.Fatal(err)
			}
			audit := &recordingLogger{}
			b.Log = audit
			server := operatorChatsServer(t, b)
			if status, _, chats := operatorChatsRequest(t, server, "?query=alice"); status != 200 || len(chats) != 1 || chats[0].JID != outside {
				t.Fatalf("deploy allow-list not applied: status=%d chats=%+v", status, chats)
			}
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
			if !strings.Contains(logs, "[DEBUG] Operator GET /operator/v1/chats") || !strings.Contains(logs, "results=1") || strings.Contains(logs, "[INFO]") {
				t.Fatal("search audit is not per-request DEBUG", logs)
			}
			for _, secret := range []string{"alice", "Alice", outside, "5511777777777", "PRIVATE-", fakeOperatorToken} {
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
	for range 121 {
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

func operatorChatsContacts(t *testing.T, b *Bridge, fixture string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contacts.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT,PRIMARY KEY(our_jid,their_jid));
		CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE NOT NULL);` + fixture); err != nil {
		t.Fatal(err)
	}
	if b.Store.waDB, err = openWhatsmeowContactsDB(path); err != nil {
		t.Fatal(err)
	}
}

// The SDK mutates client.Store.ID during pairing/logout: the account comes
// from the bridge's atomic snapshot (run under -race).
func TestOperatorChatsAccountFromAtomicDeviceSnapshot(t *testing.T) {
	b := newSettingsBridge(t)
	operatorChatsContacts(t, b, `INSERT INTO whatsmeow_contacts VALUES('5511000000000:1@s.whatsapp.net','5511888888888@s.whatsapp.net','Bob',NULL,NULL,NULL);
		INSERT INTO whatsmeow_contacts VALUES('5511999999999:1@s.whatsapp.net','5511888888888@s.whatsapp.net','Alice',NULL,NULL,NULL);`)
	if err := b.Store.StoreChat("5511888888888@s.whatsapp.net", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	device := types.NewADJID("5511999999999", 0, 1)
	account := device.String()
	b.runtimeClient.Store(b.Client)
	b.runtimePaired.Store(true)
	b.runtimeDeviceID.Store(&account)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
				b.Client.Store.ID = nil
				b.Client.Store.ID = &device
			}
		}
	}()
	for range 20 {
		w := httptest.NewRecorder()
		b.handleOperatorChats(w, httptest.NewRequest("GET", "/operator/v1/chats", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Alice"`) {
			close(done)
			<-stopped
			t.Fatalf("status=%d account not taken from the device snapshot", w.Code)
		}
	}
	close(done)
	<-stopped
}

func TestOperatorChatsUnreadableStampIsUnknown(t *testing.T) {
	b := newSettingsBridge(t)
	if err := b.Store.StoreChat("5511999999999@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Migrations leave unreadable legacy values on purpose.
	if _, err := b.Store.db.Exec(`UPDATE chats SET last_message_time='not-a-time'`); err != nil {
		t.Fatal(err)
	}
	status, _, chats := operatorChatsRequest(t, operatorChatsServer(t, b), "")
	if status != 200 || len(chats) != 1 || chats[0].LastMessageTime != nil {
		t.Fatalf("status=%d chats=%+v", status, chats)
	}
}

func TestOperatorChatsWhitespaceNameNinthDigitAndUnicodeFolding(t *testing.T) {
	b := newSettingsBridge(t)
	for _, row := range []struct{ jid, name string }{
		{"551188887777@s.whatsapp.net", "Old Mobile"}, {"5511988887777@s.whatsapp.net", "New Mobile"},
		{"5511999999999@s.whatsapp.net", "   "}, {"120363000000000001@g.us", "Café Grupo"},
		{"120363000000000002@g.us", "ΟΔΟΣ"}, {"120363000000000003@g.us", ""},
	} {
		if err := b.Store.StoreChat(row.jid, row.name, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	server := operatorChatsServer(t, b)
	for _, tc := range []struct{ query, want string }{
		{"+55 11 98888-7777", "Old Mobile"}, {"551188887777", "New Mobile"}, {"11988887777", "Old Mobile"},
		{"5511999999999", "5511999999999"}, {"café", "Café Grupo"}, {"οδος", "ΟΔΟΣ"},
		{"120363000000000003", "120363000000000003"}, // a blank group shows, and matches, its JID user
	} {
		status, _, chats := operatorChatsRequest(t, server, "?query="+url.QueryEscape(tc.query))
		found := false
		for _, chat := range chats {
			found = found || chat.Name == tc.want
		}
		if status != 200 || !found {
			t.Errorf("query=%q status=%d chats=%+v", tc.query, status, chats)
		}
	}
}

// Contact lookups must not wait on a messages.db connection held by the scan.
func TestOperatorChatsReleasesArchiveConnectionBeforeContactLookups(t *testing.T) {
	b := newSettingsBridge(t)
	if _, err := b.Store.db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT);
		CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT);
		INSERT INTO whatsmeow_contacts VALUES('self','5511999999999@s.whatsapp.net','Alice',NULL,NULL,NULL);`); err != nil {
		t.Fatal(err)
	}
	// More rows than one name-lookup page, so the scan is still open then.
	for i := range 300 {
		if err := b.Store.StoreChat(fmt.Sprintf("%d@s.whatsapp.net", i+1), "", time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Store.StoreChat("5511999999999@s.whatsapp.net", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	b.Store.db.SetMaxOpenConns(1) // the contacts handle shares the archive pool
	b.Store.waDB = b.Store.db
	t.Cleanup(func() { b.Store.waDB = nil })
	status, _, chats := operatorChatsRequest(t, operatorChatsServer(t, b), "?query=alice")
	if status != 200 || len(chats) != 1 || chats[0].Name != "Alice" {
		t.Fatalf("search held its archive connection: status=%d chats=%+v", status, chats)
	}
}

func TestOperatorChatsUnfilteredScanReadsOnlyTheLimit(t *testing.T) {
	b := newSettingsBridge(t)
	for i := range 300 {
		if err := b.Store.StoreChat(fmt.Sprintf("%d@s.whatsapp.net", i+1), "", time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := b.Store.operatorChatRows(context.Background(), chatPolicy{}, "", 20)
	if err != nil || len(rows) != 20 || rows[0].JID != "300@s.whatsapp.net" {
		t.Fatalf("unfiltered page not bounded by limit: %d %v", len(rows), err)
	}
}

func TestOperatorChatsOwnRateLimitAndAggregatedAudit(t *testing.T) {
	b := newSettingsBridge(t)
	server := operatorChatsServer(t, b)
	for range 60 {
		if status, _, _ := operatorChatsRequest(t, server, ""); status != 200 {
			t.Fatal("search refused within its budget", status)
		}
	}
	req, _ := http.NewRequest("GET", server.URL+"/operator/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("searches exhausted the budget of the other operator routes")
	}
	logs := &recordingLogger{}
	audit := &operatorSearchAudit{interval: 20 * time.Millisecond, logger: logs}
	for range 3 {
		audit.record()
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "[INFO] Operator GET /operator/v1/chats requests=3 in the last minute") {
		if time.Now().After(deadline) {
			t.Fatal("no aggregated INFO audit", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
