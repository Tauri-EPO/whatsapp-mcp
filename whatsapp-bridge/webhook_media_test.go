package main

// The webhook's copy of an image is read through the store root, nothing on
// the way is followed, and the bytes must hash to what the message declared
// (issue #493).

import (
	"context"
	"crypto/sha256"
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

const (
	webhookTestName  = "image_20260904_150405_IMG1.jpg"
	webhookOpenWarn  = "[WARN] Could not open media file for the webhook"
	webhookHashWarn  = "[WARN] Cached media is not the file the message declared"
	webhookNoContent = "application/octet-stream"
)

// sha256Of is the hash a message declares for those bytes (file_sha256).
func sha256Of(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

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

// hardLinkOrSkip links target to name, skipping where the file system has no
// hard links.
func hardLinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Link(target, name); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
}

func TestWebhookMedia_ReadsTheCachedFileAndSniffsItsType(t *testing.T) {
	webhookMediaStore(t, mediaTestChat)
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, newTestMessageStore(t), rec)

	mimeType, data := b.webhookMedia(mediaTestChat, webhookTestName, sha256Of(webhookTestPNG))
	if mimeType != "image/png" || string(data) != string(webhookTestPNG) {
		t.Fatalf("mimeType=%q data=%q, want the file sniffed as image/png", mimeType, data)
	}
	if strings.Contains(rec.String(), "[WARN]") {
		t.Errorf("an ordinary read logged a warning:\n%s", rec.String())
	}
}

// Every way of making the name lead somewhere else yields no bytes and a WARN:
// the old os.ReadFile followed each of the links and put the target in the
// payload. The hash is the victim's own in every case, so only the path checks
// stand between the file and the webhook.
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
		{"the file is a symlink to another file of the same chat", func(t *testing.T, chatDir string, _ []string) (string, string) {
			if err := os.WriteFile(filepath.Join(chatDir, "other.jpg"), []byte("victim-content"), 0o600); err != nil {
				t.Fatal(err)
			}
			symlinkOrSkip(t, "other.jpg", filepath.Join(chatDir, "linked.jpg"))
			return mediaTestChat, "linked.jpg"
		}},
		{"the chat directory is a symlink out of the store", func(t *testing.T, _ string, victims []string) (string, string) {
			symlinkOrSkip(t, filepath.Dir(victims[0]), storePath("outside@g.us"))
			return "outside@g.us", "outside.db"
		}},
		{"the chat directory is a symlink to the store itself", func(t *testing.T, _ string, _ []string) (string, string) {
			symlinkOrSkip(t, ".", storePath("alias@g.us"))
			return "alias@g.us", "victim.db"
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

			mimeType, data := b.webhookMedia(chatJID, filename, sha256Of([]byte("victim-content")))
			if len(data) != 0 || mimeType != webhookNoContent {
				t.Fatalf("mimeType=%q data=%q, want no bytes", mimeType, data)
			}
			if !strings.Contains(rec.String(), webhookOpenWarn) {
				t.Errorf("want a WARN saying why, got:\n%s", rec.String())
			}
		})
	}
}

// A hard link or a rename puts another regular file of the store under the
// cached name with no link to refuse, so the bytes are bound to the message:
// only a file that hashes to the declared file_sha256 is sent.
func TestWebhookMedia_SendsOnlyTheBytesTheMessageDeclared(t *testing.T) {
	declared := sha256Of(webhookTestPNG)
	cases := []struct {
		name  string
		want  []byte
		plant func(t *testing.T, cached string, victims []string)
	}{
		{"a hard link to another file of the store", declared, func(t *testing.T, cached string, victims []string) {
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			hardLinkOrSkip(t, victims[1], cached)
		}},
		{"a hard link to a file outside the store", declared, func(t *testing.T, cached string, victims []string) {
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			hardLinkOrSkip(t, victims[0], cached)
		}},
		{"another store file renamed onto the name", declared, func(t *testing.T, cached string, victims []string) {
			if err := os.Rename(victims[1], cached); err != nil {
				t.Fatal(err)
			}
		}},
		{"the message declared no hash", nil, func(*testing.T, string, []string) {}},
		{"the message declared another hash", sha256Of([]byte("something else")), func(*testing.T, string, []string) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatDir, victims := webhookMediaStore(t, mediaTestChat)
			tc.plant(t, filepath.Join(chatDir, webhookTestName), victims)
			rec := installRecordingLogger(t)
			b := testBridge(t, nil, newTestMessageStore(t), rec)

			mimeType, data := b.webhookMedia(mediaTestChat, webhookTestName, tc.want)
			if len(data) != 0 || mimeType != webhookNoContent {
				t.Fatalf("mimeType=%q data=%q, want no bytes", mimeType, data)
			}
			if !strings.Contains(rec.String(), webhookHashWarn) {
				t.Errorf("want the hash WARN, got:\n%s", rec.String())
			}
		})
	}
}

func TestWebhookMedia_WithoutAStoreRootReadsNothing(t *testing.T) {
	webhookMediaStore(t, mediaTestChat)
	b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
	b.StoreRoot = nil
	if _, data := b.webhookMedia(mediaTestChat, webhookTestName, sha256Of(webhookTestPNG)); len(data) != 0 {
		t.Fatalf("read %q without a store root", data)
	}
}

// An empty cached file has no bytes to sniff: the type stays the neutral one,
// not the text/plain http.DetectContentType gives an empty slice.
func TestWebhookMedia_EmptyFileHasNoType(t *testing.T) {
	chatDir, _ := webhookMediaStore(t, mediaTestChat)
	if err := os.WriteFile(filepath.Join(chatDir, "empty.jpg"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
	mimeType, data := b.webhookMedia(mediaTestChat, "empty.jpg", sha256Of(nil))
	if len(data) != 0 || mimeType != webhookNoContent {
		t.Fatalf("mimeType=%q len(data)=%d, want %s and no bytes", mimeType, len(data), webhookNoContent)
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

	mimeType, data := b.webhookMedia(mediaTestChat, "big.jpg", sha256Of(webhookTestPNG))
	if len(data) != 0 || mimeType != "image/png" {
		t.Fatalf("mimeType=%q len(data)=%d, want the type and no bytes", mimeType, len(data))
	}
	if !strings.Contains(rec.String(), "too large for base64 encoding") {
		t.Errorf("want the size warning, got:\n%s", rec.String())
	}
}

// End to end: an inbound image is forwarded with its bytes when the cached file
// is the file the message declared, and without them (caption intact, neutral
// type, a WARN) when the name was made to lead elsewhere after the download.
func TestHandleMessage_WebhookImageComesFromTheStoreRoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		swap     func(cached string, victims []string) error // nil = leave the cached file alone
		wantWarn string                                      // "" = the bytes are sent
	}{
		{"cached file", nil, ""},
		{"replaced by a symlink out of the store", func(cached string, victims []string) error {
			return os.Symlink(victims[0], cached)
		}, webhookOpenWarn},
		{"replaced by a symlink to another file of the store", func(cached string, _ []string) error {
			return os.Symlink("../victim.db", cached)
		}, webhookOpenWarn},
		{"replaced by a hard link to another file of the store", func(cached string, victims []string) error {
			return os.Link(victims[1], cached)
		}, webhookHashWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := phonePN.String() // the chat handleMessage files the fixture message under
			chatDir, victims := webhookMediaStore(t, chat)
			cached := filepath.Join(chatDir, webhookTestName)
			if tc.swap != nil {
				// Skip early where links need privileges or do not exist.
				if err := tc.swap(filepath.Join(chatDir, "probe"), victims); err != nil {
					t.Skipf("links unavailable here: %v", err)
				}
			}
			srv, webhookCh := captureWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			msg := buildImageMessage(phonePN, phonePN, false, "the caption")
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
			msg.Message.ImageMessage.FileSHA256 = sha256Of(webhookTestPNG)

			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), rec)
			b.MediaAutoDownload = false
			// The download "succeeds" and names the cached file; what the name
			// leads to changes right after, as someone with the store would do it.
			var downloads int
			b.DownloadMedia = func(_ context.Context, _ string, chatJID string) (bool, string, string, string, error) {
				downloads++
				if chatJID != chat {
					t.Errorf("download asked for chat %q, the fixture is in %q", chatJID, chat)
				}
				if tc.swap != nil {
					if err := os.Remove(cached); err != nil {
						t.Error(err)
					}
					if err := tc.swap(cached, victims); err != nil {
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
				wantMedia, wantType := base64.StdEncoding.EncodeToString(webhookTestPNG), "image/png"
				if tc.wantWarn != "" {
					wantMedia, wantType = "", webhookNoContent
				}
				if payload.MediaBase64 != wantMedia {
					got, _ := base64.StdEncoding.DecodeString(payload.MediaBase64)
					t.Fatalf("media in the payload = %q, want %d encoded byte(s)", got, len(wantMedia))
				}
				if payload.MimeType != wantType {
					t.Errorf("MimeType = %q, want %q", payload.MimeType, wantType)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the webhook must be delivered either way")
			}
			if tc.wantWarn != "" && !strings.Contains(rec.String(), tc.wantWarn) {
				t.Errorf("want %q in the log, got:\n%s", tc.wantWarn, rec.String())
			}
			if tc.wantWarn == "" && strings.Contains(rec.String(), "[WARN]") {
				t.Errorf("an ordinary image logged a warning:\n%s", rec.String())
			}
		})
	}
}
