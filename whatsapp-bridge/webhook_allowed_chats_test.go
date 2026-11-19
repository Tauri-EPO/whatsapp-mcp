package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const otherGroup = "120363000000000002@g.us"

// allowedChatsFixture is the runtime receiver fixture with a phone/LID map, so
// twins resolve the way they do for the chat allow-list.
func allowedChatsFixture(t *testing.T) (*Bridge, *httptest.Server, string, <-chan signedWebhookReceipt, *recordingLogger) {
	t.Helper()
	b, server, destination, receipts, logger := runtimeWebhookFixture(t, nil)
	b.Client = newTestClient(&mockLIDStore{
		lidByPN: map[types.JID]types.JID{phonePN: phoneLID},
		pnByLID: map[types.JID]types.JID{phoneLID: phonePN},
	})
	return b, server, destination, receipts, logger
}

func allowedChatsMessage(kind string, chat types.JID, id string) *events.Message {
	var msg *events.Message
	switch kind {
	case "image":
		msg = buildImageMessage(chat, phonePN, false, "receiver list image")
	case "reaction":
		msg = buildReactionMessage(chat, phonePN, false, "FAKE-TARGET-"+id, "👍")
	default:
		msg = buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "receiver list text")
	}
	msg.Info.ID = id
	return msg
}

// Every case runs for text, images and reactions: they share one admission.
func TestRuntimeWebhookAllowedChatsDelivery(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	other := types.NewJID("120363000000000002", types.GroupServer)
	ninth := types.NewJID("551199999999", types.DefaultUserServer)
	for _, tc := range []struct {
		name     string
		list     any // nil = field omitted; "null" = JSON null
		runtime  []string
		chat     types.JID
		delivers bool
	}{
		{"allowed DM", []string{phonePN.String()}, nil, phonePN, true},
		{"allowed bare number DM", []string{phonePN.User}, nil, phonePN, true},
		{"allowed group", []string{mgGroup}, nil, group, true},
		{"group outside the list", []string{mgGroup}, nil, other, false},
		{"DM outside a group list", []string{mgGroup}, nil, phonePN, false},
		{"group wildcard", []string{"*@g.us"}, nil, other, true},
		{"group wildcard leaves DMs out", []string{"*@g.us"}, nil, phonePN, false},
		{"phone entry admits its LID twin", []string{phonePN.String()}, nil, phoneLID, true},
		{"LID entry admits its phone twin", []string{phoneLID.String()}, nil, phonePN, true},
		{"ninth digit spelling", []string{"5511999999999"}, nil, ninth, true},
		{"empty list fails closed (DM)", []string{}, nil, phonePN, false},
		{"empty list fails closed (group)", []string{}, nil, group, false},
		{"null keeps every chat", "null", nil, other, true},
		{"omitted keeps every chat", nil, nil, other, true},
		{"list cannot widen chats.allowed", []string{mgGroup, phonePN.String()}, []string{mgGroup}, phonePN, false},
		{"intersection keeps the common chat", []string{mgGroup, phonePN.String()}, []string{mgGroup}, group, true},
		{"wildcard cannot widen chats.allowed", []string{"*@g.us"}, []string{mgGroup}, other, false},
	} {
		for _, kind := range []string{"text", "image", "reaction"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				b, server, destination, receipts, _ := allowedChatsFixture(t)
				if tc.runtime != nil {
					settingsChats(t, b, "")
					patch, _ := json.Marshal(map[string]any{"chats.allowed": tc.runtime})
					if code, data := webhookOperatorRequest(t, server, "PATCH", "/operator/v1/settings", string(patch)); code != 200 {
						t.Fatalf("chats.allowed=%d %s", code, data)
					}
				}
				fields := map[string]any{}
				switch list := tc.list.(type) {
				case string:
					fields["allowed_chats"] = nil
				case []string:
					fields["allowed_chats"] = list
				}
				if code, data := configureWebhookFields(t, server, destination, fields); code != 200 {
					t.Fatalf("configure=%d %s", code, data)
				}
				rw := b.RuntimeWebhook
				dropped, failures := rw.dropped.Load(), rw.failures.Load()
				b.handleMessage(allowedChatsMessage(kind, tc.chat, "FAKE-LIST-"+kind))
				if tc.delivers {
					var event runtimeWebhookEvent
					if err := json.Unmarshal(receiveWebhook(t, receipts).body, &event); err != nil {
						t.Fatal(err)
					}
					if event.MessageID != "FAKE-LIST-"+kind {
						t.Fatalf("delivered another event: %+v", event)
					}
				} else {
					webhookBarrier(t, b, receipts)
				}
				// Withholding by the receiver's own list is not delivery loss.
				state := rw.snapshot()
				if rw.dropped.Load() != dropped || rw.failures.Load() != failures || state.Dropped != 0 || state.Failures != 0 || !state.Enabled {
					t.Fatalf("filter counted as loss: dropped=%d failures=%d state=%+v", rw.dropped.Load()-dropped, rw.failures.Load()-failures, state)
				}
			})
		}
	}
}

// A disabled receiver counts arrivals it would deliver as loss; arrivals its
// list withholds are not arrivals for it at all.
func TestRuntimeWebhookAllowedChatsDisabledReceiverNoLoss(t *testing.T) {
	b, server, destination, _, _ := allowedChatsFixture(t)
	if code, data := configureWebhookFields(t, server, destination, map[string]any{"allowed_chats": []string{mgGroup}}); code != 200 {
		t.Fatalf("configure=%d %s", code, data)
	}
	rw := b.RuntimeWebhook
	rw.mu.Lock()
	rw.state.Enabled = false
	rw.mu.Unlock()
	b.handleMessage(allowedChatsMessage("text", phonePN, "FAKE-DISABLED-OUT"))
	if got := rw.dropped.Load(); got != 0 {
		t.Fatalf("withheld arrival counted as loss: %d", got)
	}
	b.handleMessage(allowedChatsMessage("text", types.NewJID("120363000000000001", types.GroupServer), "FAKE-DISABLED-IN"))
	if got := rw.dropped.Load(); got != 1 {
		t.Fatalf("admitted arrival on a disabled receiver dropped=%d, want 1", got)
	}
}

func TestRuntimeWebhookAllowedChatsValidation(t *testing.T) {
	full := make([]string, maxWebhookAllowedChats)
	for i := range full {
		full[i] = "120363000000000" + strconv.Itoa(100+i) + "@g.us"
	}
	for _, tc := range []struct {
		name  string
		value any
		code  int
	}{
		{"cap", full, 200},
		{"over the cap", append(append([]string{}, full...), mgGroup), 400},
		{"blank entry", []string{""}, 400},
		{"space entry", []string{" "}, 400},
		{"two @", []string{phonePN.String() + "@g.us"}, 400},
		{"comma-joined", []string{phonePN.User + "," + mgGroup}, 400},
		{"device suffix", []string{"11234567890:3@s.whatsapp.net"}, 200},
		{"name instead of a JID", []string{"Alice"}, 400},
		{"one malformed among valid", []string{mgGroup, "not a chat"}, 400},
		{"not an array", mgGroup, 400},
		{"number entries", []int{1}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, server, destination, _, logger := allowedChatsFixture(t)
			code, data := configureWebhookFields(t, server, destination, map[string]any{"allowed_chats": tc.value})
			if code != tc.code || b.RuntimeWebhook.configured() != (tc.code == 200) {
				t.Fatalf("status=%d configured=%t body=%s", code, b.RuntimeWebhook.configured(), data)
			}
			if tc.name == "over the cap" && !bytes.Contains(data, []byte("at most 100 entries")) {
				t.Fatalf("cap refusal not explained: %s", data)
			}
			for _, private := range []string{"Alice", "not a chat", phonePN.User, "120363000000000"} {
				if bytes.Contains(data, []byte(private)) || strings.Contains(logger.String(), private) {
					t.Fatal("PUT refusal echoed an entry")
				}
			}
		})
	}
}

func TestRuntimeWebhookAllowedChatsViewsAndLogsHideEntries(t *testing.T) {
	b, server, destination, receipts, logger := allowedChatsFixture(t)
	globalLogger := installRecordingLogger(t)
	list := []string{phoneLID.String(), mgGroup, "*@g.us", phonePN.User}
	if code, data := configureWebhookFields(t, server, destination, map[string]any{"allowed_chats": list}); code != 200 || bytes.Contains(data, []byte("g.us")) {
		t.Fatalf("configure=%d %s", code, data)
	}
	status, _ := json.Marshal(b.webhookStatus())
	views := map[string][]byte{"bridge /api/webhook view": status}
	for _, path := range []string{"/operator/v1/webhook", "/operator/v1/health"} {
		code, data := webhookOperatorRequest(t, server, "GET", path, "")
		if code != 200 {
			t.Fatalf("%s=%d", path, code)
		}
		views[path] = data
	}
	for name, data := range views {
		if !bytes.Contains(data, []byte(`"allowed_chats":{"mode":"list","entries":4}`)) {
			t.Fatalf("%s lacks the mode and count: %s", name, data)
		}
		for _, private := range []string{phoneLID.User, phonePN.User, "120363000000000001", "g.us"} {
			if bytes.Contains(data, []byte(private)) {
				t.Fatalf("%s exposes an allowed_chats entry: %s", name, data)
			}
		}
	}
	// Outside chat, legacy off: the withheld line names the filter, not the chat.
	other := types.NewJID("120363000000000002", types.GroupServer)
	b.handleMessage(allowedChatsMessage("text", types.NewJID("5511888888888", types.DefaultUserServer), "FAKE-WITHHELD"))
	webhookBarrier(t, b, receipts)
	// The per-message DEBUG echo of the archive is separate; the configuration
	// lines and the withheld line must carry no identifier.
	var lines []string
	for _, line := range strings.Split(logger.String()+globalLogger.String(), "\n") {
		if strings.Contains(line, "webhook") || strings.Contains(line, "Webhook") || strings.Contains(line, "not forwarded") {
			lines = append(lines, line)
		}
	}
	logs := strings.Join(lines, "\n")
	if !strings.Contains(logs, "legacy webhook: WEBHOOK_ENABLED=false; runtime webhook: receiver filter allowed_chats") {
		t.Fatalf("withheld log does not name the filter: %s", logs)
	}
	for _, private := range []string{"5511888888888", phoneLID.User, phonePN.User, "120363000000000001", other.User, "g.us", "FAKE-WITHHELD"} {
		if strings.Contains(logs, private) {
			t.Fatalf("webhook logs contain a chat or message identifier: %s", logs)
		}
	}
	// Removing the receiver returns the views to "all" with no count.
	if code, _ := webhookOperatorRequest(t, server, "DELETE", "/operator/v1/webhook", ""); code != 204 {
		t.Fatal("delete failed")
	}
	if got := b.webhookStatus().AllowedChats; got != allChatsFilter {
		t.Fatalf("view after removal=%+v", got)
	}
	if code, data := configureWebhookFields(t, server, destination, nil); code != 200 || !bytes.Contains(data, []byte(`"allowed_chats":{"mode":"all","entries":0}`)) {
		t.Fatalf("omitted list view=%d %s", code, data)
	}
}

// The receiver list never reaches the archive, the chat policy every REST
// handler authorizes with, the legacy webhook or the non-message events.
func TestRuntimeWebhookAllowedChatsLeavesOtherPathsIntact(t *testing.T) {
	b, server, destination, receipts, _ := allowedChatsFixture(t)
	legacy, payloads := captureWebhook(t)
	b.Webhook.enabled, b.Webhook.url, b.Webhook.client = true, legacy.URL, legacy.Client()
	settingsChats(t, b, "")
	before, err := b.settingsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configureConnectionWebhook(t, server, destination, map[string]any{"events": []string{"message.received", "connection.changed"}, "allowed_chats": []string{}})

	b.handleMessage(allowedChatsMessage("text", phonePN, "FAKE-INTACT"))
	var stored int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = ? AND chat_jid = ?", "FAKE-INTACT", phonePN.String()).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("message outside allowed_chats not archived: %d %v", stored, err)
	}
	select {
	case p := <-payloads:
		if p.MessageID != "FAKE-INTACT" {
			t.Fatalf("legacy payload=%+v", p)
		}
	default:
		t.Fatal("receiver list narrowed the legacy webhook")
	}
	after, err := b.settingsSnapshot(context.Background())
	if err != nil || after.Version != before.Version || after.chats.restricted {
		t.Fatalf("receiver list changed the runtime chat policy: %+v %v", after.chats, err)
	}
	for _, chat := range []string{phonePN.String(), otherGroup} {
		if !b.Policy.effective(context.Background()).Allows(chat) {
			t.Fatalf("effective chat policy refuses %s", chat)
		}
		if _, ok := authorizeChat(httptest.NewRecorder(), b.Policy.effective(context.Background()), chat, false); !ok {
			t.Fatalf("REST authorization refuses %s", chat)
		}
	}
	b.queueRuntimeConnection(connectionPayload{Type: "connection", State: "connected"})
	receiveConnectionState(t, receipts, "connected")
	code, data := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(data, []byte(`"delivered":true`)) {
		t.Fatalf("synthetic test with an empty list=%d %s", code, data)
	}
}
