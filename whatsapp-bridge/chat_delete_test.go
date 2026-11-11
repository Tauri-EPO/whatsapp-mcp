package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

const chatDeleteLID = "100000000000001@lid"

func chatDeleteSeed(t *testing.T, ms *MessageStore, id, chat, mediaType string, ts time.Time) {
	t.Helper()
	msg := storedMessage{ID: id, ChatJID: chat, Sender: chatDeleteLID, Content: "hello " + id, Timestamp: ts, MediaType: mediaType}
	if mediaType != "" {
		msg.URL, msg.MediaKey, msg.FileSHA256, msg.FileEncSHA256, msg.FileLength = "u", []byte("k"), []byte("s"), []byte("e"), 1
	}
	if err := ms.StoreMessage(msg); err != nil {
		t.Fatal(err)
	}
}

func chatDeleteDepsFor(store *MessageStore, send appStateSendFunc, cleanup func(context.Context, []string, time.Time) (chatDeleteCleanup, error)) chatDeleteDeps {
	return chatDeleteDeps{
		archiveDeps: archiveDeps{store: store, policy: chatPolicy{}, connected: func() bool { return true },
			resolve: func(context.Context, string) (types.JID, error) { return types.ParseJID(chatDeleteLID) },
			twin:    func(context.Context, types.JID) (types.JID, error) { return types.ParseJID(archiveTestChat) },
			send:    send,
		},
		cleanup: cleanup,
	}
}

func TestDeleteChatPatchAndCleanupOrder(t *testing.T) {
	store := newTestMessageStore(t)
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	chatDeleteSeed(t, store, "OLD", archiveTestChat, "", ts.Add(-time.Hour))
	chatDeleteSeed(t, store, "NEW", chatDeleteLID, "", ts)
	var events []string
	deps := chatDeleteDepsFor(store, func(_ context.Context, patch appstate.PatchInfo) error {
		events = append(events, "send")
		m := patch.Mutations[0]
		if m.Index[0] != appstate.IndexDeleteChat || m.Index[1] != chatDeleteLID || m.Index[2] != "1" || patch.Type != appstate.WAPatchRegularHigh {
			t.Fatalf("index=%v type=%v", m.Index, patch.Type)
		}
		r := m.Value.GetDeleteChatAction().GetMessageRange()
		if r.GetLastMessageTimestamp() != ts.Unix() || r.GetMessages()[0].GetKey().GetID() != "NEW" {
			t.Fatalf("range=%v", r)
		}
		return nil
	}, func(ctx context.Context, chats []string, upTo time.Time) (chatDeleteCleanup, error) {
		events = append(events, "cleanup")
		if ctx.Err() != nil || !upTo.Equal(ts) || strings.Join(chats, ",") != archiveTestChat+","+chatDeleteLID {
			t.Fatalf("chats=%v upTo=%v", chats, upTo)
		}
		return chatDeleteCleanup{Messages: 2, MediaRemoved: 1}, nil
	})
	rec := httptest.NewRecorder()
	handleDeleteChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","delete_media":true,"confirm":true}`)))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || strings.Join(events, ",") != "send,cleanup" || body["local_messages_removed"] != float64(2) || body["confirmed"] != false {
		t.Fatalf("status=%d events=%v body=%s", rec.Code, events, rec.Body.String())
	}
}

func TestDeleteChatRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		seed       bool
		status     int
	}{
		{"no confirm", `{"chat_jid":"` + archiveTestChat + `"}`, true, 400},
		{"confirm false", `{"chat_jid":"` + archiveTestChat + `","confirm":false}`, true, 400},
		{"missing chat", `{"confirm":true}`, true, 400},
		{"broadcast", `{"chat_jid":"status@broadcast","confirm":true}`, true, 400},
		{"no anchor", `{"chat_jid":"` + archiveTestChat + `","confirm":true}`, false, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMessageStore(t)
			if tc.seed {
				chatDeleteSeed(t, store, "M1", archiveTestChat, "", time.Now())
			}
			deps := chatDeleteDepsFor(store,
				func(context.Context, appstate.PatchInfo) error { t.Fatal("sent"); return nil },
				func(context.Context, []string, time.Time) (chatDeleteCleanup, error) {
					t.Fatal("cleaned")
					return chatDeleteCleanup{}, nil
				})
			rec := httptest.NewRecorder()
			handleDeleteChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDeleteChatRejectedOrUnknownPatchKeepsRows(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{errors.New("server said no"), 502},
		{appStateNotSentError{context.DeadlineExceeded}, 408},
	} {
		store := newTestMessageStore(t)
		chatDeleteSeed(t, store, "M1", archiveTestChat, "", time.Now())
		deps := chatDeleteDepsFor(store, func(context.Context, appstate.PatchInfo) error { return tc.err },
			func(context.Context, []string, time.Time) (chatDeleteCleanup, error) {
				t.Fatal("cleanup after a patch that was not accepted")
				return chatDeleteCleanup{}, nil
			})
		rec := httptest.NewRecorder()
		handleDeleteChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","confirm":true}`)))
		if rec.Code != tc.status {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestDeleteChatCleanupErrorIsReportedNotRetried(t *testing.T) {
	store := newTestMessageStore(t)
	chatDeleteSeed(t, store, "M1", archiveTestChat, "", time.Now())
	deps := chatDeleteDepsFor(store, func(context.Context, appstate.PatchInfo) error { return nil },
		func(context.Context, []string, time.Time) (chatDeleteCleanup, error) {
			return chatDeleteCleanup{}, errors.New("disk on fire")
		})
	rec := httptest.NewRecorder()
	handleDeleteChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","confirm":true}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "do not resend") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteChatLocalRemovesUpToAnchor(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	anchor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	other := "5511888888888@s.whatsapp.net"
	for _, chat := range []string{archiveTestChat, chatDeleteLID, other} {
		if err := ms.StoreChat(chat, "", anchor); err != nil {
			t.Fatal(err)
		}
	}
	chatDeleteSeed(t, ms, "IMG", archiveTestChat, "image", anchor.Add(-time.Hour))
	chatDeleteSeed(t, ms, "TXT", chatDeleteLID, "", anchor)
	chatDeleteSeed(t, ms, "LATER", chatDeleteLID, "", anchor.Add(time.Minute))
	chatDeleteSeed(t, ms, "KEEP", other, "image", anchor.Add(-time.Hour))
	write := func(chat, id string, ts time.Time) string {
		dir := chatMediaDir(chat)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, mediaFileName("image", ts, id, ""))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gone := write(archiveTestChat, "IMG", anchor.Add(-time.Hour))
	kept := write(other, "KEEP", anchor.Add(-time.Hour))

	result, err := b.deleteChatLocal(context.Background(), []string{archiveTestChat, chatDeleteLID}, anchor)
	if err != nil || result.Messages != 2 || result.MediaRemoved != 1 || result.MediaFailed != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fileExists(gone) || !fileExists(kept) {
		t.Fatalf("gone=%v kept=%v", fileExists(gone), fileExists(kept))
	}
	count := func(query string, args ...any) int {
		var n int
		if err := ms.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM messages WHERE chat_jid IN (?, ?)`, archiveTestChat, chatDeleteLID); n != 1 {
		t.Fatalf("remaining rows in deleted chat = %d, want only LATER", n)
	}
	if count(`SELECT COUNT(*) FROM chats WHERE jid = ?`, archiveTestChat) != 0 || count(`SELECT COUNT(*) FROM chats WHERE jid = ?`, chatDeleteLID) != 1 {
		t.Fatal("empty alias must go, alias with a newer row must stay")
	}
	if count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, other) != 1 {
		t.Fatal("other chat touched")
	}
}

// deleteChatStorage stands in for S3: its references cascade away with the
// message rows, so Delete must run while the rows still exist.
type deleteChatStorage struct {
	mediaStorage
	t     *testing.T
	ms    *MessageStore
	fail  map[string]bool
	calls *int
}

func (s deleteChatStorage) Delete(_ context.Context, rows []mediaRow, dry bool) ([]PurgeResult, error) {
	*s.calls++
	out := make([]PurgeResult, 0, len(rows))
	for _, row := range rows {
		var n int
		if err := s.ms.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ? AND chat_jid = ?`, row.ID, row.ChatJID).Scan(&n); err != nil || n != 1 || dry {
			s.t.Fatalf("media delete after its row was gone (n=%d err=%v dry=%v)", n, err, dry)
		}
		if s.fail[row.ID] {
			out = append(out, PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID, Reason: "unreachable"})
			continue
		}
		out = append(out, PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID, Purged: true, Bytes: 1})
	}
	return out, nil
}

func TestDeleteChatLocalMediaBeforeRowsAndFailedFileKeepsRow(t *testing.T) {
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	calls := 0
	b.MediaStorage = deleteChatStorage{t: t, ms: ms, fail: map[string]bool{"BAD": true}, calls: &calls}
	anchor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(archiveTestChat, "", anchor); err != nil {
		t.Fatal(err)
	}
	chatDeleteSeed(t, ms, "GOOD", archiveTestChat, "image", anchor.Add(-time.Minute))
	chatDeleteSeed(t, ms, "BAD", archiveTestChat, "video", anchor.Add(-time.Minute))
	chatDeleteSeed(t, ms, "TXT", archiveTestChat, "", anchor)
	result, err := b.deleteChatLocal(context.Background(), []string{archiveTestChat}, anchor)
	if err != nil || calls != 1 || result.MediaRemoved != 1 || result.MediaFailed != 1 || result.Messages != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	var left []string
	rows, err := ms.db.Query(`SELECT id FROM messages WHERE chat_jid = ?`, archiveTestChat)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	_ = rows.Close()
	if strings.Join(left, ",") != "BAD" {
		t.Fatalf("left=%v: only the row whose file could not be removed may stay", left)
	}
	var chats int
	_ = ms.db.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid = ?`, archiveTestChat).Scan(&chats)
	if chats != 1 {
		t.Fatal("chat row removed while a message is still stored under it")
	}
}

func TestDeleteChatMuxGuards(t *testing.T) {
	for _, tc := range []struct {
		name, allow, deny, chats string
		readOnly                 bool
		status                   int
	}{
		{"read-only", "", "", "", true, 403},
		{"allow tools", "archive_chat", "", "", false, 403},
		{"deny tools", "", "delete_chat", "", false, 403},
		{"chat list", "", "", "5511888888888", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.ReadOnly = readOnlyPolicy{enabled: tc.readOnly}
			b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
			b.Policy = b.runtimeChatPolicy(parseChatPolicy(tc.chats))
			b.Connected = func() bool { t.Fatal("connected called past guard"); return false }
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/chat/delete", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","confirm":true}`))
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
