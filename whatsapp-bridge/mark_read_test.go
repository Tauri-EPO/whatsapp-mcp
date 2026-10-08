package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	markReadGroup = "120363012345678901@g.us"
	markReadDM    = "5511999999999@s.whatsapp.net"
)

// recordedReceipt is one call the handler made to whatsmeow's MarkRead.
type recordedReceipt struct {
	ids    []string
	chat   types.JID
	sender types.JID
	readAt time.Time
}

// markReadRecorder collects the receipts a handler sends and can be told to
// fail from a given call onwards.
type markReadRecorder struct {
	calls    []recordedReceipt
	failFrom int // 0 = never fail
}

func (rec *markReadRecorder) markRead(_ context.Context, ids []types.MessageID, readAt time.Time, chat, sender types.JID) error {
	if rec.failFrom > 0 && len(rec.calls) >= rec.failFrom-1 {
		return fmt.Errorf("receipt rejected")
	}
	plain := make([]string, len(ids))
	for i, id := range ids {
		plain[i] = string(id)
	}
	rec.calls = append(rec.calls, recordedReceipt{ids: plain, chat: chat, sender: sender, readAt: readAt})
	return nil
}

func (rec *markReadRecorder) allIDs() []string {
	var all []string
	for _, call := range rec.calls {
		all = append(all, call.ids...)
	}
	return all
}

// testResolveJID stands in for resolveRecipientJID: it accepts bare numbers and
// full JIDs, and rewrites one known phone number to a LID so the tests prove
// the receipt is addressed with the resolved form.
func testResolveJID(raw string) (types.JID, error) {
	jid, err := parseSenderJID(raw)
	if err != nil {
		return types.EmptyJID, err
	}
	if jid.User == "5511777777777" && jid.Server == types.DefaultUserServer {
		return types.NewJID("77770000000001", types.HiddenUserServer), nil
	}
	return jid, nil
}

func newMarkReadDeps(t *testing.T, store *MessageStore, rec *markReadRecorder) markReadDeps {
	t.Helper()
	return markReadDeps{
		store:     store,
		connected: func() bool { return true },
		resolve:   testResolveJID,
		markRead:  rec.markRead,
		log:       testLogger(),
	}
}

func seedMarkReadChat(t *testing.T, store *MessageStore, jid string, lastRead *time.Time) {
	t.Helper()
	var marker any
	if lastRead != nil {
		marker = dbTime(*lastRead)
	}
	if _, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, ?, ?)`,
		jid, "Test chat", dbTime(time.Now()), marker,
	); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
}

func seedMarkReadMessage(t *testing.T, store *MessageStore, chatJID, id, sender string, ts time.Time, isFromMe bool, deletedAt *time.Time) {
	t.Helper()
	var deleted any
	if deletedAt != nil {
		deleted = dbTime(*deletedAt)
	}
	if _, err := store.db.Exec(
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, "hello", dbTime(ts), isFromMe, deleted,
	); err != nil {
		t.Fatalf("seed message %s: %v", id, err)
	}
}

func postMarkRead(t *testing.T, deps markReadDeps, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mark-read", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	markReadHandler(deps)(w, req)
	return w
}

func decodeMarkRead(t *testing.T, w *httptest.ResponseRecorder) MarkReadResponse {
	t.Helper()
	var resp MarkReadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return resp
}

func chatReadMarker(t *testing.T, store *MessageStore, chatJID string) time.Time {
	t.Helper()
	var raw any
	if err := store.db.QueryRow(`SELECT last_read_time FROM chats WHERE jid = ?`, chatJID).Scan(&raw); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if raw == nil {
		return time.Time{}
	}
	return anchorTime(raw)
}

// TestMarkWholeGroupChatRead is the whole-chat form on a group with more than
// 500 unread messages from three senders: every inbound message up to `up_to`
// is acknowledged, in batches of markReadBatch, one participant per receipt,
// and nothing newer or outbound goes out.
func TestMarkWholeGroupChatRead(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadGroup, nil)

	senders := []string{"5511111111111", "5511222222222", "5511777777777"}
	const perSender = 174 // 522 inbound rows in total
	var lastPending time.Time
	for i := range perSender {
		for s, sender := range senders {
			ts := base.Add(time.Duration(i*len(senders)+s) * time.Second)
			seedMarkReadMessage(t, store, markReadGroup, fmt.Sprintf("IN-%s-%03d", sender, i), sender, ts, false, nil)
			lastPending = ts
		}
	}
	// Outbound, deleted and after-the-cut rows must all stay out of the receipts.
	seedMarkReadMessage(t, store, markReadGroup, "OUT-1", "me", base.Add(time.Second), true, nil)
	deletedAt := base.Add(2 * time.Second)
	seedMarkReadMessage(t, store, markReadGroup, "DEL-1", senders[0], base.Add(2*time.Second), false, &deletedAt)
	newer := lastPending.Add(time.Hour)
	seedMarkReadMessage(t, store, markReadGroup, "FUTURE-1", senders[0], newer, false, nil)

	rec := &markReadRecorder{}
	deps := newMarkReadDeps(t, store, rec)
	w := postMarkRead(t, deps, map[string]any{
		"chat_jid": markReadGroup,
		"up_to":    lastPending.Format(time.RFC3339),
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeMarkRead(t, w)
	wantMessages := perSender * len(senders)
	wantBatches := len(senders) * ((perSender + markReadBatch - 1) / markReadBatch)
	if !resp.Success || resp.Messages != wantMessages || resp.Senders != len(senders) || resp.Batches != wantBatches {
		t.Fatalf("unexpected response %+v (want %d messages, %d senders, %d batches)",
			resp, wantMessages, len(senders), wantBatches)
	}
	if resp.Truncated {
		t.Fatalf("expected truncated=false for %d messages", wantMessages)
	}

	if len(rec.calls) != wantBatches {
		t.Fatalf("expected %d receipts, got %d", wantBatches, len(rec.calls))
	}
	seen := map[string]bool{}
	for _, call := range rec.calls {
		if len(call.ids) > markReadBatch {
			t.Fatalf("receipt carried %d ids, batch is %d", len(call.ids), markReadBatch)
		}
		if call.chat.String() != markReadGroup {
			t.Fatalf("receipt addressed to %s, want %s", call.chat, markReadGroup)
		}
		if call.sender.IsEmpty() {
			t.Fatalf("group receipt without a participant: %+v", call)
		}
		for _, id := range call.ids {
			if !strings.HasPrefix(id, "IN-") {
				t.Fatalf("receipt carried non-inbound id %q", id)
			}
			if seen[id] {
				t.Fatalf("id %q acknowledged twice", id)
			}
			seen[id] = true
			// One participant per receipt, matching the messages it carries.
			if !strings.HasPrefix(id, "IN-"+call.sender.User) && call.sender.Server != types.HiddenUserServer {
				t.Fatalf("id %q sent with participant %s", id, call.sender)
			}
		}
	}
	if len(seen) != wantMessages {
		t.Fatalf("acknowledged %d distinct ids, want %d", len(seen), wantMessages)
	}
	if seen["FUTURE-1"] || seen["DEL-1"] || seen["OUT-1"] {
		t.Fatalf("acknowledged a message that should have been excluded: %v", seen)
	}

	// The phone-form sender the LID map knows must reach WhatsApp as the LID.
	var sawLID bool
	for _, call := range rec.calls {
		if call.sender.Server == types.HiddenUserServer {
			sawLID = true
		}
	}
	if !sawLID {
		t.Fatalf("expected one participant resolved to a LID, got %+v", rec.calls)
	}

	if marker := chatReadMarker(t, store, markReadGroup); !marker.Equal(lastPending) {
		t.Fatalf("read marker is %s, want %s", marker, lastPending)
	}
}

// TestMarkWholeDMReadUsesNoParticipant: a one-to-one receipt carries no
// participant, and messages the marker already covers are not re-sent.
func TestMarkWholeDMReadUsesNoParticipant(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	alreadyRead := base.Add(2 * time.Second)
	seedMarkReadChat(t, store, markReadDM, &alreadyRead)
	for i := range 5 {
		seedMarkReadMessage(t, store, markReadDM, fmt.Sprintf("DM-%d", i), "5511999999999", base.Add(time.Duration(i)*time.Second), false, nil)
	}

	rec := &markReadRecorder{}
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadDM})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected one receipt, got %d", len(rec.calls))
	}
	if !rec.calls[0].sender.IsEmpty() {
		t.Fatalf("DM receipt carried participant %s", rec.calls[0].sender)
	}
	// DM-0, DM-1 and DM-2 are at or before the existing marker.
	if got := rec.calls[0].ids; len(got) != 2 || got[0] != "DM-3" || got[1] != "DM-4" {
		t.Fatalf("acknowledged %v, want DM-3 and DM-4", got)
	}
	if resp := decodeMarkRead(t, w); resp.Messages != 2 || resp.Senders != 1 || resp.Batches != 1 {
		t.Fatalf("unexpected counts %+v", resp)
	}
}

// TestMarkWholeChatReadNothingPending: an already-read chat sends no receipt.
func TestMarkWholeChatReadNothingPending(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	read := base.Add(time.Hour)
	seedMarkReadChat(t, store, markReadDM, &read)
	seedMarkReadMessage(t, store, markReadDM, "DM-0", "5511999999999", base, false, nil)

	rec := &markReadRecorder{}
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadDM})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected no receipt, got %+v", rec.calls)
	}
	if resp := decodeMarkRead(t, w); !resp.Success || resp.Messages != 0 {
		t.Fatalf("unexpected response %+v", resp)
	}
	if marker := chatReadMarker(t, store, markReadDM); !marker.Equal(read) {
		t.Fatalf("read marker moved to %s, want %s", marker, read)
	}
}

// TestMarkWholeChatReadKeepsTiedTimestamps: the marker never lands inside a
// second that still holds an unacknowledged message. The next call selects with
// timestamp > marker, so it would skip that message for good.
func TestMarkWholeChatReadKeepsTiedTimestamps(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadGroup, nil)
	// M1 and M2 share a second; M1 and M3 are the first sender, M2 the second,
	// so the failing receipt is the one covering M2.
	seedMarkReadMessage(t, store, markReadGroup, "M1", "5511111111111", base, false, nil)
	seedMarkReadMessage(t, store, markReadGroup, "M2", "5511222222222", base, false, nil)
	seedMarkReadMessage(t, store, markReadGroup, "M3", "5511111111111", base.Add(time.Second), false, nil)

	rec := &markReadRecorder{failFrom: 2}
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadGroup})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if got := rec.allIDs(); len(got) != 2 || got[0] != "M1" || got[1] != "M3" {
		t.Fatalf("acknowledged %v, want the first sender's two messages", got)
	}
	if resp := decodeMarkRead(t, w); resp.Messages != 0 {
		t.Fatalf("marker covered %d message(s), want 0: M1 shares its second with M2", resp.Messages)
	}
	if marker := chatReadMarker(t, store, markReadGroup); !marker.IsZero() {
		t.Fatalf("read marker moved to %s, which would skip M2 on the next call", marker)
	}
}

// TestMarkWholeChatReadSkipsUnaddressableRows: history-sync rows attributed to
// the group itself get no receipt, but they must not pin the read marker in
// front of the rest of the chat.
func TestMarkWholeChatReadSkipsUnaddressableRows(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadGroup, nil)
	seedMarkReadMessage(t, store, markReadGroup, "H1", "120363012345678901", base, false, nil)
	seedMarkReadMessage(t, store, markReadGroup, "H2", "", base.Add(time.Second), false, nil)
	last := base.Add(2 * time.Second)
	seedMarkReadMessage(t, store, markReadGroup, "M1", "5511111111111", last, false, nil)

	rec := &markReadRecorder{}
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadGroup})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := rec.allIDs(); len(got) != 1 || got[0] != "M1" {
		t.Fatalf("acknowledged %v, want only the addressable message", got)
	}
	if resp := decodeMarkRead(t, w); resp.Messages != 3 || resp.Senders != 1 {
		t.Fatalf("unexpected counts %+v: the marker should cover all three rows", resp)
	}
	if marker := chatReadMarker(t, store, markReadGroup); !marker.Equal(last) {
		t.Fatalf("read marker is %s, want %s", marker, last)
	}
}

// TestMarkWholeChatReadPartialFailure: when a receipt fails, the marker only
// advances over the uninterrupted prefix that was acknowledged.
func TestMarkWholeChatReadPartialFailure(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadDM, nil)
	total := markReadBatch + 5
	for i := range total {
		seedMarkReadMessage(t, store, markReadDM, fmt.Sprintf("DM-%03d", i), "5511999999999", base.Add(time.Duration(i)*time.Second), false, nil)
	}

	rec := &markReadRecorder{failFrom: 2} // first batch lands, second fails
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadDM})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeMarkRead(t, w)
	if resp.Success || resp.Messages != markReadBatch || resp.Batches != 1 {
		t.Fatalf("unexpected response %+v", resp)
	}
	wantMarker := base.Add(time.Duration(markReadBatch-1) * time.Second)
	if marker := chatReadMarker(t, store, markReadDM); !marker.Equal(wantMarker) {
		t.Fatalf("read marker is %s, want %s (the acknowledged prefix)", marker, wantMarker)
	}
	if got := len(rec.allIDs()); got != markReadBatch {
		t.Fatalf("acknowledged %d ids, want %d", got, markReadBatch)
	}
}

// TestMarkWholeChatReadTruncates: a backlog larger than markReadMaxMessages is
// marked in one capped pass and reported as truncated.
func TestMarkWholeChatReadTruncates(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadDM, nil)
	for i := range markReadMaxMessages + 10 {
		seedMarkReadMessage(t, store, markReadDM, fmt.Sprintf("DM-%05d", i), "5511999999999", base.Add(time.Duration(i)*time.Second), false, nil)
	}

	rec := &markReadRecorder{}
	w := postMarkRead(t, newMarkReadDeps(t, store, rec), map[string]any{"chat_jid": markReadDM})

	resp := decodeMarkRead(t, w)
	if !resp.Truncated || resp.Messages != markReadMaxMessages {
		t.Fatalf("unexpected response %+v", resp)
	}
	if got := len(rec.allIDs()); got != markReadMaxMessages {
		t.Fatalf("acknowledged %d ids, want the %d cap", got, markReadMaxMessages)
	}
}

// TestMarkWholeChatReadRejects covers the request shapes that must fail before
// any receipt leaves the bridge.
func TestMarkWholeChatReadRejects(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"no chat", map[string]any{"up_to": "2026-09-01T08:00:00Z"}, http.StatusBadRequest},
		{"sender without ids", map[string]any{"chat_jid": markReadDM, "sender_jid": "5511999999999"}, http.StatusBadRequest},
		{"bad up_to", map[string]any{"chat_jid": markReadDM, "up_to": "yesterday"}, http.StatusBadRequest},
		{"up_to with ids", map[string]any{"chat_jid": markReadDM, "message_ids": []string{"DM-0"}, "up_to": "2026-09-01T08:00:00Z"}, http.StatusBadRequest},
		{"bad chat", map[string]any{"chat_jid": "not-a-jid"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMessageStore(t)
			seedMarkReadChat(t, store, markReadDM, nil)
			seedMarkReadMessage(t, store, markReadDM, "DM-0", "5511999999999", time.Now().Add(-time.Minute), false, nil)
			rec := &markReadRecorder{}
			w := postMarkRead(t, newMarkReadDeps(t, store, rec), tc.body)
			if w.Code != tc.code {
				t.Fatalf("expected %d, got %d: %s", tc.code, w.Code, w.Body.String())
			}
			if len(rec.calls) != 0 {
				t.Fatalf("expected no receipt, got %+v", rec.calls)
			}
		})
	}
}

// TestMarkWholeChatReadChatPolicy: the allow-list denies the whole-chat form
// the same way it denies the listed one, before the archive is even read.
func TestMarkWholeChatReadChatPolicy(t *testing.T) {
	store := newTestMessageStore(t)
	seedMarkReadChat(t, store, markReadDM, nil)
	seedMarkReadMessage(t, store, markReadDM, "DM-0", "5511999999999", time.Now().Add(-time.Minute), false, nil)

	rec := &markReadRecorder{}
	deps := newMarkReadDeps(t, store, rec)
	deps.policy = parseChatPolicy("5511888888888@s.whatsapp.net")

	for _, body := range []map[string]any{
		{"chat_jid": markReadDM},
		{"chat_jid": markReadDM, "message_ids": []string{"DM-0"}},
	} {
		w := postMarkRead(t, deps, body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %v, got %d: %s", body, w.Code, w.Body.String())
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected no receipt, got %+v", rec.calls)
	}
	if marker := chatReadMarker(t, store, markReadDM); !marker.IsZero() {
		t.Fatalf("read marker moved to %s on a denied chat", marker)
	}
}

// TestMarkListedMessagesRead: the explicit-IDs form still sends one receipt and
// still refuses IDs that are not inbound messages of that chat and sender.
func TestMarkListedMessagesRead(t *testing.T) {
	store := newTestMessageStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	seedMarkReadChat(t, store, markReadGroup, nil)
	seedMarkReadMessage(t, store, markReadGroup, "IN-1", "5511111111111", base, false, nil)
	seedMarkReadMessage(t, store, markReadGroup, "IN-2", "5511111111111", base.Add(time.Second), false, nil)
	seedMarkReadMessage(t, store, markReadGroup, "OUT-1", "me", base.Add(2*time.Second), true, nil)

	rec := &markReadRecorder{}
	deps := newMarkReadDeps(t, store, rec)
	w := postMarkRead(t, deps, map[string]any{
		"chat_jid":    markReadGroup,
		"message_ids": []string{"IN-1", "IN-2"},
		"sender_jid":  "5511111111111@s.whatsapp.net",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 || len(rec.calls[0].ids) != 2 {
		t.Fatalf("expected one receipt with two ids, got %+v", rec.calls)
	}
	if resp := decodeMarkRead(t, w); resp.Messages != 2 || resp.Senders != 1 || resp.Batches != 1 {
		t.Fatalf("unexpected counts %+v", resp)
	}
	if marker := chatReadMarker(t, store, markReadGroup); !marker.Equal(base.Add(time.Second)) {
		t.Fatalf("read marker is %s, want the newest listed message", marker)
	}

	rec.calls = nil
	w = postMarkRead(t, deps, map[string]any{
		"chat_jid":    markReadGroup,
		"message_ids": []string{"OUT-1"},
		"sender_jid":  "5511111111111@s.whatsapp.net",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an outbound id, got %d: %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected no receipt, got %+v", rec.calls)
	}
}

// TestMarkReadNotConnected: neither form touches the archive while the client
// is offline.
func TestMarkReadNotConnected(t *testing.T) {
	store := newTestMessageStore(t)
	seedMarkReadChat(t, store, markReadDM, nil)
	seedMarkReadMessage(t, store, markReadDM, "DM-0", "5511999999999", time.Now().Add(-time.Minute), false, nil)

	rec := &markReadRecorder{}
	deps := newMarkReadDeps(t, store, rec)
	deps.connected = func() bool { return false }

	for _, body := range []map[string]any{
		{"chat_jid": markReadDM},
		{"chat_jid": markReadDM, "message_ids": []string{"DM-0"}},
	} {
		w := postMarkRead(t, deps, body)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for %v, got %d: %s", body, w.Code, w.Body.String())
		}
	}
	if marker := chatReadMarker(t, store, markReadDM); !marker.IsZero() {
		t.Fatalf("read marker moved to %s while disconnected", marker)
	}
}
