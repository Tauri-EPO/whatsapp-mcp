package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wmStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"modernc.org/sqlite"
)

func TestLocalContactNamesAgreeWithOperatorSearch(t *testing.T) {
	for _, tc := range []struct{ title, chat, stored, contact, full, push, first, business, want string }{
		{"numeric placeholder", phonePN.String(), "+" + phonePN.User, phonePN.String(), phonePN.User, "Alice", "Bob", "Carol", "Alice"},
		{"formatted phone identity", phonePN.String(), "+" + phonePN.User[:2] + " " + phonePN.User[2:4] + "-" + phonePN.User[4:], phonePN.String(), "Alice", "", "", "", "Alice"},
		{"LID only", phoneLID.String(), "", phoneLID.String(), "", "Alice", "Bob", "Carol", "Alice"},
		{"phone reached via LID", phoneLID.String(), phoneLID.User, phonePN.String(), "Alice", "Bob", "Carol", "Dave", "Alice"},
		{"formatted phone twin", phoneLID.String(), "+" + phonePN.User, phonePN.String(), "Alice", "", "", "", "Alice"},
		{"empty fields", phonePN.String(), phonePN.User, phonePN.String(), "", " ", "", "", phonePN.User},
		{"full first", phonePN.String(), phonePN.User, phonePN.String(), "Alice", "Bob", "Carol", "Dave", "Alice"},
		{"first fallback", phonePN.String(), phonePN.User, phonePN.String(), "", "", "Carol", "Dave", "Carol"},
		{"business fallback", phonePN.String(), phonePN.User, phonePN.String(), "", "", "", "Dave", "Dave"},
		{"numeric direct name", phonePN.String(), "2024", phonePN.String(), "Alice", "", "", "", "2024"},
		{"numeric newsletter name", types.NewJID("120363000000000001", types.NewsletterServer).String(), "2024", "", "", "", "", "", "2024"},
		{"numeric broadcast name", "status@broadcast", "2024", "", "", "", "", "", "2024"},
		{"numeric group name", "120363000000000001@g.us", "2024", "", "", "", "", "", "2024"},
		{"device-suffixed identity", phonePN.User + ":3@" + phonePN.Server, phonePN.User, phonePN.String(), "Alice", "", "", "", "Alice"},
	} {
		t.Run(tc.title, func(t *testing.T) {
			b := newSettingsBridge(t)
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
			if err != nil {
				t.Fatal(err)
			}
			boundPool(db, 2)
			t.Cleanup(func() { _ = db.Close() })
			b.Store.waDB = db
			if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT);
                CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT);`); err != nil {
				t.Fatal(err)
			}
			account := types.NewJID("100000000000002", types.DefaultUserServer)
			if _, err := db.Exec(`INSERT INTO whatsmeow_contacts VALUES(?,?,?,?,?,?)`, account.String(), tc.contact, tc.full, tc.push, tc.first, tc.business); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO whatsmeow_lid_map VALUES(?,?)`, phoneLID.User, phonePN.User); err != nil {
				t.Fatal(err)
			}
			if err := b.Store.StoreChat(tc.chat, tc.stored, time.Now()); err != nil {
				t.Fatal(err)
			}
			network := 0
			b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { network++; return nil, nil }
			server := operatorChatsServer(t, b)
			status, _, chats := operatorChatsRequest(t, server, "")
			if status != 200 || len(chats) != 1 || chats[0].Name != tc.want {
				t.Fatalf("operator: status=%d chats=%+v", status, chats)
			}
			if names, failures := b.Store.names.size(); names != 0 || failures != 0 || network != 0 {
				t.Fatal("operator contacted WhatsApp or warmed names")
			}
			client := newTestClient(&mockLIDStore{})
			client.Store.ID = &account
			got := GetChatName(client, b.Store, localContactJID(tc.chat), tc.chat, nil, "", false, testLogger())
			if got != chats[0].Name {
				t.Fatalf("chat name=%q operator name=%q", got, chats[0].Name)
			}
			if tc.stored == "2024" {
				if cached, ok := b.Store.names.get(tc.chat); !ok || cached != "2024" {
					t.Fatal("real numeric name lost its cache")
				}
			}
		})
	}
}

// The spy delegates to real SQLite and records query properties, not values.
type contactQueryTrace struct {
	mu      sync.Mutex
	queries []string
}
type contactTraceConnector struct {
	path  string
	trace *contactQueryTrace
	base  *sqlite.Driver
}

func (c contactTraceConnector) Driver() driver.Driver { return c.base }
func (c contactTraceConnector) Connect(_ context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	return contactTraceConn{Conn: conn, trace: c.trace}, nil
}

type contactTraceConn struct {
	driver.Conn
	trace *contactQueryTrace
}

func (c contactTraceConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.trace.mu.Lock()
	c.trace.queries = append(c.trace.queries, query)
	c.trace.mu.Unlock()
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestNamedLIDPhoneSearchDoesNotReadContactNames(t *testing.T) {
	b := newSettingsBridge(t)
	path := filepath.Join(t.TempDir(), "contacts.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, 2)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT,push_name TEXT,first_name TEXT,business_name TEXT);
        CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO whatsmeow_contacts VALUES('self',?,'Alice',NULL,NULL,NULL)`, phoneLID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO whatsmeow_lid_map VALUES(?,?)`, phoneLID.User, phonePN.User); err != nil {
		t.Fatal(err)
	}
	trace := &contactQueryTrace{}
	b.Store.waDB = sql.OpenDB(contactTraceConnector{path: path, trace: trace, base: &sqlite.Driver{}})
	boundPool(b.Store.waDB, 2)
	if err := b.Store.StoreChat(phoneLID.String(), "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	server := operatorChatsServer(t, b)
	status, _, chats := operatorChatsRequest(t, server, "?query="+phonePN.User)
	if status != 200 || len(chats) != 1 || chats[0].Name != "Alice" {
		t.Fatalf("named LID: status=%d chats=%+v", status, chats)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	for _, query := range trace.queries {
		if strings.Contains(query, "full_name") {
			t.Fatal("named LID read contact-name fields for a phone search")
		}
	}
}

func TestContactNameCursorErrorsAreWarnedWithoutContactValues(t *testing.T) {
	b := newSettingsBridge(t)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, 2)
	t.Cleanup(func() { _ = db.Close() })
	b.Store.waDB = db
	if _, err := db.Exec(`CREATE TABLE raw_contacts(our_jid TEXT,their_jid TEXT,full_name TEXT);
        CREATE VIEW whatsmeow_contacts AS SELECT our_jid,their_jid,json_extract(full_name,'$') AS full_name,NULL AS push_name,NULL AS first_name,NULL AS business_name FROM raw_contacts;
        CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO raw_contacts VALUES('self',?,'"Alice"'),('self',?,'{')`, phonePN.String(), phoneLID.String()); err != nil {
		t.Fatal(err)
	}
	for _, jid := range []types.JID{phonePN, phoneLID} {
		if err := b.Store.StoreChat(jid.String(), jid.User, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	b.Log = newTextWriter("Bridge", "INFO", &out, false)
	server := operatorChatsServer(t, b)
	status, _, _ := operatorChatsRequest(t, server, "")
	if status != 200 || !strings.Contains(out.String(), "malformed JSON") {
		t.Fatalf("cursor failure was silent: status=%d log=%s", status, out.String())
	}
	if strings.Contains(out.String(), phonePN.User) || strings.Contains(out.String(), "Alice") {
		t.Fatal("contact values leaked into query warnings")
	}
}

func TestMissingContactTablesRemainSilent(t *testing.T) {
	store := newTestMessageStore(t)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, 2)
	t.Cleanup(func() { _ = db.Close() })
	store.waDB = db
	client := newTestClient(&mockLIDStore{})
	var out bytes.Buffer
	logger := newTextWriter("Bridge", "INFO", &out, false)
	if got := GetChatName(client, store, phonePN, phonePN.String(), nil, "", false, logger); got != phonePN.User {
		t.Fatal(got)
	}
	if out.Len() != 0 {
		t.Fatal("optional missing contact tables produced a warning")
	}
}

type recordingContactStore struct {
	wmStore.ContactStore
	got types.JID
}

func (c *recordingContactStore) GetContact(_ context.Context, jid types.JID) (types.ContactInfo, error) {
	c.got = jid
	return types.ContactInfo{FullName: "Alice"}, nil
}
func TestDeviceIdentityUsesTheSameNonADSDKKey(t *testing.T) {
	store := newTestMessageStore(t)
	client := newTestClient(&mockLIDStore{})
	contacts := &recordingContactStore{}
	client.Store.Contacts = contacts
	raw := phonePN.User + ":3@" + phonePN.Server
	jid, err := types.ParseJID(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := GetChatName(client, store, jid, raw, nil, "", false, testLogger()); got != "Alice" {
		t.Fatal(got)
	}
	if contacts.got != phonePN.ToNonAD() {
		t.Fatalf("SDK contact key was not normalized: %s", contacts.got)
	}
}

func TestContactBookQueryFailuresAreWarned(t *testing.T) {
	store := newTestMessageStore(t)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, 2)
	store.waDB = db
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(&mockLIDStore{})
	client.Store.ID = &phonePN
	var out bytes.Buffer
	logger := newTextWriter("Bridge", "INFO", &out, false)
	if got := GetChatName(client, store, phonePN, phonePN.String(), nil, "", false, logger); got != phonePN.User {
		t.Fatal(got)
	}
	for _, table := range []string{"whatsmeow_lid_map", "whatsmeow_contacts"} {
		if !strings.Contains(out.String(), table) {
			t.Fatalf("%s query failure was silent", table)
		}
	}
	if strings.Contains(out.String(), phonePN.User) {
		t.Fatal("query warnings included an identity")
	}
}
