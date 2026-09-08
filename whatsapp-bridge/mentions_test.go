package main

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMentionsColumn(t *testing.T) {
	cases := []struct {
		name string
		jids []string
		want string
	}{
		{"none", nil, ""},
		{"lid", []string{"158883943301358@lid"}, "158883943301358"},
		{"phone", []string{"5511999999999@s.whatsapp.net"}, "5511999999999"},
		{"two forms of two people", []string{"158883943301358@lid", "5511999999999@s.whatsapp.net"}, "158883943301358,5511999999999"},
		{"duplicates collapse", []string{"1588@lid", "1588@lid", "1588@s.whatsapp.net"}, "1588"},
		{"device suffix dropped", []string{"5511999999999:12@s.whatsapp.net"}, "5511999999999"},
		{"bare user", []string{"5511999999999"}, "5511999999999"},
		{"empty entries skipped", []string{"", "@lid", "1588@lid"}, "1588"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionsColumn(tc.jids); got != tc.want {
				t.Fatalf("mentionsColumn(%v) = %q, want %q", tc.jids, got, tc.want)
			}
		})
	}
}

// A mention arrives in ContextInfo and has to land in the column, on the live
// path and inside a history-sync batch alike.
func TestPersistMessageStoresMentions(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "120363000000000000@g.us"
	if err := ms.StoreChat(chat, "Team", time.Now()); err != nil {
		t.Fatal(err)
	}

	mentioning := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("Lucas chegou! @158883943301358"),
		ContextInfo: &waE2E.ContextInfo{
			MentionedJID: []string{"158883943301358@lid", "5511888888888@s.whatsapp.net"},
		},
	}}
	ex := extractMessage(mentioning, time.Now(), "M1")
	if err := persistMessage(ms, "M1", chat, "5511777777777", time.Now(), false, ex, true, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := queryMentions(t, ms, "M1"); got != "158883943301358,5511888888888" {
		t.Fatalf("mentions = %q", got)
	}

	// A plain message leaves the column NULL: the archive pays nothing for it.
	plain := extractMessage(&waE2E.Message{Conversation: proto.String("bom dia")}, time.Now(), "M2")
	if err := persistMessage(ms, "M2", chat, "5511777777777", time.Now(), false, plain, true, testLogger()); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := ms.db.QueryRow(`SELECT mentions IS NULL FROM messages WHERE id = 'M2'`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatalf("mentions = %q for a message with none, want NULL", queryMentions(t, ms, "M2"))
	}

	// History sync writes through the batch, which must set the column too.
	err := ms.Batch(func(b *messageBatch) error {
		return persistMessage(b, "M3", chat, "5511777777777", time.Now(), false,
			extractMessage(mentioning, time.Now(), "M3"), false, testLogger())
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := queryMentions(t, ms, "M3"); got != "158883943301358,5511888888888" {
		t.Fatalf("batch mentions = %q", got)
	}
}

func queryMentions(t *testing.T, ms *MessageStore, id string) string {
	t.Helper()
	var mentions string
	if err := ms.db.QueryRow(`SELECT COALESCE(mentions, '') FROM messages WHERE id = ?`, id).Scan(&mentions); err != nil {
		t.Fatalf("read mentions of %s: %v", id, err)
	}
	return mentions
}

func TestMentionsFromText(t *testing.T) {
	cases := map[string]string{
		"o que acha desse, @158883943301358 ?":        "158883943301358",
		"@158883943301358 @5511888888888":             "158883943301358,5511888888888",
		"@158883943301358 e de novo @158883943301358": "158883943301358",
		"nothing here":          "",
		"mail@example.com":      "", // no digits after the @
		"@123":                  "", // too short to be a number
		"cost is 20@5 per unit": "",
	}
	for content, want := range cases {
		if got := mentionsFromText(content); got != want {
			t.Errorf("mentionsFromText(%q) = %q, want %q", content, got, want)
		}
	}
}

// Rows stored before the column existed keep their mentions only in the text.
// The migration recovers them once, logs the count, and marks every row it
// read so the next boot has nothing left to scan.
func TestMigrateMentionsBackfill(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	t.Chdir(t.TempDir())
	rec := installRecordingLogger(t)

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	db := ms.db

	const chat = "120363000000000000@g.us"
	seedLegacyRow(t, db, `INSERT INTO chats (jid, name) VALUES (?, 'Team')`, chat)
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('OLD1', ?, 's', 'Lucas chegou! @158883943301358', '2026-08-08 12:00:00+00:00', 0)`, chat)
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('OLD2', ?, 's', 'bom dia a todos', '2026-08-08 12:01:00+00:00', 0)`, chat)
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('OLD3', ?, 's', 'escreve pro joao@example.com', '2026-08-08 12:02:00+00:00', 0)`, chat)
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)
		VALUES ('NEW1', ?, 's', '@5511888888888 obrigado', '2026-08-08 12:03:00+00:00', 0, '5511888888888')`, chat)

	if err := migrateMentionsBackfill(db); err != nil {
		t.Fatalf("migrateMentionsBackfill: %v", err)
	}

	for id, want := range map[string]string{
		"OLD1": "158883943301358",
		"OLD2": "",
		"OLD3": "",
		"NEW1": "5511888888888",
	} {
		if got := queryMentions(t, ms, id); got != want {
			t.Errorf("%s mentions = %q, want %q", id, got, want)
		}
	}
	if logged := rec.String(); !strings.Contains(logged, "recovered mentions for 1 message(s)") {
		t.Errorf("expected the migration to log the count, got:\n%s", logged)
	}

	// Every row it read carries a value now, including the ones with nothing to
	// recover: that mark is what stops the next boot from reading them again.
	var unscanned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE mentions IS NULL AND content LIKE '%@%'`).Scan(&unscanned); err != nil {
		t.Fatal(err)
	}
	if unscanned != 0 {
		t.Errorf("%d row(s) left for the next boot to scan, want 0", unscanned)
	}

	// Idempotent: a second pass reads nothing, and a row stored afterwards is
	// picked up on its own.
	if filled, err := backfillMentionsFromContent(db); err != nil || filled != 0 {
		t.Errorf("second pass filled %d row(s) (err %v), want 0", filled, err)
	}
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('OLD4', ?, 's', 'e o @158883943301358 ?', '2026-08-08 12:04:00+00:00', 0)`, chat)
	if err := migrateMentionsBackfill(db); err != nil {
		t.Fatal(err)
	}
	if got := queryMentions(t, ms, "OLD4"); got != "158883943301358" {
		t.Errorf("OLD4 mentions = %q after a later boot", got)
	}
}
