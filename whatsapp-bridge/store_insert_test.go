package main

import (
	"testing"
	"time"
)

func TestStoreMessagePreparationFailureCanRecover(t *testing.T) {
	ms := newTestMessageStore(t)
	timestamp := time.Unix(1700000000, 0)
	if err := ms.StoreChat(phonePN.String(), "Original", timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec("ALTER TABLE messages RENAME TO messages_unavailable"); err != nil {
		t.Fatal(err)
	}
	write := func(content string) error {
		return ms.StoreMessage("RETRY", phonePN.String(), phonePN.String(), content, timestamp, false, "", "", "", nil, nil, nil, nil, "")
	}
	if err := write("Refused"); err == nil {
		t.Fatal("message insert accepted an unavailable table")
	}
	if _, err := ms.db.Exec("ALTER TABLE messages_unavailable RENAME TO messages"); err != nil {
		t.Fatal(err)
	}
	if err := write("Recovered"); err != nil {
		t.Fatalf("failed preparation poisoned later writes: %v", err)
	}
	if err := write("Updated"); err != nil {
		t.Fatal(err)
	}
	var rows int
	var content string
	if err := ms.db.QueryRow("SELECT COUNT(*),MAX(content) FROM messages WHERE id='RETRY' AND chat_jid=?", phonePN.String()).Scan(&rows, &content); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || content != "Updated" {
		t.Fatalf("recovered upsert rows=%d content=%q", rows, content)
	}
}
