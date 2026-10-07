package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// codedError stands in for the driver's error: all isBusyError looks at is the
// SQLite result code.
type codedError int

func (e codedError) Error() string { return fmt.Sprintf("sqlite code %d", int(e)) }
func (e codedError) Code() int     { return int(e) }

func TestIsBusyError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                       {nil, false},
		"not a sqlite error":        {errors.New("disk on fire"), false},
		"SQLITE_BUSY":               {codedError(5), true},
		"SQLITE_LOCKED":             {codedError(6), true},
		"SQLITE_BUSY_SNAPSHOT":      {codedError(5 | 2<<8), true},
		"SQLITE_LOCKED_SHAREDCACHE": {codedError(6 | 1<<8), true},
		"SQLITE_CONSTRAINT":         {codedError(19), false},
		"SQLITE_FULL":               {codedError(13), false},
		"wrapped busy":              {fmt.Errorf("insert: %w", codedError(5)), true},
	}
	for name, tc := range cases {
		if got := isBusyError(tc.err); got != tc.want {
			t.Errorf("%s: isBusyError = %v, want %v", name, got, tc.want)
		}
	}
}

// lockedStore is a file-backed store whose write lock the test holds on a
// second connection, the way a history-sync transaction does. busy_timeout is
// a few milliseconds so a blocked insert fails fast with the real SQLITE_BUSY.
type lockedStore struct {
	ms     *MessageStore
	holder *sql.Conn
}

func newLockedStore(t *testing.T) *lockedStore {
	t.Helper()
	dsn := sqliteURI(filepath.Join(t.TempDir(), "messages.db"), "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5)&"+sqliteTimeFormat)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP,
			ephemeral_expiration INTEGER NOT NULL DEFAULT 0, ephemeral_setting_timestamp INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0,
			target_message_id TEXT, quoted_message_id TEXT, mentions TEXT,
			PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid));`); err != nil {
		t.Fatal(err)
	}
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	return &lockedStore{ms: &MessageStore{db: db, names: newChatNameCache()}, holder: holder}
}

func (l *lockedStore) lock(t *testing.T) {
	t.Helper()
	if _, err := l.holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
}

func (l *lockedStore) unlock(t *testing.T) {
	t.Helper()
	if _, err := l.holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}
}

func errorLines(log string) []string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "[ERROR]") {
			lines = append(lines, line)
		}
	}
	return lines
}

func lastMessageTime(t *testing.T, ms *MessageStore, chatJID string) string {
	t.Helper()
	// CAST: the text as stored, not the driver's rendering of a TIMESTAMP.
	var ts string
	if err := ms.db.QueryRow("SELECT COALESCE(CAST(last_message_time AS TEXT), '') FROM chats WHERE jid = ?", chatJID).Scan(&ts); err != nil {
		t.Fatalf("chat %s: %v", chatJID, err)
	}
	return ts
}

// A live insert that finds the database busy is tried again and kept; one that
// stays busy is given up after the bound: one ERROR naming the message, one
// count, a webhook that says it was not stored, and a chat whose last message
// time did not move (issues #518, #519, #531).
func TestHandleMessage_BusyStoreIsRetriedThenGivenUp(t *testing.T) {
	const text = "words that must stay out of the error"
	cases := []struct {
		name          string
		newChat       bool // no row for the chat yet: the message row needs one first
		releaseAfter  int  // retries after which the lock is released; 0 = never
		wantAttempts  int32
		wantStored    bool
		wantFailures  int64
		wantErrorLogs int
	}{
		{name: "busy once, then free", releaseAfter: 1, wantAttempts: 1, wantStored: true},
		{name: "busy once, then free, in a chat with no row yet", newChat: true, releaseAfter: 1, wantAttempts: 1, wantStored: true},
		{name: "busy until the bound", releaseAfter: 0, wantAttempts: 2, wantStored: false, wantFailures: 1, wantErrorLogs: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, webhookCh := captureRawWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			rec := installRecordingLogger(t)
			store := newLockedStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), store.ms, rec)
			b.StoreRetryDelays = []time.Duration{time.Hour, time.Hour} // never slept: the seam below stands in
			var waits atomic.Int32
			b.storeRetryWait = func(time.Duration) bool {
				if int(waits.Add(1)) == tc.releaseAfter {
					store.unlock(t)
				}
				return true
			}

			// An earlier message, so the chat has a last message time to keep.
			before := ""
			if !tc.newChat {
				earlier := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "earlier")
				earlier.Info.ID, earlier.Info.Timestamp = "EARLIER", time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
				b.handleMessage(earlier)
				<-webhookCh
				before = lastMessageTime(t, store.ms, phonePN.String())
			}

			store.lock(t)
			msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, text)
			msg.Info.ID, msg.Info.Timestamp = "BUSY1", time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
			b.handleMessage(msg)
			if tc.releaseAfter == 0 {
				store.unlock(t)
			}

			if got := waits.Load(); got != tc.wantAttempts {
				t.Errorf("retries = %d, want %d", got, tc.wantAttempts)
			}
			var rows int
			if err := store.ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'BUSY1'").Scan(&rows); err != nil || (rows == 1) != tc.wantStored {
				t.Errorf("stored rows = %d (err %v), want stored=%v", rows, err, tc.wantStored)
			}
			if got := b.metrics.storeFailures.Load(); got != tc.wantFailures {
				t.Errorf("storeFailures = %d, want %d", got, tc.wantFailures)
			}
			errs := errorLines(rec.String())
			if len(errs) != tc.wantErrorLogs {
				t.Fatalf("ERROR lines = %d, want %d:\n%s", len(errs), tc.wantErrorLogs, rec.String())
			}
			for _, line := range errs {
				if !strings.Contains(line, "message BUSY1 in "+phonePN.String()) || strings.Contains(line, text) {
					t.Errorf("the ERROR must name the message and the chat, never the content: %q", line)
				}
			}
			after := ""
			if !tc.newChat || tc.wantStored {
				after = lastMessageTime(t, store.ms, phonePN.String())
			}
			if moved := after != before; moved != tc.wantStored {
				t.Errorf("last_message_time %q -> %q, moved=%v, want moved=%v", before, after, moved, tc.wantStored)
			}
			select {
			case payload := <-webhookCh:
				stored, present := payload["stored"]
				if payload["messageId"] != "BUSY1" || payload["content"] != text {
					t.Errorf("webhook payload = %v", payload)
				}
				if tc.wantStored && present {
					t.Errorf("a stored message must not carry the field, got stored=%v", stored)
				}
				if !tc.wantStored && (!present || stored != false) {
					t.Errorf("want \"stored\": false on the webhook of a message that was not stored, got %v (present=%v)", stored, present)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the webhook must fire either way")
			}
		})
	}
}

// An error that is not "busy" is final at once: no retry, one ERROR, one count.
func TestStoreLive_OtherErrorsAreNotRetried(t *testing.T) {
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, nil, rec)
	b.storeRetryWait = func(time.Duration) bool {
		t.Error("a non-busy error must not wait for a retry")
		return true
	}
	attempts := 0
	if b.storeLive("message", "M1", "chat@s.whatsapp.net", func() error { attempts++; return codedError(19) }) {
		t.Fatal("storeLive reported success")
	}
	if attempts != 1 || b.metrics.storeFailures.Load() != 1 || len(errorLines(rec.String())) != 1 {
		t.Errorf("attempts = %d, failures = %d, log:\n%s", attempts, b.metrics.storeFailures.Load(), rec.String())
	}

	// A bridge that is shutting down does not start another attempt.
	b.storeRetryWait = func(time.Duration) bool { return false }
	attempts = 0
	b.storeLive("message", "M2", "chat@s.whatsapp.net", func() error { attempts++; return codedError(5) })
	if attempts != 1 {
		t.Errorf("attempts during shutdown = %d, want 1", attempts)
	}
}

// The wait between two attempts, without the test seam: it elapses on its own,
// and a bridge that is shutting down does not sit through it.
func TestWaitStoreRetry(t *testing.T) {
	b := testBridge(t, nil, nil, testLogger())
	if !b.waitStoreRetry(time.Millisecond) {
		t.Fatal("the wait must elapse and allow the next attempt")
	}
	b.cancel()
	done := make(chan bool, 1)
	go func() { done <- b.waitStoreRetry(time.Hour) }()
	select {
	case again := <-done:
		if again {
			t.Fatal("no further attempt once the bridge is shutting down")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait ignored the shutdown")
	}
}

// Reactions, poll votes and history rows lose data the same way: each write
// that fails is one ERROR with the message ID and the chat, and one count
// (issue #520).
func TestStoreFailuresOfReactionsVotesAndHistoryAreErrors(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)

	t.Run("reaction", func(t *testing.T) {
		srv, webhookCh := captureRawWebhook(t)
		t.Setenv("WEBHOOK_URL", srv.URL)
		rec := installRecordingLogger(t)
		ms := newTestMessageStore(t)
		failMessageInserts(t, ms)
		b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
		msg := buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "")
		msg.Info.ID = "REACT1"
		msg.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{ID: proto.String("TARGET1")}, Text: proto.String("+1"),
		}}
		b.handleMessage(msg)
		errs := errorLines(rec.String())
		if len(errs) != 1 || !strings.Contains(errs[0], "reaction REACT1 in "+group.String()) {
			t.Fatalf("want one ERROR naming the reaction and the chat, got %q", errs)
		}
		if got := b.metrics.storeFailures.Load(); got != 1 {
			t.Errorf("storeFailures = %d, want 1", got)
		}
		var chats int
		if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats WHERE last_message_time IS NOT NULL").Scan(&chats); err != nil || chats != 0 {
			t.Errorf("%d chats claim activity (err %v) although the only event was not stored", chats, err)
		}
		select {
		case payload := <-webhookCh:
			if stored, present := payload["stored"]; payload["eventType"] != "reaction" || !present || stored != false {
				t.Errorf("reaction webhook = %v, want \"stored\": false", payload)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the reaction must still reach the webhook")
		}
	})

	// A decoded vote writes two things: the structured vote /api/poll reads and
	// the message row. Either can fail.
	vote := func(id string) *events.Message {
		msg := buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "")
		msg.Info.ID = id
		msg.Message = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
			PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("POLL1"), RemoteJID: proto.String(group.String())},
			Vote:                   &waE2E.PollEncValue{EncPayload: []byte("x"), EncIV: []byte("y")},
		}}
		return msg
	}
	withPoll := func(t *testing.T, rec *recordingLogger) (*Bridge, *MessageStore) {
		t.Helper()
		t.Setenv("WEBHOOK_ENABLED", "false")
		ms := newTestMessageStore(t)
		b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
		b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) { return [][]byte{hashOf("Sushi")}, nil }
		creation := buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "")
		creation.Info.ID = "POLL1"
		creation.Message = pollCreationMsg("Lunch?", "Pizza", "Sushi")
		b.handleMessage(creation)
		if len(errorLines(rec.String())) != 0 {
			t.Fatalf("the poll itself must store cleanly:\n%s", rec.String())
		}
		return b, ms
	}

	t.Run("poll vote row", func(t *testing.T) {
		rec := installRecordingLogger(t)
		b, ms := withPoll(t, rec)
		failMessageInserts(t, ms)
		b.handleMessage(vote("VOTE1"))
		errs := errorLines(rec.String())
		if len(errs) != 1 || !strings.Contains(errs[0], "poll vote VOTE1 in "+group.String()) {
			t.Fatalf("want one ERROR naming the vote and the chat, got %q", errs)
		}
		if got := b.metrics.storeFailures.Load(); got != 1 {
			t.Errorf("storeFailures = %d, want 1", got)
		}
	})

	t.Run("structured poll vote", func(t *testing.T) {
		rec := installRecordingLogger(t)
		b, ms := withPoll(t, rec)
		if _, err := ms.db.Exec(`CREATE TRIGGER fail_vote_insert BEFORE INSERT ON poll_votes
			BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
			t.Fatal(err)
		}
		b.handleMessage(vote("VOTE2"))
		errs := errorLines(rec.String())
		if len(errs) != 1 || !strings.Contains(errs[0], "VOTE2") || !strings.Contains(errs[0], "POLL1") || !strings.Contains(errs[0], group.String()) {
			t.Fatalf("want one ERROR naming the vote, the poll and the chat, got %q", errs)
		}
		if got := b.metrics.storeFailures.Load(); got != 1 {
			t.Errorf("storeFailures = %d, want 1: what /api/poll reads is lost", got)
		}

		// A vote that cannot be decoded leaves a marker; losing that is the same loss.
		b.PollVoteDecrypt = func(_ context.Context, _ *events.Message) ([][]byte, error) { return nil, errors.New("no secret") }
		b.handleMessage(vote("VOTE3"))
		errs = errorLines(rec.String())
		if len(errs) != 2 || !strings.Contains(errs[1], "VOTE3") || !strings.Contains(errs[1], "POLL1") || !strings.Contains(errs[1], group.String()) {
			t.Fatalf("want a second ERROR naming the undecodable vote, got %q", errs)
		}
		if got := b.metrics.storeFailures.Load(); got != 2 {
			t.Errorf("storeFailures = %d, want 2", got)
		}
	})

	t.Run("history row", func(t *testing.T) {
		rec := installRecordingLogger(t)
		ms := newTestMessageStore(t)
		failMessageInserts(t, ms)
		b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, rec)
		b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
			SyncType: waHistorySync.HistorySync_RECENT.Enum(),
			Conversations: []*waHistorySync.Conversation{{
				ID: proto.String(group.String()),
				Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
					Key:              &waCommon.MessageKey{ID: proto.String("HIST1"), FromMe: proto.Bool(false), RemoteJID: proto.String(group.String())},
					Participant:      proto.String(phonePN.String()),
					MessageTimestamp: proto.Uint64(1772359200),
					Message:          &waE2E.Message{Conversation: proto.String("words that stay out of the log line")},
				}}},
			}},
		}})
		errs := errorLines(rec.String())
		if len(errs) != 1 || !strings.Contains(errs[0], "history message HIST1 in "+group.String()) || strings.Contains(errs[0], "words that stay") {
			t.Fatalf("want one ERROR naming the row and the chat, never the content, got %q", errs)
		}
		if got := b.metrics.storeFailures.Load(); got != 1 {
			t.Errorf("storeFailures = %d, want 1", got)
		}
	})
}

// A message the bridge does not store is not activity: it creates no chat row
// and leaves the last message time where the last stored row put it. Stored
// messages and reactions still move it (issue #531).
func TestHandleMessage_LastMessageTimeFollowsStoredRows(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	at := func(hour int) time.Time { return time.Date(2026, 3, 1, hour, 0, 0, 0, time.UTC) }

	// A kind the bridge drops at the "no content and no media" gate, first.
	empty := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
	empty.Info.ID, empty.Info.Timestamp = "EMPTY0", at(9)
	empty.Message = &waE2E.Message{MessageHistoryNotice: &waE2E.MessageHistoryNotice{}}
	b.handleMessage(empty)
	var chats int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil || chats != 0 {
		t.Fatalf("a dropped first message left %d chat rows (err %v): a chat with nothing in it", chats, err)
	}

	text := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "hello")
	text.Info.ID, text.Info.Timestamp = "TEXT1", at(10)
	b.handleMessage(text)
	stored := lastMessageTime(t, ms, phonePN.String())
	if stored != dbTime(at(10)) {
		t.Fatalf("a stored message must move last_message_time: %q, want %q", stored, dbTime(at(10)))
	}

	empty.Info.ID, empty.Info.Timestamp = "EMPTY1", at(11)
	b.handleMessage(empty)
	if got := lastMessageTime(t, ms, phonePN.String()); got != stored {
		t.Errorf("a dropped message moved last_message_time: %q -> %q", stored, got)
	}

	reaction := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
	reaction.Info.ID, reaction.Info.Timestamp = "REACT1", at(12)
	reaction.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("TEXT1")}, Text: proto.String("+1"),
	}}
	b.handleMessage(reaction)
	if got := lastMessageTime(t, ms, phonePN.String()); got != dbTime(at(12)) {
		t.Errorf("a stored reaction keeps moving last_message_time: %q, want %q", got, dbTime(at(12)))
	}
}
