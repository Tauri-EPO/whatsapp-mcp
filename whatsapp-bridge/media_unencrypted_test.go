package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestUnencryptedNewsletterLiveToSDKPlaintextHTTPAndCache(t *testing.T) {
	for _, shape := range []string{"plaintext", "empty-blobs", "tampered", "key-only", "enc-only", "no-hash"} {
		t.Run(shape, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			b.MediaAutoDownload = false
			data := []byte("synthetic newsletter media bytes")
			sum := sha256.Sum256(data)
			sha := sum[:]
			var key, enc []byte
			if shape == "empty-blobs" {
				key, enc = []byte{}, []byte{}
			}
			if shape == "key-only" {
				key = bytes.Repeat([]byte{1}, 32)
			}
			if shape == "enc-only" {
				enc = bytes.Repeat([]byte{1}, 32)
			}
			if shape == "no-hash" {
				sha = nil
			}
			if shape == "tampered" {
				sha = bytes.Repeat([]byte{1}, 32)
			}
			chat := types.NewJID("120363000000000001", types.NewsletterServer)
			event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
			event.Info.ID, event.Info.Timestamp = "NEWSLETTER", time.Unix(1772359200, 0)
			event.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("channel caption"), DirectPath: proto.String("/plaintext?source=fake"), FileSHA256: sha, MediaKey: key, FileEncSHA256: enc, FileLength: proto.Uint64(uint64(len(data)))}}
			b.handleEvent(event, nil)
			var savedChat string
			if err := ms.db.QueryRow("SELECT chat_jid FROM messages WHERE id='NEWSLETTER'").Scan(&savedChat); err != nil || savedChat != chat.String() {
				t.Fatalf("channel row absent: chat=%s err=%v", savedChat, err)
			}
			var requests atomic.Int32
			transfers := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); _, _ = w.Write(data) }))
			defer srv.Close()
			b.Client.SetMediaHTTPClient(srv.Client())
			b.mediaTransfer = func(ctx context.Context, msg whatsmeow.DownloadableMessage, rel string) (int64, error) {
				transfers++
				if msg.GetMediaKey() != nil || msg.GetFileEncSHA256() != nil || msg.GetDirectPath() != "/plaintext?source=fake" {
					t.Fatal("plaintext adapter changed SDK shape")
				}
				return writeMediaDownload(ctx, b.StoreRoot, rel, func(downloadCtx context.Context, file whatsmeow.File) error {
					//nolint:staticcheck // test bypasses paired media-host discovery only.
					return b.Client.DangerousInternals().DownloadAndDecryptToFile(downloadCtx, srv.URL, msg.GetMediaKey(), whatsmeow.MediaImage, msg.GetFileEncSHA256(), msg.GetFileSHA256(), file)
				})
			}
			ok, _, _, file, err := b.downloadMedia(context.Background(), "NEWSLETTER", chat.String())
			switch shape {
			case "plaintext", "empty-blobs":
				if err != nil || !ok || transfers != 1 || requests.Load() != 1 {
					t.Fatalf("plaintext transfer=%d HTTP=%d ok=%v err=%v", transfers, requests.Load(), ok, err)
				}
				got, readErr := os.ReadFile(file) //nolint:gosec // file is the validated cache path inside this isolated test store.
				if readErr != nil || !bytes.Equal(got, data) {
					t.Fatal("SDK plaintext bytes changed")
				}
				if ok, _, _, _, err = b.downloadMedia(context.Background(), "NEWSLETTER", chat.String()); err != nil || !ok || transfers != 1 {
					t.Fatal("second download missed cache")
				}
			case "tampered":
				if ok || transfers != 1 || requests.Load() != 1 || !errors.Is(err, whatsmeow.ErrInvalidUnencryptedMediaSHA256) {
					t.Fatalf("hash gate bypassed: %v transfers=%d HTTP=%d", err, transfers, requests.Load())
				}
			default:
				if ok || !errors.Is(err, errMediaUnavailable) || transfers != 0 || requests.Load() != 0 {
					t.Fatalf("incomplete credentials reached transfer: %v transfers=%d HTTP=%d", err, transfers, requests.Load())
				}
			}
		})
	}
}

func TestUnencryptedExceptionPreservesEncryptedReplayAndRejectsOtherChats(t *testing.T) {
	for _, chat := range []string{phonePN.String(), "120363000000000001@g.us"} {
		t.Run(chat, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, nil, ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			if err := ms.StoreChat(chat, "Alice", stamp); err != nil {
				t.Fatal(err)
			}
			key, sha, enc := []byte("key"), []byte("hash"), []byte("encrypted hash")
			store := func(id, caption string, key, enc []byte, ts time.Time) {
				t.Helper()
				if err := ms.StoreMessage(id, chat, phonePN.String(), caption, ts, false, "image", "", "https://example.invalid/media", key, sha, enc, uint64(3), "", messageMediaOptions{directPath: "/fake-media"}); err != nil {
					t.Fatal(err)
				}
			}
			store("ENCRYPTED", "original caption", key, enc, stamp)
			transfers := 0
			b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
				transfers++
				return writeLikeDownloadToPath(rel, []byte("abc"))
			}
			ok, _, _, cached, err := b.downloadMedia(context.Background(), "ENCRYPTED", chat)
			if !ok || err != nil {
				t.Fatalf("initial encrypted download: %v", err)
			}
			store("ENCRYPTED", "", nil, nil, stamp.Add(time.Hour))
			var savedKey, savedEnc []byte
			var caption string
			var timestamp time.Time
			if err := ms.db.QueryRow("SELECT media_key, file_enc_sha256, content, timestamp FROM messages WHERE id=? AND chat_jid=?", "ENCRYPTED", chat).Scan(&savedKey, &savedEnc, &caption, &timestamp); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(savedKey, key) || !bytes.Equal(savedEnc, enc) || caption != "original caption" || !timestamp.Equal(stamp) {
				t.Fatalf("sparse replay erased encrypted snapshot: key=%q enc=%q caption=%q timestamp=%q", savedKey, savedEnc, caption, timestamp)
			}
			if ok, _, _, again, err := b.downloadMedia(context.Background(), "ENCRYPTED", chat); !ok || err != nil || again != cached || transfers != 1 {
				t.Fatalf("replay lost cache: transfers=%d err=%v", transfers, err)
			}
			store("INCOMPLETE", "", nil, nil, stamp)
			if ok, _, _, _, err := b.downloadMedia(context.Background(), "INCOMPLETE", chat); ok || !errors.Is(err, errMediaUnavailable) || transfers != 1 {
				t.Fatalf("non-newsletter plaintext reached transfer: transfers=%d err=%v", transfers, err)
			}
		})
	}
}
