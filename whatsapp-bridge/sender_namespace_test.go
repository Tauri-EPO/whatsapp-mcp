package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// querySenderServer returns (sender, sender_server) for one message row;
// an unset namespace comes back as the empty string.
func querySenderServer(t *testing.T, ms *MessageStore, id, chatJID string) (string, string) {
	t.Helper()
	var sender string
	var server sql.NullString
	if err := ms.db.QueryRow(
		"SELECT sender, sender_server FROM messages WHERE id = ? AND chat_jid = ?", id, chatJID,
	).Scan(&sender, &server); err != nil {
		t.Fatalf("read message %s/%s: %v", chatJID, id, err)
	}
	return sender, server.String
}

func TestSplitSenderJID(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantUser   string
		wantServer any
	}{
		{"phone JID", "11234567890@s.whatsapp.net", "11234567890", "s.whatsapp.net"},
		{"lid JID", "191134718546018@lid", "191134718546018", "lid"},
		{"hosted phone JID", "11234567890@hosted", "11234567890", "s.whatsapp.net"},
		{"hosted lid JID", "191134718546018@hosted.lid", "191134718546018", "lid"},
		{"bare user", "11234567890", "11234567890", nil},
		{"group JID", "254110094043-1619359480@g.us", "254110094043-1619359480", nil},
		{"broadcast JID", "status@broadcast", "status", nil},
		{"empty", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, server := splitSenderJID(tc.in)
			if user != tc.wantUser || server != tc.wantServer {
				t.Fatalf("splitSenderJID(%q) = (%q, %v), want (%q, %v)",
					tc.in, user, server, tc.wantUser, tc.wantServer)
			}
		})
	}
}

func TestStoredSender(t *testing.T) {
	cases := []struct {
		name string
		in   types.JID
		want string
	}{
		{"lid", phoneLID, "185366493536339@lid"},
		{"phone", phonePN, "11234567890@s.whatsapp.net"},
		{"device suffix dropped", types.JID{User: "11234567890", Server: types.DefaultUserServer, Device: 12},
			"11234567890@s.whatsapp.net"},
		{"no user", types.JID{Server: types.DefaultUserServer}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storedSender(tc.in); got != tc.want {
				t.Fatalf("storedSender(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStoreMessage_RecordsSenderNamespace covers the single-row insert path:
// the namespace of the JID the caller passes lands in its own column, a bare
// user part leaves it unset, and an upsert from a bare caller does not erase
// a namespace already recorded.
func TestStoreMessage_RecordsSenderNamespace(t *testing.T) {
	ms := newTestMessageStore(t)
	chat := "status@broadcast"
	ts := time.Now()

	if err := ms.StoreMessage("LID", chat, "191134718546018@lid", "status", ts, false,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatalf("store lid message: %v", err)
	}
	if err := ms.StoreMessage("PN", chat, "11234567890@s.whatsapp.net", "hi", ts, false,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatalf("store phone message: %v", err)
	}
	if err := ms.StoreMessage("BARE", chat, "11234567890", "legacy caller", ts, false,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatalf("store bare message: %v", err)
	}

	if sender, server := querySenderServer(t, ms, "LID", chat); sender != "191134718546018" || server != "lid" {
		t.Errorf("lid row = (%q, %q), want (%q, %q)", sender, server, "191134718546018", "lid")
	}
	if sender, server := querySenderServer(t, ms, "PN", chat); sender != "11234567890" || server != "s.whatsapp.net" {
		t.Errorf("phone row = (%q, %q), want (%q, %q)", sender, server, "11234567890", "s.whatsapp.net")
	}
	if sender, server := querySenderServer(t, ms, "BARE", chat); sender != "11234567890" || server != "" {
		t.Errorf("bare row = (%q, %q), want (%q, unset)", sender, server, "11234567890")
	}

	// An edit/replay that only knows the bare user part must not downgrade a
	// row that already carries its namespace.
	if err := ms.StoreMessage("LID", chat, "191134718546018", "status edited", ts, false,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatalf("re-store lid message: %v", err)
	}
	if _, server := querySenderServer(t, ms, "LID", chat); server != "lid" {
		t.Errorf("namespace after bare upsert = %q, want %q", server, "lid")
	}

	// ... but a bare upsert that also changes the user part must not leave the
	// old namespace describing the new number.
	if err := ms.StoreMessage("LID", chat, "5511999999999", "different sender", ts, false,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatalf("re-store with another sender: %v", err)
	}
	if sender, server := querySenderServer(t, ms, "LID", chat); sender != "5511999999999" || server != "" {
		t.Errorf("row after sender change = (%q, %q), want (%q, unset)", sender, server, "5511999999999")
	}
}

// TestBatchStoreMessage_RecordsSenderNamespace covers the history-sync path,
// which writes through the prepared statement in store_batch.go.
func TestBatchStoreMessage_RecordsSenderNamespace(t *testing.T) {
	ms := newTestMessageStore(t)
	chat := "254110094043-1619359480@g.us"

	if err := ms.Batch(func(b *messageBatch) error {
		return b.StoreMessage("H1", chat, "191134718546018@lid", "history", time.Now(), false,
			"", "", "", nil, nil, nil, 0, "")
	}); err != nil {
		t.Fatalf("batch store: %v", err)
	}

	if sender, server := querySenderServer(t, ms, "H1", chat); sender != "191134718546018" || server != "lid" {
		t.Errorf("history row = (%q, %q), want (%q, %q)", sender, server, "191134718546018", "lid")
	}
}

// TestHandleMessage_RecordsSenderNamespace is the live path: an unresolved
// LID sender must be stored as a LID, a phone sender as a phone number.
func TestHandleMessage_RecordsSenderNamespace(t *testing.T) {
	t.Run("unresolved lid", func(t *testing.T) {
		client := newTestClient(&mockLIDStore{})
		ms := newTestMessageStore(t)
		msg := buildTextMessage(phoneLID, phoneLID, types.EmptyJID, types.EmptyJID, false, "orphan")

		testBridge(client, ms, testLogger()).handleMessage(msg)

		sender, server := querySenderServer(t, ms, msg.Info.ID, phoneLID.String())
		if sender != phoneLID.User || server != types.HiddenUserServer {
			t.Fatalf("stored sender = (%q, %q), want (%q, %q)",
				sender, server, phoneLID.User, types.HiddenUserServer)
		}
	})

	t.Run("phone", func(t *testing.T) {
		client := newTestClient(&mockLIDStore{})
		ms := newTestMessageStore(t)
		msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "hello")

		testBridge(client, ms, testLogger()).handleMessage(msg)

		sender, server := querySenderServer(t, ms, msg.Info.ID, phonePN.String())
		if sender != phonePN.User || server != types.DefaultUserServer {
			t.Fatalf("stored sender = (%q, %q), want (%q, %q)",
				sender, server, phonePN.User, types.DefaultUserServer)
		}
	})

	t.Run("lid resolved to phone", func(t *testing.T) {
		client := newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}})
		ms := newTestMessageStore(t)
		msg := buildTextMessage(phoneLID, phoneLID, types.EmptyJID, types.EmptyJID, false, "mapped")

		testBridge(client, ms, testLogger()).handleMessage(msg)

		sender, server := querySenderServer(t, ms, msg.Info.ID, phonePN.String())
		if sender != phonePN.User || server != types.DefaultUserServer {
			t.Fatalf("stored sender = (%q, %q), want (%q, %q)",
				sender, server, phonePN.User, types.DefaultUserServer)
		}
	})
}

// seedWhatsAppDB writes a whatsmeow database with a LID map and a contact
// list, and returns its path.
func seedWhatsAppDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	waDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("create whatsapp db: %v", err)
	}
	defer func() { _ = waDB.Close() }()

	if _, err := waDB.Exec(`
		CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT NOT NULL);
		INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('271234567890123', '5511999999999');
		CREATE TABLE whatsmeow_contacts (
			our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT,
			push_name TEXT, business_name TEXT, PRIMARY KEY (our_jid, their_jid)
		);
		INSERT INTO whatsmeow_contacts (our_jid, their_jid, full_name)
			VALUES ('10000000000@s.whatsapp.net', '5511888888888@s.whatsapp.net', 'Known Contact');
	`); err != nil {
		t.Fatalf("seed whatsapp db: %v", err)
	}
	return path
}

// TestMigrateSenderNamespaces_ClassifiesLegacyRows walks every class of the
// backfill on rows an older bridge left without a namespace, then re-runs it
// to prove the second pass is a no-op.
func TestMigrateSenderNamespaces_ClassifiesLegacyRows(t *testing.T) {
	ms := newTestMessageStore(t)
	logger := testLogger()
	whatsappDBPath := seedWhatsAppDB(t)

	const chatPhone = "5511777777777@s.whatsapp.net"
	const broadcast = "status@broadcast"
	// Seeded with literals: the driver restarts parameter numbering on every
	// statement of a multi-statement Exec, so placeholders here would bind to
	// the wrong rows.
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, name, last_message_time) VALUES
			('5511777777777@s.whatsapp.net', 'Peer',   '2026-03-01 10:00:00+00:00'),
			('status@broadcast',             'Status', '2026-03-01 10:00:00+00:00'),
			('9988776655@lid',               'Anon',   '2026-03-01 10:00:00+00:00');

		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
			('mapped_lid',  '5511777777777@s.whatsapp.net', '271234567890123', 'lid map hit',   '2026-03-01 10:00:00+00:00', 0),
			('mapped_pn',   '5511777777777@s.whatsapp.net', '5511999999999',   'phone map hit', '2026-03-01 10:01:00+00:00', 0),
			('chat_phone',  '5511777777777@s.whatsapp.net', '5511777777777',   'known chat',    '2026-03-01 10:02:00+00:00', 0),
			('contact_pn',  '5511777777777@s.whatsapp.net', '5511888888888',   'known contact', '2026-03-01 10:03:00+00:00', 0),
			('chat_lid',    '9988776655@lid',               '9988776655',      'known lid chat','2026-03-01 10:04:00+00:00', 0),
			('long_lid',    'status@broadcast',             '191134718546018', 'unmapped lid',  '2026-03-01 10:05:00+00:00', 0),
			('short_phone', 'status@broadcast',             '5511666666666',   'unknown phone', '2026-03-01 10:06:00+00:00', 0),
			('group_user',  'status@broadcast',             '254110094043-1619359480', 'group fallback', '2026-03-01 10:07:00+00:00', 0);
	`); err != nil {
		t.Fatalf("seed message store: %v", err)
	}

	if err := ms.MigrateSenderNamespaces(whatsappDBPath, logger); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	want := map[string]string{
		"mapped_lid":  "lid",            // whatsmeow knows this user as a LID
		"mapped_pn":   "s.whatsapp.net", // ... and this one as the number behind it
		"chat_phone":  "s.whatsapp.net", // a chat JID names it
		"contact_pn":  "s.whatsapp.net", // the contact list names it
		"chat_lid":    "lid",            // a chat JID names it, in the other namespace
		"long_lid":    "lid",            // 15 digits, unknown everywhere
		"short_phone": "",               // no evidence, left for the MCP fallback
		"group_user":  "",               // not a user namespace at all
	}
	for id, wantServer := range want {
		chat := broadcast
		switch id {
		case "mapped_lid", "mapped_pn", "chat_phone", "contact_pn":
			chat = chatPhone
		case "chat_lid":
			chat = "9988776655@lid"
		}
		if _, server := querySenderServer(t, ms, id, chat); server != wantServer {
			t.Errorf("%s: sender_server = %q, want %q", id, server, wantServer)
		}
	}

	// Idempotent: a second run classifies nothing new and changes nothing.
	before := senderNamespaceSnapshot(t, ms)
	if err := ms.MigrateSenderNamespaces(whatsappDBPath, logger); err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	after := senderNamespaceSnapshot(t, ms)
	for id, server := range before {
		if after[id] != server {
			t.Errorf("%s changed on the second run: %q -> %q", id, server, after[id])
		}
	}
}

// senderNamespaceSnapshot maps message ID -> sender_server for every row.
func senderNamespaceSnapshot(t *testing.T, ms *MessageStore) map[string]string {
	t.Helper()
	rows, err := ms.db.Query("SELECT id, COALESCE(sender_server, '') FROM messages")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]string{}
	for rows.Next() {
		var id, server string
		if err := rows.Scan(&id, &server); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		got[id] = server
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return got
}

// TestMigrateSenderNamespaces_WithoutWhatsAppDB: a restored backup (or a store
// waiting to be re-paired) has no whatsapp.db, and the rules that read only
// messages.db must still run instead of leaving every sender unclassified.
func TestMigrateSenderNamespaces_WithoutWhatsAppDB(t *testing.T) {
	ms := newTestMessageStore(t)
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, last_message_time) VALUES
			('status@broadcast',             '2026-03-01 10:00:00+00:00'),
			('5511777777777@s.whatsapp.net', '2026-03-01 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
			('long_lid',   'status@broadcast', '191134718546018', 'x', '2026-03-01 10:00:00+00:00', 0),
			('chat_phone', 'status@broadcast', '5511777777777',   'y', '2026-03-01 10:01:00+00:00', 0);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing-whatsapp.db")
	if err := ms.MigrateSenderNamespaces(missing, testLogger()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if _, server := querySenderServer(t, ms, "long_lid", "status@broadcast"); server != "lid" {
		t.Errorf("long_lid sender_server = %q, want %q", server, "lid")
	}
	if _, server := querySenderServer(t, ms, "chat_phone", "status@broadcast"); server != "s.whatsapp.net" {
		t.Errorf("chat_phone sender_server = %q, want %q", server, "s.whatsapp.net")
	}
}

// TestMigrateSenderNamespaces_NothingToDoSkipsAttach: a fully classified store
// never touches whatsapp.db, so a bridge whose store is migrated pays nothing
// for the backfill at startup.
func TestMigrateSenderNamespaces_NothingToDoSkipsAttach(t *testing.T) {
	ms := newTestMessageStore(t)
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, last_message_time) VALUES ('status@broadcast', '2026-03-01 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me)
			VALUES ('m1', 'status@broadcast', '191134718546018', 'lid', 'x', '2026-03-01 10:00:00+00:00', 0);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A path that would fail to attach if the migration ever reached it.
	unreadable := filepath.Join(t.TempDir(), "nope", "whatsapp.db")
	if err := ms.MigrateSenderNamespaces(unreadable, testLogger()); err != nil {
		t.Fatalf("expected a no-op, got error: %v", err)
	}
}

// TestMigrateLegacyLIDSendersToPhones_UpdatesNamespace: when whatsmeow learns
// the number behind a LID, the sender rewrite must move the namespace with the
// user part instead of leaving the row marked as a LID.
func TestMigrateLegacyLIDSendersToPhones_UpdatesNamespace(t *testing.T) {
	ms := newTestMessageStore(t)
	logger := testLogger()
	whatsappDBPath := seedWhatsAppDB(t)

	const chat = "5511999999999@s.whatsapp.net"
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, last_message_time)
			VALUES ('5511999999999@s.whatsapp.net', '2026-03-01 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me)
			VALUES ('m1', '5511999999999@s.whatsapp.net', '271234567890123', 'lid', 'x', '2026-03-01 10:00:00+00:00', 0);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := ms.MigrateLegacyLIDSendersToPhones(whatsappDBPath, logger); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	sender, server := querySenderServer(t, ms, "m1", chat)
	if sender != "5511999999999" || server != "s.whatsapp.net" {
		t.Fatalf("row = (%q, %q), want (%q, %q)", sender, server, "5511999999999", "s.whatsapp.net")
	}
}

// TestMigrateLegacyLIDSendersToPhones_LeavesRecordedPhonesAlone: the rewrite
// matches on the bare digits, so a phone number that happens to spell a LID in
// the map must be protected by the namespace the row already carries.
func TestMigrateLegacyLIDSendersToPhones_LeavesRecordedPhonesAlone(t *testing.T) {
	ms := newTestMessageStore(t)
	whatsappDBPath := seedWhatsAppDB(t)

	const chat = "271234567890123@s.whatsapp.net"
	if _, err := ms.db.Exec(`
		INSERT INTO chats (jid, last_message_time)
			VALUES ('271234567890123@s.whatsapp.net', '2026-03-01 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me)
			VALUES ('m1', '271234567890123@s.whatsapp.net', '271234567890123', 's.whatsapp.net',
			        'x', '2026-03-01 10:00:00+00:00', 0);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := ms.MigrateLegacyLIDSendersToPhones(whatsappDBPath, testLogger()); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	sender, server := querySenderServer(t, ms, "m1", chat)
	if sender != "271234567890123" || server != "s.whatsapp.net" {
		t.Fatalf("row = (%q, %q), want it untouched", sender, server)
	}
}
