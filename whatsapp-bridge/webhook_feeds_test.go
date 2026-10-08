package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestFeedWebhookSwitchesAreIndependentAndStrict(t *testing.T) {
	for _, name := range []string{webhookForwardChannelsEnv, webhookForwardBroadcastsEnv} {
		for _, value := range append(append([]string{}, boolTrue...), boolFalse...) {
			sw, err := parseBridgeSwitches(func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			want := false
			for _, yes := range boolTrue {
				want = want || value == yes
			}
			got, other := sw.ForwardChannels, sw.ForwardBroadcasts
			if name == webhookForwardBroadcastsEnv {
				got, other = other, got
			}
			if got != want || other || sw.ForwardStatus || !sw.WebhookEnabled || !sw.ForwardSelf {
				t.Fatalf("%s=%s: %+v", name, value, sw)
			}
		}
	}
	_, err := parseBridgeSwitches(func(name string) string {
		if name == webhookForwardChannelsEnv || name == webhookForwardBroadcastsEnv {
			return "typo"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), webhookForwardChannelsEnv) || !strings.Contains(err.Error(), webhookForwardBroadcastsEnv) {
		t.Fatalf("two bad switches: %v", err)
	}
}

func TestFeedWebhookPolicyAllGates(t *testing.T) {
	chats := []types.JID{phonePN, types.NewJID("120363000000000001", types.GroupServer), types.StatusBroadcastJID, types.NewJID("example", types.NewsletterServer), types.NewJID("example", types.BroadcastServer), phoneLID, types.NewJID("example", "example.invalid"), types.EmptyJID}
	for flags := 0; flags < 32; flags++ {
		b := &Bridge{Webhook: newWebhookSender("", flags&1 != 0), ForwardSelf: flags&2 != 0, ForwardStatus: flags&4 != 0, ForwardChannels: flags&8 != 0, ForwardBroadcasts: flags&16 != 0}
		for index, chat := range chats {
			for _, fromMe := range []bool{false, true} {
				feedAllowed := true
				switch index {
				case 2:
					feedAllowed = flags&4 != 0
				case 3:
					feedAllowed = flags&8 != 0
				case 4:
					feedAllowed = flags&16 != 0
				case 6, 7:
					feedAllowed = false
				}
				want := flags&1 != 0 && (!fromMe || flags&2 != 0) && feedAllowed
				if got := b.forwardsToWebhook(chat, fromMe); got != want {
					t.Fatalf("flags=%d chat=%s fromMe=%v: %v want %v", flags, chat, fromMe, got, want)
				}
			}
		}
	}
}

// Actual event -> store -> HTTP capture for every feed and payload family.
// A denied feed still has its row, and every allowed message is delivered once.
func TestFeedsReachWebhookOnlyWhenAsked(t *testing.T) {
	feeds := []struct {
		name string
		chat types.JID
	}{
		{"channel", types.NewJID("example", types.NewsletterServer)},
		{"broadcast", types.NewJID("example", types.BroadcastServer)},
		{"status", types.StatusBroadcastJID},
		{"unknown", types.NewJID("example", "example.invalid")},
	}
	for _, feed := range feeds {
		for _, on := range []bool{false, true} {
			for _, kind := range []string{"text", "image", "reaction"} {
				t.Run(fmt.Sprintf("%s/%t/%s", feed.name, on, kind), func(t *testing.T) {
					wantForward := on && feed.name != "unknown"
					payloads := make(chan WebhookPayload, 1)
					var requests atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						var payload WebhookPayload
						if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
							select {
							case payloads <- payload:
							default:
							}
						}
						w.WriteHeader(http.StatusOK)
					}))
					t.Cleanup(srv.Close)
					t.Setenv("WEBHOOK_URL", srv.URL)
					b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
					b.MediaAutoDownload = false
					// Turn unrelated opt-ins on: they must not enable this feed.
					b.ForwardStatus, b.ForwardChannels, b.ForwardBroadcasts = true, true, true
					switch feed.name {
					case "channel":
						b.ForwardChannels = on
					case "broadcast":
						b.ForwardBroadcasts = on
					case "status":
						b.ForwardStatus = on
					}
					var msg *events.Message
					switch kind {
					case "image":
						msg = buildImageMessage(feed.chat, phonePN, false, "")
					default:
						msg = buildTextMessage(feed.chat, phonePN, types.EmptyJID, types.EmptyJID, false, "hello")
						if kind == "reaction" {
							msg.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("TARGET1")}, Text: proto.String("+1")}}
						}
					}
					b.handleMessage(msg)
					wantRequests := int32(0)
					if wantForward {
						wantRequests = 1
					}
					if got := requests.Load(); got != wantRequests {
						t.Fatalf("HTTP requests=%d want=%d", got, wantRequests)
					}
					select {
					case payload := <-payloads:
						if !wantForward || payload.ChatJID != feed.chat.String() {
							t.Fatalf("unexpected payload: %+v", payload)
						}
					default:
						if wantForward {
							t.Fatal("opted-in event was not delivered")
						}
					}
					select {
					case extra := <-payloads:
						t.Fatalf("duplicate payload: %+v", extra)
					default:
					}
					if rows, err := b.Store.GetMessages(feed.chat.String(), 10); err != nil || len(rows) != 1 {
						t.Fatalf("stored=%d err=%v", len(rows), err)
					}
				})
			}
		}
	}
}
