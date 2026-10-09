package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReceiptProgressSurvivesLIDMigration(t *testing.T) {
	ms := newTestMessageStore(t)
	lid, phone := "111@lid", "222@s.whatsapp.net"
	stamp := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	for _, chat := range []string{lid, phone} {
		seedMarkReadChat(t, ms, chat, nil)
	}
	for _, id := range []string{"copied", "collision", "phone-acked", "pending"} {
		seedMarkReadMessage(t, ms, lid, id, "111", stamp, false, nil)
		if id != "copied" {
			seedMarkReadMessage(t, ms, phone, id, "222", stamp, false, nil)
		}
	}
	if _, err := ms.db.Exec(`UPDATE messages SET read_receipt_sent=1 WHERE
		(chat_jid=? AND id IN ('copied','collision')) OR (chat_jid=? AND id='phone-acked')`, lid, phone); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	waDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waDB.Close() }()
	if _, err := waDB.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT NOT NULL);
		INSERT INTO whatsmeow_lid_map VALUES ('111','222')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"copied", "collision", "phone-acked", "pending"} {
		var progress int
		if err := ms.db.QueryRow(`SELECT read_receipt_sent FROM messages WHERE chat_jid=? AND id=?`, phone, id).Scan(&progress); err != nil {
			t.Fatal(err)
		}
		want := 1
		if id == "pending" {
			want = 0
		}
		if progress != want {
			t.Errorf("%s: receipt progress %d, want %d", id, progress, want)
		}
	}
	rows, err := ms.UnreadInboundMessages(phone, stamp, 1000)
	if err != nil || len(rows) != 1 || rows[0].ID != "pending" {
		t.Fatalf("migration returned acknowledged receipts as unread: %+v, %v", rows, err)
	}
}
