package main

import (
	"context"
	"errors"
	"sync/atomic"
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

func TestUndecodableVotesAreCountedNotTallied(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "120363000000000001@g.us"
	if err := ms.StorePoll("POLL1", chat, &pollCreation{Question: "Q", Options: []string{"A", "B"}, SelectableCount: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := ms.StorePollVote("POLL1", chat, "111", []string{"A"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreUndecodablePollVote("POLL1", chat, "222", t0); err != nil {
		t.Fatal(err)
	}
	res, err := ms.PollResults("POLL1", chat)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalVoters != 1 || res.UndecodableVotes != 1 || res.Options[0].Count != 1 || len(res.Votes) != 1 {
		t.Fatalf("results = %+v", res)
	}

	// A later decodable vote from the same voter replaces the undecodable one.
	if err := ms.StorePollVote("POLL1", chat, "222", []string{"B"}, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	res, _ = ms.PollResults("POLL1", chat)
	if res.TotalVoters != 2 || res.UndecodableVotes != 0 || res.Options[1].Count != 1 {
		t.Fatalf("results after re-vote = %+v", res)
	}
}

func TestHandleMessage_UndecryptableVoteIsRecorded(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) {
		return nil, whatsmeow.ErrOriginalMessageSecretNotFound
	}
	vote := buildImageMessage(phonePN, phonePN, false, "")
	vote.Info.ID = "VOTE-X"
	vote.Message = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
		PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("OLD-POLL"), RemoteJID: proto.String(phonePN.String())},
		Vote:                   &waE2E.PollEncValue{EncPayload: []byte("x"), EncIV: []byte("y")},
	}}
	b.handleMessage(vote)

	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'VOTE-X'`).Scan(&n)
	if n != 0 {
		t.Fatal("undecodable vote must not produce a message row")
	}
	var undecodable int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM poll_votes WHERE poll_message_id = 'OLD-POLL' AND selected_json IS NULL`).Scan(&undecodable)
	if undecodable != 1 {
		t.Fatalf("undecodable poll_votes rows = %d, want 1", undecodable)
	}
}

// historySyncWithPoll builds a RECENT sync for a DM: newest-first, a vote
// from the contact followed by the poll creation from us.
func historySyncWithPoll(chat types.JID, now time.Time) *events.HistorySync {
	return &events.HistorySync{
		Data: &waHistorySync.HistorySync{
			SyncType: waHistorySync.HistorySync_RECENT.Enum(),
			Conversations: []*waHistorySync.Conversation{{
				ID:   proto.String(chat.String()),
				Name: proto.String("Contact"),
				Messages: []*waHistorySync.HistorySyncMsg{
					{Message: &waWeb.WebMessageInfo{
						Key:              &waCommon.MessageKey{ID: proto.String("HVOTE1"), FromMe: proto.Bool(false), RemoteJID: proto.String(chat.String())},
						MessageTimestamp: proto.Uint64(uint64(now.Unix())), //nolint:gosec // test fixture
						Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
							PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("HPOLL1"), FromMe: proto.Bool(true), RemoteJID: proto.String(chat.String())},
							Vote:                   &waE2E.PollEncValue{EncPayload: []byte("x"), EncIV: []byte("y")},
						}},
					}},
					{Message: &waWeb.WebMessageInfo{
						Key:              &waCommon.MessageKey{ID: proto.String("HPOLL1"), FromMe: proto.Bool(true), RemoteJID: proto.String(chat.String())},
						MessageTimestamp: proto.Uint64(uint64(now.Add(-time.Minute).Unix())), //nolint:gosec // test fixture
						Message:          pollCreationMsg("Jantar?", "Pizza", "Sushi"),
					}},
				},
			}},
		},
	}
}

func TestHandleHistorySync_StoresPollAndDecodesVoteAfterRetry(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	// Two retries, both immediate (the decrypter below only needs the first):
	// the decode runs in b.historyVotes, so the pacing belongs to this bridge
	// and not to a shared slice (issue #382).
	b.HistoryVoteRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	var calls atomic.Int32
	b.PollVoteDecrypt = func(_ context.Context, evt *events.Message) ([][]byte, error) {
		// First attempt: whatsmeow has not written the secret yet.
		if calls.Add(1) == 1 {
			return nil, whatsmeow.ErrOriginalMessageSecretNotFound
		}
		if evt.Info.ID != "HVOTE1" || evt.Info.Chat != phonePN.ToNonAD() {
			return nil, errors.New("unexpected event")
		}
		return [][]byte{hashOf("Sushi")}, nil
	}

	b.handleHistorySync(historySyncWithPoll(phonePN, time.Now()))
	b.historyVotes.Wait()

	// The poll row exists (history creations used to skip StorePoll).
	if p, err := ms.GetPoll("HPOLL1", phonePN.String()); err != nil || len(p.Options) != 2 {
		t.Fatalf("history poll not stored: %v %+v", err, p)
	}
	if calls.Load() != 2 {
		t.Fatalf("decrypt attempts = %d, want 2 (one retry)", calls.Load())
	}
	res, err := ms.PollResults("HPOLL1", phonePN.String())
	if err != nil || res.TotalVoters != 1 || res.UndecodableVotes != 0 || res.Options[1].Count != 1 {
		t.Fatalf("results = %+v (err %v)", res, err)
	}
	var content, target string
	if err := ms.db.QueryRow(`SELECT content, target_message_id FROM messages WHERE id = 'HVOTE1'`).Scan(&content, &target); err != nil {
		t.Fatalf("vote row missing: %v", err)
	}
	if content != "🗳️ voted: Sushi" || target != "HPOLL1" {
		t.Fatalf("vote row = %q -> %q", content, target)
	}
}

func TestHandleHistorySync_VoteWithoutSecretIsUndecodable(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	b.HistoryVoteRetryDelays = []time.Duration{time.Millisecond}
	b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) {
		return nil, whatsmeow.ErrOriginalMessageSecretNotFound
	}

	b.handleHistorySync(historySyncWithPoll(phonePN, time.Now()))
	b.historyVotes.Wait()

	res, err := ms.PollResults("HPOLL1", phonePN.String())
	if err != nil || res.TotalVoters != 0 || res.UndecodableVotes != 1 {
		t.Fatalf("results = %+v (err %v)", res, err)
	}
	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'HVOTE1'`).Scan(&n)
	if n != 0 {
		t.Fatal("undecodable history vote must not produce a message row")
	}
}

func TestHistoryPollVoteMarkersFollowCommittedRows(t *testing.T) {
	for _, policy := range []string{"read", "unread", "sparse", "undecodable", "collision"} {
		t.Run(policy, func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.HistoryVoteRetryDelays = nil
			b.PollVoteDecrypt = func(context.Context, *events.Message) ([][]byte, error) {
				if policy == "undecodable" {
					return nil, whatsmeow.ErrOriginalMessageSecretNotFound
				}
				return [][]byte{hashOf("Sushi")}, nil
			}
			stamp := time.Unix(1700000100, 0)
			fixture := historySyncWithPoll(phonePN, stamp)
			conv := fixture.Data.Conversations[0]
			if policy != "sparse" {
				conv.UnreadCount = proto.Uint32(0)
			}
			if policy == "unread" {
				conv.MarkedAsUnread = proto.Bool(true)
			}
			if err := ms.StoreChat(phonePN.String(), "Contact", stamp.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := ms.MarkChatRead(phonePN.String(), stamp.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if policy == "collision" {
				if err := persistMessage(ms, "HVOTE1", phonePN.String(), selfPhone.String(), stamp.Add(-time.Minute), true, extractMessage(livePosition(0, 0.1), stamp, "HVOTE1"), false, b.Log); err != nil {
					t.Fatal(err)
				}
			}
			b.handleHistorySync(fixture)
			b.historyVotes.Wait()
			var activity, read string
			if err := ms.db.QueryRow("SELECT CAST(last_message_time AS TEXT), CAST(last_read_time AS TEXT) FROM chats WHERE jid=?", phonePN.String()).Scan(&activity, &read); err != nil {
				t.Fatal(err)
			}
			wantActivity, wantRead := stamp, stamp.Add(-time.Minute)
			if policy == "undecodable" || policy == "collision" {
				wantActivity = wantRead
			}
			if policy == "read" {
				wantRead = stamp
			}
			var unread int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages m JOIN chats c ON m.chat_jid=c.jid WHERE m.id='HVOTE1' AND m.is_from_me=0 AND m.timestamp>c.last_read_time").Scan(&unread); err != nil {
				t.Fatal(err)
			}
			wantUnread := 0
			if policy == "unread" || policy == "sparse" {
				wantUnread = 1
			}
			if activity != dbTime(wantActivity) || read != dbTime(wantRead) || unread != wantUnread {
				t.Fatalf("activity=%s read=%s unread=%d; want %s %s %d", activity, read, unread, dbTime(wantActivity), dbTime(wantRead), wantUnread)
			}
		})
	}
}

func TestHistoryPollVoteMarkerFailureRollsBackMessage(t *testing.T) {
	ms := newTestMessageStore(t)
	stamp := time.Unix(1700000100, 0)
	chat := phonePN.String()
	if err := ms.StoreChat(chat, "Contact", stamp.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkChatRead(chat, stamp.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec("CREATE TRIGGER reject_vote_marker BEFORE UPDATE OF last_read_time ON chats BEGIN SELECT RAISE(ABORT, 'marker refused'); END"); err != nil {
		t.Fatal(err)
	}
	_, err := ms.storePollVoteMessageResult("HVOTE1", chat, phonePN.String(), "vote", stamp, false, "HPOLL1", testLogger(), historyVoteMarkers{name: "Contact", read: true})
	if err == nil {
		t.Fatal("marker failure was ignored")
	}
	var rows int
	var activity, read string
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='HVOTE1'").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT CAST(last_message_time AS TEXT), CAST(last_read_time AS TEXT) FROM chats WHERE jid=?", chat).Scan(&activity, &read); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || activity != dbTime(stamp.Add(-time.Minute)) || read != activity {
		t.Fatalf("failed marker left partial writes: rows=%d activity=%s read=%s", rows, activity, read)
	}
	if _, err := ms.db.Exec("DROP TRIGGER reject_vote_marker"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.storePollVoteMessageResult("HVOTE1", chat, phonePN.String(), "vote", stamp, false, "HPOLL1", testLogger(), historyVoteMarkers{name: "Contact", read: true}); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT CAST(last_message_time AS TEXT), CAST(last_read_time AS TEXT) FROM chats WHERE jid=?", chat).Scan(&activity, &read); err != nil || activity != dbTime(stamp) || read != activity {
		t.Fatalf("replay failed: activity=%s read=%s err=%v", activity, read, err)
	}
}
