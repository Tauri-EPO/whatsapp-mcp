package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type callRow struct {
	chatJID, fromJID, callType, result, reason string
	isFromMe, isGroup                          bool
}

func queryCall(t *testing.T, ms *MessageStore, callID string) callRow {
	t.Helper()
	var r callRow
	var result, reason *string
	err := ms.db.QueryRow(`SELECT chat_jid, from_jid, call_type, is_from_me, is_group, result, reason FROM calls WHERE call_id = ?`, callID).
		Scan(&r.chatJID, &r.fromJID, &r.callType, &r.isFromMe, &r.isGroup, &result, &reason)
	if err != nil {
		t.Fatalf("call %s: %v", callID, err)
	}
	if result != nil {
		r.result = *result
	}
	if reason != nil {
		r.reason = *reason
	}
	return r
}

func TestHandleEvent_CallLifecycle(t *testing.T) {
	self := types.NewJID("5511999999999", types.DefaultUserServer)
	peer := types.NewJID("5511888888888", types.DefaultUserServer)
	group := types.NewJID("120363012345678901", types.GroupServer)
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), ms, installRecordingLogger(t))
	reconnect := make(chan bool, 1)
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	// 1:1 incoming offer: voice by default, chat = caller.
	b.handleEvent(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{From: peer.ToNonAD(), CallID: "c1", Timestamp: at, CallCreator: peer}}, reconnect)
	row := queryCall(t, ms, "c1")
	if row.chatJID != peer.String() || row.fromJID != peer.String() || row.callType != "voice" || row.isGroup || row.isFromMe {
		t.Errorf("offer row = %+v", row)
	}

	b.handleEvent(&events.CallAccept{BasicCallMeta: types.BasicCallMeta{From: peer, CallID: "c1", CallCreator: peer}}, reconnect)
	if row = queryCall(t, ms, "c1"); row.result != "answered" {
		t.Errorf("after accept: result = %q", row.result)
	}
	b.handleEvent(&events.CallTerminate{BasicCallMeta: types.BasicCallMeta{From: peer, CallID: "c1", CallCreator: peer, Timestamp: at.Add(90 * time.Second)}, Reason: "timeout"}, reconnect)
	if row = queryCall(t, ms, "c1"); row.reason != "timeout" || row.result != "ended" {
		t.Errorf("after terminate: %+v", row)
	}
	var duration int
	if err := ms.db.QueryRow(`SELECT duration_sec FROM calls WHERE call_id = 'c1'`).Scan(&duration); err != nil || duration != 90 {
		t.Errorf("duration_sec = %d err = %v, want 90", duration, err)
	}

	// Group video call arrives as CallOfferNotice with Media/Type set.
	b.handleEvent(&events.CallOfferNotice{BasicCallMeta: types.BasicCallMeta{From: peer, CallID: "g1", Timestamp: at, GroupJID: group, CallCreator: peer}, Media: "video", Type: "group"}, reconnect)
	row = queryCall(t, ms, "g1")
	if row.chatJID != group.String() || row.callType != "video" || !row.isGroup {
		t.Errorf("group offer row = %+v", row)
	}
	b.handleEvent(&events.CallReject{BasicCallMeta: types.BasicCallMeta{From: peer, CallID: "g1", GroupJID: group, CallCreator: peer}}, reconnect)
	if row = queryCall(t, ms, "g1"); row.result != "rejected" {
		t.Errorf("after reject: result = %q", row.result)
	}

	// Outbound (creator is us) is recorded as from-me even though WhatsApp
	// rarely delivers it to linked devices.
	b.handleEvent(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{From: peer, CallID: "c2", Timestamp: at, CallCreator: self}}, reconnect)
	if row = queryCall(t, ms, "c2"); !row.isFromMe || row.chatJID != self.String() {
		t.Errorf("outbound offer row = %+v", row)
	}

	select {
	case <-reconnect:
		t.Fatal("call events must not trigger reconnection")
	default:
	}
}

func TestCallChatJID(t *testing.T) {
	peer := types.NewJID("5511888888888", types.DefaultUserServer)
	peerAD := types.JID{User: "5511888888888", Server: types.DefaultUserServer, Device: 3}
	group := types.NewJID("123@g.us", types.GroupServer)
	if got := callChatJID(types.BasicCallMeta{From: peerAD, GroupJID: group}); got != group.String() {
		t.Errorf("group wins: %q", got)
	}
	if got := callChatJID(types.BasicCallMeta{From: peerAD, CallCreator: peerAD}); got != peer.String() {
		t.Errorf("creator without device suffix: %q", got)
	}
	if got := callChatJID(types.BasicCallMeta{From: peerAD}); got != peer.String() {
		t.Errorf("from without device suffix: %q", got)
	}
}

func TestHandleEvent_GroupInfoRenameAndEphemeral(t *testing.T) {
	group := types.NewJID("120363012345678901", types.GroupServer)
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
	if err := ms.StoreChat(group.String(), "Old name", time.Now()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

	b.handleEvent(&events.GroupInfo{JID: group, Timestamp: at,
		Name:      &types.GroupName{Name: "  New name  "},
		Ephemeral: &types.GroupEphemeral{IsEphemeral: true, DisappearingTimer: 86400},
	}, nil)
	var name string
	if err := ms.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, group.String()).Scan(&name); err != nil || name != "New name" {
		t.Errorf("name = %q err = %v (trimmed)", name, err)
	}
	settings, err := ms.GetChatEphemeralSettings(group.String())
	if err != nil || settings.Expiration != 86400 || settings.SettingTimestamp != at.Unix() {
		t.Errorf("ephemeral = %+v err = %v", settings, err)
	}

	// Blank rename is ignored; turning disappearing messages off stores 0.
	b.handleEvent(&events.GroupInfo{JID: group, Timestamp: at.Add(time.Hour),
		Name:      &types.GroupName{Name: "   "},
		Ephemeral: &types.GroupEphemeral{IsEphemeral: false, DisappearingTimer: 86400},
	}, nil)
	if err := ms.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, group.String()).Scan(&name); err != nil || name != "New name" {
		t.Errorf("blank rename applied: %q", name)
	}
	if settings, _ = ms.GetChatEphemeralSettings(group.String()); settings.Expiration != 0 {
		t.Errorf("ephemeral off: %+v", settings)
	}
}

func TestHandleEvent_SelfReadReceiptMarksChatRead(t *testing.T) {
	peer := types.NewJID("5511888888888", types.DefaultUserServer)
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
	t1 := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	if err := ms.StoreChat(peer.String(), "Peer", t2); err != nil {
		t.Fatal(err)
	}
	for id, ts := range map[string]time.Time{"m1": t1, "m2": t2} {
		if err := ms.StoreMessage(storedMessage{
			ID:         id,
			ChatJID:    peer.String(),
			Sender:     peer.User,
			Content:    "hi " + id,
			Timestamp:  ts,
			FileLength: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A peer's read receipt for our messages is not our read state.
	b.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: peer, Sender: peer}, Type: types.ReceiptTypeRead, MessageIDs: []types.MessageID{"m2"}, Timestamp: t2.Add(time.Hour)}, nil)
	var readAt *time.Time
	if err := ms.db.QueryRow(`SELECT last_read_time FROM chats WHERE jid = ?`, peer.String()).Scan(&readAt); err != nil {
		t.Fatal(err)
	}
	if readAt != nil {
		t.Errorf("peer receipt advanced our read marker to %v", readAt)
	}

	// Our own read (from another device) marks the chat read at the newest
	// acknowledged message's time, not the receipt's later timestamp.
	b.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: peer, IsFromMe: true}, Type: types.ReceiptTypeReadSelf, MessageIDs: []types.MessageID{"m1", "m2"}, Timestamp: t2.Add(time.Hour)}, nil)
	if err := ms.db.QueryRow(`SELECT last_read_time FROM chats WHERE jid = ?`, peer.String()).Scan(&readAt); err != nil {
		t.Fatal(err)
	}
	if readAt == nil || !readAt.Equal(t2) {
		t.Errorf("last_read_time = %v, want %v", readAt, t2)
	}
}

func TestHandleEvent_ConnectionEventsSignalReconnect(t *testing.T) {
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
	for _, evt := range []any{
		&events.Disconnected{},
		&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable},
		&events.StreamError{Code: "515"},
	} {
		reconnect := make(chan bool, 1)
		b.handleEvent(evt, reconnect)
		select {
		case <-reconnect:
		default:
			t.Errorf("%T did not signal reconnect", evt)
		}
		// A second signal while one is pending must not block the handler.
		reconnect <- true
		done := make(chan struct{})
		go func() { b.handleEvent(evt, reconnect); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%T blocked on a full reconnect channel", evt)
		}
	}

	b.handleEvent(&events.Connected{}, nil)
	if !strings.Contains(rec.String(), "Successfully connected") {
		t.Errorf("Connected should log the lifecycle line; got %s", rec.String())
	}
}

func TestHandleEvent_UndecryptableRemembersOriginalTime(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	orig := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	b.handleEvent(&events.UndecryptableMessage{Info: types.MessageInfo{ID: "u1", Timestamp: orig}}, nil)
	if got, ok := b.origTimes.take("u1"); !ok || !got.Equal(orig) {
		t.Errorf("origTimes.take = %v %v", got, ok)
	}
	if _, ok := b.origTimes.take("u1"); ok {
		t.Errorf("take must be one-shot")
	}
}

func TestHandleEvent_UnclaimedMediaRetryIsIgnored(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	b.handleEvent(&events.MediaRetry{MessageID: "nobody-waiting"}, nil) // must not panic or block
}

func TestReconnectLoop_BacksOffAndResetsOnSuccess(t *testing.T) {
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
	// This bridge's backoff, not the package's: the loop below reads it from
	// another goroutine (issue #382).
	b.ReconnectInitialBackoff, b.ReconnectMaxBackoff = 5*time.Millisecond, 20*time.Millisecond
	var dials atomic.Int32
	b.Connect = func() error {
		if dials.Add(1) < 3 {
			return errors.New("dns down")
		}
		return nil
	}

	reconnect := make(chan bool, 1)
	reconnect <- true
	done := make(chan struct{})
	go func() { b.reconnectLoop(reconnect); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	b.Shutdown(time.Second)
	<-done

	if got := dials.Load(); got != 3 {
		t.Fatalf("dials = %d, want 3 (two failures re-signal the loop, success stops it)", got)
	}
	if got := b.metrics.reconnects.Load(); got != 3 {
		t.Errorf("reconnects counter = %d, want 3", got)
	}
	if !strings.Contains(rec.String(), "Reconnection failed") || !strings.Contains(rec.String(), "Reconnected successfully") {
		t.Errorf("log lines: %s", rec.String())
	}
	select {
	case <-reconnect:
		t.Errorf("a successful dial must not queue another attempt")
	default:
	}
}

func TestHandleEvent_UnknownEventIsIgnored(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	b.handleEvent(&events.AppState{}, nil) // no case: must not panic
	b.handleEvent("not an event", nil)
}

// failMessageInserts makes every StoreMessage on ms fail the way a full disk
// would, while the rest of the store (chats, reads) keeps answering.
func failMessageInserts(t *testing.T, ms *MessageStore) {
	t.Helper()
	if _, err := ms.db.Exec(`CREATE TRIGGER fail_message_insert BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
}

// A message whose row could not be written has nothing for downloadMedia to
// look up: neither download path runs, the failure is one ERROR naming the
// message, and the webhook still carries it downstream (issue #454).
func TestHandleMessage_StoreFailureRunsNoMediaDownload(t *testing.T) {
	const caption = "a caption that must stay out of the error"
	cases := []struct {
		name    string
		webhook bool // the synchronous download for the payload, or the background queue
	}{
		{name: "image for the webhook payload", webhook: true},
		{name: "background caching", webhook: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var webhookCh <-chan WebhookPayload
			if tc.webhook {
				srv, ch := captureWebhook(t)
				t.Setenv("WEBHOOK_URL", srv.URL)
				webhookCh = ch
			} else {
				t.Setenv("WEBHOOK_ENABLED", "false")
			}
			msg := buildImageMessage(phonePN, phonePN, false, caption)
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
			msg.Message.ImageMessage.FileSHA256 = []byte("test-sha256")
			msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")
			msg.Info.ID = "LOST1"

			rec := installRecordingLogger(t)
			ms := newTestMessageStore(t)
			failMessageInserts(t, ms)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			var downloads atomic.Int32
			b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
				downloads.Add(1)
				return false, "", "", "", errors.New("failed to find message")
			}
			// No workers: a download queued by mistake stays countable.
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)

			b.handleMessage(msg)

			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("the failing store kept %d rows (err %v); the test proves nothing", rows, err)
			}
			if got := downloads.Load(); got != 0 {
				t.Errorf("synchronous downloads = %d, want 0", got)
			}
			if got := b.autoDownloads.queued(); got != 0 {
				t.Errorf("queued downloads = %d, want 0", got)
			}
			if got := b.metrics.messagesStored.Load(); got != 0 {
				t.Errorf("messagesStored = %d for a message that was not stored", got)
			}
			var errorLines []string
			for _, line := range strings.Split(rec.String(), "\n") {
				if strings.HasPrefix(line, "[ERROR]") {
					errorLines = append(errorLines, line)
				}
			}
			if len(errorLines) != 1 {
				t.Fatalf("want one ERROR line, got %d:\n%s", len(errorLines), rec.String())
			}
			if line := errorLines[0]; !strings.Contains(line, "LOST1") || !strings.Contains(line, phonePN.String()) {
				t.Errorf("the ERROR must name the message and the chat: %q", line)
			}
			// Not in the error, and not in the DEBUG echo either: that one is
			// for messages that were stored.
			if strings.Contains(rec.String(), caption) {
				t.Errorf("the content of a message that was not stored reached the log:\n%s", rec.String())
			}
			if !tc.webhook {
				return
			}
			select {
			case payload := <-webhookCh:
				if payload.MessageID != "LOST1" || payload.Content != caption || payload.MediaBase64 != "" {
					t.Errorf("webhook payload = %+v, want the message without media bytes", payload)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the webhook must still fire for a message that could not be stored")
			}
		})
	}
}

// resolveLIDChat falls back to the LID map like resolveUserJID does, and like
// it must survive a client that has none instead of dereferencing nil.
func TestResolveLIDChat_WithoutALIDStoreKeepsTheChat(t *testing.T) {
	installRecordingLogger(t) // the "could not resolve" warning is expected here
	clients := map[string]*whatsmeow.Client{
		"nil client":      nil,
		"no device store": {},
		"no LID store":    {Store: &store.Device{}},
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			if got := resolveLIDChat(client, phoneLID, types.EmptyJID, types.EmptyJID, false); got != phoneLID {
				t.Fatalf("resolveLIDChat() = %s, want the LID chat %s unchanged", got, phoneLID)
			}
		})
	}
}

// The history WhatsApp shares when a member is added to a group arrives as a
// message kind of its own. It is not decoded yet, but it must not vanish
// without a trace either: one INFO line and a counter, nothing stored, nothing
// downloaded, and none of the fields that locate or decrypt the blob in the
// log (issue #468).
func TestHandleMessage_SharedGroupHistoryIsLoggedAndCounted(t *testing.T) {
	const (
		receiver   = "15550001111@s.whatsapp.net"
		directPath = "/v/t62.0000-00/secret-direct-path"
		mediaKey   = "secret-media-key"
		fileHash   = "secret-file-hash"
	)
	meta := &waE2E.MessageHistoryMetadata{
		HistoryReceivers:               []string{receiver},
		NonHistoryReceivers:            []string{receiver, receiver},
		MessageCount:                   proto.Int64(42),
		OldestMessageTimestampInWindow: proto.Int64(1700000000),
		OldestMessageTimestampInBundle: proto.Int64(1700003600),
	}
	group := types.NewJID("120363000000000042", types.GroupServer)
	cases := map[string]*waE2E.Message{
		"bundle": {MessageHistoryBundle: &waE2E.MessageHistoryBundle{
			Mimetype:               proto.String("application/octet-stream"),
			DirectPath:             proto.String(directPath),
			MediaKey:               []byte(mediaKey),
			FileSHA256:             []byte(fileHash),
			FileEncSHA256:          []byte(fileHash),
			MessageHistoryMetadata: meta,
		}},
		"notice": {MessageHistoryNotice: &waE2E.MessageHistoryNotice{MessageHistoryMetadata: meta}},
	}
	for kind, message := range cases {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("WEBHOOK_ENABLED", "false")
			rec := installRecordingLogger(t)
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			var downloads atomic.Int32
			b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
				downloads.Add(1)
				return false, "", "", "", nil
			}
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)

			msg := buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			msg.Info.ID = "SHARE1"
			msg.Message = message
			b.handleMessage(msg)

			var lines []string
			for _, line := range strings.Split(rec.String(), "\n") {
				if strings.Contains(line, "Group history") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "[INFO]") {
				t.Fatalf("want one INFO line about the shared history, got %q", lines)
			}
			for _, want := range []string{
				"Group history " + kind + " seen in " + group.String(), "SHARE1", "from_me=false",
				"42 messages", "oldest in window 1700000000", "oldest in bundle 1700003600",
				"1 history receivers", "2 other receivers",
			} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("the line does not carry %q: %s", want, lines[0])
				}
			}
			// The identifiers as text, and the keys in every spelling a
			// careless %v, %x or base64 would give them.
			secrets := []string{receiver, "15550001111", directPath}
			for _, raw := range []string{mediaKey, fileHash} {
				secrets = append(secrets, raw, hex.EncodeToString([]byte(raw)),
					base64.StdEncoding.EncodeToString([]byte(raw)), fmt.Sprintf("%d", []byte(raw)))
			}
			for _, secret := range secrets {
				if strings.Contains(rec.String(), secret) {
					t.Errorf("%q reached the log:\n%s", secret, rec.String())
				}
			}
			if got := b.metrics.groupHistoryShares.Load(); got != 1 {
				t.Errorf("groupHistoryShares = %d, want 1", got)
			}
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 0 {
				t.Errorf("stored rows = %d (err %v), want none: the bundle is not decoded yet", rows, err)
			}
			if got := b.metrics.messagesStored.Load(); got != 0 {
				t.Errorf("messagesStored = %d, want 0", got)
			}
			if downloads.Load() != 0 || b.autoDownloads.queued() != 0 {
				t.Errorf("downloads run = %d, queued = %d; want none", downloads.Load(), b.autoDownloads.queued())
			}
		})
	}

	// A share without metadata says so instead of printing zeros that read
	// like an empty share at the epoch; our own copy is told apart.
	t.Run("no metadata, from us", func(t *testing.T) {
		t.Setenv("WEBHOOK_ENABLED", "false")
		rec := installRecordingLogger(t)
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
		msg := buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, true, "")
		msg.Message = &waE2E.Message{MessageHistoryNotice: &waE2E.MessageHistoryNotice{}}
		b.handleMessage(msg)
		if log := rec.String(); !strings.Contains(log, "from_me=true): no metadata;") || strings.Contains(log, "0 messages") {
			t.Errorf("want the absent metadata named as absent:\n%s", log)
		}
		if got := b.metrics.groupHistoryShares.Load(); got != 1 {
			t.Errorf("groupHistoryShares = %d, want 1", got)
		}
	})

	// An ordinary message is not a share.
	t.Run("a text message does not count", func(t *testing.T) {
		t.Setenv("WEBHOOK_ENABLED", "false")
		rec := installRecordingLogger(t)
		b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
		b.handleMessage(buildTextMessage(group, phonePN, types.EmptyJID, types.EmptyJID, false, "hello"))
		if got := b.metrics.groupHistoryShares.Load(); got != 0 || strings.Contains(rec.String(), "Group history") {
			t.Errorf("groupHistoryShares = %d for a text message; log:\n%s", got, rec.String())
		}
	})
}
