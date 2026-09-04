package main

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// quoteCtx is a ContextInfo quoting message "Q1" from a known participant.
func quoteCtx(mentions ...string) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{
		StanzaID:      proto.String("Q1"),
		Participant:   proto.String("5511888888888@s.whatsapp.net"),
		QuotedMessage: &waE2E.Message{Conversation: proto.String("original")},
		MentionedJID:  mentions,
	}
}

// contextCarriers lists every message kind whose ContextInfo the extractors read.
func contextCarriers(ctx *waE2E.ContextInfo) map[string]*waE2E.Message {
	return map[string]*waE2E.Message{
		"extended": {ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("t"), ContextInfo: ctx}},
		"image":    {ImageMessage: &waE2E.ImageMessage{ContextInfo: ctx}},
		"video":    {VideoMessage: &waE2E.VideoMessage{ContextInfo: ctx}},
		"document": {DocumentMessage: &waE2E.DocumentMessage{ContextInfo: ctx}},
		"audio":    {AudioMessage: &waE2E.AudioMessage{ContextInfo: ctx}},
	}
}

func TestExtractQuotedMessageInfo_AllCarriers(t *testing.T) {
	for kind, msg := range contextCarriers(quoteCtx()) {
		id, sender, content := extractQuotedMessageInfo(msg)
		if id != "Q1" || sender != "5511888888888@s.whatsapp.net" || content != "original" {
			t.Errorf("%s: got (%q, %q, %q)", kind, id, sender, content)
		}
	}
	for kind, msg := range contextCarriers(nil) {
		if id, sender, content := extractQuotedMessageInfo(msg); id != "" || sender != "" || content != "" {
			t.Errorf("%s without context: got (%q, %q, %q)", kind, id, sender, content)
		}
	}
	if id, _, _ := extractQuotedMessageInfo(nil); id != "" {
		t.Errorf("nil message: %q", id)
	}
	// Quote of a media message: the content is whatever extractTextContent
	// renders for it (caption), never a crash on a missing Conversation.
	mediaQuote := &waE2E.ContextInfo{StanzaID: proto.String("Q2"), QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look")}}}
	if _, _, content := extractQuotedMessageInfo(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: mediaQuote}}); !strings.Contains(content, "look") {
		t.Errorf("quoted media caption lost: %q", content)
	}
	// Sticker has no ContextInfo hook here: no quote.
	if id, _, _ := extractQuotedMessageInfo(&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}); id != "" {
		t.Errorf("sticker: %q", id)
	}
}

func TestExtractMentionedJIDs_AllCarriers(t *testing.T) {
	mentions := []string{"5511777777777@s.whatsapp.net", "123456789@lid"}
	for kind, msg := range contextCarriers(quoteCtx(mentions...)) {
		got := extractMentionedJIDs(msg)
		if len(got) != 2 || got[0] != mentions[0] || got[1] != mentions[1] {
			t.Errorf("%s: got %v", kind, got)
		}
		// The result is a copy: mutating it must not touch the protobuf.
		got[0] = "changed"
		if again := extractMentionedJIDs(msg); again[0] != mentions[0] {
			t.Errorf("%s: extractMentionedJIDs returned the protobuf slice itself", kind)
		}
	}
	for kind, msg := range contextCarriers(quoteCtx()) {
		if got := extractMentionedJIDs(msg); got != nil {
			t.Errorf("%s with empty mentions: got %v, want nil", kind, got)
		}
	}
	if got := extractMentionedJIDs(nil); got != nil {
		t.Errorf("nil message: %v", got)
	}
	if got := extractMentionedJIDs(&waE2E.Message{Conversation: proto.String("plain")}); got != nil {
		t.Errorf("plain conversation: %v", got)
	}
}

func TestExtractMediaInfo_AllKinds(t *testing.T) {
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	const id = "ABCDEF"
	cdn := struct {
		url           string
		key, sha, enc []byte
		length        uint64
	}{"https://mmg.whatsapp.net/v/x.enc?oh=1", []byte("k"), []byte("s"), []byte("e"), 777}

	cases := []struct {
		name     string
		msg      *waE2E.Message
		wantType string
		wantFile string
	}{
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "image", "image_20260904_150405_ABCDEF.jpg"},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "video", "video_20260904_150405_ABCDEF.mp4"},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "audio", "audio_20260904_150405_ABCDEF.ogg"},
		{"document with name", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("Report Q3.pdf"), URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "document", "Report Q3.pdf"},
		{"document without name", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "document", "document_20260904_150405_ABCDEF"},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{URL: &cdn.url, MediaKey: cdn.key, FileSHA256: cdn.sha, FileEncSHA256: cdn.enc, FileLength: &cdn.length}}, "sticker", "sticker_20260904_150405_ABCDEF.webp"},
	}
	for _, tc := range cases {
		gotType, gotFile, gotURL, gotKey, gotSHA, gotEnc, gotLen := extractMediaInfo(tc.msg, ts, id)
		if gotType != tc.wantType || gotFile != tc.wantFile {
			t.Errorf("%s: type/file = %q/%q, want %q/%q", tc.name, gotType, gotFile, tc.wantType, tc.wantFile)
		}
		if gotURL != cdn.url || string(gotKey) != "k" || string(gotSHA) != "s" || string(gotEnc) != "e" || gotLen != 777 {
			t.Errorf("%s: CDN fields not propagated: %q %q %q %q %d", tc.name, gotURL, gotKey, gotSHA, gotEnc, gotLen)
		}
	}

	// Without a message ID the suffix is the timestamp alone (history rows
	// from before IDs were embedded); with a zero timestamp "now" is used.
	if _, file, _, _, _, _, _ := extractMediaInfo(cases[0].msg, ts, ""); file != "image_20260904_150405.jpg" {
		t.Errorf("no id: %q", file)
	}
	year := time.Now().Format("2006")
	if _, file, _, _, _, _, _ := extractMediaInfo(cases[0].msg, time.Time{}, id); !strings.HasPrefix(file, "image_"+year) {
		t.Errorf("zero timestamp should fall back to now: %q", file)
	}

	for name, msg := range map[string]*waE2E.Message{
		"nil":      nil,
		"text":     {Conversation: proto.String("hi")},
		"contact":  {ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("x")}},
		"location": {LocationMessage: &waE2E.LocationMessage{}},
	} {
		if gotType, gotFile, _, _, _, _, _ := extractMediaInfo(msg, ts, id); gotType != "" || gotFile != "" {
			t.Errorf("%s: got media (%q, %q)", name, gotType, gotFile)
		}
	}
}

func TestExtractVCardPhones_And_FormatContactContent(t *testing.T) {
	vcard := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Ana\r\nTEL;type=CELL;waid=5511999999999:+55 11 99999-9999\r\nTEL;type=HOME:+55 11 3333-3333\r\nEND:VCARD"
	phones := extractVCardPhones(vcard)
	if len(phones) != 2 || phones[0] != "+55 11 99999-9999" || phones[1] != "+55 11 3333-3333" {
		t.Errorf("phones = %v", phones)
	}
	if got := extractVCardPhones("no phones here"); len(got) != 0 {
		t.Errorf("no TEL lines: %v", got)
	}
	content := formatContactContent("Ana", vcard)
	if !strings.Contains(content, "Ana") || !strings.Contains(content, "+55 11 99999-9999") {
		t.Errorf("contact content = %q", content)
	}
	if got := formatContactContent("", ""); got != "" {
		t.Errorf("empty contact renders nothing, got %q", got)
	}
	if got := formatContactContent("Bob", ""); got != "Bob" {
		t.Errorf("name only: %q", got)
	}
	if got := extractVCardPhones("item1.TEL;type=CELL:+1 555 0100"); len(got) != 1 || got[0] != "+1 555 0100" {
		t.Errorf("iPhone group prefix: %v", got)
	}
}

func TestExtractChatEphemeralFromMessage_AllCarriers(t *testing.T) {
	ctx := &waE2E.ContextInfo{Expiration: proto.Uint32(604800), EphemeralSettingTimestamp: proto.Int64(1710000000)}
	for kind, msg := range contextCarriers(ctx) {
		got := extractChatEphemeralFromMessage(msg)
		if got.Expiration != 604800 || got.SettingTimestamp != 1710000000 {
			t.Errorf("%s: %+v", kind, got)
		}
	}
	if got := extractChatEphemeralFromMessage(&waE2E.Message{Conversation: proto.String("x")}); got.Expiration != 0 || got.SettingTimestamp != 0 {
		t.Errorf("plain text: %+v", got)
	}
	if got := extractChatEphemeralFromMessage(nil); got.Expiration != 0 {
		t.Errorf("nil: %+v", got)
	}
}
