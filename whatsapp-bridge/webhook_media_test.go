package main

// The webhook's copy of an image is read through the store root, and nothing
// on the way is followed (issue #493).

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

// A PNG signature and a little more: enough for http.DetectContentType, and
// not the .jpg the generated file name claims.
var webhookTestPNG = []byte("\x89PNG\r\n\x1a\n" + "not really pixels")

const webhookTestName = "image_20260904_150405_IMG1.jpg"

// webhookMediaStore builds a store with one cached image in chat and two files
// no webhook may carry: one outside the store, one inside it.
func webhookMediaStore(t *testing.T, chat string) (chatDir string, victims []string) {
	t.Helper()
	scratch := storeInScratch(t)
	chatDir = chatMediaDir(chat)
	if err := os.Mkdir(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, webhookTestName), webhookTestPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	victims = []string{filepath.Join(scratch, "outside.db"), storePath("victim.db")}
	for _, v := range victims {
		if err := os.WriteFile(v, []byte("victim-content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return chatDir, victims
}

func TestWebhookMedia_ReadsTheCachedFileAndSniffsItsType(t *testing.T) {
	webhookMediaStore(t, mediaTestChat)
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, newTestMessageStore(t), rec)

	mimeType, data := b.webhookMedia(mediaTestChat, webhookTestName)
	if mimeType != "image/png" || string(data) != string(webhookTestPNG) {
		t.Fatalf("mimeType=%q data=%q, want the file sniffed as image/png", mimeType, data)
	}
	if strings.Contains(rec.String(), "[WARN]") {
		t.Errorf("an ordinary read logged a warning:\n%s", rec.String())
	}
}

// Every way of making the name lead somewhere else yields no bytes and a WARN:
// the old os.ReadFile followed each of the links and put the target in the
// payload.
func TestWebhookMedia_FollowsNoLinkAndLeavesNoStore(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, chatDir string, victims []string) (chatJID, filename string)
	}{
		{"the file is a symlink out of the store", func(t *testing.T, chatDir string, victims []string) (string, string) {
			symlinkOrSkip(t, victims[0], filepath.Join(chatDir, "linked.jpg"))
			return mediaTestChat, "linked.jpg"
		}},
		{"the file is a symlink to another file of the store", func(t *testing.T, chatDir string, _ []string) (string, string) {
			symlinkOrSkip(t, "../victim.db", filepath.Join(chatDir, "linked.jpg"))
			return mediaTestChat, "linked.jpg"
		}},
		{"the chat directory is a symlink out of the store", func(t *testing.T, _ string, victims []string) (string, string) {
			symlinkOrSkip(t, filepath.Dir(victims[0]), storePath("outside@g.us"))
			return "outside@g.us", "outside.db"
		}},
		{"the chat directory is a symlink to another directory of the store", func(t *testing.T, chatDir string, _ []string) (string, string) {
			symlinkOrSkip(t, filepath.Base(chatDir), storePath("alias@g.us"))
			return "alias@g.us", webhookTestName
		}},
		{"the file name climbs out of the chat directory", func(*testing.T, string, []string) (string, string) {
			return mediaTestChat, "../victim.db"
		}},
		{"the chat JID climbs out of the store", func(*testing.T, string, []string) (string, string) {
			return "..", "outside.db"
		}},
		{"the name is a directory", func(t *testing.T, chatDir string, _ []string) (string, string) {
			if err := os.Mkdir(filepath.Join(chatDir, "dir.jpg"), 0o700); err != nil {
				t.Fatal(err)
			}
			return mediaTestChat, "dir.jpg"
		}},
		{"nothing is cached under that name", func(*testing.T, string, []string) (string, string) {
			return mediaTestChat, "missing.jpg"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatDir, victims := webhookMediaStore(t, mediaTestChat)
			chatJID, filename := tc.plant(t, chatDir, victims)
			rec := installRecordingLogger(t)
			b := testBridge(t, nil, newTestMessageStore(t), rec)

			mimeType, data := b.webhookMedia(chatJID, filename)
			if data != nil || mimeType != "application/octet-stream" {
				t.Fatalf("mimeType=%q data=%q, want no bytes", mimeType, data)
			}
			if !strings.Contains(rec.String(), "[WARN] Could not open media file for the webhook") {
				t.Errorf("want a WARN saying why, got:\n%s", rec.String())
			}
		})
	}
}

func TestWebhookMedia_WithoutAStoreRootReadsNothing(t *testing.T) {
	webhookMediaStore(t, mediaTestChat)
	b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
	b.StoreRoot = nil
	if _, data := b.webhookMedia(mediaTestChat, webhookTestName); data != nil {
		t.Fatalf("read %q without a store root", data)
	}
}

// Over the cap the bytes stay out of the payload, but the type is still sniffed.
func TestWebhookMedia_FileOverTheCapIsSniffedNotRead(t *testing.T) {
	chatDir, _ := webhookMediaStore(t, mediaTestChat)
	big := filepath.Join(chatDir, "big.jpg")
	if err := os.WriteFile(big, webhookTestPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, maxMediaBase64Bytes+1); err != nil { // sparse: no 10 MB written
		t.Fatal(err)
	}
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, newTestMessageStore(t), rec)

	mimeType, data := b.webhookMedia(mediaTestChat, "big.jpg")
	if data != nil || mimeType != "image/png" {
		t.Fatalf("mimeType=%q len(data)=%d, want the type and no bytes", mimeType, len(data))
	}
	if !strings.Contains(rec.String(), "too large for base64 encoding") {
		t.Errorf("want the size warning, got:\n%s", rec.String())
	}
}

// End to end: an inbound image is forwarded with its bytes when the cached file
// is a file, and without them (caption intact) when the name was replaced by a
// symlink after the download.
func TestHandleMessage_WebhookImageComesFromTheStoreRoot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		linkTarget string // "" = leave the cached file alone; "outside" = the file outside the store
		wantBytes  bool
	}{
		{"cached file", "", true},
		{"replaced by a symlink out of the store", "outside", false},
		{"replaced by a symlink to another file of the store", "../victim.db", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := phonePN.String() // the chat handleMessage files the fixture message under
			chatDir, victims := webhookMediaStore(t, chat)
			cached := filepath.Join(chatDir, webhookTestName)
			target := tc.linkTarget
			if target == "outside" {
				target = victims[0]
			}
			if target != "" {
				symlinkOrSkip(t, target, filepath.Join(chatDir, "probe")) // skip early where links need privileges
			}
			srv, webhookCh := captureWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			msg := buildImageMessage(phonePN, phonePN, false, "the caption")
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")

			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), installRecordingLogger(t))
			b.MediaAutoDownload = false
			// The download "succeeds" and names the cached file; what the name
			// points at changes right after, as someone with the store would do it.
			var downloads int
			b.DownloadMedia = func(_ context.Context, _ string, chatJID string) (bool, string, string, string, error) {
				downloads++
				if chatJID != chat {
					t.Errorf("download asked for chat %q, the fixture is in %q", chatJID, chat)
				}
				if target != "" {
					if err := os.Remove(cached); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(target, cached); err != nil {
						t.Error(err)
					}
				}
				return true, "image", webhookTestName, cached, nil
			}
			b.handleMessage(msg)
			if downloads != 1 {
				t.Fatalf("synchronous downloads = %d, want 1: the test proves nothing otherwise", downloads)
			}

			select {
			case payload := <-webhookCh:
				if payload.Content != "the caption" || payload.MediaType != "image" {
					t.Fatalf("payload = %+v, want the image message with its caption", payload)
				}
				want := ""
				if tc.wantBytes {
					want = base64.StdEncoding.EncodeToString(webhookTestPNG)
				}
				if payload.MediaBase64 != want {
					got, _ := base64.StdEncoding.DecodeString(payload.MediaBase64)
					t.Fatalf("media in the payload = %q, want %d encoded byte(s)", got, len(want))
				}
				if tc.wantBytes && payload.MimeType != "image/png" {
					t.Errorf("MimeType = %q, want the sniffed image/png", payload.MimeType)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the webhook must be delivered either way")
			}
		})
	}
}
