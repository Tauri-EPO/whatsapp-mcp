package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestForwardSniffsCachedMediaAndKeepsCachePaths(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, category, mime string
		data                 []byte
	}{
		{"PNG", "image", "image/png", png},
		{"WebP", "image", "image/webp", []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")},
		{"GIF", "image", "image/gif", []byte("GIF89a\x01\x00\x01\x00fake")},
		{"MOV", "video", "video/quicktime", []byte("\x00\x00\x00\x18ftypqt  \x00\x00\x00\x00qt  \x00\x00\x00\x00")},
		{"AVI", "video", "video/avi", []byte("RIFF\x10\x00\x00\x00AVI fake")},
		{"image with MP4", "image", "image/jpeg", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42\x00\x00\x00\x00")},
		{"video with PNG", "video", "video/mp4", png},
		{"empty image", "image", "image/jpeg", nil},
		{"short image", "image", "image/jpeg", []byte("0123456789")},
		{"empty video", "video", "video/mp4", nil},
		{"short video", "video", "video/mp4", []byte("0123456789")},
		{"BMP", "image", "image/jpeg", []byte("BM fake")},
		{"ICO", "image", "image/jpeg", []byte("\x00\x00\x01\x00fake")},
		{"WebM", "video", "video/mp4", []byte("\x1a\x45\xdf\xa3fake")},
		{"non Opus audio", "audio", "audio/mpeg", []byte("ID3 fake audio")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			b := testBridge(t, nil, ms, testLogger())
			ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
			if err := ms.StoreChat(efChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage("MIME1", efChat, "x", "caption", ts, false, tc.category, "", fixtureMediaURL, []byte("key"), []byte("sha"), []byte("enc"), uint64(len(tc.data)), ""); err != nil {
				t.Fatal(err)
			}
			dir := chatMediaDir(efChat)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			cached := filepath.Join(dir, mediaFileName(tc.category, ts, "MIME1", ""))
			if err := os.WriteFile(cached, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
				t.Error("existing cache must not be downloaded again")
				return 0, fmt.Errorf("unexpected cache transfer")
			}
			client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
			b.Client = client
			b.Connected = func() bool { return true }
			calls, uploads := 0, 0
			network := messageSendNetwork{
				connected: func() bool { return true },
				upload: func(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					uploads++
					if !bytes.Equal(data, tc.data) {
						t.Fatal("sender did not read the cached bytes")
					}
					want := whatsmeow.MediaImage
					if tc.category == "audio" {
						want = whatsmeow.MediaAudio
					}
					if tc.category == "video" {
						want = whatsmeow.MediaVideo
					}
					if kind != want {
						t.Fatalf("upload type=%s want=%s", kind, want)
					}
					return whatsmeow.UploadResponse{URL: fixtureMediaURL, MediaKey: []byte("fake key"), FileSHA256: []byte("fake sha"), FileEncSHA256: []byte("fake enc"), FileLength: uint64(len(data))}, nil
				},
				send: func(_ context.Context, recipient types.JID, packet *waE2E.Message) (whatsmeow.SendResponse, error) {
					calls++
					if recipient.String() != "120363000000000001@g.us" {
						t.Fatalf("recipient=%s", recipient)
					}
					got := packet.GetImageMessage().GetMimetype()
					if tc.category == "audio" {
						got = packet.GetAudioMessage().GetMimetype()
					}
					if tc.category == "video" {
						got = packet.GetVideoMessage().GetMimetype()
					}
					if got != tc.mime {
						t.Fatalf("wire mime=%s want=%s", got, tc.mime)
					}
					return whatsmeow.SendResponse{ID: "FORWARDED1", Timestamp: ts}, nil
				},
			}
			deps := forwardDeps{lookup: ms.messageContentLookup, download: b.downloadMedia,
				resolveRecipient: func(ctx context.Context, w http.ResponseWriter, to string) (string, bool) {
					return b.registeredRecipient(ctx, w, to, nil)
				},
				send: func(ctx context.Context, recipient, caption, mediaPath, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage) {
					if mediaPath != cached {
						t.Fatalf("path changed: %q", mediaPath)
					}
					return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, recipient, caption, mediaPath, quotedID, quotedSender, quotedContent, mentions, network)
				},
			}
			body := fmt.Sprintf(`{"chat_jid":%q,"message_id":"MIME1","to_chat_jid":"120363000000000001@g.us"}`, efChat)
			code, response := efPost(t, handleForwardMessage(deps, chatPolicy{}), body)
			if code != http.StatusOK || !response.Success || calls != 1 || uploads != 1 {
				t.Fatalf("forward=%d %+v sends=%d uploads=%d", code, response, calls, uploads)
			}
			var storedCaption, storedKind string
			var storedLength uint64
			if err := ms.db.QueryRow("SELECT content,media_type,file_length FROM messages WHERE id='FORWARDED1' AND chat_jid='120363000000000001@g.us'").Scan(&storedCaption, &storedKind, &storedLength); err != nil || storedCaption != map[bool]string{true: "", false: "caption"}[tc.category == "audio"] || storedKind != tc.category || storedLength != uint64(len(tc.data)) {
				t.Fatalf("outbound row: caption=%s kind=%s length=%d err=%v", storedCaption, storedKind, storedLength, err)
			}
			if ok, _, _, path, err := b.downloadMedia(t.Context(), "MIME1", efChat); !ok || err != nil || path != cached {
				t.Fatalf("cache lookup=%v %q %v", ok, path, err)
			}
			if code, response := purgeCall(t, b, fmt.Sprintf(`{"items":[{"chat_jid":%q,"message_id":"MIME1"}],"dry_run":false}`, efChat)); code != http.StatusOK || response.PurgedFiles != 1 {
				t.Fatalf("purge=%d %+v", code, response)
			}
			if _, err := os.Stat(cached); !os.IsNotExist(err) {
				t.Fatalf("original cache path was not purged: %v", err)
			}
		})
	}
}

func TestMediaSniffPreservesDocumentAndUnknownContracts(t *testing.T) {
	for _, tc := range []struct {
		path, mime, category string
		data                 []byte
	}{
		{"file.bin", "application/octet-stream", "document", []byte("GIF89a")},
		{"image.jpg", "image/jpeg", "image", []byte("unrecognized image")},
		{"video.mp4", "video/mp4", "video", []byte("unrecognized video")},
	} {
		_, mime, category := classifyMediaData(tc.path, tc.data)
		if mime != tc.mime || category != tc.category {
			t.Fatalf("%s: %s %s", tc.path, mime, category)
		}
	}
}

func TestCachedMediaSniffShortVideoCapacity(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("0123456789"), []byte("\x00\x00\x00\x18ftypqt")} {
		t.Run(fmt.Sprint(len(data)), func(t *testing.T) {
			// os.ReadFile reserves extra capacity. Pin the helper's actual
			// short-slice boundary independently of that allocation detail.
			data = data[:len(data):len(data)]
			_, mime, category := classifyMediaData("video.mp4", data)
			if mime != "video/mp4" || category != "video" {
				t.Fatalf("short video changed: mime=%s category=%s", mime, category)
			}
		})
	}
}
