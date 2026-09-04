package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"google.golang.org/protobuf/proto"
)

func TestResolveMediaMaxBytes(t *testing.T) {
	if got := resolveMediaMaxBytes(""); got != defaultMediaMaxBytes {
		t.Fatalf("default = %d", got)
	}
	if got := resolveMediaMaxBytes(" 1048576 "); got != 1048576 {
		t.Fatalf("explicit = %d", got)
	}
	if got := resolveMediaMaxBytes("0"); got != 0 {
		t.Fatalf("zero (unlimited) = %d", got)
	}
	if got := resolveMediaMaxBytes("lots"); got != defaultMediaMaxBytes {
		t.Fatalf("garbage should fall back, got %d", got)
	}
}

func TestDownloadToPathLeavesNoPartFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "video.mp4")
	// A nil client fails (ErrClientIsNil) before any bytes are written: the
	// temp file must be removed and the final name must not exist.
	var client *whatsmeow.Client
	_, err := downloadToPath(t.Context(), client, &MediaDownloader{DirectPath: "/v/t62/x.enc", MediaType: whatsmeow.MediaVideo}, target)
	if err == nil {
		t.Fatal("expected an error from a nil client")
	}
	if _, statErr := os.Stat(target + ".part"); !os.IsNotExist(statErr) {
		t.Fatal(".part file must be cleaned up after a failed download")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatal("final file must not exist after a failed download")
	}
}

func TestHandleMessageSkipsAutoDownloadAboveMaxBytes(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	msg := buildImageMessage(phonePN, phonePN, false, "")
	msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
	msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
	msg.Message.ImageMessage.FileLength = proto.Uint64(50 * 1024 * 1024)

	var calls atomic.Int32
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.MediaAutoDownload = true
	b.MediaMaxBytes = 10 * 1024 * 1024
	b.DownloadMedia = func(_ string, _ string) (bool, string, string, string, error) {
		calls.Add(1)
		return false, "", "", "", nil
	}
	b.handleMessage(msg)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("auto-download ran %d times for a file above the cap", calls.Load())
	}

	// Under the cap (or cap disabled) it downloads as before.
	b.MediaMaxBytes = 0
	msg.Info.ID = "IMG2"
	b.handleMessage(msg)
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("auto-download should run with the cap disabled, calls=%d", calls.Load())
	}
}
