package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
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

// storeRootAt opens dir the way main() opens the store, and closes the handle
// when the test ends (before t.TempDir() removes the directory).
func storeRootAt(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// symlinkOrSkip links target to name, skipping the test where the platform or
// the account does not allow it (unprivileged Windows).
func symlinkOrSkip(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

func TestSweepMediaRemovesOnlyOldChatFiles(t *testing.T) {
	now := time.Now()
	root, paths := seedStore(t, now)
	sr := storeRootAt(t, root)

	removed, freed, failed := sweepMedia(sr, 30*24*time.Hour, now)
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
	if removed, _, _ := sweepMedia(sr, 30*24*time.Hour, now); removed != 0 {
		t.Fatalf("second sweep removed %d", removed)
	}
	// A store that could not be opened is reported as one failure, not a panic.
	if _, _, failed := sweepMedia(nil, time.Hour, now); failed != 1 {
		t.Fatalf("nil root failed=%d", failed)
	}
}

// A symlink under a chat directory must not make the sweep delete — or even
// reach — anything outside the store. The walk half of that (a symlink is not a
// regular file, and WalkDir never descends into one) held before this change
// too; the assertion that only passes with the os.Root is the last one, where a
// delete addressed through the symlinked directory is refused even though the
// same relative path resolves out of the store for a plain os.Remove.
func TestSweepMediaRefusesSymlinkEscape(t *testing.T) {
	now := time.Now()
	root, paths := seedStore(t, now)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jpg")
	if err := os.WriteFile(secret, []byte("secret-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(secret, old, old); err != nil {
		t.Fatal(err)
	}
	chat := filepath.Dir(paths["old"])
	link := filepath.Join(chat, "linked.jpg")
	symlinkOrSkip(t, secret, link)
	symlinkOrSkip(t, outside, filepath.Join(chat, "elsewhere"))

	sr := storeRootAt(t, root)
	removed, freed, _ := sweepMedia(sr, 30*24*time.Hour, now)
	if removed != 1 || freed != int64(len("old-content")) {
		t.Fatalf("removed=%d freed=%d, want only the real old file", removed, freed)
	}
	// The control is the root, not the walk. The same store-relative path
	// resolves to the file outside the store for ordinary path resolution...
	escaping := filepath.Base(chat) + "/elsewhere/secret.jpg"
	if _, err := os.Stat(filepath.Join(root, escaping)); err != nil {
		t.Fatalf("the escaping path should resolve for a plain os.Stat: %v", err)
	}
	// ...and the root refuses it, where os.Remove would have deleted it.
	if err := sr.Remove(escaping); err == nil {
		t.Fatal("the store root deleted a file through a symlink out of the store")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the sweep followed a symlink out of the store: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink itself is not a media file and should survive: %v", err)
	}
}

func TestStoreUsageAndStats(t *testing.T) {
	now := time.Now()
	root, _ := seedStore(t, now)

	sr := storeRootAt(t, root)
	storeBytes, mediaBytes, files := storeUsage(sr)
	wantMedia := int64(len("old-content") + len("fresh-content"))
	wantStore := wantMedia + int64(len("db-content")+len("token-content"))
	if storeBytes != wantStore || mediaBytes != wantMedia || files != 2 {
		t.Fatalf("usage = %d/%d/%d, want %d/%d/2", storeBytes, mediaBytes, files, wantStore, wantMedia)
	}

	stats := newStoreStats(sr)
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
	// No store root (it could not be opened): zeros, no error surfaced.
	if s, m, f := storeUsage(nil); s != 0 || m != 0 || f != 0 {
		t.Fatalf("nil root usage = %d/%d/%d", s, m, f)
	}
}

func TestHealthReportsStoreSize(t *testing.T) {
	root, _ := seedStore(t, time.Now())
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.storeStats = newStoreStats(storeRootAt(t, root))
	mux := b.newRESTMux(8080, "test-token-0123456789")

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
	msg.Message.ImageMessage.FileSHA256 = []byte("test-sha256")
	msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")

	var calls atomic.Int32
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.MediaAutoDownload = false
	b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
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

func TestResolveStatusAutoDownload(t *testing.T) {
	cases := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "", want: false},
		{raw: "  ", want: false},
		{raw: "true", want: true},
		{raw: " ON ", want: true},
		{raw: "1", want: true},
		{raw: "false", want: false},
		{raw: "0", want: false},
		// main() stops on these instead of guessing which way the disk goes.
		{raw: "treu", wantErr: true},
		{raw: "2", wantErr: true},
	}
	for _, tc := range cases {
		got, err := resolveStatusAutoDownload(tc.raw)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), mediaAutoDownloadStatusEnv) {
				t.Errorf("resolveStatusAutoDownload(%q) = %v, %v; want an error naming %s", tc.raw, got, err, mediaAutoDownloadStatusEnv)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("resolveStatusAutoDownload(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
}

// Status media is stored as a row and left on the CDN unless
// WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS asks for it; every other chat caches as
// before, and WHATSAPP_MEDIA_AUTODOWNLOAD=false still turns everything off
// (issue #447).
func TestHandleMessageStatusMediaIsOptIn(t *testing.T) {
	cases := []struct {
		name         string
		chat         types.JID
		autoDownload bool
		status       bool
		wantQueued   int
	}{
		{name: "status, default", chat: types.StatusBroadcastJID, autoDownload: true, wantQueued: 0},
		{name: "status, flag on", chat: types.StatusBroadcastJID, autoDownload: true, status: true, wantQueued: 1},
		{name: "status, flag on, auto-download off", chat: types.StatusBroadcastJID, status: true, wantQueued: 0},
		{name: "ordinary chat, default", chat: phonePN, autoDownload: true, wantQueued: 1},
		{name: "ordinary chat, flag on", chat: phonePN, autoDownload: true, status: true, wantQueued: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEBHOOK_ENABLED", "false")
			msg := buildImageMessage(tc.chat, phonePN, false, "")
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
			msg.Message.ImageMessage.FileSHA256 = []byte("test-sha256")
			msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")

			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			b.MediaAutoDownload, b.MediaAutoDownloadStatus = tc.autoDownload, tc.status
			// No workers: what handleMessage queued stays in the backlog, so
			// the count is exact without waiting for a goroutine.
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)

			b.handleMessage(msg)
			if got := b.autoDownloads.queued(); got != tc.wantQueued {
				t.Fatalf("queued downloads = %d, want %d", got, tc.wantQueued)
			}
			// The row keeps its media metadata either way: /api/download
			// fetches the file on demand.
			msgs, err := b.Store.GetMessages(tc.chat.String(), 10)
			if err != nil || len(msgs) != 1 || msgs[0].MediaType != "image" {
				t.Fatalf("stored messages = %+v, err %v", msgs, err)
			}
		})
	}
}

// The synchronous download that feeds the webhook payload is the other door:
// a status image is forwarded without its bytes unless the flag is on, and an
// ordinary chat keeps its payload download.
func TestHandleMessageStatusImageSkipsWebhookDownload(t *testing.T) {
	cases := []struct {
		name      string
		chat      types.JID
		status    bool
		noAuto    bool
		wantCalls int32
	}{
		{name: "status, default", chat: types.StatusBroadcastJID, wantCalls: 0},
		{name: "status, flag on", chat: types.StatusBroadcastJID, status: true, wantCalls: 1},
		// WHATSAPP_MEDIA_AUTODOWNLOAD=false wins: the flag does not reopen
		// the webhook door for the status feed.
		{name: "status, flag on, auto-download off", chat: types.StatusBroadcastJID, status: true, noAuto: true, wantCalls: 0},
		{name: "ordinary chat, default", chat: phonePN, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, webhookCh := captureWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			msg := buildImageMessage(tc.chat, phonePN, false, "")
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
			msg.Message.ImageMessage.FileSHA256 = []byte("test-sha256")
			msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")

			var calls atomic.Int32
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			b.MediaAutoDownload, b.MediaAutoDownloadStatus = !tc.noAuto, tc.status
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)
			b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
				calls.Add(1)
				return false, "", "", "", nil
			}

			b.handleMessage(msg)
			// The webhook download runs inside handleMessage, and its failure
			// falls back to the queue: a skipped status image reaches neither.
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("synchronous downloads = %d, want %d", got, tc.wantCalls)
			}
			if got := b.autoDownloads.queued(); got != int(tc.wantCalls) {
				t.Fatalf("queued downloads = %d, want %d", got, tc.wantCalls)
			}
			select {
			case payload := <-webhookCh:
				if payload.MediaType != "image" || payload.MediaBase64 != "" {
					t.Fatalf("webhook payload = %+v, want the image message without bytes", payload)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the message must still reach the webhook")
			}
		})
	}
}

// End to end with the default configuration: the status image arrives, the
// row is stored, nothing is written under store/status@broadcast/, and the
// on-demand path (/api/download calls downloadMedia) still fetches the file.
func TestStatusMediaIsFetchedOnDemandOnly(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	t.Setenv(storeDirEnv, t.TempDir())
	url, key, sha, enc, length := fullMediaInfo()
	msg := buildImageMessage(types.StatusBroadcastJID, phonePN, false, "")
	msg.Message.ImageMessage.URL = proto.String(url)
	msg.Message.ImageMessage.MediaKey = key
	msg.Message.ImageMessage.FileSHA256 = sha
	msg.Message.ImageMessage.FileEncSHA256 = enc
	msg.Message.ImageMessage.FileLength = proto.Uint64(length)

	var transfers atomic.Int32
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, localPath string) (int64, error) {
		transfers.Add(1)
		return writeLikeDownloadToPath(localPath, []byte("status image"))
	}

	// No workers: a download queued by mistake stays countable in the backlog.
	b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)

	b.handleMessage(msg)
	if got := b.autoDownloads.queued(); got != 0 {
		t.Fatalf("queued downloads = %d, want 0", got)
	}
	statusDir := chatMediaDir(types.StatusBroadcastJID.String())
	if entries, err := os.ReadDir(statusDir); err == nil && len(entries) > 0 {
		t.Fatalf("%d files cached under %s with the default configuration", len(entries), statusDir)
	}
	if got := transfers.Load(); got != 0 {
		t.Fatalf("transfers on arrival = %d, want 0", got)
	}

	ok, mediaType, _, path, err := b.downloadMedia(context.Background(), msg.Info.ID, types.StatusBroadcastJID.String())
	if !ok || err != nil || mediaType != "image" {
		t.Fatalf("on-demand download: ok=%v type=%q err=%v", ok, mediaType, err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "status image" { //nolint:gosec // path returned by downloadMedia under t.TempDir()
		t.Fatalf("downloaded file = %q, err %v", data, readErr)
	}
	if got := transfers.Load(); got != 1 {
		t.Fatalf("transfers after the on-demand download = %d, want 1", got)
	}
}

func TestRunMediaRetentionSweepsOnStart(t *testing.T) {
	now := time.Now()
	root, paths := seedStore(t, now)
	t.Setenv("WHATSAPP_STORE_DIR", root)

	// testBridge opens WHATSAPP_STORE_DIR as the store root, which is what the
	// sweep deletes through.
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.storeStats = newStoreStats(b.StoreRoot)
	done := make(chan struct{})
	go func() {
		b.MediaRetention = 30 * 24 * time.Hour
		b.runMediaRetention()
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
