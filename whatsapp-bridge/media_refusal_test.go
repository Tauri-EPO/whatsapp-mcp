package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestMediaRefusalIsNamedCountedAndNotRequeued(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	srv, _ := captureWebhook(t)
	t.Setenv("WEBHOOK_URL", srv.URL)
	ms := newConcurrentTestStore(t)
	log := installRecordingLogger(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, log)
	b.MediaAutoDownload = true
	b.Connected = func() bool { return true }
	// A mistaken requeue remains observable instead of racing a worker.
	b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)
	msg := buildImageMessage(phonePN, phonePN, false, "caption")
	msg.Info.ID = "BAD/ID"
	msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
	msg.Message.ImageMessage.MediaKey = []byte("key")
	msg.Message.ImageMessage.FileSHA256 = []byte("sha")
	msg.Message.ImageMessage.FileEncSHA256 = []byte("enc")
	msg.Message.ImageMessage.FileLength = proto.Uint64(10)
	b.handleMessage(msg)
	if b.autoDownloads.queued() != 0 || strings.Count(log.String(), "Refusing to cache media") != 1 {
		t.Fatalf("permanent refusal retried: queued=%d log=%s", b.autoDownloads.queued(), log.String())
	}
	if b.metrics.mediaRefusals.Load() != 1 {
		t.Fatal("refusal was not counted")
	}
	metrics := httptest.NewRecorder()
	b.handleMetrics()(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), "whatsapp_bridge_media_refusals_total 1") {
		t.Fatalf("missing metric: %s", metrics.Body.String())
	}
	_, _, _, _, err := b.downloadMedia(context.Background(), msg.Info.ID, phonePN.String())
	if !errors.Is(err, errMediaRefused) || errors.Is(err, errMediaUnavailable) {
		t.Fatalf("wrong classification: %v", err)
	}
	response := httptest.NewRecorder()
	b.handleDownload()(response, httptest.NewRequest(http.MethodPost, "/api/download",
		strings.NewReader(`{"message_id":"BAD/ID","chat_jid":"11234567890@s.whatsapp.net"}`)))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"media_refused"`) {
		t.Fatalf("wrong REST contract: %d %s", response.Code, response.Body.String())
	}
}
