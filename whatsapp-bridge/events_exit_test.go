package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types/events"
)

// exitRecorder captures Bridge.Exit calls instead of terminating the test binary.
type exitRecorder struct {
	calls  int
	reason string
	code   int
}

func (r *exitRecorder) fn(reason string, code int) {
	r.calls++
	r.reason = reason
	r.code = code
}

func TestHandleEvent_LoggedOutExitsForRepair(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	rec := &exitRecorder{}
	b.Exit = rec.fn

	b.handleEvent(&events.LoggedOut{OnConnect: false, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))

	if rec.calls != 1 || rec.code != exitCodeLoggedOut {
		t.Fatalf("Exit calls=%d code=%d, want 1/%d", rec.calls, rec.code, exitCodeLoggedOut)
	}
	if rec.reason == "" {
		t.Fatal("exit reason must explain what happened")
	}
}

func TestHandleEvent_ClientOutdatedExits(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	rec := &exitRecorder{}
	b.Exit = rec.fn

	b.handleEvent(&events.ClientOutdated{}, make(chan bool, 1))

	if rec.calls != 1 || rec.code != exitCodeClientOutdated {
		t.Fatalf("Exit calls=%d code=%d, want 1/%d", rec.calls, rec.code, exitCodeClientOutdated)
	}
}

func TestHandleEvent_DisconnectedDoesNotExit(t *testing.T) {
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	rec := &exitRecorder{}
	b.Exit = rec.fn
	reconnect := make(chan bool, 1)

	b.handleEvent(&events.Disconnected{}, reconnect)

	if rec.calls != 0 {
		t.Fatal("a plain disconnect must reconnect, not exit")
	}
	select {
	case <-reconnect:
	default:
		t.Fatal("Disconnected should signal the reconnect loop")
	}
}
