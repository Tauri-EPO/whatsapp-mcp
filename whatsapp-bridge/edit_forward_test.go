package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const efChat = "5511999999999@s.whatsapp.net"

func efPost(t *testing.T, h http.HandlerFunc, body string) (int, editForwardResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var resp editForwardResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func seedEditStore(t *testing.T) *MessageStore {
	t.Helper()
	ms := newTestMessageStore(t)
	_ = ms.StoreChat(efChat, "Alice", time.Now())
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ms.StoreMessage("MINE", efChat, "me", "typo hre", time.Now(), true, "", "", "", nil, nil, nil, 0, ""))
	must(ms.StoreMessage("THEIRS", efChat, "5511999999999", "hello", time.Now(), false, "", "", "", nil, nil, nil, 0, ""))
	must(ms.StoreMessage("PIC", efChat, "5511999999999", "look", time.Now(), false, "image", "pic.jpg", "https://x", []byte("k"), []byte("s"), []byte("e"), 10, ""))
	must(ms.StoreMessage("VOTE", efChat, "5511999999999", "🗳️ voted", time.Now(), false, "poll_vote", "P1", "", nil, nil, nil, 0, ""))
	return ms
}

func TestEditOwnMessageUpdatesLocalContent(t *testing.T) {
	ms := seedEditStore(t)
	var got struct {
		chat types.JID
		id   string
		text string
	}
	h := handleEditMessage(ms, func(_ context.Context, chat types.JID, id types.MessageID, text string) error {
		got.chat, got.id, got.text = chat, id, text
		return nil
	}, chatPolicy{})
	code, resp := efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"MINE","text":" typo here "}`)
	if code != http.StatusOK || !resp.Success || resp.MessageID != "MINE" {
		t.Fatalf("%d %+v", code, resp)
	}
	if got.id != "MINE" || got.text != "typo here" || got.chat.String() != efChat {
		t.Fatalf("edit call = %+v", got)
	}
	var content string
	_ = ms.db.QueryRow(`SELECT content FROM messages WHERE id = 'MINE'`).Scan(&content)
	if content != "typo here" {
		t.Fatalf("local content = %q", content)
	}
}

func TestEditRefusals(t *testing.T) {
	ms := seedEditStore(t)
	calls := 0
	h := handleEditMessage(ms, func(_ context.Context, _ types.JID, _ types.MessageID, _ string) error { calls++; return nil }, chatPolicy{})
	cases := map[string]int{
		`{"chat_jid":"` + efChat + `","message_id":"THEIRS","text":"x"}`: http.StatusForbidden,
		`{"chat_jid":"` + efChat + `","message_id":"NOPE","text":"x"}`:   http.StatusNotFound,
		`{"chat_jid":"` + efChat + `","message_id":"MINE","text":"  "}`:  http.StatusBadRequest,
		`{"chat_jid":"","message_id":"MINE","text":"x"}`:                 http.StatusBadRequest,
	}
	for body, want := range cases {
		if code, _ := efPost(t, h, body); code != want {
			t.Errorf("%s → %d, want %d", body, code, want)
		}
	}
	if calls != 0 {
		t.Fatal("no edit must be sent for refused requests")
	}
	// bridge failure
	h = handleEditMessage(ms, func(_ context.Context, _ types.JID, _ types.MessageID, _ string) error { return errors.New("offline") }, chatPolicy{})
	if code, resp := efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"MINE","text":"x"}`); code != http.StatusBadGateway || !strings.Contains(resp.Message, "offline") {
		t.Fatalf("%d %+v", code, resp)
	}
	// policy
	h = handleEditMessage(ms, nil, parseChatPolicy("5511000000000"))
	if code, _ := efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"MINE","text":"x"}`); code != http.StatusForbidden {
		t.Fatalf("policy → %d", code)
	}
}

func TestForwardTextAndMedia(t *testing.T) {
	ms := seedEditStore(t)
	var sends []struct{ to, content, media string }
	deps := forwardDeps{
		lookup: ms.messageContentLookup,
		download: func(id, chat string) (bool, string, string, string, error) {
			return true, "image", "pic.jpg", "/store/" + chat + "/pic.jpg", nil
		},
		send: func(recipient, message, mediaPath, _, _, _ string, _ []string) (bool, string, sentMessage) {
			sends = append(sends, struct{ to, content, media string }{recipient, message, mediaPath})
			return true, "sent", sentMessage{ID: "NEW1", ChatJID: recipient, Timestamp: time.Unix(1_800_000_000, 0)}
		},
	}
	h := handleForwardMessage(deps, chatPolicy{})
	code, resp := efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"THEIRS","to_chat_jid":"120363000000000001@g.us"}`)
	if code != http.StatusOK || resp.MessageID != "NEW1" || resp.Timestamp == "" {
		t.Fatalf("text forward: %d %+v", code, resp)
	}
	code, _ = efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"PIC","to_chat_jid":"5511888888888"}`)
	if code != http.StatusOK {
		t.Fatalf("media forward: %d", code)
	}
	if len(sends) != 2 || sends[0].content != "hello" || sends[0].media != "" || sends[1].media != "/store/"+efChat+"/pic.jpg" || sends[1].content != "look" {
		t.Fatalf("sends = %+v", sends)
	}
}

func TestForwardRefusals(t *testing.T) {
	ms := seedEditStore(t)
	deps := forwardDeps{
		lookup: ms.messageContentLookup,
		download: func(id, chat string) (bool, string, string, string, error) {
			return false, "", "", "", errors.New("expired")
		},
		send: func(_, _, _, _, _, _ string, _ []string) (bool, string, sentMessage) {
			t.Fatal("send must not be called")
			return false, "", sentMessage{}
		},
	}
	h := handleForwardMessage(deps, chatPolicy{})
	cases := map[string]int{
		`{"chat_jid":"` + efChat + `","message_id":"NOPE","to_chat_jid":"x@s.whatsapp.net"}`: http.StatusNotFound,
		`{"chat_jid":"` + efChat + `","message_id":"VOTE","to_chat_jid":"x@s.whatsapp.net"}`: http.StatusBadRequest,
		`{"chat_jid":"` + efChat + `","message_id":"PIC","to_chat_jid":"x@s.whatsapp.net"}`:  http.StatusBadGateway, // media fetch failed
		`{"chat_jid":"` + efChat + `","message_id":"THEIRS"}`:                                http.StatusBadRequest,
	}
	for body, want := range cases {
		if code, _ := efPost(t, h, body); code != want {
			t.Errorf("%s → %d, want %d", body, code, want)
		}
	}
	// destination outside the allow-list is refused even when the source is allowed
	h = handleForwardMessage(deps, parseChatPolicy("5511999999999"))
	if code, _ := efPost(t, h, `{"chat_jid":"`+efChat+`","message_id":"THEIRS","to_chat_jid":"5511000000000"}`); code != http.StatusForbidden {
		t.Fatalf("destination policy → %d", code)
	}
}
