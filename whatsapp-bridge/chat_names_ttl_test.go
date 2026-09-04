package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestChatNameCacheExpires(t *testing.T) {
	c := newChatNameCache()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.put("g@g.us", "Family")
	if name, ok := c.get("g@g.us"); !ok || name != "Family" {
		t.Fatalf("fresh entry: %q %v", name, ok)
	}
	now = now.Add(chatNameTTL + time.Second)
	if _, ok := c.get("g@g.us"); ok {
		t.Fatal("entry should expire after chatNameTTL")
	}
	if n, _ := c.size(); n != 0 {
		t.Fatalf("expired entry should be evicted, size=%d", n)
	}
}

func TestGroupFailuresArePruned(t *testing.T) {
	c := newChatNameCache()
	base := time.Now()
	for i := 0; i < 100; i++ {
		c.rememberGroupFailure(types.JID{User: string(rune('a'+i%26)) + string(rune('0'+i/26)), Server: types.GroupServer}.String(), base.Add(-groupInfoRetryAfter-time.Minute))
	}
	c.rememberGroupFailure("fresh@g.us", base)
	if _, failures := c.size(); failures != 1 {
		t.Fatalf("expired failures should be pruned, got %d", failures)
	}
}

func TestGroupRenameUpdatesStoreAndCache(t *testing.T) {
	ms := newTestMessageStore(t)
	ms.names = newChatNameCache()
	group := types.JID{User: "120363000000000001", Server: types.GroupServer}
	if err := ms.StoreChat(group.String(), "Old Name", time.Now()); err != nil {
		t.Fatal(err)
	}
	ms.names.put(group.String(), "Old Name")

	b := testBridge(newTestClient(&mockLIDStore{}), ms, testLogger())
	b.handleEvent(&events.GroupInfo{JID: group, Name: &types.GroupName{Name: "New Name"}}, make(chan bool, 1))

	if name, ok := ms.names.get(group.String()); !ok || name != "New Name" {
		t.Fatalf("cache after rename = %q (%v)", name, ok)
	}
	var stored string
	if err := ms.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, group.String()).Scan(&stored); err != nil || stored != "New Name" {
		t.Fatalf("chats.name after rename = %q (err %v)", stored, err)
	}
	// GetChatName now answers with the new name without a network lookup.
	if got := GetChatName(nil, ms, group, group.String(), nil, "", false, testLogger()); got != "New Name" {
		t.Fatalf("GetChatName = %q", got)
	}
}

func TestInvalidateForgetsNameAndFailure(t *testing.T) {
	c := newChatNameCache()
	c.put("g@g.us", "X")
	c.rememberGroupFailure("g@g.us", time.Now())
	c.invalidate("g@g.us")
	if n, f := c.size(); n != 0 || f != 0 {
		t.Fatalf("size after invalidate = %d/%d", n, f)
	}
	if !c.groupLookupAllowed("g@g.us", time.Now()) {
		t.Fatal("lookup should be allowed again after invalidate")
	}
}
