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

// The answer to a fetch sent before the account was seen back in the group
// must not start the long wait, and expired entries are dropped.
func TestRosterFailuresOrderAndPruning(t *testing.T) {
	now := time.Now()
	f := newRosterFailures()
	asked := now
	f.forget(testGroupJID, now.Add(time.Second)) // the rejoin lands while the fetch is in flight
	f.noteGone(testGroupJID, asked, now.Add(2*time.Second))
	if f.recent(testGroupJID, now.Add(time.Minute)) {
		t.Fatal("a stale not-in-group answer put a rejoined group back in the long wait")
	}
	f.noteGone(testGroupJID, now.Add(time.Hour), now.Add(time.Hour))
	if !f.recent(testGroupJID, now.Add(2*time.Hour)) {
		t.Fatal("a later answer must still start the wait")
	}
	if f.recent(testGroupJID, now.Add(time.Hour+groupRosterGoneRetryAfter)) || len(f.waits) != 0 {
		t.Fatalf("an expired wait was kept: %v", f.waits)
	}
}

func TestLeavingStartsTheLongWaitAndOwnMessagesEndIt(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	b, _, fetches := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
	left := time.Now()
	b.applyGroupParticipantChanges(&events.GroupInfo{JID: group, Leave: []types.JID{selfPhone}})
	b.syncGroupRosters(context.Background(), left.Add(groupRosterRetryAfter+time.Minute), 10, 0)
	if *fetches != 0 {
		t.Fatalf("a group we saw ourselves leave was asked about after six hours: fetches=%d", *fetches)
	}

	// A message of ours queued before the departure does not reopen it...
	b.noteGroupSender(b.Policy, testGroupJID, selfPhone, left.Add(-time.Minute))
	if !b.rosterFailures.recent(testGroupJID, left.Add(time.Hour)) {
		t.Fatal("an older message of ours ended the wait")
	}
	// ...one written after it does: we are in the group again.
	b.noteGroupSender(b.Policy, testGroupJID, selfPhone, left.Add(time.Hour))
	if b.rosterFailures.recent(testGroupJID, left.Add(time.Hour)) {
		t.Fatal("a newer message of ours left the group in the wait")
	}
}

// Any roster WhatsApp hands back, list_group_members included, proves the
// account is in the group.
func TestAStoredRosterEndsTheWait(t *testing.T) {
	b, _, _ := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
	now := time.Now()
	b.syncGroupRosters(context.Background(), now, 10, 0)
	if !b.rosterFailures.recent(testGroupJID, now.Add(time.Minute)) {
		t.Fatal("setup: the group is not waiting")
	}
	b.recordGroupRoster(testGroupJID, []GroupMember{{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999"}}, time.Now(), nil)
	if b.rosterFailures.recent(testGroupJID, now.Add(time.Minute)) {
		t.Fatal("a stored roster left the group in the wait")
	}
}

// A rejoin seen while the allow-list excludes the group still ends the wait,
// so allowing it later does not inherit a week of silence.
func TestRejoinWhileExcludedEndsTheWait(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	b, _, _ := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
	now := time.Now()
	b.syncGroupRosters(context.Background(), now, 10, 0)
	excluded := parseChatPolicy("5511999999999@s.whatsapp.net")
	b.applyGroupParticipantChangesWithPolicy(excluded, &events.GroupInfo{JID: group, Join: []types.JID{selfPhone}})
	if b.rosterFailures.recent(testGroupJID, now.Add(time.Minute)) {
		t.Fatal("an excluded group kept its wait after we were added back")
	}
}

// JoinedGroup fetches the roster right away instead of at the next pass.
func TestJoinedGroupFetchesTheRosterNow(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	b, _, _ := goneGroupBridge(t, whatsmeow.ErrNotInGroup)
	fetched := make(chan struct{}, 4)
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		fetched <- struct{}{}
		return fakeGroup(), nil
	}
	b.handleEvent(&events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group}}, nil)
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("the rejoined group's roster was not fetched")
	}
}
