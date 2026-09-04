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
	b := testBridge(newTestClient(&mockLIDStore{}), ms, testLogger())
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
	prev := historyVoteRetryDelays
	historyVoteRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { historyVoteRetryDelays = prev })

	ms := newTestMessageStore(t)
	b := testBridge(newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
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
	prev := historyVoteRetryDelays
	historyVoteRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { historyVoteRetryDelays = prev })

	ms := newTestMessageStore(t)
	b := testBridge(newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
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
