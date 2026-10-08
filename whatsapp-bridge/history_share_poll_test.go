package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestPeerHistoryCannotReplacePollVotes(t *testing.T) {
	for _, decodable := range []bool{false, true} {
		for _, liveShare := range []bool{false, true} {
			t.Run(map[bool]string{false: "undecryptable", true: "decodable"}[decodable]+"/"+map[bool]string{false: "phone delivered share", true: "live share"}[liveShare], func(t *testing.T) {
				ms := newTestMessageStore(t)
				b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
				b.MediaAutoDownload = false
				b.HistoryVoteRetryDelays = nil
				chat := types.NewJID("120363000000000001", types.GroupServer)
				stamp := time.Unix(1700000000, 0)
				if err := ms.StorePoll("HPOLL1", chat.String(), &pollCreation{Question: "Q", Options: []string{"Pizza", "Sushi"}, SelectableCount: 1}, stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StorePollVote("HPOLL1", chat.String(), phonePN.User, []string{"Pizza"}, stamp); err != nil {
					t.Fatal(err)
				}
				var before, beforeTime string
				if err := ms.db.QueryRow("SELECT selected_json,CAST(voted_at AS TEXT) FROM poll_votes WHERE poll_message_id='HPOLL1' AND chat_jid=? AND voter=?", chat.String(), phonePN.User).Scan(&before, &beforeTime); err != nil {
					t.Fatal(err)
				}
				calls := 0
				b.PollVoteDecrypt = func(context.Context, *events.Message) ([][]byte, error) {
					calls++
					if decodable {
						return [][]byte{hashOf("Sushi")}, nil
					}
					return nil, whatsmeow.ErrOriginalMessageSecretNotFound
				}
				fixture := historySyncWithPoll(chat, stamp.Add(time.Minute))
				fixture.Data.Conversations[0].Messages = fixture.Data.Conversations[0].Messages[:1]
				fixture.Data.Conversations[0].Messages[0].Message.Participant = proto.String(phonePN.String())
				plain, err := proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
				if liveShare {
					event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Message.MessageHistoryBundle = bundle
					b.handleMessage(event)
				} else {
					outer := shareHistoryFixture(1)
					outer.Data.Conversations[0].Messages[0].Message.Message.MessageHistoryBundle = bundle
					outer.Data.Conversations[0].Messages[0].Message.Message.Conversation = nil
					b.handleHistorySync(outer)
				}
				waitHistoryShares(t, b)
				b.historyVotes.Wait()
				var after, afterTime string
				if err := ms.db.QueryRow("SELECT selected_json,CAST(voted_at AS TEXT) FROM poll_votes WHERE poll_message_id='HPOLL1' AND chat_jid=? AND voter=?", chat.String(), phonePN.User).Scan(&after, &afterTime); err != nil {
					t.Fatal(err)
				}
				var votes, messages int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM poll_votes").Scan(&votes); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages); err != nil {
					t.Fatal(err)
				}
				if calls != 0 || votes != 1 || messages != 0 || before != after || beforeTime != afterTime || requests.Load() != 1 {
					t.Fatalf("peer vote changed archive: decrypt=%d votes=%d messages=%d selected=%q -> %q time=%q -> %q requests=%d", calls, votes, messages, before, after, beforeTime, afterTime, requests.Load())
				}
				// The same update delivered by our own phone still follows its
				// authenticated history path, including undecodable vote storage.
				b.handleHistorySync(fixture)
				b.historyVotes.Wait()
				if calls != 1 {
					t.Fatalf("own-phone vote path lost: decrypt=%d", calls)
				}
				var selected any
				if err := ms.db.QueryRow("SELECT selected_json FROM poll_votes WHERE poll_message_id='HPOLL1' AND chat_jid=? AND voter=?", chat.String(), phonePN.User).Scan(&selected); err != nil {
					t.Fatal(err)
				}
				if decodable && selected != `["Sushi"]` || !decodable && selected != nil {
					t.Fatalf("own-phone control selected=%v", selected)
				}
			})
		}
	}
}
