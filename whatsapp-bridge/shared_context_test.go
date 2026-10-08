package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// These payloads display ordinary fake conversational content. The pinned
// descriptor supplies the ContextInfo field; tests enter through handleMessage.
func sharedContextFixtures() map[string]*waE2E.Message {
	fixtures := map[string]*waE2E.Message{
		"extendedTextMessage":        {ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("body")}},
		"imageMessage":               {ImageMessage: &waE2E.ImageMessage{Caption: proto.String("caption")}},
		"videoMessage":               {VideoMessage: &waE2E.VideoMessage{Caption: proto.String("caption")}},
		"ptvMessage":                 {PtvMessage: &waE2E.VideoMessage{Caption: proto.String("caption")}},
		"documentMessage":            {DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("caption")}},
		"audioMessage":               {AudioMessage: &waE2E.AudioMessage{}},
		"stickerMessage":             {StickerMessage: &waE2E.StickerMessage{}},
		"contactMessage":             {ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Alice")}},
		"contactsArrayMessage":       {ContactsArrayMessage: &waE2E.ContactsArrayMessage{Contacts: []*waE2E.ContactMessage{{DisplayName: proto.String("Alice")}}}},
		"locationMessage":            {LocationMessage: &waE2E.LocationMessage{Name: proto.String("Example park"), DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2)}},
		"liveLocationMessage":        {LiveLocationMessage: &waE2E.LiveLocationMessage{Caption: proto.String("Walking"), DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2)}},
		"eventMessage":               {EventMessage: &waE2E.EventMessage{Name: proto.String("Picnic")}},
		"groupInviteMessage":         {GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Example group"), InviteCode: proto.String("must-stay-omitted")}},
		"productMessage":             {ProductMessage: &waE2E.ProductMessage{Body: proto.String("Example item")}},
		"orderMessage":               {OrderMessage: &waE2E.OrderMessage{Message: proto.String("Example order"), Token: proto.String("must-stay-omitted")}},
		"listResponseMessage":        {ListResponseMessage: &waE2E.ListResponseMessage{Title: proto.String("Chosen item")}},
		"interactiveResponseMessage": {InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{Body: &waE2E.InteractiveResponseMessage_Body{Text: proto.String("Chosen item")}}},
		"templateMessage":            {TemplateMessage: &waE2E.TemplateMessage{HydratedTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{HydratedContentText: proto.String("body")}}},
		"buttonsMessage":             {ButtonsMessage: &waE2E.ButtonsMessage{ContentText: proto.String("body")}},
		"interactiveMessage":         {InteractiveMessage: &waE2E.InteractiveMessage{Body: &waE2E.InteractiveMessage_Body{Text: proto.String("body")}}},
		"listMessage":                {ListMessage: &waE2E.ListMessage{Description: proto.String("body")}},
		"buttonsResponseMessage":     {ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Chosen item"}}},
		"templateButtonReplyMessage": {TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{SelectedDisplayText: proto.String("Chosen item")}},
	}
	for _, name := range []string{"pollCreationMessage", "pollCreationMessageV2", "pollCreationMessageV3", "pollCreationMessageV5", "pollCreationMessageV6"} {
		m := &waE2E.Message{}
		field := m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
		m.ProtoReflect().Set(field, protoreflect.ValueOfMessage((&waE2E.PollCreationMessage{Name: proto.String("Choose"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("One")}, {OptionName: proto.String("Two")}}}).ProtoReflect()))
		fixtures[name] = m
	}
	return fixtures
}

func setSharedTestContext(m *waE2E.Message, name string, ctx *waE2E.ContextInfo) {
	f := m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
	part := m.ProtoReflect().Get(f).Message()
	cf := part.Descriptor().Fields().ByName("contextInfo")
	part.Set(cf, protoreflect.ValueOfMessage(ctx.ProtoReflect()))
}

func sharedTestContext() *waE2E.ContextInfo {
	ctx := quoteCtx(phonePN.String(), phoneLID.String())
	ctx.Participant = proto.String(phoneLID.String())
	ctx.Expiration, ctx.EphemeralSettingTimestamp = proto.Uint32(86400), proto.Int64(1710000000)
	return ctx
}

func sharedHeaderFixtures() map[string]*waE2E.Message {
	fixtures := map[string]*waE2E.Message{}
	paths := []string{"templateMessage.hydratedTemplate", "templateMessage.hydratedFourRowTemplate", "templateMessage.fourRowTemplate", "buttonsMessage", "interactiveMessage.header", "templateMessage.interactiveMessageTemplate.header"}
	for _, path := range paths {
		for _, media := range []string{"imageMessage", "videoMessage", "documentMessage"} {
			m := &waE2E.Message{}
			node := m.ProtoReflect()
			for _, name := range strings.Split(path+"."+media, ".") {
				node = node.Mutable(node.Descriptor().Fields().ByName(protoreflect.Name(name))).Message()
			}
			fields := node.Descriptor().Fields()
			node.Set(fields.ByName("contextInfo"), protoreflect.ValueOfMessage(sharedTestContext().ProtoReflect()))
			node.Set(fields.ByName("URL"), protoreflect.ValueOfString("https://example.com/media"))
			node.Set(fields.ByName("mediaKey"), protoreflect.ValueOfBytes([]byte("fake-key")))
			node.Set(fields.ByName("caption"), protoreflect.ValueOfString("header caption"))
			fixtures["header/"+path+"/"+media] = m
		}
	}
	return fixtures
}

func TestSharedContextPinnedInventory(t *testing.T) {
	// Direct fields with a shared context but no supported conversational body
	// remain outside the helper. MessageContextInfo is a different protobuf type.
	omitted := strings.Fields("call pollResultSnapshotMessage pollResultSnapshotMessageV3 albumMessage stickerPackMessage eventInviteMessage newsletterAdminInviteMessage newsletterFollowerInviteMessageV2 requestPhoneNumberMessage requestLocationMessage musicMessage splitPaymentMessage richResponseMessage instantImageMessage messageHistoryBundle messageHistoryNotice")
	allowOmitted := map[string]bool{}
	for _, name := range omitted {
		allowOmitted[name] = true
	}
	fixtures := sharedContextFixtures()
	fields := (&waE2E.Message{}).ProtoReflect().Descriptor().Fields()
	count, omittedCount := 0, 0
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Message() == nil {
			continue
		}
		ctx := f.Message().Fields().ByName("contextInfo")
		if ctx == nil || ctx.Message().FullName() != "WAWebProtobufsE2E.ContextInfo" {
			continue
		}
		count++
		name := string(f.Name())
		if fixtures[name] == nil && !allowOmitted[name] {
			t.Errorf("unaccounted direct ContextInfo family %s", name)
		}
		if fixtures[name] == nil && allowOmitted[name] {
			omittedCount++
			m := &waE2E.Message{}
			m.ProtoReflect().Mutable(f)
			setSharedTestContext(m, name, sharedTestContext())
			if sharedContextInfo(m) != nil {
				t.Errorf("intentionally omitted context extracted: %s", name)
			}
			t.Logf("omitted: %s", name)
		}
	}
	t.Logf("pinned direct carriers=%d supported=%d intentionally omitted=%d", count, len(fixtures), omittedCount)
}

func TestSharedContextLiveSQLiteAndWebhook(t *testing.T) {
	fixtures := sharedContextFixtures()
	for name, payload := range fixtures {
		setSharedTestContext(payload, name, sharedTestContext())
	}
	for name, payload := range sharedHeaderFixtures() {
		fixtures[name] = payload
	}
	for name, payload := range fixtures {
		t.Run(name, func(t *testing.T) {
			deliveries := make(chan WebhookPayload, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p WebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				deliveries <- p
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			b.MediaAutoDownload = false
			b.Webhook = newWebhookSender("fake-token", true)
			b.Webhook.url = srv.URL
			event := buildTextMessage(phoneLID, phoneLID, phonePN, types.EmptyJID, false, "")
			event.Info.ID, event.Message = "CTX1", payload
			b.handleMessage(event)
			var quote, mentions sql.NullString
			var senderServer, content string
			if err := b.Store.db.QueryRow("SELECT quoted_message_id, mentions, sender_server, content FROM messages WHERE id = ? AND chat_jid = ?", "CTX1", phonePN.String()).Scan(&quote, &mentions, &senderServer, &content); err != nil {
				t.Fatal(err)
			}
			if quote.String != "Q1" {
				t.Errorf("SQLite quote=%q", quote.String)
			}
			var targets []string
			targets = strings.Split(mentions.String, ",")
			if !reflect.DeepEqual(targets, []string{phonePN.User, phoneLID.User}) {
				t.Errorf("SQLite mentions=%v", targets)
			}
			settings, err := b.Store.GetChatEphemeralSettings(phonePN.String())
			if err != nil || settings.Expiration != 86400 || settings.SettingTimestamp != 1710000000 {
				t.Errorf("SQLite ephemeral=%+v err=%v", settings, err)
			}
			if senderServer != types.DefaultUserServer || strings.Contains(content, "must-stay-omitted") {
				t.Errorf("namespace/token regression %q %q", senderServer, content)
			}
			wantDelivery := name != "audioMessage" && name != "stickerMessage"
			if strings.HasPrefix(name, "header/") && strings.HasSuffix(name, "/videoMessage") {
				wantDelivery = false
			}
			if !wantDelivery {
				if len(deliveries) != 0 {
					t.Fatal("non-text media webhook gate changed")
				}
				return
			}
			if len(deliveries) != 1 {
				t.Fatalf("webhook deliveries=%d", len(deliveries))
			}
			p := <-deliveries
			if p.QuotedMessageId != "Q1" || p.QuotedSender != phoneLID.String() || p.QuotedContent != "original" || !reflect.DeepEqual(p.MentionedJIDs, []string{phonePN.String(), phoneLID.String()}) || p.ChatJID != phonePN.String() || p.Content != content {
				t.Errorf("webhook=%+v", p)
			}
		})
	}
}

func TestSharedContextSparseAndHeaderPrecedence(t *testing.T) {
	for name, m := range sharedContextFixtures() {
		if sharedContextInfo(m) != nil || extractMentionedJIDs(m) != nil || extractChatEphemeralFromMessage(m) != (ChatEphemeralSettings{}) {
			t.Errorf("%s invents sparse context", name)
		}
		setSharedTestContext(m, name, &waE2E.ContextInfo{})
		id, author, text := extractQuotedMessageInfo(m)
		if id != "" || author != "" || text != "" {
			t.Errorf("%s invents sparse quote", name)
		}
	}
	// Every supported header layout and media part, including oneof headers.
	paths := []string{"templateMessage.hydratedTemplate", "templateMessage.hydratedFourRowTemplate", "templateMessage.fourRowTemplate", "buttonsMessage", "interactiveMessage.header", "templateMessage.interactiveMessageTemplate.header"}
	for _, path := range paths {
		for _, media := range []string{"imageMessage", "videoMessage", "documentMessage"} {
			m := &waE2E.Message{}
			node := m.ProtoReflect()
			for _, name := range strings.Split(path+"."+media, ".") {
				node = node.Mutable(node.Descriptor().Fields().ByName(protoreflect.Name(name))).Message()
			}
			ctx := sharedTestContext()
			node.Set(node.Descriptor().Fields().ByName("contextInfo"), protoreflect.ValueOfMessage(ctx.ProtoReflect()))
			if sharedContextInfo(m) != ctx {
				t.Errorf("header context lost: %s.%s", path, media)
			}
			outer := m.ProtoReflect().Get(m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(strings.Split(path, ".")[0]))).Message()
			outerCtx := &waE2E.ContextInfo{StanzaID: proto.String("OUTER")}
			outer.Set(outer.Descriptor().Fields().ByName("contextInfo"), protoreflect.ValueOfMessage(outerCtx.ProtoReflect()))
			if sharedContextInfo(m) != outerCtx {
				t.Errorf("outer context precedence lost: %s", path)
			}
		}
	}
	if sharedContextInfo(nil) != nil {
		t.Error("nil context")
	}
}

func TestSharedContextSparseLiveAndUnmappedLID(t *testing.T) {
	for _, kind := range []string{"absent", "empty", "unmapped-lid"} {
		t.Run(kind, func(t *testing.T) {
			delivery := make(chan WebhookPayload, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var delivered WebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&delivered); err != nil {
					t.Error(err)
				}
				delivery <- delivered
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			b.Webhook = newWebhookSender("fake-token", true)
			b.Webhook.url = srv.URL
			var ctx *waE2E.ContextInfo
			if kind == "empty" {
				ctx = &waE2E.ContextInfo{}
			}
			if kind == "unmapped-lid" {
				ctx = sharedTestContext()
			}
			payload := &waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{Title: proto.String("Chosen item"), ContextInfo: ctx}}
			event := buildTextMessage(phoneLID, phoneLID, types.EmptyJID, types.EmptyJID, false, "")
			event.Message = payload
			b.handleMessage(event)
			if len(delivery) != 1 {
				t.Fatal("missing sparse/LID webhook")
			}
			delivered := <-delivery
			var quote, mentions sql.NullString
			var namespace string
			if err := b.Store.db.QueryRow("SELECT quoted_message_id, mentions, sender_server FROM messages WHERE id = ? AND chat_jid = ?", event.Info.ID, phoneLID.String()).Scan(&quote, &mentions, &namespace); err != nil {
				t.Fatal(err)
			}
			if namespace != types.HiddenUserServer || delivered.ChatJID != phoneLID.String() {
				t.Fatalf("unmapped namespace changed: %q %+v", namespace, delivered)
			}
			if kind != "unmapped-lid" {
				if quote.Valid || mentions.Valid || delivered.QuotedMessageId != "" || len(delivered.MentionedJIDs) != 0 {
					t.Errorf("sparse context invented metadata: quote=%v mentions=%v webhook=%+v", quote, mentions, delivered)
				}
			} else if quote.String != "Q1" || mentions.String != phonePN.User+","+phoneLID.User || delivered.QuotedSender != phoneLID.String() {
				t.Errorf("unmapped LID context lost: quote=%v mentions=%v webhook=%+v", quote, mentions, delivered)
			}
		})
	}
}

func TestSharedContextHistoryScope(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	fixtures := sharedContextFixtures()
	for name, m := range fixtures {
		setSharedTestContext(m, name, sharedTestContext())
	}
	for name, m := range sharedHeaderFixtures() {
		fixtures[name] = m
	}
	history := largeHistoryFixture(len(fixtures))
	i := 0
	for _, m := range fixtures {
		history.Data.Conversations[0].Messages[i].Message.Message = m
		i++
	}
	b.handleHistorySync(history)
	for i := 0; i < len(fixtures); i++ {
		var quote sql.NullString
		var mentions string
		err := b.Store.db.QueryRow("SELECT quoted_message_id, mentions FROM messages WHERE id = ? AND chat_jid = ?", fmt.Sprintf("H%d", i), phonePN.String()).Scan(&quote, &mentions)
		if err != nil {
			t.Fatal(err)
		}
		// #619 owns the history quoted=false caller. This change preserves it
		// explicitly; native mentions already share persistMessage in history.
		if quote.Valid {
			t.Errorf("history quote scope changed: %q", quote.String)
		}
		var targets []string
		targets = strings.Split(mentions, ",")
		if !reflect.DeepEqual(targets, []string{phonePN.User, phoneLID.User}) {
			t.Errorf("history mentions=%v", targets)
		}
	}
	settings, err := b.Store.GetChatEphemeralSettings(phonePN.String())
	if err != nil || settings.Expiration != 0 {
		t.Errorf("history conversation settings scope changed: %+v %v", settings, err)
	}
}
