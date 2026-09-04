package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func TestResolveMediaRetention(t *testing.T) {
	if d, err := resolveMediaRetention(""); err != nil || d != 0 {
		t.Fatalf("unset: got %v, %v", d, err)
	}
	if d, err := resolveMediaRetention(" 30 "); err != nil || d != 30*24*time.Hour {
		t.Fatalf("30 days: got %v, %v", d, err)
	}
	if d, err := resolveMediaRetention("0"); err != nil || d != 0 {
		t.Fatalf("0 disables: got %v, %v", d, err)
	}
	for _, bad := range []string{"-1", "abc", "1.5"} {
		if _, err := resolveMediaRetention(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if got := retentionSummary(0); got != "off" {
		t.Errorf("summary off = %q", got)
	}
	if got := retentionSummary(7 * 24 * time.Hour); got != "7 days" {
		t.Errorf("summary 7d = %q", got)
	}
}

// seedStore writes a fake store: two databases at the root, one chat dir with
// an old and a fresh media file. Returns the paths keyed by role.
func seedStore(t *testing.T, now time.Time) (string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	chat := filepath.Join(root, "5511999999999@s.whatsapp.net")
	if err := os.MkdirAll(chat, 0o750); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"db":    filepath.Join(root, "messages.db"),
		"token": filepath.Join(root, ".bridge-token"),
		"old":   filepath.Join(chat, "20260101_old.jpg"),
		"fresh": filepath.Join(chat, "20260904_fresh.jpg"),
	}
	for role, p := range paths {
		if err := os.WriteFile(p, []byte(role+"-content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-40 * 24 * time.Hour)
	for _, role := range []string{"db", "token", "old"} {
		if err := os.Chtimes(paths[role], old, old); err != nil {
			t.Fatal(err)
		}
	}
	return root, paths
}

func TestSweepMediaRemovesOnlyOldChatFiles(t *testing.T) {
	now := time.Now()
	root, paths := seedStore(t, now)

	removed, freed, failed := sweepMedia(root, 30*24*time.Hour, now)
	if removed != 1 || failed != 0 {
		t.Fatalf("removed=%d failed=%d, want 1/0", removed, failed)
	}
	if freed != int64(len("old-content")) {
		t.Fatalf("freed=%d", freed)
	}
	if _, err := os.Stat(paths["old"]); !os.IsNotExist(err) {
		t.Fatal("old media should be gone")
	}
	for _, role := range []string{"db", "token", "fresh"} {
		if _, err := os.Stat(paths[role]); err != nil {
			t.Errorf("%s must survive the sweep: %v", role, err)
		}
	}

	// Nothing else to do on the second pass.
	if removed, _, _ := sweepMedia(root, 30*24*time.Hour, now); removed != 0 {
		t.Fatalf("second sweep removed %d", removed)
	}
	// A missing root is reported as one failure, not a panic.
	if _, _, failed := sweepMedia(filepath.Join(root, "nope"), time.Hour, now); failed != 1 {
		t.Fatalf("missing root failed=%d", failed)
	}
}

func TestStoreUsageAndStats(t *testing.T) {
	now := time.Now()
	root, _ := seedStore(t, now)

	storeBytes, mediaBytes, files := storeUsage(root)
	wantMedia := int64(len("old-content") + len("fresh-content"))
	wantStore := wantMedia + int64(len("db-content")+len("token-content"))
	if storeBytes != wantStore || mediaBytes != wantMedia || files != 2 {
		t.Fatalf("usage = %d/%d/%d, want %d/%d/2", storeBytes, mediaBytes, files, wantStore, wantMedia)
	}

	stats := newStoreStats(root)
	if s, _, _ := stats.snapshot(now); s != wantStore {
		t.Fatalf("snapshot = %d", s)
	}
	// Cached: a change on disk is not visible until the TTL expires or invalidate().
	if err := os.WriteFile(filepath.Join(root, "extra.db"), []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := stats.snapshot(now.Add(time.Minute)); s != wantStore {
		t.Fatalf("cached snapshot = %d, want %d", s, wantStore)
	}
	stats.invalidate()
	if s, _, _ := stats.snapshot(now.Add(time.Minute)); s != wantStore+4 {
		t.Fatalf("refreshed snapshot = %d, want %d", s, wantStore+4)
	}
	// Missing directory: zeros, no error surfaced.
	if s, m, f := storeUsage(filepath.Join(root, "missing")); s != 0 || m != 0 || f != 0 {
		t.Fatalf("missing dir usage = %d/%d/%d", s, m, f)
	}
}

func TestHealthReportsStoreSize(t *testing.T) {
	root, _ := seedStore(t, time.Now())
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.storeStats = newStoreStats(root)
	mux := b.newRESTMux(8080, "test-token-0123456789", nil)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/health", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer test-token-0123456789")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["media_files"] != float64(2) {
		t.Fatalf("media_files = %v", body["media_files"])
	}
	if body["media_bytes"].(float64) <= 0 || body["store_bytes"].(float64) <= body["media_bytes"].(float64) {
		t.Fatalf("store_bytes=%v media_bytes=%v", body["store_bytes"], body["media_bytes"])
	}
}

func TestHandleMessageSkipsAutoDownloadWhenDisabled(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")

	msg := buildImageMessage(phonePN, phonePN, false, "")
	msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
	msg.Message.ImageMessage.MediaKey = []byte("test-media-key")

	var calls atomic.Int32
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.MediaAutoDownload = false
	b.DownloadMedia = func(_ string, _ string) (bool, string, string, string, error) {
		calls.Add(1)
		return false, "", "", "", nil
	}

	b.handleMessage(msg)
	time.Sleep(50 * time.Millisecond) // give a stray goroutine the chance to run
	if calls.Load() != 0 {
		t.Fatalf("DownloadMedia called %d times with auto-download disabled", calls.Load())
	}

	// The message itself is still stored with its media metadata, so a later
	// /api/download can fetch it.
	msgs, err := b.Store.GetMessages(phonePN.String(), 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("stored messages = %d, err %v", len(msgs), err)
	}
	if msgs[0].MediaType != "image" {
		t.Fatalf("media_type = %q", msgs[0].MediaType)
	}
}

func TestRunMediaRetentionSweepsOnStart(t *testing.T) {
	now := time.Now()
	root, paths := seedStore(t, now)
	t.Setenv("WHATSAPP_STORE_DIR", root)

	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.storeStats = newStoreStats(root)
	done := make(chan struct{})
	go func() {
		b.runMediaRetention(30 * 24 * time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(paths["old"]); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial sweep did not remove the old file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retention goroutine did not stop")
	}
	if _, err := os.Stat(paths["fresh"]); err != nil {
		t.Fatalf("fresh file removed: %v", err)
	}
}
