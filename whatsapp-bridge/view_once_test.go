package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func viewOnceImage(caption string) *waE2E.Message {
	img := &waE2E.ImageMessage{
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
	return &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: img}}}
}

func TestUnwrapViewOnce(t *testing.T) {
	plain := &waE2E.Message{Conversation: proto.String("hi")}
	if inner, wrapped := unwrapViewOnce(plain); wrapped || inner != plain {
		t.Fatal("plain message must pass through")
	}
	if _, wrapped := unwrapViewOnce(nil); wrapped {
		t.Fatal("nil is not wrapped")
	}
	for name, env := range map[string]*waE2E.Message{
		"v1":    {ViewOnceMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}}},
		"v2":    viewOnceImage(""),
		"v2ext": {ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}}},
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
	b := testBridge(client, ms, logger)
	b.DownloadMedia = func(_ string, _ string) (bool, string, string, string, error) {
		return false, "", "", "", nil
	}

	evt := buildImageMessage(phonePN, phonePN, false, "")
	evt.Info.ID = "VO1"
	evt.Message = viewOnceImage("só uma vez")
	b.handleMessage(evt)

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
