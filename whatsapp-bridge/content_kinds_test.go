package main

import (
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestAdditionalKindsStoredAndSearchable(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	b.Webhook = newWebhookSender("", false)
	cases := []struct {
		name                  string
		msg                   *waE2E.Message
		content, media, query string
	}{
		{"event", &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("Picnic"), Description: proto.String("Bring lunch"), StartTime: proto.Int64(1700000000), IsCanceled: proto.Bool(true)}}, "Event canceled — Picnic — Bring lunch — starts 2023-11-14T22:13:20Z", "", "Picnic"},
		{"invite", &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Book club"), Caption: proto.String("Join us"), GroupJID: proto.String("120363000000000001@g.us"), InviteCode: proto.String("example"), InviteExpiration: proto.Int64(1700000000)}}, "Group invite — Book club — Join us — group 120363000000000001@g.us — invitation token example — expires 2023-11-14T22:13:20Z", "", "club"},
		{"product", &waE2E.Message{ProductMessage: &waE2E.ProductMessage{Product: &waE2E.ProductMessage_ProductSnapshot{Title: proto.String("Notebook"), Description: proto.String("Blue cover"), CurrencyCode: proto.String("USD"), PriceAmount1000: proto.Int64(12500)}, Body: proto.String("Available"), Footer: proto.String("Today")}}, "Product — Notebook — Blue cover — Available — Today — USD 12.500", "", "Notebook"},
		{"order", &waE2E.Message{OrderMessage: &waE2E.OrderMessage{OrderTitle: proto.String("Stationery"), Message: proto.String("Delivery"), OrderID: proto.String("ORDER1"), ItemCount: proto.Int32(2), TotalAmount1000: proto.Int64(25000), TotalCurrencyCode: proto.String("USD"), Token: proto.String("opaque-order-token")}}, "Order — Stationery — Delivery — ORDER1 — 2 items — USD 25.000", "", "Stationery"},
		{"sent-payment", &waE2E.Message{SendPaymentMessage: &waE2E.SendPaymentMessage{NoteMessage: &waE2E.Message{Conversation: proto.String("Lunch share")}, TransactionData: proto.String("opaque-transaction")}}, "Payment sent — Lunch share", "", "Lunch"},
		{"requested-payment", &waE2E.Message{RequestPaymentMessage: &waE2E.RequestPaymentMessage{NoteMessage: &waE2E.Message{Conversation: proto.String("Coffee share")}, Amount1000: proto.Uint64(1500), CurrencyCodeIso4217: proto.String("USD")}}, "Payment request — Coffee share — USD 1.500", "", "Coffee"},
		{"money-payment", &waE2E.Message{RequestPaymentMessage: &waE2E.RequestPaymentMessage{Amount: &waE2E.Money{Value: proto.Int64(1234), Offset: proto.Uint32(2), CurrencyCode: proto.String("USD")}}}, "Payment request — USD value=1234 offset=2", "", "request"},
		{"list", &waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{Title: proto.String("Pickup"), Description: proto.String("Tomorrow"), SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String("option-pickup")}}}, "List reply — Pickup — Tomorrow — option-pickup", "", "Pickup"},
		{"interactive", &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{Body: &waE2E.InteractiveResponseMessage_Body{Text: proto.String("Confirm")}, InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{Name: proto.String("selection"), ParamsJSON: proto.String("{\"id\":\"pickup\"}")}}}}, "Interactive reply — Confirm — selection — {\"id\":\"pickup\"}", "", "Confirm"},

		{"button-id", &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{SelectedButtonID: proto.String("pickup")}}, "Button reply — pickup", "", "pickup"},
		{"button-text", &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{SelectedButtonID: proto.String("pickup"), Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Pickup"}}}, "Pickup", "", "Pickup"},
		{"template-button-id", &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedID: proto.String("pickup")}}, "Template button reply — pickup", "", "pickup"},
		{"template-button-text", &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedID: proto.String("pickup"), SelectedDisplayText: proto.String("Pickup")}}, "Pickup", "", "Pickup"},
		{"round-video", &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: proto.String("Video greeting"), URL: proto.String("https://example.test/video"), DirectPath: proto.String("/media/video"), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(42)}}, "Video greeting", "video", "greeting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
		})
	}
	if got := b.metrics.messagesStored.Load(); got != int64(len(cases)) {
		t.Fatalf("stored count=%d", got)
	}
}

func TestSparseKindsHaveTypeLabels(t *testing.T) {
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
	}
	if got := extractMessage(&waE2E.Message{PtvMessage: &waE2E.VideoMessage{}}, time.Now(), "PTV"); got.empty() || got.mediaType != "video" {
		t.Fatalf("round video: %+v", got)
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
	for value, want := range map[string]string{"0": "USD 0.000", "-1": "USD -0.001", "18446744073709551615": "USD 18446744073709551.615", "-9223372036854775808": "USD -9223372036854775.808"} {
		if got := formatAmount1000(value, "USD"); got != want {
			t.Errorf("%s: %s want %s", value, got, want)
		}
	}
}
