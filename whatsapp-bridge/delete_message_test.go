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

func seedMessage(t *testing.T, ms *MessageStore, id, chatJID string, fromMe bool) {
	t.Helper()
	if err := ms.StoreChat(chatJID, "chat", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, chatJID, "5511999999999", "hello", time.Now(), fromMe, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
}

func postDelete(h http.HandlerFunc, body string) (*httptest.ResponseRecorder, DeleteMessageResponse) {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/delete", strings.NewReader(body)))
	var resp DeleteMessageResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestHandleDeleteMessage(t *testing.T) {
	const chat = "5511888888888@s.whatsapp.net"
	ms := newTestMessageStore(t)
	seedMessage(t, ms, "mine", chat, true)
	seedMessage(t, ms, "theirs", chat, false)

	var revoked []string
	revoke := func(_ context.Context, c types.JID, id types.MessageID) error {
		if id == "boom" {
			return errors.New("network down")
		}
		revoked = append(revoked, c.String()+"/"+string(id))
		return nil
	}
	h := handleDeleteMessage(ms, revoke, parseChatPolicy(""))

	t.Run("local delete removes the row and sends nothing", func(t *testing.T) {
		rec, resp := postDelete(h, `{"chat_jid":"`+chat+`","message_id":"theirs","for_everyone":false}`)
		if rec.Code != http.StatusOK || !resp.Success || resp.ForEveryone {
			t.Fatalf("status=%d resp=%+v", rec.Code, resp)
		}
		if got, _ := ms.GetMessageIsFromMe("theirs", chat); got != nil {
			t.Fatal("row should be gone")
		}
		if len(revoked) != 0 {
			t.Fatal("local delete must not revoke")
		}
		rec, _ = postDelete(h, `{"chat_jid":"`+chat+`","message_id":"theirs","for_everyone":false}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("second local delete status=%d", rec.Code)
		}
	})

	t.Run("revoke own message marks deleted_at", func(t *testing.T) {
		rec, resp := postDelete(h, `{"chat_jid":"`+chat+`","message_id":"mine","for_everyone":true}`)
		if rec.Code != http.StatusOK || !resp.Success || !resp.ForEveryone {
			t.Fatalf("status=%d resp=%+v", rec.Code, resp)
		}
		if len(revoked) != 1 || revoked[0] != chat+"/mine" {
			t.Fatalf("revoked = %v", revoked)
		}
		var deletedAt *string
		if err := ms.db.QueryRow(`SELECT deleted_at FROM messages WHERE id = 'mine'`).Scan(&deletedAt); err != nil || deletedAt == nil {
			t.Fatalf("deleted_at not set (err=%v)", err)
		}
	})

	t.Run("cannot revoke someone else's message", func(t *testing.T) {
		seedMessage(t, ms, "theirs2", chat, false)
		rec, _ := postDelete(h, `{"chat_jid":"`+chat+`","message_id":"theirs2","for_everyone":true}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("unknown message cannot be revoked", func(t *testing.T) {
		rec, _ := postDelete(h, `{"chat_jid":"`+chat+`","message_id":"nope","for_everyone":true}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d", rec.Code)
		}
	})

	t.Run("revoke failure is reported", func(t *testing.T) {
		seedMessage(t, ms, "boom", chat, true)
		rec, resp := postDelete(h, `{"chat_jid":"`+chat+`","message_id":"boom","for_everyone":true}`)
		if rec.Code != http.StatusBadGateway || resp.Success {
			t.Fatalf("status=%d resp=%+v", rec.Code, resp)
		}
	})

	t.Run("validation", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"chat_jid":"x","message_id":""}`, `{"chat_jid":"1.2.3@s.whatsapp.net","message_id":"a"}`, `not json`} {
			if rec, _ := postDelete(h, body); rec.Code != http.StatusBadRequest {
				t.Fatalf("body %q: status=%d", body, rec.Code)
			}
		}
	})

	t.Run("allow-list", func(t *testing.T) {
		restricted := handleDeleteMessage(ms, revoke, parseChatPolicy("5511999999999"))
		if rec, _ := postDelete(restricted, `{"chat_jid":"`+chat+`","message_id":"mine","for_everyone":false}`); rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d", rec.Code)
		}
	})
}
