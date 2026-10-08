package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func shareHistoryFixture(rows int) *events.HistorySync {
	fixture := largeHistoryFixture(rows)
	fixture.Data.SyncType = waHistorySync.HistorySync_RECENT.Enum()
	fixture.Data.Conversations[0].ID = proto.String("120363000000000001@g.us")
	return fixture
}

func compressShare(t *testing.T, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Only paired media-host discovery is replaced. The fixture and production
// both use the SDK's encrypted hash, MAC, CBC and plaintext hash validation.
func encryptedShareServer(t *testing.T, b *Bridge, compressed []byte, tamper string) (*waE2E.MessageHistoryBundle, *atomic.Int32) {
	t.Helper()
	key := bytes.Repeat([]byte{2}, 32)
	expanded := hkdfutil.SHA256(key, nil, []byte(whatsmeow.MediaHistory), 112)
	cipher, err := cbcutil.Encrypt(expanded[16:48], expanded[:16], compressed)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, expanded[48:80])
	_, _ = mac.Write(expanded[:16])
	_, _ = mac.Write(cipher)
	wire := append(cipher, mac.Sum(nil)[:10]...)
	plainHash, wireHash := sha256.Sum256(compressed), sha256.Sum256(wire)
	if tamper == "wire hash" {
		wire[0] ^= 1
	} else if tamper == "MAC" {
		wire[len(wire)-1] ^= 1
		wireHash = sha256.Sum256(wire)
	} else if tamper == "plain hash" {
		plainHash[0] ^= 1
	}
	requests := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.(http.Flusher).Flush() // no Content-Length: the actual byte bound must work
		_, _ = w.Write(wire)
	}))
	t.Cleanup(srv.Close)
	b.Client.SetMediaHTTPClient(srv.Client())
	b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
		if whatsmeow.GetMediaType(notif) != whatsmeow.MediaHistory {
			t.Fatal("bundle adapter selected the wrong HKDF media type")
		}
		//nolint:staticcheck // test-only SDK entrypoint bypasses paired host discovery.
		return b.Client.DangerousInternals().DownloadAndDecryptToFile(ctx, srv.URL, notif.MediaKey, whatsmeow.MediaHistory, notif.FileEncSHA256, notif.FileSHA256, file)
	}
	return &waE2E.MessageHistoryBundle{DirectPath: proto.String("/private-history-path"), MediaKey: key, FileSHA256: plainHash[:], FileEncSHA256: wireHash[:]}, requests
}

func TestHistoryShareEncryptedHTTPToCanonicalSQLite(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live", false: "history replay"}[live], func(t *testing.T) {
			ms, lock := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			fixture := shareHistoryFixture(501)
			chat := "120363000000000001@g.us"
			fixture.Data.Conversations[0].ID = &chat
			// Group attribution, mentions and poll auxiliary persistence share
			// exactly the same canonical importer as ordinary history chunks.
			fixture.Data.Conversations[0].Messages[0].Message.Participant = proto.String(phonePN.String())
			fixture.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("history searchable"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{phonePN.String()}}}}
			fixture.Data.Conversations[0].Messages[1].Message.Message = &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("history searchable"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Alice")}, {OptionName: proto.String("Bob")}}, SelectableOptionsCount: proto.Uint32(1)}}
			fixture.Data.Conversations[0].Messages[2].Message.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("history searchable"), FileName: proto.String("example.pdf"), Mimetype: proto.String("application/pdf"), DirectPath: proto.String("/document-path"), MediaKey: bytes.Repeat([]byte{5}, 32), FileSHA256: bytes.Repeat([]byte{6}, 32), FileEncSHA256: bytes.Repeat([]byte{7}, 32), FileLength: proto.Uint64(42)}}
			plain, err := proto.Marshal(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
			// One real BUSY at the first chunk proves download/log/counter
			// do not replay when the canonical transaction retries.
			attempts, waits := 0, 0
			release := func() {}
			b.historyBatchWriter = func(fn func(*messageBatch) error) error {
				attempts++
				if attempts == 1 {
					release = lock()
				}
				return ms.Batch(fn)
			}
			b.storeRetryWait = func(time.Duration) bool { waits++; release(); return true }
			message := &waE2E.Message{MessageHistoryBundle: bundle}
			if live {
				msg := buildTextMessage(types.NewJID("120363000000000001", types.GroupServer), phonePN, types.EmptyJID, types.EmptyJID, false, "")
				msg.Message = message
				b.handleMessage(msg)
			} else {
				outer := shareHistoryFixture(1)
				outer.Data.Conversations[0].Messages[0].Message.Message = message
				b.handleHistorySync(outer)
			}
			var rows, indexed int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'").Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			var sender, server, mentions string
			if err := ms.db.QueryRow("SELECT sender,sender_server,mentions FROM messages WHERE chat_jid=? AND id='H0'", chat).Scan(&sender, &server, &mentions); err != nil {
				t.Fatal(err)
			}
			if rows != 501 || indexed != rows || b.metrics.historyMessages.Load() != 501 || b.metrics.storeFailures.Load() != 0 || requests.Load() != 1 || waits != 1 || b.metrics.groupHistoryShares.Load() != 1 {
				t.Fatalf("rows=%d FTS=%d history=%d losses=%d HTTP=%d waits=%d shares=%d", rows, indexed, b.metrics.historyMessages.Load(), b.metrics.storeFailures.Load(), requests.Load(), waits, b.metrics.groupHistoryShares.Load())
			}
			if sender != phonePN.User || server != phonePN.Server || mentions != phonePN.User {
				t.Fatalf("canonical attribution/mentions lost: %s/%s/%s", sender, server, mentions)
			}
			var polls, media int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM polls WHERE chat_jid=? AND message_id='H1'", chat).Scan(&polls); err != nil {
				t.Fatal(err)
			}
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid=? AND id='H2' AND media_type='document' AND direct_path='/document-path' AND file_length=42", chat).Scan(&media); err != nil {
				t.Fatal(err)
			}
			if polls != 1 || media != 1 {
				t.Fatalf("canonical poll/media persistence lost: polls=%d media=%d", polls, media)
			}
			if strings.Count(rec.String(), "Group history bundle seen") != 1 || strings.Contains(rec.String(), bundle.GetDirectPath()) {
				t.Fatalf("recognition replayed or path logged: %s", rec.String())
			}
		})
	}
}

func TestHistoryShareRejectsTamperingAndBadPayloads(t *testing.T) {
	for _, failure := range []string{"wire hash", "MAC", "plain hash", "bad zlib", "bad proto", "truncated zlib", "inflate bound", "compressed bound", "wire bound"} {
		t.Run(failure, func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			plain, err := proto.Marshal(shareHistoryFixture(1).Data)
			if err != nil {
				t.Fatal(err)
			}
			compressed := compressShare(t, plain)
			compressedLimit, inflatedLimit := int64(4096), int64(4096)
			switch failure {
			case "bad zlib":
				compressed = []byte("bad zlib")
			case "bad proto":
				compressed = compressShare(t, []byte{0xff})
			case "truncated zlib":
				compressed = compressed[:len(compressed)-1]
			case "inflate bound":
				inflatedLimit = int64(len(plain) - 1)
			case "compressed bound":
				compressedLimit = int64(len(compressed) - 1)
			case "wire bound":
				compressedLimit = 1
			}
			bundle, requests := encryptedShareServer(t, b, compressed, failure)
			if _, err := b.decodeHistoryShare(t.Context(), bundle, compressedLimit, inflatedLimit); err == nil {
				t.Fatal("invalid bundle accepted")
			}
			var rows int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 0 || requests.Load() != 1 {
				t.Fatalf("refusal wrote rows=%d or retried HTTP=%d: %v", rows, requests.Load(), err)
			}
		})
	}
}

func TestHistoryShareInflateExactLimitAndCancellation(t *testing.T) {
	plain, err := proto.Marshal(shareHistoryFixture(1).Data)
	if err != nil {
		t.Fatal(err)
	}
	compressed := compressShare(t, plain)
	data, err := inflateHistoryShare(t.Context(), bytes.NewReader(compressed), int64(len(plain)))
	if err != nil || len(data.GetConversations()) != 1 {
		t.Fatalf("exact inflated limit rejected: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := inflateHistoryShare(ctx, bytes.NewReader(compressed), 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("inflate ignored cancellation: %v", err)
	}
}

func TestHistoryShareNoticeAndNestedBundleDoNotDownload(t *testing.T) {
	ms := newTestMessageStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	fixture := shareHistoryFixture(2)
	fixture.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{MessageHistoryNotice: &waE2E.MessageHistoryNotice{MessageHistoryMetadata: &waE2E.MessageHistoryMetadata{HistoryReceivers: []string{"15550001111@s.whatsapp.net"}}}}
	fixture.Data.Conversations[0].Messages[1].Message.Message = &waE2E.Message{MessageHistoryBundle: &waE2E.MessageHistoryBundle{DirectPath: proto.String("/nested-secret")}}
	plain, err := proto.Marshal(fixture.Data)
	if err != nil {
		t.Fatal(err)
	}
	bundle, requests := encryptedShareServer(t, b, compressShare(t, plain), "")
	b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
	if requests.Load() != 1 || b.metrics.groupHistoryShares.Load() != 3 || strings.Count(rec.String(), "Group history") != 3 {
		t.Fatalf("nested download/recognition: HTTP=%d shares=%d logs=%s", requests.Load(), b.metrics.groupHistoryShares.Load(), rec.String())
	}
	for _, private := range []string{"/nested-secret", bundle.GetDirectPath(), "15550001111"} {
		if strings.Contains(rec.String(), private) {
			t.Fatalf("private field logged: %s", private)
		}
	}
}

func TestHistoryShareHTTPDownloadCancelledAndTemporaryFileRemoved(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ms := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	t.Cleanup(srv.Close)
	b.Client.SetMediaHTTPClient(srv.Client())
	b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
		//nolint:staticcheck // test-only SDK entrypoint bypasses paired host discovery.
		return b.Client.DangerousInternals().DownloadAndDecryptToFile(ctx, srv.URL, notif.MediaKey, whatsmeow.MediaHistory, notif.FileEncSHA256, notif.FileSHA256, file)
	}
	bundle := &waE2E.MessageHistoryBundle{DirectPath: proto.String("/secret"), MediaKey: bytes.Repeat([]byte{2}, 32), FileSHA256: bytes.Repeat([]byte{3}, 32), FileEncSHA256: bytes.Repeat([]byte{4}, 32)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := b.decodeHistoryShare(ctx, bundle, 4096, 4096); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download ignored cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled HTTP remained active")
	}
	files, err := os.ReadDir(tmp)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary encrypted file survived: count=%d err=%v", len(files), err)
	}
	// Pre-cancellation must stop before invoking the download seam.
	b.historyShareDownload = func(context.Context, *waE2E.HistorySyncNotification, whatsmeow.File) error {
		t.Fatal("download after cancel")
		return io.EOF
	}
	if _, err := b.decodeHistoryShare(ctx, bundle, 4096, 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel ignored: %v", err)
	}
}

func TestHistoryShareIndependentImportsSerialiseAndShutdownStopsWaiter(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent shares both imported", true: "waiting share cancelled"}[shutdown], func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			plain, err := proto.Marshal(shareHistoryFixture(1).Data)
			if err != nil {
				t.Fatal(err)
			}
			compressed := compressShare(t, plain)
			bundle := &waE2E.MessageHistoryBundle{DirectPath: proto.String("/secret"), MediaKey: bytes.Repeat([]byte{2}, 32), FileSHA256: bytes.Repeat([]byte{3}, 32), FileEncSHA256: bytes.Repeat([]byte{4}, 32)}
			// Hold the sole import slot before starting both existing callers.
			b.historyShareInit.Do(func() { b.historyShareGate = make(chan struct{}, 1) })
			b.historyShareGate <- struct{}{}
			var calls, active atomic.Int32
			b.historyShareDownload = func(_ context.Context, _ *waE2E.HistorySyncNotification, file whatsmeow.File) error {
				if active.Add(1) != 1 {
					t.Error("independent imports overlapped")
				}
				defer active.Add(-1)
				calls.Add(1)
				_, err := file.Write(compressed)
				return err
			}
			done := make(chan struct{}, 2)
			for range 2 {
				go func() {
					b.handleHistoryShare(&waE2E.Message{MessageHistoryBundle: bundle}, "120363000000000001@g.us", "SHARE", false)
					done <- struct{}{}
				}()
			}
			if shutdown {
				b.cancel()
			} else {
				<-b.historyShareGate
			}
			for range 2 {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("share caller did not finish")
				}
			}
			want := int32(2)
			if shutdown {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("downloads=%d want=%d", calls.Load(), want)
			}
		})
	}
}

func TestHistoryShareRecognitionPrecedesChatFailure(t *testing.T) {
	ms := newTestMessageStore(t)
	if _, err := ms.db.Exec("CREATE TRIGGER fail_chat BEFORE INSERT ON chats BEGIN SELECT RAISE(ABORT,'chat denied'); END"); err != nil {
		t.Fatal(err)
	}
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	fixture := shareHistoryFixture(2)
	fixture.Data.Conversations[0].Messages[0].Message.Message = &waE2E.Message{MessageHistoryNotice: &waE2E.MessageHistoryNotice{}}
	b.handleHistorySync(fixture)
	if b.metrics.groupHistoryShares.Load() != 1 || b.metrics.storeFailures.Load() != 1 || strings.Count(rec.String(), "Group history notice seen") != 1 {
		t.Fatalf("recognition or ordinary row failure accounting lost: shares=%d failures=%d logs=%s", b.metrics.groupHistoryShares.Load(), b.metrics.storeFailures.Load(), rec.String())
	}
}

type shareFailingTransport struct{ requests atomic.Int32 }

func (r *shareFailingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	r.requests.Add(1)
	return nil, &net.DNSError{Err: "synthetic transport failure", Name: "private-transport-name"}
}

type shareRetryLogger struct {
	waLog.Logger
	cancel context.CancelFunc
}

func (l shareRetryLogger) Warnf(msg string, args ...any) {
	l.Logger.Warnf(msg, args...)
	if strings.HasPrefix(msg, "Failed to download media due to network error:") {
		l.cancel() // deterministic stop after the real SDK emitted its retry log
	}
}

func TestHistoryShareSDKTransportRetryLogIsPrivate(t *testing.T) {
	ms := newTestMessageStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Production installs this root-owned adapter once at client creation.
	b.Client.Log = sdkSafeLogger{Logger: shareRetryLogger{Logger: rec, cancel: cancel}}
	transport := &shareFailingTransport{}
	b.Client.SetMediaHTTPClient(&http.Client{Transport: transport})
	bundle := &waE2E.MessageHistoryBundle{DirectPath: proto.String("/private-history-path"), MediaKey: bytes.Repeat([]byte{2}, 32), FileSHA256: bytes.Repeat([]byte{3}, 32), FileEncSHA256: bytes.Repeat([]byte{4}, 32)}
	const privateURL = "https://example.invalid/private-history-path?hash=private-hash"
	b.historyShareDownload = func(ctx context.Context, notif *waE2E.HistorySyncNotification, file whatsmeow.File) error {
		//nolint:staticcheck // test-only SDK entrypoint bypasses paired host discovery.
		return b.Client.DangerousInternals().DownloadAndDecryptToFile(ctx, privateURL, notif.MediaKey, whatsmeow.MediaHistory, notif.FileEncSHA256, notif.FileSHA256, file)
	}
	if _, err := b.decodeHistoryShare(ctx, bundle, 4096, 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry cancellation not observed: %v", err)
	}
	log := rec.String()
	if transport.requests.Load() != 1 || strings.Count(log, "Failed to download media due to network error:") != 1 {
		t.Fatalf("SDK transport retry was not exercised: HTTP=%d log=%s", transport.requests.Load(), log)
	}
	for _, private := range []string{privateURL, bundle.GetDirectPath(), "private-hash", "private-transport-name", "synthetic transport failure"} {
		if strings.Contains(log, private) {
			t.Fatalf("SDK error exposed transport detail %q: %s", private, log)
		}
	}
}
