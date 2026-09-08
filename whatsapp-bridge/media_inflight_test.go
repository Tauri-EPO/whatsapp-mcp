package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// newConcurrentTestStore is newTestMessageStore pinned to a single connection.
// The in-memory DSN gives every extra pool connection a database of its own, so
// two goroutines reading at the same time would otherwise hit an empty schema.
func newConcurrentTestStore(t *testing.T) *MessageStore {
	t.Helper()
	ms := newTestMessageStore(t)
	ms.db.SetMaxOpenConns(1)
	return ms
}

// seedMediaRowIn stores one downloadable media row in the given chat and
// returns the destination path downloadMedia will transfer into.
func seedMediaRowIn(t *testing.T, ms *MessageStore, chat, id string) string {
	t.Helper()
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.Local)
	url, key, sha, enc, length := fullMediaInfo()
	if err := ms.StoreChat(chat, "Test", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, chat, "5511999999999", "caption", ts, false, "image", "", url, key, sha, enc, length, ""); err != nil {
		t.Fatal(err)
	}
	dest, err := filepath.Abs(filepath.Join(chatMediaDir(chat), mediaFileName("image", ts, id, "")))
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

// writeLikeDownloadToPath finishes a fake transfer the way the real one does:
// through the shared "<file>.part" temp file and an atomic rename. Two
// transfers running at once for the same destination would truncate it.
func writeLikeDownloadToPath(localPath string, payload []byte) (int64, error) {
	tmp := localPath + ".part"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, localPath); err != nil {
		return 0, err
	}
	return int64(len(payload)), nil
}

// awaitCallers blocks until want callers are parked on the transfer for key.
// It fails the test instead of racing on a sleep.
func awaitCallers(t *testing.T, g *mediaTransferGroup, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g.waiting(key) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d caller(s) on %s (have %d)", want, key, g.waiting(key))
}

type downloadResult struct {
	ok   bool
	path string
	err  error
}

// startDownload runs downloadMedia in the background and reports its result.
func startDownload(ctx context.Context, b *Bridge, id, chat string) <-chan downloadResult {
	out := make(chan downloadResult, 1)
	go func() {
		ok, _, _, path, err := b.downloadMedia(ctx, id, chat)
		out <- downloadResult{ok, path, err}
	}()
	return out
}

// blockingTransfer is a fake transfer that reports when it starts, waits for
// release and then writes payload through the real ".part" dance.
func blockingTransfer(started chan<- struct{}, release <-chan struct{}, payload []byte, runs *atomic.Int32) mediaTransferFunc {
	return func(_ context.Context, _ whatsmeow.DownloadableMessage, localPath string) (int64, error) {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return writeLikeDownloadToPath(localPath, payload)
	}
}

// Two callers miss the cache for the same message: exactly one transfer runs,
// both get the finished file and its bytes are the transferred ones.
func TestDownloadMediaSharesOneTransferPerDestination(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	dest := seedMediaRowIn(t, ms, mediaTestChat, "IMG1")

	payload := []byte("decrypted media bytes")
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var transfers atomic.Int32
	b.mediaTransfer = blockingTransfer(started, release, payload, &transfers)

	first := startDownload(context.Background(), b, "IMG1", mediaTestChat)
	<-started // the transfer is running
	second := startDownload(context.Background(), b, "IMG1", mediaTestChat)
	awaitCallers(t, &b.mediaTransfers, dest, 2)
	close(release)

	for i, res := range []downloadResult{<-first, <-second} {
		if !res.ok || res.err != nil {
			t.Fatalf("caller %d: ok=%v err=%v", i, res.ok, res.err)
		}
		if res.path != dest {
			t.Fatalf("caller %d: path = %s, want %s", i, res.path, dest)
		}
	}
	if got := transfers.Load(); got != 1 {
		t.Fatalf("transfers = %d, want 1", got)
	}
	got, err := os.ReadFile(dest) //nolint:gosec // path built by the test under t.TempDir()
	if err != nil || string(got) != string(payload) {
		t.Fatalf("cached file = %q (err %v), want %q", got, err, payload)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("the temp file must not survive the transfer")
	}
}

// A caller that gives up (its request was cancelled) neither cancels the
// transfer nor touches its files, whether it started that transfer or joined it.
func TestDownloadMediaCancellationLeavesTheTransferAlone(t *testing.T) {
	for _, tc := range []struct{ name, cancelled string }{
		{"waiter gives up", "second"},
		{"starter gives up", "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newConcurrentTestStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			dest := seedMediaRowIn(t, ms, mediaTestChat, "IMG2")

			payload := []byte("transferred bytes")
			release := make(chan struct{})
			started := make(chan struct{}, 2)
			var transfers atomic.Int32
			b.mediaTransfer = blockingTransfer(started, release, payload, &transfers)

			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			first := startDownload(firstCtx, b, "IMG2", mediaTestChat)
			<-started
			secondCtx, cancelSecond := context.WithCancel(context.Background())
			defer cancelSecond()
			second := startDownload(secondCtx, b, "IMG2", mediaTestChat)
			awaitCallers(t, &b.mediaTransfers, dest, 2)

			gone, kept := second, first
			cancel := cancelSecond
			if tc.cancelled == "first" {
				gone, kept = first, second
				cancel = cancelFirst
			}
			cancel()
			// downloadMedia formats the reason into its error, so match the text.
			if res := <-gone; res.ok || res.err == nil || !strings.Contains(res.err.Error(), context.Canceled.Error()) {
				t.Fatalf("cancelled caller: ok=%v err=%v", res.ok, res.err)
			}
			close(release)
			if res := <-kept; !res.ok || res.err != nil {
				t.Fatalf("remaining caller: ok=%v err=%v", res.ok, res.err)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != string(payload) { //nolint:gosec // path built by the test under t.TempDir()
				t.Fatalf("cached file = %q (err %v), want %q", got, err, payload)
			}
			if got := transfers.Load(); got != 1 {
				t.Fatalf("transfers = %d, want 1", got)
			}
		})
	}
}

// The transfer's failure reaches every caller, leaves nothing behind, and does
// not poison the destination for a later attempt.
func TestDownloadMediaPropagatesTransferFailure(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	dest := seedMediaRowIn(t, ms, mediaTestChat, "IMG3")

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var transfers atomic.Int32
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, _ string) (int64, error) {
		transfers.Add(1)
		started <- struct{}{}
		<-release
		return 0, errors.New("cdn said no")
	}

	first := startDownload(context.Background(), b, "IMG3", mediaTestChat)
	<-started
	second := startDownload(context.Background(), b, "IMG3", mediaTestChat)
	awaitCallers(t, &b.mediaTransfers, dest, 2)
	close(release)

	for i, res := range []downloadResult{<-first, <-second} {
		if res.ok || res.err == nil {
			t.Fatalf("caller %d: ok=%v err=%v", i, res.ok, res.err)
		}
	}
	if got := transfers.Load(); got != 1 {
		t.Fatalf("transfers = %d, want 1", got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a failed transfer must not leave a cached file")
	}
	// The key is released, so a later call transfers again.
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, localPath string) (int64, error) {
		transfers.Add(1)
		return writeLikeDownloadToPath(localPath, []byte("ok"))
	}
	if ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG3", mediaTestChat); !ok || err != nil {
		t.Fatalf("retry after failure: ok=%v err=%v", ok, err)
	}
	if got := transfers.Load(); got != 2 {
		t.Fatalf("transfers = %d, want 2 after the retry", got)
	}
}

// The same message id forwarded into another chat is a different destination:
// both transfers run at the same time instead of serialising.
func TestDownloadMediaKeepsChatsIndependent(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	const other = "120363000000000000@g.us"
	seedMediaRowIn(t, ms, mediaTestChat, "IMG4")
	seedMediaRowIn(t, ms, other, "IMG4")

	both := make(chan struct{})
	var transfers atomic.Int32
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, localPath string) (int64, error) {
		// Neither transfer can finish until the other one has started: they
		// deadlock (and the test fails on timeout) if the group serialises them.
		if transfers.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			return 0, errors.New("the other chat never started its transfer")
		}
		return writeLikeDownloadToPath(localPath, []byte("data"))
	}

	dm := startDownload(context.Background(), b, "IMG4", mediaTestChat)
	group := startDownload(context.Background(), b, "IMG4", other)
	for i, res := range []downloadResult{<-dm, <-group} {
		if !res.ok || res.err != nil {
			t.Errorf("caller %d: ok=%v err=%v", i, res.ok, res.err)
		}
	}
	if got := transfers.Load(); got != 2 {
		t.Fatalf("transfers = %d, want 2", got)
	}
}

// A transfer that panics fails its callers instead of taking the bridge down.
func TestDownloadMediaSurvivesAPanickingTransfer(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	seedMediaRowIn(t, ms, mediaTestChat, "IMG5")
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, _ string) (int64, error) {
		panic("whatsmeow blew up mid-download")
	}
	ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG5", mediaTestChat)
	if ok || err == nil || !strings.Contains(err.Error(), "whatsmeow blew up") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// The key is released, so the next call is a normal transfer.
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, localPath string) (int64, error) {
		return writeLikeDownloadToPath(localPath, []byte("ok"))
	}
	if ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG5", mediaTestChat); !ok || err != nil {
		t.Fatalf("after the panic: ok=%v err=%v", ok, err)
	}
}

// Shutdown cancels the lifecycle context and waits for the transfer, so no
// download is still reading messages.db when main closes it.
func TestShutdownDrainsMediaTransfers(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	seedMediaRowIn(t, ms, mediaTestChat, "IMG6")

	started := make(chan struct{}, 1)
	var finished atomic.Bool
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ string) (int64, error) {
		started <- struct{}{}
		<-ctx.Done() // the lifecycle context, cancelled by Shutdown
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
		return 0, ctx.Err()
	}
	caller, cancel := context.WithCancel(context.Background())
	res := startDownload(caller, b, "IMG6", mediaTestChat)
	<-started
	cancel() // the caller walks away; the transfer keeps running
	<-res

	b.Shutdown(5 * time.Second)
	if !finished.Load() {
		t.Fatal("Shutdown returned while a media transfer was still running")
	}
}

// The transfer keeps the deadline of the caller that started it but not its
// cancellation, and the bridge lifecycle can still cancel it.
func TestTransferContext(t *testing.T) {
	lifecycle, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	starter, cancelStarter := context.WithCancel(context.Background())
	ctx, cancel := transferContext(lifecycle, starter)
	defer cancel()
	cancelStarter()
	select {
	case <-ctx.Done():
		t.Fatal("the starter's cancellation must not reach the transfer")
	default:
	}
	cancelLifecycle()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown must cancel the transfer")
	}

	// A caller deadline is inherited; a nil lifecycle (bridges built without
	// one in tests) still yields a usable context.
	deadline := time.Now().Add(time.Hour)
	withDeadline, cancelDeadline := context.WithDeadline(context.Background(), deadline)
	defer cancelDeadline()
	ctx, cancel = transferContext(nil, withDeadline) //nolint:staticcheck // SA1012: the nil lifecycle is exactly what this case asserts transferContext tolerates
	defer cancel()
	if got, has := ctx.Deadline(); !has || !got.Equal(deadline) {
		t.Fatalf("deadline = %v (has %v), want %v", got, has, deadline)
	}
}
