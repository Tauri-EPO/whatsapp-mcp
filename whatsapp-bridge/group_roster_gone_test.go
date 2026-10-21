package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// goneGroupBridge archives testGroupJID and answers every roster fetch with
// answer, counting the fetches.
func goneGroupBridge(t *testing.T, answer error) (*Bridge, *recordingLogger, *int) {
	t.Helper()
	store := newTestMessageStore(t)
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
		testGroupJID, "Obra", dbTime(time.Now())); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	fetches := 0
	store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		fetches++
		return nil, answer
	}
	logger := installRecordingLogger(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), store, logger)
	b.GroupRosterSync = groupRosterSyncInterval
	b.Connected = func() bool { return true }
	b.rosterFailures = newRosterFailures()
	return b, logger, &fetches
}

// A group this account left, or one that was deleted, keeps its chat row and
// so is stale forever. WhatsApp's answer does not change by itself: it is
// asked again after a week, not every six hours, and it is not a warning.
func TestGroupsWeAreNotInAreAskedWeekly(t *testing.T) {
	cases := []struct {
		answer     error
		retryAfter time.Duration
		warns      bool
	}{
		{whatsmeow.ErrNotInGroup, groupRosterGoneRetryAfter, false},
		{fmt.Errorf("get group info: %w", whatsmeow.ErrGroupNotFound), groupRosterGoneRetryAfter, false},
		{errors.New("websocket closed"), groupRosterRetryAfter, true},
	}
	for _, tc := range cases {
		t.Run(tc.answer.Error(), func(t *testing.T) {
			b, logger, fetches := goneGroupBridge(t, tc.answer)
			now := time.Now()
			b.syncGroupRosters(context.Background(), now, 10, 0)
			b.syncGroupRosters(context.Background(), now.Add(tc.retryAfter-time.Minute), 10, 0)
			if *fetches != 1 {
				t.Fatalf("asked %d times before the backoff ended, want once", *fetches)
			}
			b.syncGroupRosters(context.Background(), now.Add(tc.retryAfter+time.Minute), 10, 0)
			if *fetches != 2 {
				t.Fatalf("not asked again after the backoff: fetches=%d", *fetches)
			}
			if warned := strings.Contains(logger.String(), "Group roster sync: failed"); warned != tc.warns {
				t.Fatalf("warned=%v, want %v:\n%s", warned, tc.warns, logger.String())
			}
		})
	}
}

// Being added back makes the roster readable again at once.
func TestRejoiningAGroupEndsItsBackoff(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	rejoin := map[string]func(*Bridge){
		"JoinedGroup": func(b *Bridge) {
			b.handleEvent(&events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group}}, nil)
		},
		"GroupInfo join of this account": func(b *Bridge) {
			b.applyGroupParticipantChanges(&events.GroupInfo{JID: group, Join: []types.JID{selfPhone}})
		},
	}
	for name, event := range rejoin {
		t.Run(name, func(t *testing.T) {
			b, _, fetches := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
			now := time.Now()
			b.syncGroupRosters(context.Background(), now, 10, 0)
			event(b)
			b.syncGroupRosters(context.Background(), now.Add(time.Minute), 10, 0)
			if *fetches != 2 {
				t.Fatalf("the rejoined group was not asked again: fetches=%d", *fetches)
			}
		})
	}
	t.Run("somebody else joining", func(t *testing.T) {
		b, _, fetches := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
		now := time.Now()
		b.syncGroupRosters(context.Background(), now, 10, 0)
		other := types.NewJID("5511999999999", types.DefaultUserServer)
		b.applyGroupParticipantChanges(&events.GroupInfo{JID: group, Join: []types.JID{other}})
		b.syncGroupRosters(context.Background(), now.Add(time.Minute), 10, 0)
		if *fetches != 1 {
			t.Fatalf("another member's join ended our backoff: fetches=%d", *fetches)
		}
	})
}
