package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
)

func deleteForMeDepsFor(store *MessageStore, send appStateSendFunc, cleanup func(context.Context, []deleteForMeRef) (chatDeleteCleanup, error)) deleteForMeDeps {
	d := chatDeleteDepsFor(store, send, nil)
	return deleteForMeDeps{archiveDeps: d.archiveDeps, cleanup: cleanup}
}

func TestBuildDeleteMessageForMePatchShape(t *testing.T) {
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lid, _ := parseRecipientJID(chatDeleteLID)
	patch := buildDeleteMessageForMePatch(lid, []deleteForMeRef{
		{row: mediaRow{ID: "IN1", Timestamp: ts}},
		{row: mediaRow{ID: "OUT1", Timestamp: ts.Add(time.Minute)}, fromMe: true},
	})
	if patch.Type != appstate.WAPatchRegularHigh || len(patch.Mutations) != 2 {
		t.Fatalf("type=%v n=%d", patch.Type, len(patch.Mutations))
	}
	for i, want := range []struct {
		id, fromMe string
		ts         int64
	}{{"IN1", "0", ts.Unix()}, {"OUT1", "1", ts.Add(time.Minute).Unix()}} {
		m := patch.Mutations[i]
		wantIndex := []string{appstate.IndexDeleteMessageForMe, chatDeleteLID, want.id, want.fromMe, "0"}
		if strings.Join(m.Index, "|") != strings.Join(wantIndex, "|") || m.Version != 3 {
			t.Fatalf("index=%v version=%d", m.Index, m.Version)
		}
		act := m.Value.GetDeleteMessageForMeAction()
		if !act.GetDeleteMedia() || act.GetMessageTimestamp() != want.ts {
			t.Fatalf("action=%v", act)
		}
	}
}

func TestDeleteForMeAnchorsSendsThenCleansEveryAlias(t *testing.T) {
	store := newTestMessageStore(t)
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	chatDeleteSeed(t, store, "IN1", archiveTestChat, "", ts)
	chatDeleteSeed(t, store, "IN1", chatDeleteLID, "", ts) // twin copy
	if err := store.StoreMessage(storedMessage{ID: "OUT1", ChatJID: chatDeleteLID, Sender: chatDeleteLID, Content: "mine", Timestamp: ts.Add(time.Minute), IsFromMe: true}); err != nil {
		t.Fatal(err)
	}
	var events []string
	deps := deleteForMeDepsFor(store, func(_ context.Context, patch appstate.PatchInfo) error {
		events = append(events, "send")
		if len(patch.Mutations) != 2 || patch.Mutations[0].Index[1] != chatDeleteLID || patch.Mutations[1].Index[3] != "1" {
			t.Fatalf("patch=%v", patch.Mutations)
		}
		return nil
	}, func(_ context.Context, refs []deleteForMeRef) (chatDeleteCleanup, error) {
		events = append(events, "cleanup")
		if len(refs) != 3 {
			t.Fatalf("local rows=%d, want both IN1 copies and OUT1", len(refs))
		}
		return chatDeleteCleanup{Messages: 3}, nil
	})
	rec := httptest.NewRecorder()
	handleDeleteForMe(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","message_ids":["IN1","OUT1"],"confirm":true}`)))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || strings.Join(events, ",") != "send,cleanup" || body["deleted_for_me"] != float64(2) {
		t.Fatalf("status=%d events=%v body=%s", rec.Code, events, rec.Body.String())
	}
}

func TestDeleteForMeRefusalsSendNothing(t *testing.T) {
	many := make([]string, deleteForMeMaxIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprintf("M%d", i))
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"no confirm", `{"chat_jid":"` + archiveTestChat + `","message_ids":["M1"]}`, 400},
		{"no ids", `{"chat_jid":"` + archiveTestChat + `","message_ids":[],"confirm":true}`, 400},
		{"blank id", `{"chat_jid":"` + archiveTestChat + `","message_ids":[" "],"confirm":true}`, 400},
		{"duplicate", `{"chat_jid":"` + archiveTestChat + `","message_ids":["M1","M1"],"confirm":true}`, 400},
		{"too many", `{"chat_jid":"` + archiveTestChat + `","message_ids":[` + strings.Join(many, ",") + `],"confirm":true}`, 400},
		{"group", `{"chat_jid":"120363000000000001@g.us","message_ids":["M1"],"confirm":true}`, 400},
		{"unknown id", `{"chat_jid":"` + archiveTestChat + `","message_ids":["M1","NOPE"],"confirm":true}`, 404},
		{"other chat id", `{"chat_jid":"` + archiveTestChat + `","message_ids":["ELSEWHERE"],"confirm":true}`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMessageStore(t)
			chatDeleteSeed(t, store, "M1", archiveTestChat, "", time.Now())
			chatDeleteSeed(t, store, "ELSEWHERE", "5511888888888@s.whatsapp.net", "", time.Now())
			deps := deleteForMeDepsFor(store,
				func(context.Context, appstate.PatchInfo) error { t.Fatal("sent"); return nil },
				func(context.Context, []deleteForMeRef) (chatDeleteCleanup, error) {
					t.Fatal("cleaned")
					return chatDeleteCleanup{}, nil
				})
			rec := httptest.NewRecorder()
			handleDeleteForMe(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDeleteForMeRejectedPatchKeepsRows(t *testing.T) {
	store := newTestMessageStore(t)
	chatDeleteSeed(t, store, "M1", archiveTestChat, "", time.Now())
	deps := deleteForMeDepsFor(store, func(context.Context, appstate.PatchInfo) error { return errors.New("server said no") },
		func(context.Context, []deleteForMeRef) (chatDeleteCleanup, error) {
			t.Fatal("cleanup after a rejected patch")
			return chatDeleteCleanup{}, nil
		})
	rec := httptest.NewRecorder()
	handleDeleteForMe(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","message_ids":["M1"],"confirm":true}`)))
	if rec.Code != 502 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteForMeLocalMediaFirst(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	calls := 0
	b.MediaStorage = deleteChatStorage{t: t, ms: ms, fail: map[string]bool{"BAD": true}, calls: &calls}
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	chatDeleteSeed(t, ms, "IMG", archiveTestChat, "image", ts)
	chatDeleteSeed(t, ms, "BAD", archiveTestChat, "video", ts)
	chatDeleteSeed(t, ms, "TXT", archiveTestChat, "", ts)
	chatDeleteSeed(t, ms, "KEEP", archiveTestChat, "", ts)
	_, local, _, err := ms.deleteForMeRefs(context.Background(), []string{archiveTestChat}, []string{"IMG", "BAD", "TXT"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.deleteForMeLocal(context.Background(), local)
	if err != nil || calls != 1 || result.Messages != 2 || result.MediaRemoved != 1 || result.MediaFailed != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	var n int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id IN ('BAD','KEEP')`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows left=%d, want BAD (file kept) and KEEP (not requested)", n)
	}
}

func TestDeleteForMeRefusesPointerRows(t *testing.T) {
	store := newTestMessageStore(t)
	if err := store.StoreMessage(storedMessage{ID: "R1", ChatJID: archiveTestChat, Sender: chatDeleteLID, Timestamp: time.Now(), MediaType: "reaction"}); err != nil {
		t.Fatal(err)
	}
	deps := deleteForMeDepsFor(store, func(context.Context, appstate.PatchInfo) error { t.Fatal("sent"); return nil }, nil)
	rec := httptest.NewRecorder()
	handleDeleteForMe(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","message_ids":["R1"],"confirm":true}`)))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "reaction or poll vote") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteForMeLocalRemovesOrphans(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(archiveTestChat, "", ts); err != nil {
		t.Fatal(err)
	}
	chatDeleteSeed(t, ms, "POLL", archiveTestChat, "", ts)
	if err := ms.StoreMessage(storedMessage{ID: "R1", ChatJID: archiveTestChat, Sender: chatDeleteLID, Timestamp: ts, MediaType: "reaction"}); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetTargetMessageID("R1", archiveTestChat, "POLL"); err != nil {
		t.Fatal(err)
	}
	if err := ms.StorePoll("POLL", archiveTestChat, &pollCreation{Question: "q?", Options: []string{"a", "b"}, SelectableCount: 1}, ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StorePollVote("POLL", archiveTestChat, chatDeleteLID, []string{"a"}, ts); err != nil {
		t.Fatal(err)
	}
	_, local, _, err := ms.deleteForMeRefs(context.Background(), []string{archiveTestChat}, []string{"POLL"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.deleteForMeLocal(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		var n int
		if err := ms.db.QueryRow(q, archiveTestChat).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`,
		`SELECT COUNT(*) FROM polls WHERE chat_jid = ?`,
		`SELECT COUNT(*) FROM poll_votes WHERE chat_jid = ?`,
		`SELECT COUNT(*) FROM chats WHERE jid = ?`,
	} {
		if n := count(q); n != 0 {
			t.Fatalf("%s = %d, want 0", q, n)
		}
	}
}

func TestDeleteForMeMuxGuards(t *testing.T) {
	for _, tc := range []struct {
		name, allow, deny, chats string
		readOnly                 bool
		status                   int
	}{
		{"read-only", "", "", "", true, 403},
		{"allow tools", "delete_message", "", "", false, 403},
		{"deny tools", "", "delete_messages_for_me", "", false, 403},
		{"chat list", "", "", "5511888888888", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.ReadOnly = readOnlyPolicy{enabled: tc.readOnly}
			b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
			b.Policy = b.runtimeChatPolicy(parseChatPolicy(tc.chats))
			b.Connected = func() bool { t.Fatal("connected called past guard"); return false }
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/delete-for-me", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","message_ids":["M1"],"confirm":true}`))
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
