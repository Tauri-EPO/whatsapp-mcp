package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func pollCreationMsg(question string, options ...string) *waProto.Message {
	pc := &waProto.PollCreationMessage{Name: proto.String(question), SelectableOptionsCount: proto.Uint32(1)}
	for _, o := range options {
		pc.Options = append(pc.Options, &waProto.PollCreationMessage_Option{OptionName: proto.String(o)})
	}
	return &waProto.Message{PollCreationMessageV3: pc}
}

func hashOf(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestExtractPollCreationAndContent(t *testing.T) {
	if extractPollCreation(&waProto.Message{Conversation: proto.String("hi")}) != nil {
		t.Fatal("text message is not a poll")
	}
	p := extractPollCreation(pollCreationMsg("Almoço?", "Pizza", " Sushi ", ""))
	if p == nil || p.Question != "Almoço?" || len(p.Options) != 2 || p.Options[1] != "Sushi" || p.SelectableCount != 1 {
		t.Fatalf("unexpected poll %+v", p)
	}
	if got := pollContent(p); got != "📊 Almoço? — options: Pizza | Sushi" {
		t.Fatalf("content = %q", got)
	}
}

func TestOptionNamesForHashes(t *testing.T) {
	names := optionNamesForHashes([]string{"Pizza", "Sushi"}, [][]byte{hashOf("Sushi"), hashOf("Nope"), hashOf("Pizza")})
	if len(names) != 3 || names[0] != "Sushi" || names[2] != "Pizza" || names[1][0] != '?' {
		t.Fatalf("names = %v", names)
	}
	if got := optionNamesForHashes(nil, nil); len(got) != 0 {
		t.Fatal("no hashes -> no names")
	}
}

func TestPollStoreAndResults(t *testing.T) {
	ms := newTestMessageStore(t)
	const chat = "120363000000000001@g.us"
	poll := &pollCreation{Question: "Almoço?", Options: []string{"Pizza", "Sushi"}, SelectableCount: 1}
	if err := ms.StorePoll("POLL1", chat, poll, time.Now()); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ms.StorePollVote("POLL1", chat, "111", []string{"Pizza"}, t0))
	must(ms.StorePollVote("POLL1", chat, "222", []string{"Sushi"}, t0.Add(time.Second)))
	must(ms.StorePollVote("POLL1", chat, "111", []string{"Sushi"}, t0.Add(2*time.Second))) // changed vote
	must(ms.StorePollVote("POLL1", chat, "333", []string{}, t0.Add(3*time.Second)))        // retracted
	must(ms.StorePollVote("POLL1", chat, "222", []string{"Pizza"}, t0.Add(-time.Hour)))    // stale, ignored

	res, err := ms.PollResults("POLL1", chat)
	if err != nil {
		t.Fatal(err)
	}
	if res.Question != "Almoço?" || res.TotalVoters != 2 || len(res.Options) != 2 {
		t.Fatalf("results = %+v", res)
	}
	if res.Options[0].Name != "Pizza" || res.Options[0].Count != 0 {
		t.Fatalf("pizza tally = %+v", res.Options[0])
	}
	if res.Options[1].Name != "Sushi" || res.Options[1].Count != 2 {
		t.Fatalf("sushi tally = %+v", res.Options[1])
	}
	if _, err := ms.PollResults("NOPE", chat); err == nil {
		t.Fatal("unknown poll should error")
	}

	h := handlePollResults(ms, parseChatPolicy(""))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/poll?message_id=POLL1&chat_jid="+chat, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body PollResultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Success || body.TotalVoters != 2 {
		t.Fatalf("body = %s (err %v)", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/poll?message_id=NOPE&chat_jid="+chat, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown poll status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/poll?message_id=POLL1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing chat status = %d", rec.Code)
	}
	restricted := handlePollResults(ms, parseChatPolicy("5511999999999"))
	rec = httptest.NewRecorder()
	restricted(rec, httptest.NewRequest(http.MethodGet, "/api/poll?message_id=POLL1&chat_jid="+chat, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("policy status = %d", rec.Code)
	}
}

func TestHandleMessage_PollCreationAndVote(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	client := newTestClient(&mockLIDStore{})
	ms := newTestMessageStore(t)
	logger := testLogger()

	// Creation from a contact.
	creation := buildImageMessage(phonePN, phonePN, false, "")
	creation.Message = pollCreationMsg("Almoço?", "Pizza", "Sushi")
	creation.Info.ID = "POLL1"
	testBridge(client, ms, logger).handleMessage(creation)

	var content, mediaType string
	if err := ms.db.QueryRow(`SELECT content, media_type FROM messages WHERE id = 'POLL1'`).Scan(&content, &mediaType); err != nil {
		t.Fatalf("poll row missing: %v", err)
	}
	if mediaType != "poll" || content != "📊 Almoço? — options: Pizza | Sushi" {
		t.Fatalf("stored %q / %q", mediaType, content)
	}
	if p, err := ms.GetPoll("POLL1", phonePN.String()); err != nil || len(p.Options) != 2 {
		t.Fatalf("poll not persisted: %v %+v", err, p)
	}

	// Vote with a fake decrypter (real one needs the message-secret store).
	b := testBridge(client, ms, logger)
	b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) { return [][]byte{hashOf("Sushi")}, nil }

	vote := buildImageMessage(phonePN, phonePN, false, "")
	vote.Info.ID = "VOTE1"
	vote.Message = &waProto.Message{PollUpdateMessage: &waProto.PollUpdateMessage{
		PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("POLL1"), RemoteJID: proto.String(phonePN.String())},
		Vote:                   &waProto.PollEncValue{EncPayload: []byte("x"), EncIV: []byte("y")},
	}}
	b.handleMessage(vote)

	var voteContent, voteType, target string
	if err := ms.db.QueryRow(`SELECT content, media_type, filename FROM messages WHERE id = 'VOTE1'`).Scan(&voteContent, &voteType, &target); err != nil {
		t.Fatalf("vote row missing: %v", err)
	}
	if voteType != "poll_vote" || target != "POLL1" || voteContent != "🗳️ voted: Sushi" {
		t.Fatalf("vote stored as %q / %q / %q", voteType, target, voteContent)
	}
	res, err := ms.PollResults("POLL1", phonePN.String())
	if err != nil || res.TotalVoters != 1 || res.Options[1].Count != 1 {
		t.Fatalf("results = %+v (err %v)", res, err)
	}

	// Decrypt failure: no row, no crash.
	b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) { return nil, errors.New("no secret") }
	vote.Info.ID = "VOTE2"
	b.handleMessage(vote)
	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'VOTE2'`).Scan(&n)
	if n != 0 {
		t.Fatal("undecryptable vote must not be stored")
	}
}
