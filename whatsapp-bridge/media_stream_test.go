package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"google.golang.org/protobuf/proto"
)

func TestResolveMediaMaxBytes(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  uint64
		bad   bool
	}{
		{"", defaultMediaMaxBytes, false}, {" 1048576 ", 1048576, false}, {"0", 0, false},
		{"lots", 0, true}, {"50MB", 0, true}, {"-1", 0, true}, {"+1", 0, true},
		{"18446744073709551616", 0, true},
	} {
		got, err := resolveMediaMaxBytes(tc.value)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("resolveMediaMaxBytes(%q) = %d, %v; want %d, bad=%v", tc.value, got, err, tc.want, tc.bad)
		}
	}
}

func TestDownloadToPathLeavesNoPartFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "video.mp4")
	// A nil client fails (ErrClientIsNil) before any bytes are written: the
	// temp file must be removed and the final name must not exist.
	var client *whatsmeow.Client
	_, err := downloadToPath(t.Context(), storeRootAt(t, dir), client, &MediaDownloader{DirectPath: "/v/t62/x.enc", MediaType: whatsmeow.MediaVideo}, "video.mp4")
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
	msg.Message.ImageMessage.FileSHA256 = []byte("test-sha256")
	msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")
	msg.Message.ImageMessage.FileLength = proto.Uint64(50 * 1024 * 1024)

	var calls atomic.Int32
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.MediaAutoDownload = true
	b.MediaMaxBytes = 10 * 1024 * 1024
	b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
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
