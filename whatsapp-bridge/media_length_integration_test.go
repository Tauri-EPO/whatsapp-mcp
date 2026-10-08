package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestArrivalDistinguishesUnknownEmptyAndActualOversize(t *testing.T) {
	for _, name := range []string{"unknown", "empty", "lying oversized", "small"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			t.Setenv("WEBHOOK_ENABLED", "false")
			ms := newConcurrentTestStore(t)
			log := installRecordingLogger(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, log)
			b.MediaAutoDownload, b.MediaMaxBytes = true, 16
			done := make(chan struct{}, 1)
			b.autoDownloads = newMediaJobQueue(b.ctx, 1, 4, func(ctx context.Context, job mediaJob) {
				b.runAutoDownload(ctx, job)
				done <- struct{}{}
			})
			calls := 0
			b.mediaTransfer = func(ctx context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
				calls++
				return writeMediaDownload(ctx, b.StoreRoot, relPath, func(_ context.Context, file whatsmeow.File) error {
					if name == "lying oversized" {
						_, err := file.Write(bytes.Repeat([]byte("x"), 1024))
						return err
					}
					wire, plain := 42, int64(16)
					if name == "empty" {
						wire, plain = 26, 0
					}
					if _, err := file.Write(bytes.Repeat([]byte("x"), wire)); err != nil {
						return err
					}
					return file.Truncate(plain)
				})
			}
			doc := fixtureDocument()
			doc.FileName, doc.FileLength = proto.String("file.bin"), proto.Uint64(1)
			if name == "empty" {
				doc.FileLength = proto.Uint64(0)
			}
			if name == "unknown" {
				doc.FileLength = nil
			}
			msg := buildImageMessage(phonePN, phonePN, false, "")
			msg.Info.ID, msg.Message = "ARRIVAL1", &waE2E.Message{DocumentMessage: doc}
			b.handleMessage(msg)
			var stored sql.NullInt64
			if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id = ? AND chat_jid = ?", msg.Info.ID, phonePN.String()).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored.Valid != (name != "unknown") || (name == "empty" && stored.Int64 != 0) {
				t.Fatalf("stored length=%v", stored)
			}
			if name != "unknown" {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("automatic job did not finish")
				}
			} else if calls != 0 || b.autoDownloads.queued() != 0 {
				t.Fatal("unknown length was fetched automatically")
			}
			relPath := chatMediaRel(phonePN.String()) + "/" + mediaFileName("document", msg.Info.Timestamp, msg.Info.ID, doc.GetFileName())
			if name == "lying oversized" {
				metrics := httptest.NewRecorder()
				b.handleMetrics()(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if !strings.Contains(metrics.Body.String(), "whatsapp_bridge_media_autodownload_size_skips_total 1") || b.metrics.mediaDownloadFails.Load() != 0 || !strings.Contains(log.String(), "download_media still fetches it") || strings.Contains(log.String(), "Auto-download failed") {
					t.Fatalf("cap skip observability: metrics=%s log=%s", metrics.Body.String(), log.String())
				}
			}
			info, err := b.StoreRoot.Stat(relPath)
			if name == "unknown" || name == "lying oversized" {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("unexpected cache: info=%v err=%v", info, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if name == "empty" && info.Size() != 0 {
					t.Fatal("empty file not cached")
				}
				if ok, _, _, _, err := b.downloadMedia(t.Context(), msg.Info.ID, phonePN.String()); err != nil || !ok || calls != 1 {
					t.Fatalf("cache lookup: ok=%v calls=%d err=%v", ok, calls, err)
				}
			}
			if _, err := b.StoreRoot.Stat(relPath + ".part"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary file left: %v", err)
			}
			if name == "unknown" || name == "lying oversized" {
				// Explicit downloads remain available, including a retry after a
				// capped automatic transfer. NULL is not a missing-key refusal.
				before := calls
				if ok, _, _, _, err := b.downloadMedia(t.Context(), msg.Info.ID, phonePN.String()); err != nil || !ok || calls != before+1 {
					t.Fatalf("manual fetch: ok=%v calls=%d err=%v", ok, calls, err)
				}
			}
			if name == "small" {
				// A manual cache larger than another caller's cap is kept, but
				// cannot be reported as an automatic download within that cap.
				if ok, _, _, _, err := b.downloadMedia(withMediaLimit(t.Context(), 8), msg.Info.ID, phonePN.String()); ok || !errors.Is(err, errAutoMediaLimit) {
					t.Fatalf("oversized shared cache: ok=%v err=%v", ok, err)
				}
				if ok, _, _, _, err := b.downloadMedia(t.Context(), msg.Info.ID, phonePN.String()); !ok || err != nil || calls != 1 {
					t.Fatalf("manual cache retained: ok=%v calls=%d err=%v", ok, calls, err)
				}
			}
		})
	}
}

func TestSynchronousImageActualCapIsNotRequeued(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	srv, received := captureWebhook(t)
	t.Setenv("WEBHOOK_URL", srv.URL)
	ms := newConcurrentTestStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	b.MediaAutoDownload, b.MediaMaxBytes = true, 16
	b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)
	calls := 0
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		calls++
		return writeMediaDownload(ctx, b.StoreRoot, relPath, func(_ context.Context, file whatsmeow.File) error {
			_, err := file.Write(bytes.Repeat([]byte("x"), 1024))
			return err
		})
	}
	msg := buildImageMessage(phonePN, phonePN, false, "caption")
	msg.Info.ID = "SYNC1"
	image := msg.Message.ImageMessage
	image.URL, image.MediaKey = proto.String(fixtureMediaURL), []byte("key")
	image.FileSHA256, image.FileEncSHA256 = []byte("sha"), []byte("enc")
	image.FileLength = proto.Uint64(1)
	b.handleMessage(msg)
	if calls != 1 || b.autoDownloads.queued() != 0 {
		t.Fatalf("calls=%d queued=%d", calls, b.autoDownloads.queued())
	}
	relPath := chatMediaRel(phonePN.String()) + "/" + mediaFileName("image", msg.Info.Timestamp, msg.Info.ID, "")
	if _, err := b.StoreRoot.Stat(relPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oversized synchronous image cached: %v", err)
	}
	if _, err := b.StoreRoot.Stat(relPath + ".part"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("synchronous temporary file left: %v", err)
	}
	if b.metrics.mediaDownloadFails.Load() != 0 || b.metrics.mediaAutoSizeSkips.Load() != 1 {
		t.Fatal("automatic cap must count a size skip, not a failed transfer")
	}
	select {
	case payload := <-received:
		if payload.MediaBase64 != "" {
			t.Fatal("oversized image attached to webhook")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("image event was not delivered")
	}
}
