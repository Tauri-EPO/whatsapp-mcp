package main

import (
	"go.mau.fi/whatsmeow"
	"testing"
	"time"
)

func TestOperatorCompletionRefusesCanceledAttemptBeforeHandoff(t *testing.T) {
	f := newOperatorPairingFixture(t)
	f.client.items <- whatsmeow.QRChannelItem{Event: "code", Code: "FAKE-QR", Timeout: time.Minute}
	f.stateHTTP(t, "awaiting_qr")
	// Keep the actor before its post-cancel handoff; admission must reject now.
	f.p.action.Lock()
	defer f.p.action.Unlock()
	f.p.mu.Lock()
	f.p.cancelAttempt()
	f.p.mu.Unlock()
	if f.p.beginCompletion(f.client) {
		t.Fatal("SDK device save admitted after the attempt was canceled")
	}
}
