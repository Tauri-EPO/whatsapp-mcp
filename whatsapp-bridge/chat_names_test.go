package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func groupJID(user string) types.JID { return types.JID{User: user, Server: types.GroupServer} }

func TestGetChatName_GroupResolutionOrder(t *testing.T) {
	client := newTestClient(&mockLIDStore{})
	ms := newTestMessageStore(t)
	ms.names = newChatNameCache()
	logger := testLogger()
	jid := groupJID("120363000000000001")
	calls := 0
	ms.groupInfo = func(_ context.Context, j types.JID) (*types.GroupInfo, error) {
		calls++
		if j.User == "120363000000000001" {
			g := &types.GroupInfo{JID: j}
			g.Name = "From Network"
			return g, nil
		}
		return nil, errors.New("not a member")
	}

	t.Run("conversation name wins without network", func(t *testing.T) {
		conv := &waHistorySync.Conversation{ID: proto.String(jid.String()), Name: proto.String("From Sync")}
		if got := GetChatName(client, ms, jid, jid.String(), conv, "", false, logger); got != "From Sync" {
			t.Fatalf("got %q", got)
		}
		if calls != 0 {
			t.Fatal("history sync must not fetch group info")
		}
	})

	t.Run("cached afterwards", func(t *testing.T) {
		if got := GetChatName(client, ms, jid, jid.String(), nil, "", true, logger); got != "From Sync" || calls != 0 {
			t.Fatalf("got %q calls=%d", got, calls)
		}
	})

	other := groupJID("120363000000000002")
	t.Run("history sync without a name yields placeholder, no network", func(t *testing.T) {
		if got := GetChatName(client, ms, other, other.String(), &waHistorySync.Conversation{}, "", false, logger); got != "Group 120363000000000002" {
			t.Fatalf("got %q", got)
		}
		if calls != 0 {
			t.Fatal("history sync must not fetch group info")
		}
	})

	t.Run("live message fetches once and caches failure", func(t *testing.T) {
		got := GetChatName(client, ms, other, other.String(), nil, "", true, logger)
		if got != "Group 120363000000000002" || calls != 1 {
			t.Fatalf("got %q calls=%d", got, calls)
		}
		// Second live message inside the retry window: no new fetch.
		_ = GetChatName(client, ms, other, other.String(), nil, "", true, logger)
		if calls != 1 {
			t.Fatalf("failed lookup retried too soon (calls=%d)", calls)
		}
		// After the window it is retried.
		ms.names.groupErr[other.String()] = time.Now().Add(-groupInfoRetryAfter - time.Second) // expired: lookup allowed again
		_ = GetChatName(client, ms, other, other.String(), nil, "", true, logger)
		if calls != 2 {
			t.Fatalf("expected retry after window (calls=%d)", calls)
		}
	})

	t.Run("stored placeholder is upgraded on a live message", func(t *testing.T) {
		third := groupJID("120363000000000001")
		fresh := newTestMessageStore(t)
		fresh.names = newChatNameCache()
		fresh.groupInfo = ms.groupInfo
		if err := fresh.StoreChat(third.String(), "Group 120363000000000001", time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := GetChatName(client, fresh, third, third.String(), nil, "", true, logger); got != "From Network" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestHandleHistorySync_NeverFetchesGroupInfo(t *testing.T) {
	client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
	ms := newTestMessageStore(t)
	ms.names = newChatNameCache()
	ms.groupInfo = func(_ context.Context, _ types.JID) (*types.GroupInfo, error) {
		t.Fatal("GetGroupInfo called during history sync")
		return nil, nil
	}
	logger := testLogger()

	var conversations []*waHistorySync.Conversation
	for i := 1; i <= 3; i++ {
		jid := groupJID("12036300000000000" + string(rune('0'+i)))
		conversations = append(conversations, &waHistorySync.Conversation{
			ID: proto.String(jid.String()), // no Name on purpose
			Messages: []*waHistorySync.HistorySyncMsg{{
				Message: &waWeb.WebMessageInfo{
					Key:              &waCommon.MessageKey{ID: proto.String("hist-" + jid.User), FromMe: proto.Bool(false), RemoteJID: proto.String(jid.String())},
					Participant:      proto.String("5511888888888@s.whatsapp.net"),
					MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())), //nolint:gosec // test fixture
					Message:          &waE2E.Message{Conversation: proto.String("hello")},
				},
			}},
		})
	}
	testBridge(t, client, ms, logger).handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: conversations,
	}})

	var n int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM chats WHERE name LIKE 'Group %'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("expected 3 placeholder-named groups, got %d (err %v)", n, err)
	}
}

// newSelfClient is a session paired as selfPhone that knows its own LID.
func newSelfClient() *whatsmeow.Client {
	client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
	client.Store.LID = selfLID
	return client
}

func storedChatName(t *testing.T, ms *MessageStore, chatJID string) string {
	t.Helper()
	var name string
	if err := ms.db.QueryRow("SELECT COALESCE(name, '') FROM chats WHERE jid = ?", chatJID).Scan(&name); err != nil {
		t.Fatalf("chat %s: %v", chatJID, err)
	}
	return name
}

// A chat we start with a number that is not in the contact store used to be
// named after the sender of that first message, i.e. after ourselves, and the
// name then read as a real one (issue #448).
func TestGetChatName_NeverNamesADirectChatAfterOurOwnNumber(t *testing.T) {
	const stranger = "5511777777777"
	// A session paired before LIDs: Store.LID is empty, the LID map has it.
	legacy := newTestClientWithSelf(&mockLIDStore{lidByPN: map[types.JID]types.JID{selfPhone: selfLID}}, selfPhone)
	group := groupJID("120363000000000009")
	list := types.JID{User: "1700000000", Server: types.BroadcastServer}
	cases := []struct {
		name   string
		client *whatsmeow.Client
		chat   types.JID
		stored string // chats.name before the call, "" = no row
		sender string
		want   string
	}{
		{name: "outgoing, our phone as sender", chat: phonePN, sender: selfPhone.User, want: phonePN.User},
		{name: "outgoing, our LID as sender", chat: phonePN, sender: selfLID.User, want: phonePN.User},
		{name: "outgoing, our LID known only to the LID map", client: legacy, chat: phonePN, sender: selfLID.User, want: phonePN.User},
		{name: "outgoing to a chat still in the LID namespace", chat: phoneLID, sender: selfLID.User, want: phoneLID.User},
		{name: "incoming from the peer", chat: phonePN, sender: phonePN.User, want: phonePN.User},
		{name: "incoming from a different sender keeps the sender fallback", chat: phonePN, sender: stranger, want: stranger},
		{name: "stored under our phone is a placeholder", chat: phonePN, stored: selfPhone.User, sender: selfPhone.User, want: phonePN.User},
		{name: "stored under our LID is a placeholder", chat: phonePN, stored: selfLID.User, sender: selfPhone.User, want: phonePN.User},
		{name: "stored under our phone heals on an incoming message", chat: phonePN, stored: selfPhone.User, sender: stranger, want: stranger},
		{name: "a real name is kept", chat: phonePN, stored: "Alice", sender: selfPhone.User, want: "Alice"},
		{name: "the self chat keeps our number", chat: selfPhone, stored: selfPhone.User, sender: selfPhone.User, want: selfPhone.User},
		{name: "the self chat without a row", chat: selfPhone, sender: selfPhone.User, want: selfPhone.User},
		{name: "the self chat in the LID namespace keeps its name", chat: selfLID, stored: selfPhone.User, sender: selfLID.User, want: selfPhone.User},
		{name: "the self chat in the LID namespace without a row", chat: selfLID, sender: selfPhone.User, want: selfPhone.User},
		{name: "a broadcast list we post to", chat: list, sender: selfPhone.User, want: list.User},
		{name: "a broadcast list stored under our number", chat: list, stored: selfPhone.User, sender: selfPhone.User, want: list.User},
		{name: "a group named like our number is left alone", chat: group, stored: selfPhone.User, sender: selfPhone.User, want: selfPhone.User},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client
			if client == nil {
				client = newSelfClient()
			}
			ms := newTestMessageStore(t)
			if tc.stored != "" {
				if err := ms.StoreChat(tc.chat.String(), tc.stored, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if got := GetChatName(client, ms, tc.chat, tc.chat.String(), nil, tc.sender, false, testLogger()); got != tc.want {
				t.Fatalf("GetChatName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The whole path: the first message of a chat is ours, the peer is unknown,
// and the reply carries a push name. Both addressing modes WhatsApp uses.
func TestHandleMessage_AChatWeStartIsNamedAfterThePeerThenHeals(t *testing.T) {
	cases := []struct {
		name     string
		outgoing *events.Message
		incoming *events.Message
	}{
		{
			name:     "phone addressed",
			outgoing: buildTextMessage(phonePN, selfPhone, types.EmptyJID, types.EmptyJID, true, "hi"),
			incoming: buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "hello"),
		},
		{
			name:     "LID addressed",
			outgoing: buildTextMessage(phoneLID, selfLID, types.EmptyJID, phonePN, true, "hi"),
			incoming: buildTextMessage(phoneLID, phoneLID, phonePN, types.EmptyJID, false, "hello"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_ENABLED", "false")
			ms := newTestMessageStore(t)
			b := testBridge(t, newSelfClient(), ms, testLogger())

			tc.outgoing.Info.ID = "OUT1"
			b.handleMessage(tc.outgoing)
			if got := storedChatName(t, ms, phonePN.String()); got != phonePN.User {
				t.Fatalf("after our first message the chat is named %q, want the peer's number %q", got, phonePN.User)
			}

			tc.incoming.Info.ID = "IN1"
			tc.incoming.Info.PushName = "Alice"
			b.handleMessage(tc.incoming)
			if got := storedChatName(t, ms, phonePN.String()); got != "Alice" {
				t.Fatalf("after the reply the chat is named %q, want the push name", got)
			}
		})
	}
}

func TestResetSelfNamedChats(t *testing.T) {
	self := selfUsers{phone: selfPhone.User, lid: selfLID.User}
	group := groupJID("120363000000000009")
	const (
		peerA = "5511911111111@s.whatsapp.net"
		peerB = "5511922222222@s.whatsapp.net"
		peerC = "100000000000006@lid"
		list  = "1700000000@broadcast"
		carol = "5511933333333@s.whatsapp.net"
	)
	seed := map[string]string{
		peerA:              selfPhone.User, // named after our phone
		peerB:              selfLID.User,   // named after our LID
		peerC:              selfPhone.User, // a chat still in the LID namespace
		list:               selfPhone.User, // a broadcast list we posted to
		carol:              "Carol",
		selfPhone.String(): selfPhone.User, // the chat with ourselves
		selfLID.String():   selfPhone.User, // ... and its LID form
		group.String():     selfPhone.User, // groups are never looked at
	}
	want := map[string]string{
		peerA:              "5511911111111",
		peerB:              "5511922222222",
		peerC:              "100000000000006",
		list:               "1700000000",
		carol:              "Carol",
		selfPhone.String(): selfPhone.User,
		selfLID.String():   selfPhone.User,
		group.String():     selfPhone.User,
	}
	newSeeded := func(t *testing.T) *MessageStore {
		t.Helper()
		ms := newTestMessageStore(t)
		for jid, name := range seed {
			if err := ms.StoreChat(jid, name, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		return ms
	}
	check := func(t *testing.T, ms *MessageStore, want map[string]string) {
		t.Helper()
		for jid, name := range want {
			if got := storedChatName(t, ms, jid); got != name {
				t.Errorf("%s is named %q, want %q", jid, got, name)
			}
		}
	}

	t.Run("resets once, then finds nothing", func(t *testing.T) {
		ms := newSeeded(t)
		for run, wantRenamed := range []int64{4, 0} {
			renamed, err := ms.ResetSelfNamedChats(self)
			if err != nil || renamed != wantRenamed {
				t.Fatalf("run %d renamed %d chats (err %v), want %d", run+1, renamed, err, wantRenamed)
			}
			check(t, ms, want)
		}
	})

	t.Run("a session without a LID still resets the phone-named chats", func(t *testing.T) {
		ms := newSeeded(t)
		if _, err := ms.ResetSelfNamedChats(selfUsers{phone: selfPhone.User}); err != nil {
			t.Fatal(err)
		}
		if got := storedChatName(t, ms, peerA); got != "5511911111111" {
			t.Errorf("%s is named %q", peerA, got)
		}
		if got := storedChatName(t, ms, peerB); got != selfLID.User {
			t.Errorf("%s was renamed to %q without knowing our LID", peerB, got)
		}
		if got := storedChatName(t, ms, selfPhone.String()); got != selfPhone.User {
			t.Errorf("the self chat was renamed to %q", got)
		}
	})

	t.Run("an unpaired bridge touches nothing", func(t *testing.T) {
		ms := newSeeded(t)
		if renamed, err := ms.ResetSelfNamedChats(selfUsers{}); err != nil || renamed != 0 {
			t.Fatalf("renamed %d chats, err %v", renamed, err)
		}
		check(t, ms, seed)
	})

	// What the reset leaves behind is a placeholder: once the contact store
	// knows the peer, the next resolution takes that name instead of trusting
	// the number.
	t.Run("the reset name is replaced by the contact name", func(t *testing.T) {
		ms := newSeeded(t)
		if _, err := ms.ResetSelfNamedChats(self); err != nil {
			t.Fatal(err)
		}
		waDB, err := sql.Open("sqlite", testMemoryDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = waDB.Close() })
		ms.waDB = waDB
		if _, err := waDB.Exec(`
			CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
			INSERT INTO whatsmeow_contacts VALUES (?, ?, '', 'Peer A', '', '');
		`, selfPhone.String(), peerA); err != nil {
			t.Fatal(err)
		}
		peer, _ := types.ParseJID(peerA)
		if got := GetChatName(newSelfClient(), ms, peer, peerA, nil, selfPhone.User, false, testLogger()); got != "Peer A" {
			t.Fatalf("GetChatName() = %q, want the contact name to replace the placeholder", got)
		}
	})
}
