package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestReviewRejectedSendsPreserveBudget(t *testing.T) {
	for _, kind := range []string{"invalid", "caption", "denied", "sdk_preflight", "timeout", "dry_run"} {
		t.Run(kind, func(t *testing.T) {
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
			b.RuntimeDefaults, _ = runtimeDefaults(func(string) string { return "" })
			limitPatch(t, b, `{"send.rate_per_day":20,"send.rate_per_minute":20,"send.new_chats_per_day":1,"send.min_interval_ms":1000}`)
			b.Send = b.sendBackend()
			path := filepath.Join(t.TempDir(), "fake.webp")
			if err := os.WriteFile(path, []byte("JPEG renamed to webp"), 0600); err != nil {
				t.Fatal(err)
			}
			b.MediaRoots = []string{filepath.Dir(path)}
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				t.Error("invalid sticker uploaded")
				return whatsmeow.UploadResponse{}, nil
			}
			sendErr := whatsmeow.ErrNoPermissionInGroup
			if kind == "sdk_preflight" {
				sendErr = whatsmeow.ErrNotLoggedIn
			}
			if kind == "timeout" {
				sendErr = whatsmeow.ErrMessageTimedOut
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				return whatsmeow.SendResponse{}, fmt.Errorf("wrapped: %w", sendErr)
			}
			for i := range 20 {
				req := SendMessageRequest{Recipient: testGroupJID, Message: "hello"}
				want := 403
				switch kind {
				case "invalid", "caption", "dry_run":
					req.AsSticker, req.MediaPath = true, path
					if kind != "caption" {
						req.Message = ""
					}
					req.DryRun = kind == "dry_run"
					want = 400
				case "sdk_preflight", "timeout":
					want = 500
				}
				body, _ := json.Marshal(req)
				w := httptest.NewRecorder()
				b.newRESTMux(8080, sendRecipientToken).ServeHTTP(w, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
				if kind == "timeout" && i > 0 {
					break
				}
				if w.Code != want {
					t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body)
				}
			}
			u, err := b.sendUsageSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "timeout" {
				if u.Today != 1 || u.NewChatsToday != 1 {
					t.Fatalf("ambiguous delivery refunded: %+v", u)
				}
				return
			}
			if u.Today != 0 || u.NewChatsToday != 0 {
				t.Fatalf("refused sends consumed quota: %+v", u)
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				return whatsmeow.SendResponse{ID: "AFTERREFUSAL", Timestamp: time.Now()}, nil
			}
			if w := limitSend(t, b, "120363000000000002@g.us", false); w.Code != 200 {
				t.Fatalf("first-contact/interval still reserved: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestReviewApprovalDoesNotBlockEvents(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		close(started)
		<-release
		return nil, nil
	}
	defer close(release)
	go func() {
		b.handleEvent(&events.GroupInfo{JID: types.NewJID("120363000000000001", types.GroupServer), MembershipApprovalMode: &types.GroupMembershipApprovalMode{}}, nil)
		close(returned)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no refresh requested")
	}
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("event dispatcher blocked on group fetch")
	}
}

func TestReviewGroupEventUsesOnePolicySnapshot(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	reads := 0
	b.Policy = chatPolicyHandle{resolve: func() chatPolicy { reads++; return chatPolicy{} }}
	b.handleEvent(&events.GroupInfo{JID: types.NewJID("120363000000000001", types.GroupServer), Announce: &types.GroupAnnounce{IsAnnounce: true}}, nil)
	if reads != 1 {
		t.Fatalf("policy read %d times", reads)
	}
}

func TestReviewRosterRefreshSeedsSettings(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			if err := b.Store.StoreChat(testGroupJID, "Fake group", time.Now()); err != nil {
				t.Fatal(err)
			}
			info := fakeGroup()
			info.IsAnnounce, info.IsLocked, info.IsJoinApprovalRequired, info.Topic = true, true, true, "Snapshot topic"
			b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { return info, nil }
			before := time.Now().Truncate(time.Second)
			if live {
				w := httptest.NewRecorder()
				handleGroupMembers(groupInfoFetcher(b.Store.groupInfo), nil, nil, chatPolicy{}, b.recordGroupRoster)(w, httptest.NewRequest("GET", "/api/group/members?jid="+testGroupJID, nil))
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			} else {
				b.refreshGroupRoster(context.Background(), testGroupJID, info.JID, time.Now())
			}
			var announce, locked, approval bool
			var topic, stamp string
			if err := b.Store.db.QueryRow(`SELECT group_announce,group_locked,group_join_approval_required,group_topic,CAST(group_topic_updated_at AS TEXT) FROM chats WHERE jid=?`, testGroupJID).Scan(&announce, &locked, &approval, &topic, &stamp); err != nil {
				t.Fatal(err)
			}
			at, err := parseDBTime(stamp)
			if !announce || !locked || !approval || topic != "Snapshot topic" || err != nil || at.Before(before) {
				t.Fatalf("settings=%v/%v/%v/%q at=%s err=%v", announce, locked, approval, topic, stamp, err)
			}
		})
	}
}

func TestReviewRosterRechecksPolicyAfterFetch(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	var denied atomic.Bool
	b.Policy = chatPolicyHandle{resolve: func() chatPolicy {
		if denied.Load() {
			return parseChatPolicy("120363000000000002@g.us")
		}
		return chatPolicy{}
	}}
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		denied.Store(true) // Simulate the operator narrowing policy during I/O.
		return fakeGroup(), nil
	}
	b.refreshGroupRoster(context.Background(), testGroupJID, fakeGroup().JID, time.Now())
	var count int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if got := len(readMembers(t, b.Store, testGroupJID)); got != 0 {
		t.Fatalf("denied roster persisted: %d", got)
	}
}

func TestReviewNilGroupInfoLogsGroup(t *testing.T) {
	logger := &recordingLogger{}
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), logger)
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { return nil, nil }
	if b.refreshGroupRoster(context.Background(), testGroupJID, fakeGroup().JID, time.Now()) {
		t.Fatal("nil refresh succeeded")
	}
	log := logger.String()
	if !strings.Contains(log, testGroupJID) || !strings.Contains(log, "response was nil") || strings.Contains(log, "<nil>") {
		t.Fatal(log)
	}
}

func TestReviewReservationReleasePreservesLaterSend(t *testing.T) {
	b := newSettingsBridge(t)
	limitPatch(t, b, `{"send.rate_per_day":20,"send.new_chats_per_day":2}`)
	first, later := withSendReservation(context.Background()), withSendReservation(context.Background())
	for i, ctx := range []context.Context{first, later} {
		if reason, _, err := b.reserveSend(ctx, []string{fmt.Sprintf("12036300000000000%d@g.us", i+1)}); err != nil || reason != "" {
			t.Fatalf("reason=%s err=%v", reason, err)
		}
	}
	b.releaseSend(first)
	b.releaseSend(first) // A duplicate cleanup cannot refund another send.
	u, err := b.sendUsageSnapshot(context.Background())
	if err != nil || u.Today != 1 || u.NewChatsToday != 2 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
	var last int64
	if err := b.Store.db.QueryRow("SELECT last_send_ns FROM send_usage").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != later.Value(sendReservationKey{}).(*sendReservation).last {
		t.Fatal("later interval overwritten")
	}
}

func TestReviewForwardRefusalReleasesBudget(t *testing.T) {
	b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
	b.RuntimeDefaults, _ = runtimeDefaults(func(string) string { return "" })
	limitPatch(t, b, `{"send.rate_per_day":20,"send.new_chats_per_day":1,"send.min_interval_ms":1000}`)
	b.Send = b.sendBackend()
	b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{}, whatsmeow.ErrNoPermissionInGroup
	}
	if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me) VALUES ('SOURCE',?,'100000000001','forward text',?,0)`, testGroupJID, dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"chat_jid":%q,"message_id":"SOURCE","to_chat_jid":"120363000000000002@g.us"}`, testGroupJID)
		b.newRESTMux(8080, sendRecipientToken).ServeHTTP(w, seamRequest("POST", "/api/forward", body, sendRecipientToken))
		if w.Code != 403 || !strings.Contains(w.Body.String(), groupPermissionCode) {
			t.Fatalf("forward %d: %d %s", i, w.Code, w.Body)
		}
	}
	u, err := b.sendUsageSnapshot(context.Background())
	if err != nil || u.Today != 0 || u.NewChatsToday != 0 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
}

func TestReviewLiveRosterKeepsResolvedNames(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	info := fakeGroup()
	for i := range info.Participants {
		info.Participants[i].DisplayName = ""
	}
	w := httptest.NewRecorder()
	handleGroupMembers(func(context.Context, types.JID) (*types.GroupInfo, error) { return info, nil }, func(types.JID) string { return "Resolved contact" }, nil, chatPolicy{}, b.recordGroupRoster)(w, httptest.NewRequest("GET", "/api/group/members?jid="+testGroupJID, nil))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var count int
	if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE name='Resolved contact'`).Scan(&count); err != nil || count == 0 {
		t.Fatalf("resolved names discarded: %d err=%v", count, err)
	}
}
