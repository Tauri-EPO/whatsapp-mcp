package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestImmediateStartupMigrationsLeaveNoSessionAliasInPool(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreChat(phoneLID.String(), "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, sender string }{{"LEGACY", phoneLID.User}, {"UNCLASSIFIED", phonePN.User}} {
		if err := ms.StoreMessage(storedMessage{
			ID:        row.id,
			ChatJID:   phoneLID.String(),
			Sender:    row.sender,
			Content:   "migration searchable",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	wa, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wa.Close() }()
	if _, err := wa.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT NOT NULL);
		CREATE TABLE whatsmeow_contacts (our_jid TEXT,their_jid TEXT,first_name TEXT,full_name TEXT,push_name TEXT,business_name TEXT,PRIMARY KEY(our_jid,their_jid));`); err != nil {
		t.Fatal(err)
	}
	if _, err := wa.Exec("INSERT INTO whatsmeow_lid_map(lid,pn) VALUES(?,?)", phoneLID.User, phonePN.User); err != nil {
		t.Fatal(err)
	}
	ms, err = NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	for _, migrate := range []func(string, waLog.Logger) error{ms.MigrateLegacyLIDChatsToPhoneJIDs, ms.MigrateLegacyLIDSendersToPhones, ms.MigrateSenderNamespaces} {
		if err := migrate(whatsmeowDBPath(), testLogger()); err != nil {
			t.Fatalf("startup migration: %v", err)
		}
	}
	for _, id := range []string{"LEGACY", "UNCLASSIFIED"} {
		user, server := querySenderServer(t, ms, id, phonePN.String())
		if user != phonePN.User || server != types.DefaultUserServer {
			t.Fatalf("%s sender=%s/%s", id, user, server)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var conns []*sql.Conn
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range messagesPoolConns {
		conn, err := ms.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		rows, err := conn.QueryContext(ctx, "PRAGMA database_list")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var seq int
			var name, file string
			if err := rows.Scan(&seq, &name, &file); err != nil {
				t.Fatal(err)
			}
			if name != "main" && name != "temp" {
				t.Fatalf("session alias returned to pool: %s", name)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	conns = nil
	sessionWriter, err := wa.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sessionWriter.Rollback() }()
	if _, err := sessionWriter.Exec("UPDATE whatsmeow_lid_map SET pn=pn"); err != nil {
		t.Fatal(err)
	}
	if err := ms.Batch(func(batch *messageBatch) error {
		return batch.StoreMessage(storedMessage{
			ID:        "LIVE",
			ChatJID:   phonePN.String(),
			Sender:    phonePN.String(),
			Content:   "live searchable",
			Timestamp: time.Now(),
		})
	}); err != nil {
		t.Fatalf("unrelated session writer blocked archive: %v", err)
	}
	if err := sessionWriter.Commit(); err != nil {
		t.Fatal(err)
	}
	var busy, frames, checkpointed int
	if err := wa.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil || busy != 0 {
		t.Fatalf("session WAL checkpoint blocked after migration: busy=%d frames=%d checkpointed=%d err=%v", busy, frames, checkpointed, err)
	}

	var count int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("indexed=%d err=%v", count, err)
	}
}
