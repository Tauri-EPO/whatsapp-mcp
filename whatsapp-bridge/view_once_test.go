package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func viewOnceImage(caption string) *waProto.Message {
	img := &waProto.ImageMessage{
		URL:           proto.String("https://mmg.whatsapp.net/v/vo.enc?oh=1&oe=2"),
		MediaKey:      []byte("key"),
		FileSHA256:    []byte("sha"),
		FileEncSHA256: []byte("encsha"),
		FileLength:    proto.Uint64(10),
		ViewOnce:      proto.Bool(true),
	}
	if caption != "" {
		img.Caption = proto.String(caption)
	}
	return &waProto.Message{ViewOnceMessageV2: &waProto.FutureProofMessage{Message: &waProto.Message{ImageMessage: img}}}
}

func TestUnwrapViewOnce(t *testing.T) {
	plain := &waProto.Message{Conversation: proto.String("hi")}
	if inner, wrapped := unwrapViewOnce(plain); wrapped || inner != plain {
		t.Fatal("plain message must pass through")
	}
	if _, wrapped := unwrapViewOnce(nil); wrapped {
		t.Fatal("nil is not wrapped")
	}
	for name, env := range map[string]*waProto.Message{
		"v1":    {ViewOnceMessage: &waProto.FutureProofMessage{Message: &waProto.Message{AudioMessage: &waProto.AudioMessage{}}}},
		"v2":    viewOnceImage(""),
		"v2ext": {ViewOnceMessageV2Extension: &waProto.FutureProofMessage{Message: &waProto.Message{VideoMessage: &waProto.VideoMessage{}}}},
	} {
		inner, wrapped := unwrapViewOnce(env)
		if !wrapped || inner == nil || inner == env {
			t.Fatalf("%s: not unwrapped", name)
		}
	}
	if got := viewOnceContent("", "image"); got != "🔒 view-once image" {
		t.Fatalf("content = %q", got)
	}
	if got := viewOnceContent(" look ", "image"); got != "🔒 look" {
		t.Fatalf("content = %q", got)
	}
}

func TestHandleMessage_ViewOnceImageIsStored(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	client := newTestClient(&mockLIDStore{})
	ms := newTestMessageStore(t)
	logger := testLogger()

	// Never download in this test: replace the async downloader.
	orig := downloadMediaForMessage
	downloadMediaForMessage = func(_ *whatsmeow.Client, _ *MessageStore, _ string, _ string) (bool, string, string, string, error) {
		return false, "", "", "", nil
	}
	t.Cleanup(func() { downloadMediaForMessage = orig })

	evt := buildImageMessage(phonePN, phonePN, false, "")
	evt.Info.ID = "VO1"
	evt.Message = viewOnceImage("só uma vez")
	handleMessage(client, ms, evt, logger)

	var content, mediaType, url string
	var viewOnce bool
	err := ms.db.QueryRow(`SELECT content, media_type, url, view_once FROM messages WHERE id = 'VO1'`).Scan(&content, &mediaType, &url, &viewOnce)
	if err != nil {
		t.Fatalf("view-once row missing: %v", err)
	}
	if mediaType != "image" || !viewOnce || content != "🔒 só uma vez" || url == "" {
		t.Fatalf("stored %q / %q / view_once=%v / url=%q", mediaType, content, viewOnce, url)
	}
}
