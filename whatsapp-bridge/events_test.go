package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
	b := testBridge(newTestClientWithSelf(&mockLIDStore{}, self), ms, installRecordingLogger(t))
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
	b := testBridge(newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
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
	b := testBridge(newTestClient(&mockLIDStore{}), ms, installRecordingLogger(t))
	t1 := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	if err := ms.StoreChat(peer.String(), "Peer", t2); err != nil {
		t.Fatal(err)
	}
	for id, ts := range map[string]time.Time{"m1": t1, "m2": t2} {
		if err := ms.StoreMessage(id, peer.String(), peer.User, "hi "+id, ts, false, "", "", "", nil, nil, nil, 0, ""); err != nil {
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
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
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
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
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
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	b.handleEvent(&events.MediaRetry{MessageID: "nobody-waiting"}, nil) // must not panic or block
}

func TestReconnectLoop_BacksOffAndResetsOnSuccess(t *testing.T) {
	prevInit, prevMax := reconnectInitialBackoff, reconnectMaxBackoff
	reconnectInitialBackoff, reconnectMaxBackoff = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reconnectInitialBackoff, reconnectMaxBackoff = prevInit, prevMax })

	rec := installRecordingLogger(t)
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
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
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
	b.handleEvent(&events.AppState{}, nil) // no case: must not panic
	b.handleEvent("not an event", nil)
}
