package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestAutomaticCallerJoiningManualTransferChecksSizeWithoutCancellingIt(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, testLogger())
	ts := time.Now().Truncate(time.Second)
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	if err := persistMessage(ms, "SHARED1", mediaTestChat, "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "SHARED1"), false, testLogger()); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(finish) }) })
	var calls atomic.Int32
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-finish:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return writeMediaDownload(ctx, b.StoreRoot, relPath, func(_ context.Context, f whatsmeow.File) error {
			_, err := f.Write(bytes.Repeat([]byte("x"), 64))
			return err
		})
	}
	type result struct {
		ok   bool
		path string
		err  error
	}
	manual, automatic := make(chan result, 1), make(chan result, 1)
	fetch := func(ctx context.Context, target chan result) {
		ok, _, _, path, err := b.downloadMedia(ctx, "SHARED1", mediaTestChat)
		target <- result{ok, path, err}
	}
	go fetch(t.Context(), manual)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual transfer did not start")
	}
	go fetch(withMediaLimit(t.Context(), 16), automatic)
	path := storePath(chatMediaRel(mediaTestChat), mediaFileName("document", ts, "SHARED1", doc.GetFileName()))
	deadline := time.Now().Add(5 * time.Second)
	for b.mediaTransfers.waiting(path) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("automatic caller did not join manual transfer")
		}
		time.Sleep(time.Millisecond)
	}
	release.Do(func() { close(finish) })
	var manualResult, autoResult result
	select {
	case manualResult = <-manual:
	case <-time.After(5 * time.Second):
		t.Fatal("manual transfer did not finish")
	}
	select {
	case autoResult = <-automatic:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic caller did not finish")
	}
	if !manualResult.ok || manualResult.err != nil || autoResult.ok || !errors.Is(autoResult.err, errAutoMediaLimit) || calls.Load() != 1 {
		t.Fatalf("manual=%+v automatic=%+v transfers=%d", manualResult, autoResult, calls.Load())
	}
	if ok, _, _, cached, err := b.downloadMedia(t.Context(), "SHARED1", mediaTestChat); !ok || err != nil || cached != manualResult.path || calls.Load() != 1 {
		t.Fatalf("manual cache deleted: ok=%v path=%s err=%v transfers=%d", ok, cached, err, calls.Load())
	}
}

func TestManualCallerJoiningAutomaticTransferRetriesWithoutItsCap(t *testing.T) {
	testManualCallerJoiningAutomaticTransfer(t, false, 0)
}

func TestManualCallerJoiningCappedAlternateCDNPath(t *testing.T) {
	for _, age := range []time.Duration{0, 2 * cdnFreshWindow} {
		t.Run(age.String(), func(t *testing.T) {
			testManualCallerJoiningAutomaticTransfer(t, true, age)
		})
	}
}

func testManualCallerJoiningAutomaticTransfer(t *testing.T, alternate bool, age time.Duration) {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, testLogger())
	ts := time.Now().Add(-age).Truncate(time.Second)
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	if alternate {
		doc.DirectPath = proto.String("/refused")
	}
	if err := persistMessage(ms, "SHARED2", mediaTestChat, "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "SHARED2"), false, testLogger()); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(finish) }) })
	var calls atomic.Int32
	var phoneRetries atomic.Int32
	b.mediaRetryDownload = func(context.Context, string, string, *MediaDownloader, *os.Root, string) (int64, error) {
		phoneRetries.Add(1)
		return 0, errors.New("cap failure must not request a phone retry")
	}
	b.mediaTransfer = func(ctx context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		if alternate && msg.GetDirectPath() == "/refused" {
			return 0, whatsmeow.ErrMediaDownloadFailedWith403
		}
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-finish:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			// A CDN retry can refresh metadata before the capped attempt fails.
			if err := ms.SetDirectPath("SHARED2", mediaTestChat, "/refreshed"); err != nil {
				t.Fatal(err)
			}
		} else if msg.GetDirectPath() != "/refreshed" {
			t.Fatalf("manual retry reused stale direct path %s", msg.GetDirectPath())
		}
		return writeMediaDownload(ctx, b.StoreRoot, relPath, func(_ context.Context, f whatsmeow.File) error {
			_, err := f.Write(bytes.Repeat([]byte("x"), 64))
			return err
		})
	}
	type result struct {
		ok  bool
		err error
	}
	automatic, manual := make(chan result, 1), make(chan result, 1)
	fetch := func(ctx context.Context, target chan result) {
		ok, _, _, _, err := b.downloadMedia(ctx, "SHARED2", mediaTestChat)
		target <- result{ok, err}
	}
	go fetch(withMediaLimit(t.Context(), 16), automatic)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic transfer did not start")
	}
	go fetch(t.Context(), manual)
	path := storePath(chatMediaRel(mediaTestChat), mediaFileName("document", ts, "SHARED2", doc.GetFileName()))
	deadline := time.Now().Add(5 * time.Second)
	for b.mediaTransfers.waiting(path) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("manual caller did not join automatic transfer")
		}
		time.Sleep(time.Millisecond)
	}
	release.Do(func() { close(finish) })
	var manualResult, autoResult result
	select {
	case autoResult = <-automatic:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic transfer did not finish")
	}
	select {
	case manualResult = <-manual:
	case <-time.After(5 * time.Second):
		t.Fatal("manual transfer did not retry")
	}
	if !manualResult.ok || manualResult.err != nil || autoResult.ok || !errors.Is(autoResult.err, errAutoMediaLimit) || calls.Load() != 2 {
		t.Fatalf("manual=%+v automatic=%+v transfers=%d", manualResult, autoResult, calls.Load())
	}
	if phoneRetries.Load() != 0 {
		t.Fatalf("cap failure asked the phone to retry %d times", phoneRetries.Load())
	}
	if ok, _, _, _, err := b.downloadMedia(t.Context(), "SHARED2", mediaTestChat); !ok || err != nil || calls.Load() != 2 {
		t.Fatalf("manual cache missing: ok=%v err=%v transfers=%d", ok, err, calls.Load())
	}
}
