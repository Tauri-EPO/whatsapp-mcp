package main

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestStructuredHeaderRequiresDownloadCredentials(t *testing.T) {
	for _, kind := range []string{"document", "image", "video"} {
		for _, incomplete := range []string{"thumbnail", "location-without-key", "key-without-location"} {
			doc := fixtureDocument()
			switch incomplete {
			case "thumbnail":
				doc.URL, doc.DirectPath, doc.MediaKey = nil, nil, nil
			case "location-without-key":
				doc.MediaKey = nil
			case "key-without-location":
				doc.URL, doc.DirectPath = nil, nil
			}
			var part cdnMedia = doc
			if kind == "image" {
				part = &waE2E.ImageMessage{URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, JPEGThumbnail: []byte("fake thumbnail")}
			}
			if kind == "video" {
				part = &waE2E.VideoMessage{URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, JPEGThumbnail: []byte("fake thumbnail")}
			}
			for family, msg := range mediaHeaderFixtures(kind, part) {
				for _, batch := range []bool{false, true} {
					t.Run(kind+"/"+incomplete+"/"+family+map[bool]string{false: "/live", true: "/history"}[batch], func(t *testing.T) {
						ms := newTestMessageStore(t)
						ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
						if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
							t.Fatal(err)
						}
						ex := extractMessage(msg, ts, "THUMBNAIL1")
						if ex.mediaType != "" || ex.content == "" {
							t.Fatal("incomplete header became media or lost its body")
						}
						replayWriter(t, ms, batch, func(w messageWriter) error {
							return persistMessage(w, "THUMBNAIL1", mediaTestChat, "x", ts, false, ex, false, testLogger())
						})
						var mediaType, url sql.NullString
						var key []byte
						if err := ms.db.QueryRow("SELECT media_type,url,media_key FROM messages WHERE id='THUMBNAIL1'").Scan(&mediaType, &url, &key); err != nil || mediaType.String != "" || url.String != "" || len(key) != 0 {
							t.Fatal("thumbnail-only header stored unavailable media")
						}
					})
				}
			}
		}
	}
}

func TestHeaderDocumentCaptionWhenBodyEmpty(t *testing.T) {
	doc := fixtureDocument()
	doc.Caption = proto.String("fake header caption")
	msg := &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_DocumentMessage{DocumentMessage: doc}}}
	ms := newTestMessageStore(t)
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	ex := extractMessage(msg, ts, "HEADERCAP1")
	if err := persistMessage(ms, "HEADERCAP1", mediaTestChat, "x", ts, false, ex, false, testLogger()); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='HEADERCAP1'").Scan(&content); err != nil || content != "fake header caption" {
		t.Fatal("header caption lost")
	}
}

func TestHistoryEnvelopesUseSDKOrderWithoutMutatingPayload(t *testing.T) {
	doc := fixtureDocument()
	doc.Caption, doc.FileName = proto.String("fake wrapped caption"), proto.String("Meeting 10:30.pdf")
	image := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: doc.Caption, URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, FileSHA256: doc.FileSHA256, FileEncSHA256: doc.FileEncSHA256, FileLength: doc.FileLength}}
	document := &waE2E.Message{DocumentMessage: doc}
	wrap := func(m *waE2E.Message) *waE2E.FutureProofMessage { return &waE2E.FutureProofMessage{Message: m} }
	for _, tc := range []struct {
		name, kind, text string
		msg              *waE2E.Message
		once             bool
	}{
		{"ephemeral text", "", "fake wrapped text", &waE2E.Message{EphemeralMessage: wrap(&waE2E.Message{Conversation: proto.String("fake wrapped text")})}, false},
		{"ephemeral image", "image", "fake wrapped caption", &waE2E.Message{EphemeralMessage: wrap(image)}, false},
		{"document with caption", "document", "fake wrapped caption", &waE2E.Message{DocumentWithCaptionMessage: wrap(document)}, false},
		{"ephemeral header", "document", "fake buttons body", &waE2E.Message{EphemeralMessage: wrap(mediaHeaderFixtures("document", doc)["buttons"])}, false},
		{"device sent", "image", "fake wrapped caption", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: image}}, false},
		{"edited document", "document", "fake wrapped caption", &waE2E.Message{EditedMessage: wrap(document)}, false},
		{"nested SDK order", "document", "🔒 fake wrapped caption", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{EphemeralMessage: wrap(&waE2E.Message{ViewOnceMessageV2: wrap(&waE2E.Message{DocumentWithCaptionMessage: wrap(&waE2E.Message{EditedMessage: wrap(document)})})})}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := newTestMessageStore(t)
			client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
			b := testBridge(t, client, ms, testLogger())
			history := largeHistoryFixture(1)
			history.Data.Conversations[0].Messages[0].Message.Message = tc.msg
			before := proto.Clone(tc.msg)
			b.handleHistorySync(history)
			var content, kind string
			var once bool
			if err := ms.db.QueryRow("SELECT content,media_type,view_once FROM messages WHERE id='H0'").Scan(&content, &kind, &once); err != nil || content != tc.text || kind != tc.kind || once != tc.once {
				t.Fatalf("history content=%q kind=%q flag=%t err=%v", content, kind, once, err)
			}
			if !proto.Equal(before, tc.msg) {
				t.Fatal("history mutated SDK payload")
			}
			if tc.kind != "" {
				var url, path string
				var key, sha, enc []byte
				var length int64
				if err := ms.db.QueryRow("SELECT url,direct_path,media_key,file_sha256,file_enc_sha256,file_length FROM messages WHERE id='H0'").Scan(&url, &path, &key, &sha, &enc, &length); err != nil || url != doc.GetURL() || path != doc.GetDirectPath() || !bytes.Equal(key, doc.MediaKey) || !bytes.Equal(sha, doc.FileSHA256) || !bytes.Equal(enc, doc.FileEncSHA256) || length != int64(doc.GetFileLength()) {
					t.Fatal("wrapped history lost CDN fields")
				}
			}
		})
	}
}

func TestEnvelopeContextInheritanceDoesNotMutateInner(t *testing.T) {
	inner := &waE2E.Message{Conversation: proto.String("fake text")}
	metadata := &waE2E.MessageContextInfo{}
	raw := &waE2E.Message{MessageContextInfo: metadata, EphemeralMessage: &waE2E.FutureProofMessage{Message: inner}}
	ex := extractMessage(raw, time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC), "CONTEXTWRAP1")
	if ex.content != "fake text" || !proto.Equal(ex.inner.GetMessageContextInfo(), metadata) || inner.MessageContextInfo != nil {
		t.Fatal("wrapper metadata inheritance mutated or lost")
	}
}
