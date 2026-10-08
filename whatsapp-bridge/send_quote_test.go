package main

import (
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

var quoteTestQuote = outboundQuote{
	id:          "3EB0QUOTED0001",
	participant: "5511999999999@s.whatsapp.net",
	content:     "the original text",
}

var quoteTestMentions = []string{"5511988887777@s.whatsapp.net"}

// outboundMediaKinds are the four kinds of file a send can carry, with what
// buildOutboundMedia needs to build each.
func outboundMediaKinds() []struct {
	kind      string
	mediaType whatsmeow.MediaType
	mime      string
	path      string
	data      []byte
} {
	ogg := append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*2, []byte{0xfc})...)
	return []struct {
		kind      string
		mediaType whatsmeow.MediaType
		mime      string
		path      string
		data      []byte
	}{
		{"image", whatsmeow.MediaImage, "image/png", "/out/a.png", nil},
		{"video", whatsmeow.MediaVideo, "video/mp4", "/out/a.mp4", nil},
		{"document", whatsmeow.MediaDocument, "application/pdf", "/out/a.pdf", nil},
		{"audio", whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "/out/n.ogg", ogg},
	}
}

// A file sent as a reply carries the quote on the wire, for every media kind,
// and what a recipient's bridge reads back from it is exactly what the row of
// the send records (issue #476).
func TestBuildOutboundMediaCarriesTheQuote(t *testing.T) {
	installRecordingLogger(t)
	for _, m := range outboundMediaKinds() {
		msg, _, err := buildOutboundMedia(m.mediaType, m.mime, m.path, m.data, testUpload(), "cap", quoteTestQuote, nil)
		if err != nil {
			t.Fatalf("%s: %v", m.kind, err)
		}
		ctx := *mediaContextInfo(msg)
		if ctx.GetStanzaID() != quoteTestQuote.id || ctx.GetParticipant() != quoteTestQuote.participant || ctx.GetQuotedMessage().GetConversation() != quoteTestQuote.content {
			t.Errorf("%s: quote on the wire = id %q, participant %q, content %q", m.kind, ctx.GetStanzaID(), ctx.GetParticipant(), ctx.GetQuotedMessage().GetConversation())
		}
		// The reader the bridge uses for inbound messages sees the same reply.
		gotID, gotSender, gotContent := extractQuotedMessageInfo(msg)
		if gotID != quoteTestQuote.id || gotSender != quoteTestQuote.participant || gotContent != quoteTestQuote.content {
			t.Errorf("%s: read back as id %q, sender %q, content %q", m.kind, gotID, gotSender, gotContent)
		}
	}
}

// Quote and caption mentions share one ContextInfo, and neither costs the
// other. A voice note has no caption: it can be a reply, it mentions nobody,
// and the text asked for is not what travels with it.
func TestBuildOutboundMediaQuoteMentionsAndCaption(t *testing.T) {
	installRecordingLogger(t)
	for _, m := range outboundMediaKinds() {
		msg, sentCaption, err := buildOutboundMedia(m.mediaType, m.mime, m.path, m.data, testUpload(), "see this", quoteTestQuote, quoteTestMentions)
		if err != nil {
			t.Fatalf("%s: %v", m.kind, err)
		}
		ctx := *mediaContextInfo(msg)
		if ctx.GetStanzaID() != quoteTestQuote.id {
			t.Errorf("%s: the quote was lost next to the mentions: %v", m.kind, ctx)
		}
		wantMentions, wantCaption := quoteTestMentions, "see this"
		if m.kind == "audio" {
			wantMentions, wantCaption = nil, ""
		}
		if !reflect.DeepEqual(ctx.GetMentionedJID(), wantMentions) {
			t.Errorf("%s: mentions = %v, want %v", m.kind, ctx.GetMentionedJID(), wantMentions)
		}
		// What the caller stores as the row's text is what was sent.
		if sentCaption != wantCaption || extractTextContent(msg) != wantCaption {
			t.Errorf("%s: caption to store = %q, caption on the wire = %q, want %q", m.kind, sentCaption, extractTextContent(msg), wantCaption)
		}

		// The chat's disappearing-message settings are added to that context
		// afterwards, as the send does, and keep both.
		applyChatEphemeralSettings(msg, ChatEphemeralSettings{Expiration: 86400, SettingTimestamp: 1710000000})
		ctx = *mediaContextInfo(msg)
		if ctx.GetStanzaID() != quoteTestQuote.id || ctx.GetExpiration() != 86400 || !reflect.DeepEqual(ctx.GetMentionedJID(), wantMentions) {
			t.Errorf("%s: after the ephemeral settings: %v", m.kind, ctx)
		}
	}
}

// A send that neither quotes nor mentions carries no ContextInfo at all, and
// mentions alone invent no quote: the messages are what they were before.
func TestBuildOutboundMediaWithoutAQuote(t *testing.T) {
	installRecordingLogger(t)
	for _, m := range outboundMediaKinds() {
		plain, _, err := buildOutboundMedia(m.mediaType, m.mime, m.path, m.data, testUpload(), "cap", outboundQuote{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", m.kind, err)
		}
		if ctx := *mediaContextInfo(plain); ctx != nil {
			t.Errorf("%s: a send that quotes nothing got a ContextInfo: %v", m.kind, ctx)
		}
		mentioned, _, err := buildOutboundMedia(m.mediaType, m.mime, m.path, m.data, testUpload(), "cap", outboundQuote{}, quoteTestMentions)
		if err != nil {
			t.Fatalf("%s: %v", m.kind, err)
		}
		ctx := *mediaContextInfo(mentioned)
		if m.kind == "audio" {
			if ctx != nil {
				t.Errorf("audio: a voice note has no caption, must not get mentions: %v", ctx)
			}
			continue
		}
		if !reflect.DeepEqual(ctx.GetMentionedJID(), quoteTestMentions) || ctx.StanzaID != nil || ctx.QuotedMessage != nil {
			t.Errorf("%s: mentions without a quote = %v", m.kind, ctx)
		}
	}
	// A corrupt voice note is still refused, with no message and no caption.
	if msg, caption, err := buildOutboundMedia(whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "/out/n.ogg", []byte("not ogg"), testUpload(), "x", quoteTestQuote, nil); err == nil || msg != nil || caption != "" {
		t.Errorf("corrupt ogg: msg %v, caption %q, err %v", msg, caption, err)
	}
}

// Text and media replies get their context from the same place.
func TestOutboundContextInfo(t *testing.T) {
	if ctx := outboundContextInfo(outboundQuote{}, nil); ctx != nil {
		t.Errorf("neither quote nor mentions: %v", ctx)
	}
	text := buildOutboundText("re", quoteTestQuote.id, quoteTestQuote.participant, quoteTestQuote.content, quoteTestMentions)
	want := outboundContextInfo(quoteTestQuote, quoteTestMentions)
	if !proto.Equal(text.GetExtendedTextMessage().GetContextInfo(), want) {
		t.Errorf("a text reply carries %v, want %v", text.GetExtendedTextMessage().GetContextInfo(), want)
	}
}

func TestMediaContextInfo(t *testing.T) {
	if mediaContextInfo(nil) != nil || mediaContextInfo(&waE2E.Message{}) != nil || mediaContextInfo(&waE2E.Message{Conversation: proto.String("hi")}) != nil {
		t.Errorf("no media, no context slot")
	}
	sticker := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}
	if mediaContextInfo(sticker) != nil {
		t.Errorf("a sticker is not something this bridge sends")
	}
}
