package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestClassifyMediaPath(t *testing.T) {
	cases := []struct {
		path     string
		wantType whatsmeow.MediaType
		wantMime string
		wantCat  string
	}{
		{"/x/photo.JPG", whatsmeow.MediaImage, "image/jpeg", "image"},
		{"/x/photo.jpeg", whatsmeow.MediaImage, "image/jpeg", "image"},
		{"/x/a.png", whatsmeow.MediaImage, "image/png", "image"},
		{"/x/a.gif", whatsmeow.MediaImage, "image/gif", "image"},
		{"/x/a.webp", whatsmeow.MediaImage, "image/webp", "image"},
		{"/x/note.ogg", whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "audio"},
		{"/x/clip.mp4", whatsmeow.MediaVideo, "video/mp4", "video"},
		{"/x/clip.avi", whatsmeow.MediaVideo, "video/avi", "video"},
		{"/x/clip.mov", whatsmeow.MediaVideo, "video/quicktime", "video"},
		{"/x/report.pdf", whatsmeow.MediaDocument, "application/pdf", "document"},
		{"/x/blob.zzz-unknown", whatsmeow.MediaDocument, "application/octet-stream", "document"},
		{"/x/noext", whatsmeow.MediaDocument, "application/octet-stream", "document"},
	}
	for _, tc := range cases {
		gotType, gotMime, gotCat := classifyMediaPath(tc.path)
		if gotType != tc.wantType || gotCat != tc.wantCat || !strings.HasPrefix(gotMime, tc.wantMime) {
			t.Errorf("classifyMediaPath(%q) = (%v, %q, %q), want (%v, %q, %q)", tc.path, gotType, gotMime, gotCat, tc.wantType, tc.wantMime, tc.wantCat)
		}
	}
}

// oggPage builds one Ogg page holding a single packet (segment table with one
// entry), which is how the OpusHead page and short audio pages look.
func oggPage(seq uint32, granule uint64, packet []byte) []byte {
	if len(packet) > 255 {
		panic("oggPage: packet must fit one segment")
	}
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:14], granule)
	binary.LittleEndian.PutUint32(h[18:22], seq)
	h[26] = 1
	return append(append(h, byte(len(packet))), packet...)
}

// opusHead is the 19-byte identification packet (RFC 7845 §5.1).
func opusHead(preSkip uint16, sampleRate uint32) []byte {
	p := make([]byte, 19)
	copy(p, "OpusHead")
	p[8] = 1 // version
	p[9] = 1 // channels
	binary.LittleEndian.PutUint16(p[10:12], preSkip)
	binary.LittleEndian.PutUint32(p[12:16], sampleRate)
	return p
}

func TestAnalyzeOggOpus(t *testing.T) {
	installRecordingLogger(t)
	t.Run("rejects non-ogg data", func(t *testing.T) {
		if _, _, err := analyzeOggOpus([]byte("RIFF....WAVE")); err == nil {
			t.Fatal("expected error for non-Ogg data")
		}
		if _, _, err := analyzeOggOpus(nil); err == nil {
			t.Fatal("expected error for empty data")
		}
	})
	t.Run("duration from granule with OpusHead sample rate and pre-skip", func(t *testing.T) {
		// A real ffmpeg first page is exactly 47 bytes: 27 header + 1 segment + 19 packet.
		data := append(oggPage(0, 0, opusHead(312, 16000)), oggPage(1, 16000*3+312, []byte{0xfc})...)
		if len(data) != 47+29 {
			t.Fatalf("test fixture size = %d", len(data))
		}
		dur, wave, err := analyzeOggOpus(data)
		if err != nil {
			t.Fatal(err)
		}
		// With the default 48 kHz the same granule would read as 1 s.
		if dur != 3 {
			t.Errorf("duration = %d, want 3 (sample rate must come from OpusHead)", dur)
		}
		if len(wave) != 64 {
			t.Errorf("waveform length = %d, want 64", len(wave))
		}
	})
	t.Run("rounds partial seconds up", func(t *testing.T) {
		data := append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*2+1, []byte{0xfc})...)
		dur, _, err := analyzeOggOpus(data)
		if err != nil || dur != 3 {
			t.Errorf("duration = %d, err = %v; want 3", dur, err)
		}
	})
	t.Run("clamps to 1..300 seconds", func(t *testing.T) {
		long := append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*1000, []byte{0xfc})...)
		if dur, _, _ := analyzeOggOpus(long); dur != 300 {
			t.Errorf("long duration = %d, want 300", dur)
		}
		// No granule anywhere: falls back to a size estimate, at least 1 s.
		short := oggPage(0, 0, opusHead(0, 48000))
		if dur, _, err := analyzeOggOpus(short); err != nil || dur != 1 {
			t.Errorf("short duration = %d, err = %v; want 1", dur, err)
		}
	})
	t.Run("truncated page does not panic", func(t *testing.T) {
		data := oggPage(0, 0, opusHead(0, 48000))
		for cut := 4; cut < len(data); cut++ {
			if _, _, err := analyzeOggOpus(data[:cut]); err != nil {
				t.Errorf("cut %d: unexpected error %v", cut, err)
			}
		}
	})
}

func TestPlaceholderWaveform(t *testing.T) {
	a := placeholderWaveform(12)
	b := placeholderWaveform(12)
	c := placeholderWaveform(200)
	if len(a) != 64 || !bytes.Equal(a, b) {
		t.Fatalf("waveform must be 64 bytes and deterministic per duration")
	}
	if bytes.Equal(a, c) {
		t.Errorf("different durations should give different waveforms")
	}
	for i, v := range c {
		if v > 100 {
			t.Fatalf("sample %d = %d, above WhatsApp's 0..100 range", i, v)
		}
	}
}

func testUpload() whatsmeow.UploadResponse {
	return whatsmeow.UploadResponse{
		URL:           "https://mmg.whatsapp.net/x",
		DirectPath:    "/v/x",
		MediaKey:      []byte("key"),
		FileEncSHA256: []byte("enc"),
		FileSHA256:    []byte("sha"),
		FileLength:    42,
	}
}

func TestBuildMediaMessage(t *testing.T) {
	installRecordingLogger(t)
	up := testUpload()

	img, err := buildMediaMessage(whatsmeow.MediaImage, "image/png", "/out/a.png", []byte("png"), up, "cap")
	if err != nil || img.ImageMessage == nil {
		t.Fatalf("image: %v %v", err, img)
	}
	if img.ImageMessage.GetCaption() != "cap" || img.ImageMessage.GetURL() != up.URL || img.ImageMessage.GetFileLength() != 42 || !bytes.Equal(img.ImageMessage.GetMediaKey(), up.MediaKey) {
		t.Errorf("image fields not copied from upload: %v", img.ImageMessage)
	}

	vid, err := buildMediaMessage(whatsmeow.MediaVideo, "video/mp4", "/out/a.mp4", nil, up, "v")
	if err != nil || vid.VideoMessage == nil || vid.VideoMessage.GetMimetype() != "video/mp4" || vid.VideoMessage.GetCaption() != "v" {
		t.Errorf("video: %v %v", err, vid)
	}

	doc, err := buildMediaMessage(whatsmeow.MediaDocument, "application/pdf", "/srv/outbox/Report Q3.pdf", nil, up, "see attached")
	if err != nil || doc.DocumentMessage == nil {
		t.Fatalf("document: %v %v", err, doc)
	}
	if doc.DocumentMessage.GetFileName() != "Report Q3.pdf" || doc.DocumentMessage.GetTitle() != "Report Q3.pdf" {
		t.Errorf("document filename must be the base name, got %q / %q", doc.DocumentMessage.GetFileName(), doc.DocumentMessage.GetTitle())
	}
	if doc.DocumentMessage.GetCaption() != "see attached" || doc.DocumentMessage.GetMimetype() != "application/pdf" {
		t.Errorf("document caption/mime: %v", doc.DocumentMessage)
	}

	ogg := append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*7, []byte{0xfc})...)
	aud, err := buildMediaMessage(whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "/out/n.ogg", ogg, up, "ignored")
	if err != nil || aud.AudioMessage == nil {
		t.Fatalf("audio: %v %v", err, aud)
	}
	if !aud.AudioMessage.GetPTT() || aud.AudioMessage.GetSeconds() != 7 || len(aud.AudioMessage.GetWaveform()) != 64 {
		t.Errorf("voice note fields: ptt=%v seconds=%d waveform=%d", aud.AudioMessage.GetPTT(), aud.AudioMessage.GetSeconds(), len(aud.AudioMessage.GetWaveform()))
	}

	if _, err := buildMediaMessage(whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "/out/n.ogg", []byte("not ogg"), up, ""); err == nil {
		t.Errorf("corrupt ogg must be rejected before upload metadata is sent")
	}

	mp3, err := buildMediaMessage(whatsmeow.MediaAudio, "audio/mpeg", "/out/n.mp3", []byte("id3"), up, "")
	if err != nil || mp3.AudioMessage == nil || mp3.AudioMessage.GetSeconds() != 30 || mp3.AudioMessage.GetWaveform() != nil {
		t.Errorf("non-ogg audio should fall back to 30 s and no waveform: %v %v", err, mp3)
	}
}

func TestBuildOutboundText(t *testing.T) {
	plain := buildOutboundText("hi", "", "", "", nil)
	if plain.GetConversation() != "hi" || plain.ExtendedTextMessage != nil {
		t.Errorf("plain text should be a Conversation: %v", plain)
	}

	quoted := buildOutboundText("re", "ABC", "1555@s.whatsapp.net", "orig", nil)
	ctx := quoted.GetExtendedTextMessage().GetContextInfo()
	if quoted.GetExtendedTextMessage().GetText() != "re" || ctx.GetStanzaID() != "ABC" || ctx.GetParticipant() != "1555@s.whatsapp.net" || ctx.GetQuotedMessage().GetConversation() != "orig" {
		t.Errorf("quoted reply context: %v", quoted)
	}

	mentions := []string{"1555@s.whatsapp.net", "123@lid"}
	m := buildOutboundText("@x", "", "", "", mentions)
	if got := m.GetExtendedTextMessage().GetContextInfo().GetMentionedJID(); len(got) != 2 || got[1] != "123@lid" {
		t.Errorf("mentions: %v", got)
	}
	if m.GetExtendedTextMessage().GetContextInfo().StanzaID != nil {
		t.Errorf("mentions without quote must not set StanzaID")
	}
}

func TestAttachCaptionMentions(t *testing.T) {
	mentions := []string{"1555@s.whatsapp.net"}
	for name, msg := range map[string]*waE2E.Message{
		"image":    {ImageMessage: &waE2E.ImageMessage{}},
		"video":    {VideoMessage: &waE2E.VideoMessage{}},
		"document": {DocumentMessage: &waE2E.DocumentMessage{}},
	} {
		attachCaptionMentions(msg, mentions)
		var got []string
		switch name {
		case "image":
			got = msg.ImageMessage.GetContextInfo().GetMentionedJID()
		case "video":
			got = msg.VideoMessage.GetContextInfo().GetMentionedJID()
		case "document":
			got = msg.DocumentMessage.GetContextInfo().GetMentionedJID()
		}
		if len(got) != 1 {
			t.Errorf("%s: mentions = %v", name, got)
		}
	}
	audio := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}
	attachCaptionMentions(audio, mentions)
	if audio.AudioMessage.ContextInfo != nil {
		t.Errorf("voice notes have no caption, must not get mentions")
	}
	text := &waE2E.Message{Conversation: proto.String("x")}
	attachCaptionMentions(text, nil)
	if text.GetConversation() != "x" {
		t.Errorf("no-op without mentions")
	}
}

func TestApplyChatEphemeralSettings_AllMessageKinds(t *testing.T) {
	settings := ChatEphemeralSettings{Expiration: 86400, SettingTimestamp: 1710000000}
	check := func(name string, ctx *waE2E.ContextInfo) {
		t.Helper()
		if ctx.GetExpiration() != 86400 || ctx.GetEphemeralSettingTimestamp() != 1710000000 || ctx.GetDisappearingMode().GetInitiator() != waE2E.DisappearingMode_CHANGED_IN_CHAT {
			t.Errorf("%s: context = %v", name, ctx)
		}
	}
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}
	applyChatEphemeralSettings(img, settings)
	check("image", img.ImageMessage.GetContextInfo())

	aud := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}
	applyChatEphemeralSettings(aud, settings)
	check("audio", aud.AudioMessage.GetContextInfo())

	vid := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}
	applyChatEphemeralSettings(vid, settings)
	check("video", vid.VideoMessage.GetContextInfo())

	doc := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}
	applyChatEphemeralSettings(doc, settings)
	check("document", doc.DocumentMessage.GetContextInfo())

	// Existing context (a quoted reply) keeps its fields.
	ext := buildOutboundText("re", "ABC", "p@s.whatsapp.net", "orig", nil)
	applyChatEphemeralSettings(ext, settings)
	check("extended", ext.ExtendedTextMessage.GetContextInfo())
	if ext.ExtendedTextMessage.GetContextInfo().GetStanzaID() != "ABC" {
		t.Errorf("quoted StanzaID lost when merging ephemeral settings")
	}

	// Zero settings are a no-op; the plain Conversation is not rewritten.
	plain := &waE2E.Message{Conversation: proto.String("hi")}
	applyChatEphemeralSettings(plain, ChatEphemeralSettings{})
	if plain.ExtendedTextMessage != nil || plain.GetConversation() != "hi" {
		t.Errorf("zero settings must not touch the message: %v", plain)
	}
	applyChatEphemeralSettings(nil, settings) // must not panic
}

func TestResolveRecipientJID_GroupAndMalformed(t *testing.T) {
	installRecordingLogger(t)
	client := newTestClient(&mockLIDStore{})

	group, err := resolveRecipientJID(client, "120363012345678901@g.us")
	if err != nil || group.Server != types.GroupServer {
		t.Errorf("group: %v %v", group, err)
	}

	if _, err := resolveRecipientJID(client, "123:notadevice@s.whatsapp.net"); err == nil {
		t.Errorf("malformed JID must error")
	}
}

func TestSendWhatsAppMessage_NotConnected(t *testing.T) {
	installRecordingLogger(t)
	client := newTestClient(&mockLIDStore{})
	ok, msg, sent := sendWhatsAppMessage(context.Background(), client, nil, "15551234567", "hi", "", "", "", "", nil)
	if ok || msg != "Not connected to WhatsApp" || sent.ID != "" {
		t.Errorf("got ok=%v msg=%q sent=%v", ok, msg, sent)
	}
}
