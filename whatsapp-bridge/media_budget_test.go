package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// awaitCount blocks until got() reaches want, or fails with what it saw.
func awaitCount(t *testing.T, what string, want int32, got func() int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s = %d (have %d)", what, want, got())
}

// A burst never runs more than the worker count at a time, never queues more
// than the backlog, and drops the rest instead of growing.
func TestMediaJobQueueBoundsActiveAndPendingWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var active, peak, ran atomic.Int32
	q := newMediaJobQueue(ctx, 2, 2, func(_ context.Context, _ mediaJob) {
		now := active.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		<-release
		active.Add(-1)
		ran.Add(1)
	})

	submit := func(i int) bool {
		return q.submit(mediaJob{messageID: string(rune('a' + i)), chatJID: mediaTestChat, mediaType: "image"})
	}
	// Two workers, two queue slots. Occupy the workers first, so the counts
	// below do not depend on how fast they dequeue.
	for i := range 2 {
		if !submit(i) {
			t.Fatalf("job %d was refused by an empty queue", i)
		}
	}
	awaitCount(t, "active workers", 2, active.Load)
	// Then fill the backlog...
	for i := 2; i < 4; i++ {
		if !submit(i) {
			t.Fatalf("job %d was refused while the backlog had room", i)
		}
	}
	if got := q.queued(); got != 2 {
		t.Fatalf("queued = %d, want the 2 waiting jobs", got)
	}
	// ...and everything beyond it is dropped instead of piling up.
	for i := 4; i < 6; i++ {
		if submit(i) {
			t.Fatalf("job %d was accepted past the queue's bounds", i)
		}
	}
	if got := q.dropped(); got != 2 {
		t.Fatalf("dropped counter = %d, want 2", got)
	}

	if got := q.running(); got != 2 {
		t.Fatalf("running = %d, want the 2 busy workers", got)
	}

	close(release)
	awaitCount(t, "finished jobs", 4, ran.Load)
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak concurrency = %d, want at most the 2 workers", got)
	}
	cancel()
	q.wait()
}

// Cancelling the queue's context stops the workers; wait() returns once the
// job in hand is done, so nothing is still downloading when main closes the
// store.
func TestMediaJobQueueStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	var finished atomic.Bool
	q := newMediaJobQueue(ctx, 2, 2, func(jobCtx context.Context, _ mediaJob) {
		started <- struct{}{}
		<-jobCtx.Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
	})
	if !q.submit(mediaJob{messageID: "a", chatJID: mediaTestChat}) {
		t.Fatal("submit refused the first job")
	}
	<-started
	cancel()
	q.wait()
	if !finished.Load() {
		t.Fatal("wait() returned while a job was still running")
	}
}

// A cancelled queue drops what is still waiting instead of running every job
// under a dead context and filling the shutdown log with failures.
func TestMediaJobQueueDropsTheBacklogOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var ran atomic.Int32
	q := newMediaJobQueue(ctx, 1, 2, func(context.Context, mediaJob) {
		ran.Add(1)
		started <- struct{}{}
		<-release
	})
	if !q.submit(mediaJob{messageID: "a", chatJID: mediaTestChat}) {
		t.Fatal("job 0 was refused by an empty queue")
	}
	<-started // the worker holds the first job before the backlog is filled
	for i := 1; i < 3; i++ {
		if !q.submit(mediaJob{messageID: string(rune('a' + i)), chatJID: mediaTestChat}) {
			t.Fatalf("job %d was refused while the backlog had room", i)
		}
	}
	cancel()
	close(release)
	q.wait()
	if got := ran.Load(); got != 1 {
		t.Fatalf("jobs run = %d, want only the one already in hand", got)
	}
}

// Each cached file is bounded: whatsmeow's media client has no timeout, so a
// stalled transfer would otherwise hold a worker forever.
func TestRunAutoDownloadBoundsEachFile(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	b := testBridge(nil, newTestMessageStore(t), installRecordingLogger(t))
	var deadline time.Time
	var hasDeadline bool
	b.DownloadMedia = func(ctx context.Context, _ string, _ string) (bool, string, string, string, error) {
		deadline, hasDeadline = ctx.Deadline()
		return true, "image", "image.jpg", "/tmp/image.jpg", nil
	}
	b.runAutoDownload(context.Background(), mediaJob{messageID: "A", chatJID: mediaTestChat, mediaType: "image"})
	if !hasDeadline {
		t.Fatal("the download ran without a deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > autoDownloadTimeout {
		t.Fatalf("deadline in %v, want at most %v", left, autoDownloadTimeout)
	}
}

// buildQueuedMediaMessage is an inbound video (never forwarded to a webhook,
// so it takes the auto-download branch) with the given message ID.
func buildQueuedMediaMessage(id string) *events.Message {
	msg := buildImageMessage(phonePN, phonePN, false, "")
	msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
	msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
	msg.Info.ID = id
	return msg
}

// A media burst on the event loop respects the budget: the workers cache what
// they can, the overflow is refused with a WARN and counted, and message
// storage is never blocked.
func TestHandleMessageDropsAutoDownloadWhenQueueIsFull(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	t.Setenv(storeDirEnv, t.TempDir())
	rec := installRecordingLogger(t)
	ms := newTestMessageStore(t)
	b := testBridge(newTestClient(&mockLIDStore{}), ms, rec)

	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var downloads atomic.Int32
	b.DownloadMedia = func(_ context.Context, _ string, _ string) (bool, string, string, string, error) {
		downloads.Add(1)
		started <- struct{}{}
		<-release
		return true, "image", "image.jpg", "/tmp/image.jpg", nil
	}
	// One worker, one queue slot: the third message onwards is dropped.
	b.autoDownloads = newMediaJobQueue(b.ctx, 1, 1, b.runAutoDownload)

	const burst = 5
	// The first message occupies the only worker before the rest arrive, so
	// the counts below do not depend on when that worker is scheduled.
	b.handleMessage(buildQueuedMediaMessage("A"))
	<-started
	for i := 1; i < burst; i++ {
		b.handleMessage(buildQueuedMediaMessage(string(rune('A' + i))))
	}
	// One job waits in the single slot; the other three were dropped.
	if got := b.autoDownloads.dropped(); got != burst-2 {
		t.Fatalf("dropped = %d, want %d", got, burst-2)
	}
	if !strings.Contains(rec.String(), "Auto-download queue full") {
		t.Fatalf("the drop must be visible in the log:\n%s", rec.String())
	}
	// Every message is still stored: the budget only limits caching.
	for i := range burst {
		stored, err := ms.GetMessageIsFromMe(string(rune('A'+i)), phonePN.String())
		if err != nil || stored == nil {
			t.Fatalf("message %d was not stored: %v", i, err)
		}
	}

	close(release)
	awaitCount(t, "auto-downloads", 2, downloads.Load)
	b.cancel()
	b.autoDownloads.wait()
}

// The queue is wired into the bridge's shutdown path.
func TestShutdownWaitsForAutoDownloadWorkers(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	b := testBridge(nil, newTestMessageStore(t), installRecordingLogger(t))
	started := make(chan struct{}, 1)
	var finished atomic.Bool
	b.autoDownloads = newMediaJobQueue(b.ctx, 1, 1, func(ctx context.Context, _ mediaJob) {
		started <- struct{}{}
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
	})
	b.queueAutoDownload("A", mediaTestChat, "image")
	<-started
	b.Shutdown(5 * time.Second)
	if !finished.Load() {
		t.Fatal("Shutdown returned while an auto-download was still running")
	}
}

// /metrics reports the backlog and the drops.
func TestMetricsReportAutoDownloadBudget(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	b := testBridge(nil, newTestMessageStore(t), installRecordingLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.autoDownloads = newMediaJobQueue(ctx, 0, 1, func(context.Context, mediaJob) {})
	b.queueAutoDownload("A", mediaTestChat, "image") // fills the single slot
	b.queueAutoDownload("B", mediaTestChat, "image") // dropped
	out := b.renderMetrics()
	for _, want := range []string{
		"whatsapp_bridge_media_autodownload_queued 1",
		"whatsapp_bridge_media_autodownload_running 0",
		"whatsapp_bridge_media_autodownload_drops_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
