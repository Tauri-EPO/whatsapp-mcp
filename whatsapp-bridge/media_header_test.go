package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func mediaHeaderFixtures(kind string, part cdnMedia) map[string]*waE2E.Message {
	hydrated := &waE2E.TemplateMessage_HydratedFourRowTemplate{HydratedContentText: proto.String("fake template body")}
	legacy := &waE2E.TemplateMessage_FourRowTemplate{}
	buttons := &waE2E.ButtonsMessage{ContentText: proto.String("fake buttons body")}
	interactive := &waE2E.InteractiveMessage_Header{}
	switch kind {
	case "document":
		doc := part.(*waE2E.DocumentMessage)
		hydrated.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_DocumentMessage{DocumentMessage: doc}
		legacy.Title = &waE2E.TemplateMessage_FourRowTemplate_DocumentMessage{DocumentMessage: doc}
		buttons.Header = &waE2E.ButtonsMessage_DocumentMessage{DocumentMessage: doc}
		interactive.Media = &waE2E.InteractiveMessage_Header_DocumentMessage{DocumentMessage: doc}
	case "image":
		image := part.(*waE2E.ImageMessage)
		hydrated.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_ImageMessage{ImageMessage: image}
		legacy.Title = &waE2E.TemplateMessage_FourRowTemplate_ImageMessage{ImageMessage: image}
		buttons.Header = &waE2E.ButtonsMessage_ImageMessage{ImageMessage: image}
		interactive.Media = &waE2E.InteractiveMessage_Header_ImageMessage{ImageMessage: image}
	case "video":
		video := part.(*waE2E.VideoMessage)
		hydrated.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_VideoMessage{VideoMessage: video}
		legacy.Title = &waE2E.TemplateMessage_FourRowTemplate_VideoMessage{VideoMessage: video}
		buttons.Header = &waE2E.ButtonsMessage_VideoMessage{VideoMessage: video}
		interactive.Media = &waE2E.InteractiveMessage_Header_VideoMessage{VideoMessage: video}
	}
	ia := &waE2E.InteractiveMessage{Header: interactive, Body: &waE2E.InteractiveMessage_Body{Text: proto.String("fake interactive body")}}
	return map[string]*waE2E.Message{
		"hydrated":             {TemplateMessage: &waE2E.TemplateMessage{HydratedTemplate: hydrated}},
		"hydrated format":      {TemplateMessage: &waE2E.TemplateMessage{Format: &waE2E.TemplateMessage_HydratedFourRowTemplate_{HydratedFourRowTemplate: hydrated}}},
		"legacy format":        {TemplateMessage: &waE2E.TemplateMessage{Format: &waE2E.TemplateMessage_FourRowTemplate_{FourRowTemplate: legacy}}},
		"buttons":              {ButtonsMessage: buttons},
		"interactive":          {InteractiveMessage: ia},
		"interactive template": {TemplateMessage: &waE2E.TemplateMessage{Format: &waE2E.TemplateMessage_InteractiveMessageTemplate{InteractiveMessageTemplate: ia}}},
	}
}

func TestStructuredHeaderMediaPersistsEveryCDNField(t *testing.T) {
	doc := fixtureDocument()
	doc.Mimetype = proto.String("application/pdf")
	doc.Title = proto.String("Fake document title")
	image := &waE2E.ImageMessage{URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, FileSHA256: doc.FileSHA256, FileEncSHA256: doc.FileEncSHA256, FileLength: doc.FileLength}
	video := &waE2E.VideoMessage{URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, FileSHA256: doc.FileSHA256, FileEncSHA256: doc.FileEncSHA256, FileLength: doc.FileLength}
	image.Mimetype = proto.String("image/png")
	video.Mimetype = proto.String("video/mp4")
	for kind, part := range map[string]cdnMedia{"document": doc, "image": image, "video": video} {
		for family, message := range mediaHeaderFixtures(kind, part) {
			for _, batch := range []bool{false, true} {
				t.Run(kind+"/"+family+map[bool]string{false: "/live", true: "/history"}[batch], func(t *testing.T) {
					t.Setenv(storeDirEnv, t.TempDir())
					ms := newTestMessageStore(t)
					ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
					if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
						t.Fatal(err)
					}
					ex := extractMessage(message, ts, "HEADER1")
					// Media extraction preserves the existing text projection,
					// including formats whose text was previously unsupported.
					wantText := map[string]string{"hydrated": "fake template body", "buttons": "fake buttons body", "interactive": "fake interactive body"}[family]
					if ex.content != wantText {
						t.Fatalf("header text=%q want=%q", ex.content, wantText)
					}
					if ex.mediaType != kind || ex.directPath != part.GetDirectPath() || ex.url != part.GetURL() || !bytes.Equal(ex.mediaKey, part.GetMediaKey()) || !bytes.Equal(ex.fileSHA, part.GetFileSHA256()) || !bytes.Equal(ex.fileEnc, part.GetFileEncSHA256()) || ex.fileLen != part.GetFileLength() {
						t.Fatalf("missing header extraction: %+v", ex)
					}
					if kind == "document" && ex.filename != doc.GetFileName() {
						t.Fatalf("filename=%q", ex.filename)
					}
					write := func(w messageWriter) error {
						return persistMessage(w, "HEADER1", mediaTestChat, "x", ts, false, ex, false, testLogger())
					}
					var err error
					if batch {
						err = ms.Batch(func(w *messageBatch) error { return write(w) })
					} else {
						err = write(ms)
					}
					if err != nil {
						t.Fatal(err)
					}
					var storedKind, url, content, filename string
					var path sql.NullString
					var key, sha, enc []byte
					var length uint64
					if err := ms.db.QueryRow("SELECT media_type,url,direct_path,media_key,file_sha256,file_enc_sha256,file_length,content,filename FROM messages WHERE id='HEADER1'").Scan(&storedKind, &url, &path, &key, &sha, &enc, &length, &content, &filename); err != nil {
						t.Fatal(err)
					}
					if content != wantText || (kind == "document" && filename != doc.GetFileName()) || storedKind != kind || url != part.GetURL() || path.String != part.GetDirectPath() || !bytes.Equal(key, part.GetMediaKey()) || !bytes.Equal(sha, part.GetFileSHA256()) || !bytes.Equal(enc, part.GetFileEncSHA256()) || length != part.GetFileLength() {
						t.Fatal("header CDN fields lost in persistence")
					}
					var profileJSON string
					if err := ms.db.QueryRow("SELECT media_presentation FROM messages WHERE id='HEADER1'").Scan(&profileJSON); err != nil {
						t.Fatal(err)
					}
					var profile mediaPresentation
					if err := json.Unmarshal([]byte(profileJSON), &profile); err != nil {
						t.Fatal(err)
					}
					wantMIME := map[string]string{"document": "application/pdf", "image": "image/png", "video": "video/mp4"}[kind]
					if profile.MIME != wantMIME || (kind == "document" && (profile.Name == nil || *profile.Name != doc.GetFileName() || profile.Title == nil || *profile.Title != "Fake document title")) {
						t.Fatalf("header presentation lost: %+v", profile)
					}
					b := testBridge(t, nil, ms, testLogger())
					transfers := 0
					b.mediaTransfer = func(_ context.Context, requested whatsmeow.DownloadableMessage, relPath string) (int64, error) {
						transfers++
						if requested.GetDirectPath() != part.GetDirectPath() || !bytes.Equal(requested.GetMediaKey(), part.GetMediaKey()) {
							t.Error("download did not receive header credentials")
							return 0, fmt.Errorf("wrong header credentials")
						}
						return writeLikeDownloadToPath(relPath, []byte("fake media bytes"))
					}
					for range 2 {
						if ok, gotKind, _, _, err := b.downloadMedia(t.Context(), "HEADER1", mediaTestChat); !ok || err != nil || gotKind != kind {
							t.Fatalf("download=%v %s %v", ok, gotKind, err)
						}
					}
					if transfers != 1 {
						t.Fatalf("cache missed after header download: %d", transfers)
					}
				})
			}
		}
	}
}

func TestStructuredHeaderNilAndTopLevelPrecedence(t *testing.T) {
	for _, message := range []*waE2E.Message{nil, {}, {TemplateMessage: &waE2E.TemplateMessage{}}, {ButtonsMessage: &waE2E.ButtonsMessage{}}, {InteractiveMessage: &waE2E.InteractiveMessage{Header: &waE2E.InteractiveMessage_Header{Title: proto.String("text")}}}} {
		if kind, part := mediaPartOf(message); kind != "" || part != nil {
			t.Fatalf("plain header became media: %s", kind)
		}
	}
	message := mediaHeaderFixtures("document", fixtureDocument())["buttons"]
	image := &waE2E.ImageMessage{URL: proto.String("https://example.invalid/top")}
	message.ImageMessage = image
	if kind, part := mediaPartOf(message); kind != "image" || part != image {
		t.Fatal("header took precedence over top-level media")
	}
}

func TestStructuredTextWithoutHeaderMediaIsUnchanged(t *testing.T) {
	for _, msg := range []*waE2E.Message{
		{TemplateMessage: &waE2E.TemplateMessage{HydratedTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{HydratedContentText: proto.String("fake body")}}},
		{ButtonsMessage: &waE2E.ButtonsMessage{ContentText: proto.String("fake body")}},
		{InteractiveMessage: &waE2E.InteractiveMessage{Body: &waE2E.InteractiveMessage_Body{Text: proto.String("fake body")}}},
	} {
		ex := extractMessage(msg, time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC), "TEXTHEADER1")
		if ex.content != "fake body" || ex.mediaType != "" || ex.directPath != "" || len(ex.mediaKey) != 0 {
			t.Fatalf("text-only header changed: %+v", ex)
		}
	}
}
