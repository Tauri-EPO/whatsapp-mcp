package main

import (
	"context"
	"errors"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
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
		conv := &waProto.Conversation{ID: proto.String(jid.String()), Name: proto.String("From Sync")}
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
		if got := GetChatName(client, ms, other, other.String(), &waProto.Conversation{}, "", false, logger); got != "Group 120363000000000002" {
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
		ms.names.groupErr[other.String()] = time.Now().Add(-groupInfoRetryAfter - time.Second)
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

	var conversations []*waProto.Conversation
	for i := 1; i <= 3; i++ {
		jid := groupJID("12036300000000000" + string(rune('0'+i)))
		conversations = append(conversations, &waProto.Conversation{
			ID: proto.String(jid.String()), // no Name on purpose
			Messages: []*waProto.HistorySyncMsg{{
				Message: &waProto.WebMessageInfo{
					Key:              &waCommon.MessageKey{ID: proto.String("hist-" + jid.User), FromMe: proto.Bool(false), RemoteJID: proto.String(jid.String())},
					Participant:      proto.String("5511888888888@s.whatsapp.net"),
					MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())),
					Message:          &waProto.Message{Conversation: proto.String("hello")},
				},
			}},
		})
	}
	handleHistorySync(client, ms, &events.HistorySync{Data: &waProto.HistorySync{
		SyncType: waProto.HistorySync_RECENT.Enum(), Conversations: conversations,
	}}, logger)

	var n int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM chats WHERE name LIKE 'Group %'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("expected 3 placeholder-named groups, got %d (err %v)", n, err)
	}
}
