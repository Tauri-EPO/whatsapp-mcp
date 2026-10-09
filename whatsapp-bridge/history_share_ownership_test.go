package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistoryShareOwnershipRelativeToReceiver(t *testing.T) {
	selfPN := types.NewJID("5511888888888", types.DefaultUserServer)
	selfLID := types.NewJID("100000000000006", types.HiddenUserServer)
	unknownLID := types.NewJID("100000000000009", types.HiddenUserServer)
	cases := []struct {
		name, participant         string
		fromMe, keyOnly, wantSkip bool
		wantSender, wantServer    string
	}{
		{"peer phone", phonePN.String(), true, false, false, phonePN.User, types.DefaultUserServer},
		{"peer LID", phoneLID.String(), true, false, false, phonePN.User, types.DefaultUserServer},
		{"own phone", selfPN.String(), false, false, true, selfPN.User, types.DefaultUserServer},
		{"own LID", selfLID.String(), false, false, true, selfPN.User, types.DefaultUserServer},
		{"own phone device", selfPN.User + ":3@s.whatsapp.net", true, false, true, "", ""},
		{"own LID device", selfLID.User + ":3@lid", true, false, true, "", ""},
		{"own hosted phone", selfPN.User + "@hosted", false, false, true, "", ""},
		{"own hosted LID", selfLID.User + "@hosted.lid", false, false, true, "", ""},
		{"own alias LID", unknownLID.String(), false, false, true, "", ""},
		{"own alias PN", phonePN.String(), false, false, true, "", ""},
		{"own key participant", selfPN.String(), false, true, true, "", ""},
		{"conflicting own key", phonePN.String(), true, false, true, "", ""},
		{"unmapped LID", unknownLID.String(), true, false, false, unknownLID.User, types.HiddenUserServer},
		{"missing participant", "", true, false, false, "120363000000000001", ""},
		{"malformed participant", selfPN.String() + "@lid", true, false, false, "120363000000000001", ""},
		{"key participant", phonePN.String(), true, true, false, phonePN.User, types.DefaultUserServer},
		{"namespace collision", selfPN.User + "@lid", true, false, false, selfPN.User, types.HiddenUserServer},
	}
	for _, history := range []bool{false, true} {
		for _, sample := range cases {
			t.Run(map[bool]string{false: "live", true: "history"}[history]+"/"+sample.name, func(t *testing.T) {
				ms := newTestMessageStore(t)
				lids := &mockLIDStore{lidByPN: map[types.JID]types.JID{selfPN: selfLID, phonePN: phoneLID}, pnByLID: map[types.JID]types.JID{selfLID: selfPN, phoneLID: phonePN}}
				if sample.name == "own alias LID" {
					lids.pnByLID[unknownLID] = selfPN
				}
				if sample.name == "own alias PN" {
					lids.lidByPN[phonePN] = selfLID
				}
				client := newTestClientWithSelf(lids, selfPN)
				client.Store.LID = selfLID
				recorder := installRecordingLogger(t)
				b := testBridge(t, client, ms, recorder)
				fixture := shareHistoryFixture(1)
				original := fixture.Data.Conversations[0].Messages[0].Message
				original.Key.FromMe = proto.Bool(sample.fromMe)
				if sample.keyOnly {
					original.Key.Participant = proto.String(sample.participant)
				} else {
					original.Participant = proto.String(sample.participant)
				}
				if sample.name == "conflicting own key" {
					original.Key.Participant = proto.String(selfPN.String())
				}
				plain, err := proto.Marshal(fixture.Data)
				if err != nil {
					t.Fatal(err)
				}
				bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
				message := &waE2E.Message{MessageHistoryBundle: bundle}
				const chat = "120363000000000001@g.us"
				if history {
					outer := shareHistoryFixture(1)
					outer.Data.Conversations[0].Messages[0].Message.Message = message
					b.handleHistorySync(outer)
					waitHistoryShares(t, b)
				} else {
					event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Message = message
					b.handleMessage(event)
					waitHistoryShares(t, b)
				}
				own, err := ms.GetMessageIsFromMe("H0", chat)
				if sample.wantSkip {
					var rows int
					if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='H0' AND chat_jid=?", chat).Scan(&rows); err != nil || rows != 0 || requests.Load() != 1 || !strings.Contains(recorder.String(), "skipped 1 receiver-attributed rows") {
						t.Fatalf("owner row was imported: rows=%d HTTP=%d err=%v log=%s", rows, requests.Load(), err, recorder.String())
					}
				} else if err != nil || own == nil {
					t.Fatalf("import missing: own=%v err=%v", own, err)
				} else {
					sender, server := querySenderServer(t, ms, "H0", chat)
					if requests.Load() != 1 || sender != sample.wantSender || server != sample.wantServer || *own {
						t.Errorf("peer ownership used instead of receiver identity: HTTP=%d sender=%s/%s own=%t", requests.Load(), sender, server, *own)
					}
				}
				calls := 0
				edit := handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) (int64, error) {
					calls++
					return time.Now().UnixMilli(), nil
				}, chatPolicy{}, b.storeLive)
				rec := httptest.NewRecorder()
				edit(rec, httptest.NewRequest(http.MethodPost, "/api/edit", strings.NewReader(`{"chat_jid":"120363000000000001@g.us","message_id":"H0","text":"Edited"}`)))
				wantStatus, wantCalls := http.StatusForbidden, 0
				if sample.wantSkip {
					wantStatus = http.StatusNotFound
				}
				if rec.Code != wantStatus || calls != wantCalls {
					t.Fatalf("ownership boundary status=%d edits=%d", rec.Code, calls)
				}
			})
		}
	}
}
