package main

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestReconnectLoopStopsDuringBackoff(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	var dials atomic.Int32
	b.Connect = func() error { dials.Add(1); return errors.New("still down") }

	reconnect := make(chan bool, 1)
	reconnect <- true // first attempt: sleeps 5s of backoff before dialling
	done := make(chan struct{})
	go func() {
		b.reconnectLoop(reconnect)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let the loop enter the backoff wait
	start := time.Now()
	b.Shutdown(time.Second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnectLoop did not return after Shutdown")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("shutdown waited out the backoff (%v)", time.Since(start))
	}
	if dials.Load() != 0 {
		t.Fatalf("dialled %d times during shutdown, want 0", dials.Load())
	}
}

func TestStreamReplacedTimerIsCancelledByShutdown(t *testing.T) {
	prev := streamReplacedDelay
	streamReplacedDelay = 200 * time.Millisecond
	t.Cleanup(func() { streamReplacedDelay = prev })

	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	reconnect := make(chan bool, 1)
	b.handleEvent(&events.StreamReplaced{}, reconnect)
	b.Shutdown(time.Second)

	select {
	case <-reconnect:
		t.Fatal("StreamReplaced timer signalled a reconnect after shutdown")
	case <-time.After(400 * time.Millisecond):
	}
}

func TestStreamReplacedSignalsReconnectWhenRunning(t *testing.T) {
	prev := streamReplacedDelay
	streamReplacedDelay = 20 * time.Millisecond
	t.Cleanup(func() { streamReplacedDelay = prev })

	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	reconnect := make(chan bool, 1)
	b.handleEvent(&events.StreamReplaced{}, reconnect)
	select {
	case <-reconnect:
	case <-time.After(time.Second):
		t.Fatal("expected a reconnect signal after the delay")
	}
}

func TestShutdownDrainsRESTServer(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.RESTBind = "127.0.0.1"
	b.startRESTServer(0, "test-token-0123456789") // port 0: any free port
	if b.httpServer == nil {
		t.Fatal("startRESTServer must keep the server for Shutdown")
	}
	// Give ListenAndServe a moment; Shutdown on a not-yet-listening server is still fine.
	time.Sleep(50 * time.Millisecond)
	b.Shutdown(time.Second)
	if err := b.httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("server should be closed after Shutdown, got %v", err)
	}
}

func TestShutdownWaitsForHistoryVotesWithDeadline(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.historyVotes.Add(1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		b.historyVotes.Done()
	}()
	start := time.Now()
	b.Shutdown(2 * time.Second)
	if d := time.Since(start); d < 90*time.Millisecond || d > time.Second {
		t.Fatalf("Shutdown should wait for in-flight votes (took %v)", d)
	}
}
