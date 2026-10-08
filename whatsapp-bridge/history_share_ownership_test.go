package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistoryShareOwnershipRelativeToReceiver(t *testing.T) {
	selfPN := types.NewJID("5511888888888", types.DefaultUserServer)
	selfLID := types.NewJID("100000000000006", types.HiddenUserServer)
	unknownLID := types.NewJID("100000000000009", types.HiddenUserServer)
	cases := []struct {
		name, participant        string
		fromMe, keyOnly, wantOwn bool
		wantSender, wantServer   string
	}{
		{"peer phone", phonePN.String(), true, false, false, phonePN.User, types.DefaultUserServer},
		{"peer LID", phoneLID.String(), true, false, false, phonePN.User, types.DefaultUserServer},
		{"own phone", selfPN.String(), false, false, true, selfPN.User, types.DefaultUserServer},
		{"own LID", selfLID.String(), false, false, true, selfPN.User, types.DefaultUserServer},
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
				client := newTestClientWithSelf(lids, selfPN)
				client.Store.LID = selfLID
				b := testBridge(t, client, ms, testLogger())
				fixture := shareHistoryFixture(1)
				original := fixture.Data.Conversations[0].Messages[0].Message
				original.Key.FromMe = proto.Bool(sample.fromMe)
				if sample.keyOnly {
					original.Key.Participant = proto.String(sample.participant)
				} else {
					original.Participant = proto.String(sample.participant)
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
				} else {
					event := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Message = message
					b.handleMessage(event)
				}
				sender, server := querySenderServer(t, ms, "H0", chat)
				own, err := ms.GetMessageIsFromMe("H0", chat)
				if err != nil || own == nil {
					t.Fatalf("import missing: own=%v err=%v", own, err)
				}
				if requests.Load() != 1 || sender != sample.wantSender || server != sample.wantServer || *own != sample.wantOwn {
					t.Errorf("peer ownership used instead of receiver identity: HTTP=%d sender=%s/%s own=%t", requests.Load(), sender, server, *own)
				}
				calls := 0
				edit := handleEditMessage(ms, func(context.Context, types.JID, types.MessageID, string) error { calls++; return nil }, chatPolicy{}, b.storeLive)
				rec := httptest.NewRecorder()
				edit(rec, httptest.NewRequest(http.MethodPost, "/api/edit", strings.NewReader(`{"chat_jid":"120363000000000001@g.us","message_id":"H0","text":"Edited"}`)))
				wantStatus, wantCalls := http.StatusForbidden, 0
				if sample.wantOwn {
					wantStatus, wantCalls = http.StatusOK, 1
				}
				if rec.Code != wantStatus || calls != wantCalls {
					t.Fatalf("ownership boundary status=%d edits=%d", rec.Code, calls)
				}
			})
		}
	}
}
