package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// A media message whose url and directPath name different objects: the url is
// not the direct path behind a host, so a download that used one where the
// other was meant is told apart. Every secret-looking piece is a marker the
// log tests search for.
const (
	fixtureMediaURL   = "https://mmg.whatsapp.net/d/f/URLNAMEMARKER.enc?ccb=11-4&oh=URLTOKENMARKER&oe=URLEXPIRYMARKER"
	fixtureDirectPath = "/v/t62.7119-24/PATHNAMEMARKER_n.enc?ccb=11-4&oh=PATHTOKENMARKER&oe=PATHEXPIRYMARKER&_nc_sid=5e03e0"
)

func fixtureDocument() *waE2E.DocumentMessage {
	return &waE2E.DocumentMessage{
		URL:           proto.String(fixtureMediaURL),
		DirectPath:    proto.String(fixtureDirectPath),
		MediaKey:      []byte(strings.Repeat("k", 32)),
		FileSHA256:    []byte(strings.Repeat("s", 32)),
		FileEncSHA256: []byte(strings.Repeat("e", 32)),
		FileLength:    proto.Uint64(4321),
		FileName:      proto.String("form.docx"),
	}
}

// storedDirectPath reads messages.direct_path for one row.
func storedDirectPath(t *testing.T, ms *MessageStore, id, chatJID string) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := ms.db.QueryRow("SELECT direct_path FROM messages WHERE id = ? AND chat_jid = ?", id, chatJID).Scan(&got); err != nil {
		t.Fatalf("read direct_path of %s: %v", id, err)
	}
	return got
}

func TestExtractMediaDirectPath(t *testing.T) {
	cases := map[string]*waE2E.Message{
		"/p/image":    {ImageMessage: &waE2E.ImageMessage{DirectPath: proto.String("/p/image")}},
		"/p/video":    {VideoMessage: &waE2E.VideoMessage{DirectPath: proto.String("/p/video")}},
		"/p/audio":    {AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/p/audio")}},
		"/p/document": {DocumentMessage: &waE2E.DocumentMessage{DirectPath: proto.String("/p/document")}},
		"/p/sticker":  {StickerMessage: &waE2E.StickerMessage{DirectPath: proto.String("/p/sticker")}},
	}
	for want, msg := range cases {
		if got := extractMediaDirectPath(msg); got != want {
			t.Errorf("direct path = %q, want %q", got, want)
		}
		// extractMessage carries it next to the kind extractMediaInfo read.
		if ex := extractMessage(msg, time.Now(), "ID"); ex.directPath != want || ex.mediaType == "" {
			t.Errorf("extractMessage: path %q for a %q, want %q", ex.directPath, ex.mediaType, want)
		}
	}
	for name, msg := range map[string]*waE2E.Message{
		"nil":             nil,
		"text":            {Conversation: proto.String("hi")},
		"media, no path":  {ImageMessage: &waE2E.ImageMessage{URL: proto.String(fixtureMediaURL)}},
		"empty container": {},
	} {
		if got := extractMediaDirectPath(msg); got != "" {
			t.Errorf("%s: direct path = %q, want none", name, got)
		}
	}
	// A view-once envelope is unwrapped first, like everything else.
	wrapped := &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: cases["/p/image"]}}
	if ex := extractMessage(wrapped, time.Now(), "ID"); ex.directPath != "/p/image" || !ex.viewOnce {
		t.Errorf("view-once: path %q viewOnce %v", ex.directPath, ex.viewOnce)
	}
}

// The live path and the history-sync batch share persistMessage: both store
// the message's own direct path, and neither leaves a stale one behind.
func TestPersistMessageStoresTheDirectPath(t *testing.T) {
	ts := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	withPath := extractMessage(&waE2E.Message{DocumentMessage: fixtureDocument()}, ts, "D1")
	urlOnlyDoc := fixtureDocument()
	urlOnlyDoc.DirectPath = nil
	urlOnly := extractMessage(&waE2E.Message{DocumentMessage: urlOnlyDoc}, ts, "D1")
	text := extractMessage(&waE2E.Message{Conversation: proto.String("hi")}, ts, "T1")

	writers := map[string]func(ms *MessageStore, write func(w messageWriter)){
		"single row (live messages)": func(ms *MessageStore, write func(w messageWriter)) { write(ms) },
		"batch (history sync)": func(ms *MessageStore, write func(w messageWriter)) {
			if err := ms.Batch(func(b *messageBatch) error { write(b); return nil }); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, with := range writers {
		t.Run(name, func(t *testing.T) {
			ms := newTestMessageStore(t)
			if err := ms.StoreChat(mediaTestChat, "", ts); err != nil {
				t.Fatal(err)
			}
			persist := func(id string, e extractedMessage) {
				with(ms, func(w messageWriter) {
					if err := persistMessage(w, id, mediaTestChat, "5511999999999@s.whatsapp.net", ts, false, e, false, testLogger()); err != nil {
						t.Fatal(err)
					}
				})
			}
			persist("D1", withPath)
			if got := storedDirectPath(t, ms, "D1", mediaTestChat); got.String != fixtureDirectPath {
				t.Fatalf("direct_path = %q, want the message's own %q", got.String, fixtureDirectPath)
			}
			// The same message stored again without a direct path: the row
			// must not keep the path of a url it no longer describes.
			persist("D1", urlOnly)
			if got := storedDirectPath(t, ms, "D1", mediaTestChat); got.Valid {
				t.Errorf("direct_path = %q after a write without one, want NULL", got.String)
			}
			persist("T1", text)
			if got := storedDirectPath(t, ms, "T1", mediaTestChat); got.Valid {
				t.Errorf("a text row has direct_path %q", got.String)
			}
		})
	}
}

func TestHandleMessageAndHistorySyncStoreTheDirectPath(t *testing.T) {
	t.Setenv("WEBHOOK_ENABLED", "false")
	// One connection: the in-memory store is per connection, and history sync
	// writes inside a transaction.
	ms := newConcurrentTestStore(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
	b.MediaAutoDownload = false

	live := buildImageMessage(phonePN, phonePN, false, "")
	live.Info.ID = "LIVE1"
	live.Message = &waE2E.Message{DocumentMessage: fixtureDocument()}
	b.handleMessage(live)
	if got := storedDirectPath(t, ms, "LIVE1", phonePN.String()); got.String != fixtureDirectPath {
		t.Errorf("live message: direct_path = %q, want %q", got.String, fixtureDirectPath)
	}

	b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(phonePN.String()),
			Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{ID: proto.String("HIST1"), FromMe: proto.Bool(false)},
				MessageTimestamp: proto.Uint64(uint64(time.Now().Unix())), //nolint:gosec // test fixture
				Message:          &waE2E.Message{DocumentMessage: fixtureDocument()},
			}}},
		}},
	}})
	if got := storedDirectPath(t, ms, "HIST1", phonePN.String()); got.String != fixtureDirectPath {
		t.Errorf("history sync: direct_path = %q, want %q", got.String, fixtureDirectPath)
	}
}

// seedDocumentRow stores a complete document row sent at ts, with the fixture
// url, and the given direct path ("" leaves the column NULL, as on a row an
// older bridge wrote).
func seedDocumentRow(t *testing.T, ms *MessageStore, id, directPath string, ts time.Time) {
	t.Helper()
	doc := fixtureDocument()
	if err := ms.StoreChat(mediaTestChat, "", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, mediaTestChat, "5511999999999@s.whatsapp.net", "", ts, false, "document", doc.GetFileName(),
		doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength(), ""); err != nil {
		t.Fatal(err)
	}
	if directPath != "" {
		if err := ms.SetDirectPath(id, mediaTestChat, directPath); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDownloadMediaAsksForTheStoredDirectPath(t *testing.T) {
	cases := []struct {
		name, stored, want string
	}{
		{"the message's own direct path, although the url names another object", fixtureDirectPath, fixtureDirectPath},
		{"a row without one keeps the path cut out of the url", "", extractDirectPathFromURL(fixtureMediaURL)},
		{"a stored path without its leading slash gets one", strings.TrimPrefix(fixtureDirectPath, "/"), fixtureDirectPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			seedDocumentRow(t, ms, "DOC1", tc.stored, time.Now().Add(-time.Minute))

			var asked *MediaDownloader
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
				asked, _ = msg.(*MediaDownloader)
				return writeLikeDownloadToPath(relPath, []byte("docx bytes"))
			}
			if ok, _, _, _, err := b.downloadMedia(context.Background(), "DOC1", mediaTestChat); err != nil || !ok {
				t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
			}
			if asked == nil {
				t.Fatal("the transfer was never asked for")
			}
			if asked.GetDirectPath() != tc.want {
				t.Errorf("transfer asked for %q, want %q", asked.GetDirectPath(), tc.want)
			}
			if asked.GetURL() != fixtureMediaURL {
				t.Errorf("transfer url = %q, want the stored one", asked.GetURL())
			}
		})
	}
}

// retryRecorder stands in for the media retry: it counts the calls and
// answers with err, or by writing a file when err is nil.
type retryRecorder struct {
	calls int
	err   error
}

func (r *retryRecorder) retry(_ context.Context, _, _ string, _ *MediaDownloader, _ *os.Root, relPath string) (int64, error) {
	r.calls++
	if r.err != nil {
		return 0, r.err
	}
	return writeLikeDownloadToPath(relPath, []byte("re-uploaded bytes"))
}

// longHostLabel is a well-formed DNS label run long enough that only its
// length says it should not be printed.
var longHostLabel = strings.Repeat("a", 80)

// The markers of the fixture that must never reach a log line or an error.
var fixtureSecrets = []string{"URLNAMEMARKER", "URLTOKENMARKER", "URLEXPIRYMARKER", "PATHNAMEMARKER", "PATHTOKENMARKER", "PATHEXPIRYMARKER", "t62.7119-24", "5e03e0", longHostLabel}

func assertNoFixtureSecret(t *testing.T, what, text string) {
	t.Helper()
	for _, secret := range fixtureSecrets {
		if strings.Contains(text, secret) {
			t.Errorf("%s leaks %q:\n%s", what, secret, text)
		}
	}
}

// A CDN refusal of a message a few minutes old is not an expiry: the request
// is what failed. It is reported as retryable with the status in it, the
// sender's phone is not asked, and nothing says the file is gone.
func TestDownloadMediaFreshCDNRefusalIsNotAnExpiry(t *testing.T) {
	refusals := map[int]error{
		http.StatusForbidden: whatsmeow.ErrMediaDownloadFailedWith403,
		http.StatusNotFound:  whatsmeow.ErrMediaDownloadFailedWith404,
		http.StatusGone:      whatsmeow.ErrMediaDownloadFailedWith410,
	}
	for status, refusal := range refusals {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			log := installRecordingLogger(t)
			b := testBridge(t, nil, ms, log)
			seedDocumentRow(t, ms, "FRESH1", fixtureDirectPath, time.Now().Add(-4*time.Minute))
			var askedFor []string
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, _ string) (int64, error) {
				askedFor = append(askedFor, msg.GetDirectPath())
				return 0, refusal
			}
			retry := &retryRecorder{}
			b.mediaRetryDownload = retry.retry

			ok, _, _, _, err := b.downloadMedia(context.Background(), "FRESH1", mediaTestChat)
			var refused *cdnRefusedError
			if ok || !errors.As(err, &refused) || refused.status != status {
				t.Fatalf("ok=%v err=%v, want the fresh-refusal error with status %d", ok, err, status)
			}
			if errors.Is(err, errMediaUnavailable) {
				t.Errorf("a fresh refusal must not read as a lost file: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || !strings.Contains(err.Error(), "try again later") {
				t.Errorf("error does not name the status as retryable: %v", err)
			}
			if retry.calls != 0 {
				t.Errorf("media retry sent %d time(s) for a fresh message, want none", retry.calls)
			}
			// The message's own path first, then once the path this bridge
			// used to cut out of the url, before anything is concluded.
			if want := []string{fixtureDirectPath, extractDirectPathFromURL(fixtureMediaURL)}; !reflect.DeepEqual(askedFor, want) {
				t.Errorf("paths asked for = %q, want %q", askedFor, want)
			}
			if got := b.metrics.mediaDownloadFails.Load(); got != 1 {
				t.Errorf("download failures counted = %d, want 1", got)
			}
			// The log line the next report is read from.
			line := log.String()
			for _, want := range []string{
				"[WARN] CDN refused media for message FRESH1", fmt.Sprintf("HTTP %d", status), "message age 4m",
				pathFromMessage, "/v/ (3 segments, query ccb,oh,oe,_nc_sid, no expiry stamp)", "another object than the url's path",
				"url host mmg.whatsapp.net", "key 32, sha256 32, enc sha256 32 bytes",
			} {
				if !strings.Contains(line, want) {
					t.Errorf("log lacks %q:\n%s", want, line)
				}
			}
			assertNoFixtureSecret(t, "the log", line)
			assertNoFixtureSecret(t, "the error", err.Error())
		})
	}
}

// Past the window nothing changes: a refusal may be an expired link, so the
// sender's phone is asked, and its answer decides.
func TestDownloadMediaOldCDNRefusalStillGoesToTheMediaRetry(t *testing.T) {
	old := time.Now().Add(-cdnFreshWindow - time.Minute)
	t.Run("the retry brings the file", func(t *testing.T) {
		t.Setenv(storeDirEnv, t.TempDir())
		ms := newTestMessageStore(t)
		log := installRecordingLogger(t)
		b := testBridge(t, nil, ms, log)
		seedDocumentRow(t, ms, "OLD1", "", old)
		transfers := 0
		b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
			transfers++
			return 0, whatsmeow.ErrMediaDownloadFailedWith403
		}
		retry := &retryRecorder{}
		b.mediaRetryDownload = retry.retry
		if ok, _, _, _, err := b.downloadMedia(context.Background(), "OLD1", mediaTestChat); err != nil || !ok {
			t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
		}
		if retry.calls != 1 {
			t.Errorf("media retry calls = %d, want 1", retry.calls)
		}
		// The row has no direct path, so the url's path is the one that was
		// refused: it is not asked for a second time.
		if transfers != 1 {
			t.Errorf("the CDN was asked %d times for the same path, want once", transfers)
		}
		line := log.String()
		if !strings.Contains(line, "CDN refused media for message OLD1 with HTTP 403") || !strings.Contains(line, pathFromURL) {
			t.Errorf("the refusal is logged for old messages too, naming the path source:\n%s", line)
		}
		assertNoFixtureSecret(t, "the log", line)
	})
	t.Run("the phone says the file is gone", func(t *testing.T) {
		t.Setenv(storeDirEnv, t.TempDir())
		ms := newTestMessageStore(t)
		b := testBridge(t, nil, ms, installRecordingLogger(t))
		seedDocumentRow(t, ms, "OLD2", fixtureDirectPath, old)
		b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
			return 0, whatsmeow.ErrMediaDownloadFailedWith410
		}
		retry := &retryRecorder{err: fmt.Errorf("sender's phone declined media retry: NOT_FOUND: %w", errMediaUnavailable)}
		b.mediaRetryDownload = retry.retry
		_, _, _, _, err := b.downloadMedia(context.Background(), "OLD2", mediaTestChat)
		var refused *cdnRefusedError
		if retry.calls != 1 || !errors.Is(err, errMediaUnavailable) || errors.As(err, &refused) {
			t.Fatalf("calls=%d err=%v, want one retry and the phone's answer", retry.calls, err)
		}
	})
}

// Nothing that downloaded before the bridge kept the direct path may fail
// now: when the message's own path is refused and the url names something
// else, that one is tried once, and the log says which of the two worked.
func TestDownloadMediaFallsBackToTheURLPathOnce(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	log := installRecordingLogger(t)
	b := testBridge(t, nil, ms, log)
	seedDocumentRow(t, ms, "ALT1", fixtureDirectPath, time.Now().Add(-4*time.Minute))
	var askedFor []string
	b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		askedFor = append(askedFor, msg.GetDirectPath())
		if msg.GetDirectPath() == fixtureDirectPath {
			return 0, whatsmeow.ErrMediaDownloadFailedWith403
		}
		return writeLikeDownloadToPath(relPath, []byte("docx bytes"))
	}
	retry := &retryRecorder{}
	b.mediaRetryDownload = retry.retry

	if ok, _, _, _, err := b.downloadMedia(context.Background(), "ALT1", mediaTestChat); err != nil || !ok {
		t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
	}
	if want := []string{fixtureDirectPath, extractDirectPathFromURL(fixtureMediaURL)}; !reflect.DeepEqual(askedFor, want) {
		t.Errorf("paths asked for = %q, want %q", askedFor, want)
	}
	if retry.calls != 0 {
		t.Errorf("media retry calls = %d, want none", retry.calls)
	}
	line := log.String()
	if !strings.Contains(line, "CDN refused media for message ALT1 with HTTP 403") ||
		!strings.Contains(line, "[WARN] Message ALT1 was downloaded through the path cut out of its url after its direct path was refused") {
		t.Errorf("the log must say the direct path was refused and the url's path worked:\n%s", line)
	}
	assertNoFixtureSecret(t, "the log", line)
}

// A recent message can carry an old link. When the path's own stamp says the
// link expired, the refusal is an expiry whatever the message's age, and the
// sender's phone is asked as it always was.
func TestDownloadMediaFreshMessageWithAnExpiredLinkGoesToTheMediaRetry(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	// oe=1: one second after the epoch, in hexadecimal.
	seedDocumentRow(t, ms, "STALE1", "/v/t62.7119-24/old_n.enc?ccb=11-4&oh=x&oe=1", time.Now().Add(-4*time.Minute))
	b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
		return 0, whatsmeow.ErrMediaDownloadFailedWith403
	}
	retry := &retryRecorder{}
	b.mediaRetryDownload = retry.retry
	if ok, _, _, _, err := b.downloadMedia(context.Background(), "STALE1", mediaTestChat); err != nil || !ok {
		t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
	}
	if retry.calls != 1 {
		t.Errorf("media retry calls = %d, want 1 for a link stamped as expired", retry.calls)
	}
}

func TestLinkExpiry(t *testing.T) {
	now := time.Unix(0x66000000, 0)
	cases := []struct {
		path    string
		stamped bool
		expired bool
	}{
		{"/v/x.enc?ccb=1&oh=t&oe=65FFFFFF&_nc_sid=1", true, true},
		{"/v/x.enc?ccb=1&oh=t&oe=66000001", true, false},
		{"/v/x.enc?oe=66000001", true, false},
		{"/v/x.enc?ccb=1&oh=t", false, false},
		{"/v/x.enc?oe=not-hex", false, false},
		{"/v/x.enc?oe=", false, false},
		{"/v/x.enc?zoe=1", false, false},
		{"/v/x.enc", false, false},
	}
	for _, tc := range cases {
		if _, ok := linkExpiry(tc.path); ok != tc.stamped {
			t.Errorf("%s: stamped = %v, want %v", tc.path, ok, tc.stamped)
		}
		if got := linkExpired(tc.path, now); got != tc.expired {
			t.Errorf("%s: expired = %v, want %v", tc.path, got, tc.expired)
		}
	}
}

func TestHandleDownloadFreshCDNRefusalIsRetryable(t *testing.T) {
	const token = "test-token-0123456789"
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	b.Connected = func() bool { return true }
	seedDocumentRow(t, ms, "FRESH2", fixtureDirectPath, time.Now().Add(-4*time.Minute))
	b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
		return 0, whatsmeow.ErrMediaDownloadFailedWith403
	}
	retry := &retryRecorder{}
	b.mediaRetryDownload = retry.retry

	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"message_id":"FRESH2","chat_jid":%q}`, mediaTestChat)
	b.newRESTMux(8080, token).ServeHTTP(rec, downloadRequest(token, body))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if code := downloadErrorCode(t, rec); code != "bridge_unavailable" {
		t.Errorf("error code = %q, want bridge_unavailable (never media_unavailable)", code)
	}
	if !strings.Contains(rec.Body.String(), "HTTP 403") || !strings.Contains(rec.Body.String(), "not a lost file") {
		t.Errorf("answer does not name the status: %s", rec.Body.String())
	}
	if retry.calls != 0 {
		t.Errorf("media retry sent %d time(s), want none", retry.calls)
	}
	assertNoFixtureSecret(t, "the answer", rec.Body.String())
}

func TestMediaRequestShape(t *testing.T) {
	key, sha, enc := make([]byte, 32), make([]byte, 32), make([]byte, 32)
	cases := []struct {
		name, url, path, source string
		want                    []string
	}{
		{
			name: "the message's own path, another object than the url", url: fixtureMediaURL, path: fixtureDirectPath, source: pathFromMessage,
			want: []string{"asked for " + pathFromMessage + ": /v/ (3 segments, query ccb,oh,oe,_nc_sid, no expiry stamp)", "another object than the url's path", "url host mmg.whatsapp.net"},
		},
		{
			name: "the path cut out of the url", url: fixtureMediaURL, path: extractDirectPathFromURL(fixtureMediaURL), source: pathFromURL,
			want: []string{"asked for " + pathFromURL + ": /d/ (3 segments, query ccb,oh,oe, no expiry stamp)", "same as the url's path"},
		},
		{
			// What an ordinary message looks like: the url is the direct path
			// behind a host, plus a parameter of the client's own. That is the
			// same object, and must not read as a mismatch.
			name: "the url is the direct path plus a parameter", url: "https://mmg.whatsapp.net" + fixtureDirectPath + "&mms3=true", path: fixtureDirectPath, source: pathFromMessage,
			want: []string{"same object as the url's path, other parameters"},
		},
		{
			name: "no query string at all", url: "", path: "/o1/v/t24/PATHNAMEMARKER", source: pathFromMessage,
			want: []string{"/o1/ (4 segments, no query, no expiry stamp)", "no url stored", "url host none"},
		},
		{
			name: "a query piece that is not name=value is a token", url: "", path: "/v/x.enc?PATHTOKENMARKER&ccb=1&PATHEXPIRYMARKER=2", source: pathFromMessage,
			want: []string{"query ?,ccb,?"},
		},
		{
			name: "a one-segment path may be nothing but the file name", url: "", path: "/PATHNAMEMARKER.enc?ccb=1", source: pathFromMessage,
			want: []string{"/…/ (1 segments, query ccb, no expiry stamp)"},
		},
		{
			name: "a url the path cannot be cut out of", url: "not a url PATHTOKENMARKER", path: "not a url PATHTOKENMARKER", source: pathFromURL,
			want: []string{"/…/ (1 segments, no query, no expiry stamp)", "url host not a plain host name"},
		},
		{
			// The url is the sender's: a host that is not a short DNS name is
			// not copied into a line meant to be attached to a report.
			name: "a host made to mislead", url: "https://PATHTOKENMARKER_x.example.net/v/x.enc?ccb=1", path: "/v/x.enc?ccb=1", source: pathFromMessage,
			want: []string{"url host not a plain host name", "same as the url's path"},
		},
		{
			name: "a host made long", url: "https://" + longHostLabel + ".example.net/v/x.enc?ccb=1", path: "/v/x.enc?ccb=1", source: pathFromMessage,
			want: []string{"url host not a plain host name", "same as the url's path"},
		},
		{
			name: "a link stamped as expired", url: "", path: "/v/x.enc?ccb=1&oe=1", source: pathFromMessage,
			want: []string{"query ccb,oe, expiry stamp in the past"},
		},
		{
			name: "a link stamped as still valid", url: "", path: "/v/x.enc?ccb=1&oe=7fffffff", source: pathFromMessage,
			want: []string{"query ccb,oe, expiry stamp in the future"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaRequestShape(tc.url, tc.path, tc.source, key, sha, enc)
			for _, want := range append(tc.want, "key 32, sha256 32, enc sha256 32 bytes") {
				if !strings.Contains(got, want) {
					t.Errorf("shape lacks %q:\n%s", want, got)
				}
			}
			assertNoFixtureSecret(t, "the shape", got)
		})
	}
}

// What a media retry handed back replaces the message's own path, or the next
// download would go back to the one that just expired.
func TestStoreRefreshedMediaReplacesTheDirectPath(t *testing.T) {
	ms := newTestMessageStore(t)
	installRecordingLogger(t)
	seedDocumentRow(t, ms, "RETRIED", fixtureDirectPath, time.Now().Add(-48*time.Hour))
	doc := fixtureDocument()
	const fresh = "/v/t62.7119-24/fresh_n.enc?ccb=11-4&oh=new&oe=new"
	storeRefreshedMedia(ms, "RETRIED", mediaTestChat, &MediaDownloader{
		URL: mediaURLFromDirectPath(fresh), DirectPath: fresh,
		MediaKey: doc.GetMediaKey(), FileSHA256: doc.GetFileSHA256(), FileEncSHA256: doc.GetFileEncSHA256(), FileLength: doc.GetFileLength(),
	})
	if got := storedDirectPath(t, ms, "RETRIED", mediaTestChat); got.String != fresh {
		t.Errorf("direct_path = %q, want the fresh %q", got.String, fresh)
	}
	var url string
	if err := ms.db.QueryRow("SELECT url FROM messages WHERE id = ?", "RETRIED").Scan(&url); err != nil || url != mediaCDNHost+fresh {
		t.Errorf("url = %q (%v), want %q", url, err, mediaCDNHost+fresh)
	}
}

// A store an older bridge created gets the column, and its rows download the
// way they did.
func TestDirectPathColumnIsAddedToAnOlderStore(t *testing.T) {
	ms := newTestMessageStore(t)
	if _, err := ms.db.Exec("ALTER TABLE messages DROP COLUMN direct_path"); err != nil {
		t.Fatalf("simulate the older schema: %v", err)
	}
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatalf("the migration is not idempotent: %v", err)
	}
	seedDocumentRow(t, ms, "OLDROW", "", time.Now())
	if got := storedDirectPath(t, ms, "OLDROW", mediaTestChat); got.Valid {
		t.Errorf("direct_path = %q on a row written without one, want NULL", got.String)
	}
}
