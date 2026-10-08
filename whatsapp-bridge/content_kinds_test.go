package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type contentKindCase struct {
	name                  string
	msg                   *waE2E.Message
	content, media, query string
}

func contentKindFixtures() []contentKindCase {
	return []contentKindCase{
		{"event", &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("Picnic"), Description: proto.String("Bring lunch"), StartTime: proto.Int64(1700000000), EndTime: proto.Int64(1700003600), JoinLink: proto.String("https://example.test/event"), Location: &waE2E.LocationMessage{Name: proto.String("Park")}, IsCanceled: proto.Bool(true)}}, "Event canceled — Picnic — Bring lunch — 📍 Park — starts 2023-11-14T22:13:20Z — ends 2023-11-14T23:13:20Z — https://example.test/event", "", "Picnic"},
		{"invite", &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Book club"), Caption: proto.String("Join us"), GroupJID: proto.String("120363000000000001@g.us"), InviteCode: proto.String("example"), InviteExpiration: proto.Int64(1700000000)}}, "Group invite — Book club — Join us — group 120363000000000001@g.us — expires 2023-11-14T22:13:20Z", "", "club"},
		{"product", &waE2E.Message{ProductMessage: &waE2E.ProductMessage{Product: &waE2E.ProductMessage_ProductSnapshot{Title: proto.String("Notebook"), Description: proto.String("Blue cover"), CurrencyCode: proto.String("USD"), PriceAmount1000: proto.Int64(12500), URL: proto.String("https://example.test/product")}, Body: proto.String("Available"), Footer: proto.String("Today")}}, "Product — Notebook — Blue cover — Available — Today — USD 12.50 — https://example.test/product", "", "Notebook"},
		{"order", &waE2E.Message{OrderMessage: &waE2E.OrderMessage{OrderTitle: proto.String("Stationery"), Message: proto.String("Delivery"), OrderID: proto.String("ORDER1"), ItemCount: proto.Int32(2), TotalAmount1000: proto.Int64(25000), TotalCurrencyCode: proto.String("USD"), Token: proto.String("opaque-order-token")}}, "Order — Stationery — Delivery — ORDER1 — 2 items — USD 25.00", "", "Stationery"},
		{"sent-payment", &waE2E.Message{SendPaymentMessage: &waE2E.SendPaymentMessage{NoteMessage: &waE2E.Message{Conversation: proto.String("Lunch share")}, TransactionData: proto.String("opaque-transaction")}}, "Payment sent — Lunch share", "", "Lunch"},
		{"requested-payment", &waE2E.Message{RequestPaymentMessage: &waE2E.RequestPaymentMessage{NoteMessage: &waE2E.Message{Conversation: proto.String("Coffee share")}, Amount1000: proto.Uint64(1500), CurrencyCodeIso4217: proto.String("USD")}}, "Payment request — Coffee share — USD 1.50", "", "Coffee"},
		{"money-payment", &waE2E.Message{RequestPaymentMessage: &waE2E.RequestPaymentMessage{Amount: &waE2E.Money{Value: proto.Int64(1234), Offset: proto.Uint32(2), CurrencyCode: proto.String("USD")}}}, "Payment request — USD value=1234 offset=2", "", "request"},
		{"list", &waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{Title: proto.String("Pickup"), Description: proto.String("Tomorrow"), SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String("option-pickup")}}}, "List reply — Pickup — Tomorrow", "", "Pickup"},
		{"interactive", &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{Body: &waE2E.InteractiveResponseMessage_Body{Text: proto.String("Confirm")}, InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{Name: proto.String("selection"), ParamsJSON: proto.String("{\"id\":\"pickup\"}")}}}}, "Interactive reply — Confirm — selection — {\"id\":\"pickup\"}", "", "Confirm"},
		{"interactive-large", &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{ParamsJSON: proto.String(strings.Repeat("x", 200000))}}}}, "Interactive reply — " + strings.Repeat("x", 4082) + " … [truncated]", "", "truncated"},
		{"interactive-unicode", &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{ParamsJSON: proto.String(strings.Repeat("界", 5000))}}}}, "Interactive reply — " + strings.Repeat("界", 4082) + " … [truncated]", "", "truncated"},

		{"button-id", &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{SelectedButtonID: proto.String("pickup")}}, "Button reply — pickup", "", "pickup"},
		{"button-text", &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{SelectedButtonID: proto.String("pickup"), Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Pickup"}}}, "Pickup", "", "Pickup"},
		{"template-button-id", &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedID: proto.String("pickup")}}, "Template button reply — pickup", "", "pickup"},
		{"template-button-text", &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedID: proto.String("pickup"), SelectedDisplayText: proto.String("Pickup")}}, "Pickup", "", "Pickup"},
		{"round-video", &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: proto.String("Video greeting"), URL: proto.String("https://example.test/video"), DirectPath: proto.String("/media/video"), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(42)}}, "Video greeting", "video", "greeting"},
	}
}

func TestAdditionalKindsStoredAndSearchable(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	srv, posts, payloads := contentKindsWebhook(t)
	b.Webhook = newWebhookSender("", true)
	b.Webhook.url = srv.URL
	cases := contentKindFixtures()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := posts.Load()
			msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			msg.Info.ID = tc.name
			msg.Message = tc.msg
			b.handleMessage(msg)
			var content string
			var media sql.NullString
			if err := ms.db.QueryRow("SELECT content,media_type FROM messages WHERE id=? AND chat_jid=?", tc.name, phonePN.String()).Scan(&content, &media); err != nil {
				t.Fatal(err)
			}
			if content != tc.content || media.String != tc.media {
				t.Fatalf("stored=%q/%q want=%q/%q", content, media.String, tc.content, tc.media)
			}
			var matches int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ? AND rowid IN (SELECT rowid FROM messages WHERE id=? AND chat_jid=?)", tc.query, tc.name, phonePN.String()).Scan(&matches); err != nil {
				t.Fatal(err)
			}
			if matches != 1 {
				t.Fatalf("FTS matches=%d", matches)
			}
			if posts.Load()-before != 1 {
				t.Fatalf("webhook POSTs=%d", posts.Load()-before)
			}
			payload := <-payloads
			if payload.Content != tc.content || payload.MessageID != tc.name || payload.ChatJID != phonePN.String() || payload.Stored != nil {
				t.Fatalf("webhook=%+v", payload)
			}
			if tc.media == "video" {
				assertRoundVideoColumns(t, ms, tc.name)
			}
			b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(phonePN.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(tc.name + "-history"), RemoteJID: proto.String(phonePN.String()), FromMe: proto.Bool(false)}, MessageTimestamp: proto.Uint64(1772359200), Message: proto.Clone(tc.msg).(*waE2E.Message)}}}}}}})
			var historical string
			if err := ms.db.QueryRow("SELECT content FROM messages WHERE id=?", tc.name+"-history").Scan(&historical); err != nil || historical != tc.content {
				t.Fatalf("history=%q error=%v", historical, err)
			}
			if tc.media == "video" {
				assertRoundVideoColumns(t, ms, tc.name+"-history")
			}
			if posts.Load()-before != 1 {
				t.Fatal("history replay must not emit a new webhook")
			}
		})
	}
	if got := b.metrics.messagesStored.Load(); got != int64(len(cases)) {
		t.Fatalf("stored count=%d", got)
	}
	if got := b.metrics.historyMessages.Load(); got != int64(len(cases)) {
		t.Fatalf("history count=%d", got)
	}
}

func contentKindsWebhook(t *testing.T) (*httptest.Server, *atomic.Int32, <-chan WebhookPayload) {
	t.Helper()
	posts := new(atomic.Int32)
	ch := make(chan WebhookPayload, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook: %v", err)
		}
		posts.Add(1)
		ch <- payload
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, posts, ch
}

func assertRoundVideoColumns(t *testing.T, ms *MessageStore, id string) {
	t.Helper()
	var media, filename, url, path string
	var key, sha, enc []byte
	var length uint64
	if err := ms.db.QueryRow(`SELECT media_type,filename,url,direct_path,media_key,file_sha256,file_enc_sha256,file_length FROM messages WHERE id=?`, id).Scan(&media, &filename, &url, &path, &key, &sha, &enc, &length); err != nil {
		t.Fatal(err)
	}
	if media != "video" || !strings.HasSuffix(filename, ".mp4") || !strings.Contains(filename, id) || url != "https://example.test/video" || path != "/media/video" || !bytes.Equal(key, []byte{1}) || !bytes.Equal(sha, []byte{2}) || !bytes.Equal(enc, []byte{3}) || length != 42 {
		t.Fatalf("PTV SQL=%s/%s/%s/%s/%v/%v/%v/%d", media, filename, url, path, key, sha, enc, length)
	}
}

func TestSparseKindsHaveTypeLabels(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	srv, posts, payloads := contentKindsWebhook(t)
	b.Webhook = newWebhookSender("", true)
	b.Webhook.url = srv.URL
	for name, msg := range map[string]*waE2E.Message{
		"Button reply":          {ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{}},
		"Template button reply": {TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{}},
		"Event":                 {EventMessage: &waE2E.EventMessage{}},
		"Group invite":          {GroupInviteMessage: &waE2E.GroupInviteMessage{}},
		"Product":               {ProductMessage: &waE2E.ProductMessage{}},
		"Order":                 {OrderMessage: &waE2E.OrderMessage{}},
		"Payment sent":          {SendPaymentMessage: &waE2E.SendPaymentMessage{}},
		"Payment request":       {RequestPaymentMessage: &waE2E.RequestPaymentMessage{}},
		"List reply":            {ListResponseMessage: &waE2E.ListResponseMessage{}},
		"Interactive reply":     {InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{}},
	} {
		if got := extractMessage(msg, time.Now(), "SPARSE"); got.empty() || got.content != name || got.mediaType != "" {
			t.Fatalf("%s: %+v", name, got)
		}
		before := posts.Load()
		evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
		evt.Info.ID = name
		evt.Message = msg
		b.handleMessage(evt)
		var stored string
		if err := ms.db.QueryRow("SELECT content FROM messages WHERE id=?", name).Scan(&stored); err != nil || stored != name {
			t.Fatalf("bare label=%q error=%v", stored, err)
		}
		if posts.Load() != before {
			t.Fatal("bare envelope reached webhook")
		}
		plain := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, name)
		plain.Info.ID = name + "-text"
		b.handleMessage(plain)
		if posts.Load() != before+1 || (<-payloads).Content != name {
			t.Fatal("ordinary label text must reach webhook")
		}
	}
	if got := extractMessage(&waE2E.Message{PtvMessage: &waE2E.VideoMessage{}}, time.Now(), "PTV"); got.empty() || got.mediaType != "video" {
		t.Fatalf("round video: %+v", got)
	}
}

func TestEnvelopeRenderingBoundsAndFallbacks(t *testing.T) {
	for _, money := range []*waE2E.Money{{}, {Value: proto.Int64(99), Offset: proto.Uint32(1), CurrencyCode: proto.String("USD")}} {
		msg := &waE2E.Message{RequestPaymentMessage: &waE2E.RequestPaymentMessage{Amount1000: proto.Uint64(12500), CurrencyCodeIso4217: proto.String("BRL"), Amount: money}}
		if got := extractTextContent(msg); got != "Payment request — BRL 12.50" {
			t.Fatalf("amount precedence=%q", got)
		}
	}
	for _, seconds := range []int64{0, -1, 1700000000000, math.MaxInt64, 4102444800} {
		if got := extractTextContent(&waE2E.Message{EventMessage: &waE2E.EventMessage{StartTime: proto.Int64(seconds), EndTime: proto.Int64(seconds)}}); got != "Event" {
			t.Fatalf("invalid timestamp %d became %q", seconds, got)
		}
	}
	if got := extractTextContent(&waE2E.Message{OrderMessage: &waE2E.OrderMessage{ItemCount: proto.Int32(1)}}); got != "Order — 1 item" {
		t.Fatal(got)
	}
	if got := extractTextContent(&waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String("pickup")}}}); got != "List reply — pickup" {
		t.Fatal(got)
	}
	if got := extractTextContent(&waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{Description: proto.String("Visible"), SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String("internal-id")}}}); got != "List reply — Visible" {
		t.Fatal(got)
	}
	for _, params := range []string{strings.Repeat("x", 200000), strings.Repeat("界", 5000)} {
		flow := &waE2E.InteractiveResponseMessage{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{ParamsJSON: proto.String(params)}}}
		got := extractTextContent(&waE2E.Message{InteractiveResponseMessage: flow})
		bounded := strings.TrimPrefix(got, "Interactive reply — ")
		if utf8.RuneCountInString(bounded) != 4096 || !utf8.ValidString(bounded) || !strings.HasSuffix(bounded, " … [truncated]") {
			t.Fatalf("flow cap chars=%d", utf8.RuneCountInString(bounded))
		}
	}
	for _, params := range []string{"", strings.Repeat("x", 4096)} {
		if got := cappedFlowParams(params); got != params {
			t.Fatal("short JSON changed")
		}
	}
}

func TestRoundVideoRetainsMetadata(t *testing.T) {
	ctx := &waE2E.ContextInfo{StanzaID: proto.String("QUOTE1"), Participant: proto.String("5511999999999@s.whatsapp.net"), QuotedMessage: &waE2E.Message{Conversation: proto.String("hello")}, MentionedJID: []string{"5511999999999@s.whatsapp.net"}, Expiration: proto.Uint32(3600), EphemeralSettingTimestamp: proto.Int64(1000)}
	msg := &waE2E.Message{PtvMessage: &waE2E.VideoMessage{ContextInfo: ctx, DirectPath: proto.String("/media/video"), FileLength: proto.Uint64(42), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}}}
	ex := extractMessage(msg, time.Now(), "PTV")
	if ex.mediaType != "video" || ex.directPath != "/media/video" || ex.fileLen != 42 || ex.quotedID != "QUOTE1" || ex.quotedContent != "hello" || len(ex.mentions) != 1 {
		t.Fatalf("round video metadata: %+v", ex)
	}
	if got := extractChatEphemeralFromMessage(msg); got.Expiration != 3600 || got.SettingTimestamp != 1000 {
		t.Fatalf("ephemeral=%+v", got)
	}
}

func TestAmounts1000StayExact(t *testing.T) {
	for value, want := range map[string]string{"0": "USD 0.00", "-1": "USD -0.001", "18446744073709551615": "USD 18446744073709551.615", "-9223372036854775808": "USD -9223372036854775.808"} {
		if got := formatAmount1000(value, "USD"); got != want {
			t.Errorf("%s: %s want %s", value, got, want)
		}
	}
}
