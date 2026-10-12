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

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type historyTwinLIDs struct {
	mockLIDStore
	calls int
	err   error
}

func (s *historyTwinLIDs) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	s.calls++
	if s.err != nil {
		return types.EmptyJID, s.err
	}
	return s.mockLIDStore.GetLIDForPN(ctx, pn)
}

func (s *historyTwinLIDs) GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	s.calls++
	return s.mockLIDStore.GetPNForLID(ctx, lid)
}

func TestHistoryTwinSendOutcomes(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	for _, repair := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			chat, twin types.JID
			lookupErr  bool
			failSend   int
			wantCalls  int
			status     int
			wantText   string
		}{
			{"phone and LID", phonePN, phoneLID, false, 0, 2, 200, "Sent 2 history request(s) under phone, LID spelling(s)"},
			{"LID and phone", phoneLID, phonePN, false, 0, 2, 200, "Sent 2 history request(s) under LID, phone spelling(s)"},
			{"no twin", phonePN, types.EmptyJID, false, 0, 1, 200, "Sent 1 history request(s) under phone spelling(s)"},
			{"lookup fails", phonePN, types.EmptyJID, true, 0, 1, 200, "Sent 1 history request(s) under phone spelling(s)"},
			{"group unchanged", group, phoneLID, false, 0, 1, 200, "Sent 1 history request(s) under group spelling(s)"},
			{"wrong namespace ignored", phonePN, group, false, 0, 1, 200, "Sent 1 history request(s) under phone spelling(s)"},
			{"second failure", phonePN, phoneLID, false, 2, 2, 200, "The optional twin request could not be sent"},
			{"first failure", phonePN, phoneLID, false, 1, 1, 500, "Failed to send history request"},
		} {
			t.Run(fmt.Sprintf("%s/repair=%v", tc.name, repair), func(t *testing.T) {
				ms := newTestMessageStore(t)
				ts := time.Now().Truncate(time.Second)
				if err := ms.StoreMessage(storedMessage{ID: "TARGET", ChatJID: tc.chat.String(), Content: "hello", Timestamp: ts}); err != nil {
					t.Fatal(err)
				}
				lids := &historyTwinLIDs{mockLIDStore: mockLIDStore{
					lidByPN: map[types.JID]types.JID{tc.chat: tc.twin},
					pnByLID: map[types.JID]types.JID{tc.chat: tc.twin},
				}}
				if tc.lookupErr {
					lids.err = errors.New("local mapping unavailable")
				}
				client := newTestClientWithSelf(lids, selfPhone)
				calls := 0
				mux := http.NewServeMux()
				registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, client,
					func() bool { return true }, ms, parseChatPolicy(tc.chat.String()), func(_ context.Context, msg *waE2E.Message) error {
						calls++
						wantChat := tc.chat
						if calls == 2 {
							wantChat = tc.twin
							if wantChat.Server == types.GroupServer || tc.chat.Server == types.GroupServer {
								t.Fatal("group used as a twin")
							}
						}
						anchor := msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
						wantID, wantTime := "TARGET", ts.Unix()
						if repair {
							wantID, wantTime = "", ts.Add(time.Second).Unix()
						}
						if anchor.GetChatJID() != wantChat.String() || anchor.GetOldestMsgID() != wantID ||
							anchor.GetOldestMsgTimestampMS() != wantTime || anchor.GetOnDemandMsgCount() != maxHistoryCount {
							t.Fatalf("request %d = %v", calls, anchor)
						}
						if calls == tc.failSend {
							return fmt.Errorf("send refused for %s", tc.twin)
						}
						return nil
					})
				req := HistoryRequest{ChatJID: tc.chat.String(), Count: 9999}
				if repair {
					req.BeforeMessageID = proto.String("TARGET")
				}
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(string(body))))
				if calls != tc.wantCalls || rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantText) {
					t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body.String())
				}
				if !tc.twin.IsEmpty() && strings.Contains(rec.Body.String(), tc.twin.String()) {
					t.Fatal("response exposed the discovered twin")
				}
				if tc.failSend == 2 && !strings.Contains(rec.Body.String(), "Sent 1 history request(s) under phone spelling(s)") {
					t.Fatal("failed second send counted as sent")
				}
			})
		}
	}
}

func TestHistoryTwinDeniedBeforeLookup(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, denial := range []string{"chat allow-list", "tool deny", "tool allow", "read only"} {
			t.Run(fmt.Sprintf("%s/repair=%v", denial, repair), func(t *testing.T) {
				lids := &historyTwinLIDs{}
				client := newTestClientWithSelf(lids, selfPhone)
				policy := chatPolicy{}
				tools := toolPolicy{}
				switch denial {
				case "chat allow-list":
					policy = parseChatPolicy(selfPhone.String())
				case "tool deny":
					tools.deny = map[string]bool{"request_history": true}
				case "tool allow":
					tools.allow = map[string]bool{"send_message": true}
				}
				mux := http.NewServeMux()
				registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc {
					return (readOnlyPolicy{enabled: denial == "read only"}).guard(tools.guard(h))
				}, client, func() bool { t.Fatal("denied request checked connection"); return true }, nil, policy,
					func(context.Context, *waE2E.Message) error { t.Fatal("denied request sent"); return nil })
				body := fmt.Sprintf(`{"chat_jid":%q}`, phonePN.String())
				if repair {
					body = fmt.Sprintf(`{"chat_jid":%q,"before_message_id":"TARGET"}`, phonePN.String())
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(body)))
				if rec.Code != http.StatusForbidden || lids.calls != 0 {
					t.Fatalf("denied status=%d mapping reads=%d", rec.Code, lids.calls)
				}
			})
		}
	}
}

func TestHistoryTwinReplyRepairsStoredSpelling(t *testing.T) {
	for _, storedChat := range []types.JID{phonePN, phoneLID} {
		t.Run(storedChat.Server, func(t *testing.T) {
			ms := newTestMessageStore(t)
			client := newTestClientWithSelf(&mockLIDStore{
				lidByPN: map[types.JID]types.JID{phonePN: phoneLID},
				pnByLID: map[types.JID]types.JID{phoneLID: phonePN},
			}, selfPhone)
			b := testBridge(t, client, ms, installRecordingLogger(t))
			b.MediaAutoDownload = false
			ts := time.Now().Add(-time.Hour).Truncate(time.Second)
			legacy := fixtureDocument()
			legacy.DirectPath = nil
			if err := persistMessage(ms, "TARGET", storedChat.String(), storedChat.String(), ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, "TARGET"), false, b.Log); err != nil {
				t.Fatal(err)
			}
			repaired := fixtureDocument()
			calls := 0
			mux := http.NewServeMux()
			registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, client,
				func() bool { return true }, ms, chatPolicy{}, func(_ context.Context, msg *waE2E.Message) error {
					calls++
					chat := msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest().GetChatJID()
					if calls == 1 {
						if chat != storedChat.String() {
							t.Fatal("stored spelling was not first")
						}
						return nil // simulate a phone ignoring its unknown spelling
					}
					if chat == storedChat.String() {
						t.Fatal("second request repeated the stored spelling")
					}
					b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
						SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(),
						Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat),
							Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
								Key:              &waCommon.MessageKey{ID: proto.String("TARGET"), FromMe: proto.Bool(false)},
								MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
								Message:          &waE2E.Message{DocumentMessage: repaired},
							}}}}},
					}})
					return nil
				})
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"chat_jid":%q,"before_message_id":"TARGET"}`, storedChat.String())
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(body)))
			if rec.Code != http.StatusOK || calls != 2 || storedDirectPath(t, ms, "TARGET", storedChat.String()).String != repaired.GetDirectPath() {
				t.Fatalf("twin replay did not repair original row: calls=%d status=%d", calls, rec.Code)
			}
		})
	}
}
