package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// fakePairingClient scripts one QR channel per GetQRChannel call.
type fakePairingClient struct {
	scripts    [][]whatsmeow.QRChannelItem // per attempt; nil = channel stays open until ctx
	qrCalls    int
	connects   int
	disconnect int
	connectErr error
}

func (f *fakePairingClient) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	i := f.qrCalls
	f.qrCalls++
	ch := make(chan whatsmeow.QRChannelItem)
	var script []whatsmeow.QRChannelItem
	if i < len(f.scripts) {
		script = f.scripts[i]
	}
	go func() {
		for _, item := range script {
			select {
			case ch <- item:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done() // keep the channel open like whatsmeow until the context ends
		close(ch)
	}()
	return ch, nil
}

func (f *fakePairingClient) Connect() error { f.connects++; return f.connectErr }
func (f *fakePairingClient) Disconnect()    { f.disconnect++ }

func fastOpts(out *bytes.Buffer) pairingOptions {
	return pairingOptions{attempts: 3, attemptTimeout: 2 * time.Second, retryDelay: 10 * time.Millisecond, out: out, log: testLogger()}
}

func TestConnectOrPair_TimeoutEventStartsNextAttemptImmediately(t *testing.T) {
	out := &bytes.Buffer{}
	c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{
		{{Event: "code", Code: "AAA"}, {Event: "timeout"}},
		{{Event: "code", Code: "BBB"}, {Event: "success"}},
	}}
	start := time.Now()
	if err := connectOrPair(context.Background(), c, false, fastOpts(out)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout event should not wait for the attempt deadline (took %v)", elapsed)
	}
	if c.qrCalls != 2 || c.connects != 2 || c.disconnect != 1 {
		t.Fatalf("qrCalls=%d connects=%d disconnects=%d", c.qrCalls, c.connects, c.disconnect)
	}
	// Each attempt draws its first code with the "Scan this QR code" header;
	// the second attempt is a fresh sequence, so no "refreshed" header appears.
	if strings.Count(out.String(), "Scan this QR code") != 2 || strings.Contains(out.String(), "refreshed") {
		t.Fatalf("expected one first-code header per attempt, got:\n%s", out.String())
	}
}

func TestConnectOrPair_GivesUpAfterAttemptsWithRealError(t *testing.T) {
	c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{
		{{Event: "timeout"}}, {{Event: "timeout"}}, {{Event: "timeout"}},
	}}
	err := connectOrPair(context.Background(), c, false, fastOpts(&bytes.Buffer{}))
	if err == nil || !errors.Is(err, errPairingTimeout) {
		t.Fatalf("want wrapped errPairingTimeout, got %v", err)
	}
	if c.qrCalls != 3 {
		t.Fatalf("qrCalls=%d, want 3", c.qrCalls)
	}
}

func TestConnectOrPair_AttemptDeadlineFiresWhenChannelIsSilent(t *testing.T) {
	c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{nil, {{Event: "success"}}}}
	opt := fastOpts(&bytes.Buffer{})
	opt.attemptTimeout = 100 * time.Millisecond
	if err := connectOrPair(context.Background(), c, false, opt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.qrCalls != 2 {
		t.Fatalf("silent channel should hit the per-attempt deadline and retry, qrCalls=%d", c.qrCalls)
	}
}

func TestConnectOrPair_PairedClientJustConnects(t *testing.T) {
	c := &fakePairingClient{}
	if err := connectOrPair(context.Background(), c, true, fastOpts(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if c.qrCalls != 0 || c.connects != 1 {
		t.Fatalf("qrCalls=%d connects=%d", c.qrCalls, c.connects)
	}
}

func TestConnectOrPair_ConnectErrorIsReturnedAfterRetries(t *testing.T) {
	c := &fakePairingClient{connectErr: errors.New("dns down")}
	err := connectOrPair(context.Background(), c, true, fastOpts(&bytes.Buffer{}))
	if err == nil || !strings.Contains(err.Error(), "dns down") || c.connects != 3 {
		t.Fatalf("err=%v connects=%d", err, c.connects)
	}
}

func TestConnectOrPair_ParentContextCancelStops(t *testing.T) {
	c := &fakePairingClient{scripts: [][]whatsmeow.QRChannelItem{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	err := connectOrPair(ctx, c, false, fastOpts(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation should return promptly")
	}
}
