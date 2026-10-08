package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestContactNameEventsRefreshPlaceholderWithoutMessageOrNetwork(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	eventsToSend := []struct {
		name  string
		event any
	}{
		{"contact", &events.Contact{JID: phone, Action: &waSyncAction.ContactAction{FullName: proto.String("Alice Contact")}}},
		{"first name", &events.Contact{JID: phone, Action: &waSyncAction.ContactAction{FirstName: proto.String("Alice Contact")}}},
		{"push name", &events.PushName{JID: phone, NewPushName: "Alice Contact"}},
		{"business", &events.BusinessName{JID: phone, NewBusinessName: "Alice Contact"}},
	}
	for _, event := range eventsToSend {
		t.Run(event.name, func(t *testing.T) {
			ms := newTestMessageStore(t)
			ms.names = newChatNameCache()
			stamp := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
			if err := ms.StoreChat(phone.String(), phone.User, stamp); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid = ?", phone.String()).Scan(&before); err != nil {
				t.Fatal(err)
			}
			ms.names.put(phone.String(), phone.User)
			calls := 0
			ms.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
				calls++
				t.Fatal("contact metadata must not call the network")
				return nil, nil
			}
			b := testBridge(t, newTestClient(nil), ms, testLogger())
			b.handleEvent(event.event, nil)
			var name, activity string
			if err := ms.db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", phone.String()).Scan(&name, &activity); err != nil {
				t.Fatal(err)
			}
			if name != "Alice Contact" || activity != before || calls != 0 {
				t.Fatalf("name=%q activity=%q network=%d", name, activity, calls)
			}
			if cached, ok := ms.names.get(phone.String()); !ok || cached != name {
				t.Fatalf("cache %q, present %v", cached, ok)
			}
		})
	}
}

func TestContactEventsKeepRealNamesGroupsAndAbsentChats(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	group := types.NewJID("120363000000000001", types.GroupServer)
	for _, jid := range []types.JID{phone, group} {
		for _, event := range []any{
			&events.Contact{JID: jid, Action: &waSyncAction.ContactAction{FullName: proto.String("Replacement")}},
			&events.PushName{JID: jid, NewPushName: "Replacement"},
			&events.BusinessName{JID: jid, NewBusinessName: "Replacement"},
		} {
			ms := newTestMessageStore(t)
			if err := ms.StoreChat(jid.String(), "Existing Name", time.Now()); err != nil {
				t.Fatal(err)
			}
			b := testBridge(t, newTestClient(nil), ms, testLogger())
			b.handleEvent(event, nil)
			if got := storedChatName(t, ms, jid.String()); got != "Existing Name" {
				t.Fatalf("%s replaced real/group name with %q", jid, got)
			}
		}
	}
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(nil), ms, testLogger())
	b.handleEvent(&events.Contact{JID: phone, Action: nil}, nil)
	b.handleEvent(&events.PushName{JID: phone, NewPushName: "Alice"}, nil)
	var count int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&count); err != nil || count != 0 {
		t.Fatalf("created chat without a message: count=%d err=%v", count, err)
	}
}

func TestContactEventRefreshesBothMappedNamespaces(t *testing.T) {
	phone := types.NewJID("5511999999999", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	ms := newTestMessageStore(t)
	for _, jid := range []types.JID{phone, lid} {
		if err := ms.StoreChat(jid.String(), jid.User, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	b := testBridge(t, newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{lid: phone}, lidByPN: map[types.JID]types.JID{phone: lid}}), ms, testLogger())
	b.handleEvent(&events.PushName{JID: lid, JIDAlt: phone, NewPushName: "Alice"}, nil)
	for _, jid := range []types.JID{phone, lid} {
		if got := storedChatName(t, ms, jid.String()); got != "Alice" {
			t.Fatalf("%s placeholder remained %q", jid, got)
		}
	}
}

func TestPlaceholderNameCASPreservesConcurrentRealRename(t *testing.T) {
	ms := newTestMessageStore(t)
	phone := "5511999999999@s.whatsapp.net"
	if err := ms.StoreChat(phone, "5511999999999", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ms.RenameChat(phone, "Concurrent Real Name"); err != nil {
		t.Fatal(err)
	}
	if err := ms.RenamePlaceholderChat(phone, "5511999999999", "Stale Contact Name"); err != nil {
		t.Fatal(err)
	}
	if got := storedChatName(t, ms, phone); got != "Concurrent Real Name" {
		t.Fatalf("stale event overwrote name: %q", got)
	}
}
