package main

// One media transfer per destination file.
//
// downloadMedia checks the cache and then streams into "<file>.part" before an
// atomic rename. Nothing serialised a cache miss: /api/download, the
// auto-download goroutine and an MCP transcription fetch can ask for the same
// (message id, chat) at the same moment, open that single temp file with
// O_TRUNC and truncate each other's bytes, and the loser's cleanup Remove can
// delete the winner's file. mediaTransferGroup runs one transfer per
// destination and hands its result to every caller parked on it.
//
// The key is the absolute destination path, so the same message id in two
// chats stays independent and unrelated files still download concurrently.
//
// The transfer runs on its own goroutine under transferContext, detached from
// the caller that started it: an HTTP client that hangs up must not abort a
// download the other callers (and the cache) still want, and no caller waits
// longer than its own context allows.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"go.mau.fi/whatsmeow"
)

// mediaTransferFunc streams one media file into localPath and reports the
// bytes written: downloadToPath in production, a fake in tests.
type mediaTransferFunc func(ctx context.Context, msg whatsmeow.DownloadableMessage, localPath string) (int64, error)

// errTransferAbandoned is what callers see when the transfer never published a
// result (it panicked): a failed download beats a bogus success.
var errTransferAbandoned = errors.New("media transfer did not complete")

// mediaTransferGroup tracks the transfer in flight for each destination path.
// The zero value is ready to use; one lives on Bridge.
type mediaTransferGroup struct {
	mu       sync.Mutex
	inflight map[string]*mediaTransferCall
	// wg tracks the transfer goroutines so Shutdown can wait for them: a
	// transfer outlives the request that started it and must not still be
	// querying messages.db when main closes it.
	wg sync.WaitGroup
}

type mediaTransferCall struct {
	done    chan struct{}
	waiters atomic.Int64
	written int64
	err     error
}

// do starts fn for the first caller of key and shares its result with every
// caller that arrives while it runs. Each caller honours its own ctx:
// cancelling it returns ctx.Err() and leaves the transfer, and its temp file,
// to the ones still waiting. A transfer that fails fails for all of them.
func (g *mediaTransferGroup) do(ctx context.Context, key string, fn func() (int64, error)) (int64, error) {
	g.mu.Lock()
	call, running := g.inflight[key]
	if !running {
		call = &mediaTransferCall{done: make(chan struct{}), err: errTransferAbandoned}
		if g.inflight == nil {
			g.inflight = make(map[string]*mediaTransferCall)
		}
		g.inflight[key] = call
		g.wg.Add(1)
		go func() {
			// The result is published before done is closed, so every caller
			// reading call.written/call.err after the receive sees what fn
			// returned.
			defer func() {
				g.mu.Lock()
				delete(g.inflight, key)
				g.mu.Unlock()
				close(call.done)
				g.wg.Done()
			}()
			// The transfer runs on its own goroutine now, so a panic here
			// would take the process down instead of failing one request:
			// turn it into a download error like any other.
			defer func() {
				if r := recover(); r != nil {
					call.err = fmt.Errorf("%w: %v", errTransferAbandoned, r)
				}
			}()
			call.written, call.err = fn()
		}()
	}
	call.waiters.Add(1)
	g.mu.Unlock()
	defer call.waiters.Add(-1)

	// Prefer a finished transfer over a context that expired at the same
	// moment: the file is cached, reporting it as cancelled would be a lie.
	select {
	case <-call.done:
		return call.written, call.err
	default:
	}
	select {
	case <-call.done:
		return call.written, call.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// waiting reports how many callers are parked on the transfer for key, and 0
// when none is in flight.
func (g *mediaTransferGroup) waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	call, ok := g.inflight[key]
	if !ok {
		return 0
	}
	return int(call.waiters.Load())
}

// wait blocks until every transfer in flight has finished. Shutdown calls it
// after cancelling the lifecycle context, so nothing is still reading
// messages.db when main closes the store.
func (g *mediaTransferGroup) wait() {
	g.wg.Wait()
}

// transferContext is the context a transfer runs under: the bridge lifecycle,
// so Shutdown cancels it, plus the deadline of the caller that started it, but
// never that caller's cancellation.
//
// The deadline is the starter's alone. A caller that joins with a longer
// horizon inherits that bound, which keeps a stalled transfer from pinning its
// destination forever; once it expires the key is released and the next call
// starts a fresh transfer.
func transferContext(lifecycle, starter context.Context) (context.Context, context.CancelFunc) {
	base := lifecycle
	if base == nil {
		base = context.Background()
	}
	if deadline, ok := starter.Deadline(); ok {
		return context.WithDeadline(base, deadline)
	}
	return context.WithCancel(base)
}
