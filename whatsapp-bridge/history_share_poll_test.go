package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestPeerHistoryPreservesLocallyDeletedPollMetadata(t *testing.T) {
	for _, route := range []string{"live", "history"} {
		for _, originalRow := range []bool{false, true} {
			t.Run(route+"/originalRow="+map[bool]string{false: "metadata only", true: "locally deleted"}[originalRow], func(t *testing.T) {
				ms := newTestMessageStore(t)
				b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
				const chat = "120363000000000001@g.us"
				stamp := time.Unix(1700000000, 0)
				if err := ms.StoreChat(chat, "Group", stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StorePoll("HPOLL1", chat, &pollCreation{Question: "Original", Options: []string{"Pizza", "Sushi"}, SelectableCount: 1}, stamp); err != nil {
					t.Fatal(err)
				}
				if err := ms.StorePollVote("HPOLL1", chat, phonePN.User, []string{"Pizza"}, stamp); err != nil {
					t.Fatal(err)
				}
				if originalRow {
					if err := ms.StoreMessage(storedMessage{
						ID:        "HPOLL1",
						ChatJID:   chat,
						Sender:    selfPhone.String(),
						Content:   "Original",
						Timestamp: stamp,
						IsFromMe:  true,
						MediaType: "poll",
					}); err != nil {
						t.Fatal(err)
					}
					if err := ms.DeleteMessageRow("HPOLL1", chat); err != nil {
						t.Fatal(err)
					}
				}
				pollSnapshot := func() string {
					var snapshot string
					if err := ms.db.QueryRow("SELECT json_array(question,options_json,selectable_count,CAST(created_at AS TEXT)) FROM polls WHERE message_id='HPOLL1' AND chat_jid=?", chat).Scan(&snapshot); err != nil {
						t.Fatal(err)
					}
					return snapshot
				}
				before := pollSnapshot()
				fixture := shareHistoryFixture(2)
				for i, row := range fixture.Data.Conversations[0].Messages {
					row.Message.Key.ID = proto.String([]string{"HPOLL1", "HPOLL2"}[i])
					row.Message.Key.FromMe = proto.Bool(false)
					row.Message.Participant = proto.String(phonePN.String())
					row.Message.Message = pollCreationMsg("Replacement", "Other", "Sushi")
				}
				plain, err := proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
				if route == "live" {
					b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, chat, "SHARE", false)
				} else {
					outer := shareHistoryFixture(1)
					outer.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{MessageHistoryBundle: bundle}
					b.handleHistorySync(outer)
				}
				waitHistoryShares(t, b)
				result, err := ms.PollResults("HPOLL1", chat)
				var oldRows, freshRows int
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='HPOLL1' AND chat_jid=?", chat).Scan(&oldRows); err != nil {
					t.Fatal(err)
				}
				if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='HPOLL2' AND chat_jid=?", chat).Scan(&freshRows); err != nil {
					t.Fatal(err)
				}
				if pollSnapshot() != before || err != nil || result.TotalVoters != 1 || result.Options[0].Name != "Pizza" || result.Options[0].Count != 1 || oldRows != 0 || freshRows != 1 || requests.Load() != 1 {
					t.Fatalf("retained poll changed or fresh poll lost: before=%s after=%s results=%+v err=%v rows=%d/%d HTTP=%d", before, pollSnapshot(), result, err, oldRows, freshRows, requests.Load())
				}
				b.handleHistorySync(fixture)
				b.historyVotes.Wait()
				if p, err := ms.GetPoll("HPOLL1", chat); err != nil || p.Question != "Replacement" {
					t.Fatalf("authenticated own-phone replay lost: %+v %v", p, err)
				}
			})
		}
	}
}

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
