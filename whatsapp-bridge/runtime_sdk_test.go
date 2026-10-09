package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeReplacementSDKTransportRetryLogIsPrivate(t *testing.T) {
	rec := installRecordingLogger(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := newTestClient(&mockLIDStore{})
	b := testBridge(t, first, newTestMessageStore(t), rec)
	reconnect := make(chan bool, 1)
	b.installClient(first, false, reconnect)
	second := newRuntimeClient(newTestClient(&mockLIDStore{}).Store, privatePairingLogger{shareRetryLogger{Logger: rec, cancel: cancel}})
	b.installClient(second, false, reconnect)
	second.Log.Sub("Recv").Debugf("FAKE-SECRET-FRAME")
	transport := &shareFailingTransport{}
	second.SetMediaHTTPClient(&http.Client{Transport: transport})
	bundle := &waE2E.MessageHistoryBundle{DirectPath: proto.String("/private-history-path"), MediaKey: bytes.Repeat([]byte{2}, 32), FileSHA256: bytes.Repeat([]byte{3}, 32), FileEncSHA256: bytes.Repeat([]byte{4}, 32)}
	const privateURL = "https://example.invalid/private-history-path?hash=private-hash"
	b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
		//nolint:staticcheck // Test-only paired media-host discovery seam; real SDK retry/logger path.
		return b.currentClient().DangerousInternals().DownloadAndDecryptToFile(ctx, privateURL, notif.MediaKey, whatsmeow.MediaHistory, notif.FileEncSHA256, notif.FileSHA256, file)
	}
	if _, err := b.decodeHistoryShare(ctx, bundle, 4096, 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement SDK retry cancellation ignored: %v", err)
	}
	log := rec.String()
	if transport.requests.Load() != 1 || strings.Count(log, "Failed to download media due to network error:") != 1 {
		t.Fatalf("real replacement retry was not exercised: HTTP=%d", transport.requests.Load())
	}
	for _, private := range []string{privateURL, bundle.GetDirectPath(), "private-hash", "private-transport-name", "synthetic transport failure", "FAKE-SECRET-FRAME"} {
		if strings.Contains(log, private) {
			t.Fatal("replacement SDK retry exposed a private transport value")
		}
	}
}
