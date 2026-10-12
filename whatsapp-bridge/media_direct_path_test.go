package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
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
	if err := ms.StoreMessage(storedMessage{
		ID:            id,
		ChatJID:       mediaTestChat,
		Sender:        "5511999999999@s.whatsapp.net",
		Timestamp:     ts,
		MediaType:     "document",
		Filename:      doc.GetFileName(),
		URL:           doc.GetURL(),
		MediaKey:      doc.GetMediaKey(),
		FileSHA256:    doc.GetFileSHA256(),
		FileEncSHA256: doc.GetFileEncSHA256(),
		FileLength:    doc.GetFileLength(),
	}); err != nil {
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

func TestDownloadMediaUnavailableExplainsOnlyExpiredLinks(t *testing.T) {
	past := time.Date(2020, 1, 2, 0, 30, 0, 0, time.UTC)
	future := time.Now().Add(24 * time.Hour)
	gone := fmt.Errorf("sender's phone declined media retry: NOT_FOUND: %w", errMediaUnavailable)
	for _, tc := range []struct {
		name     string
		stamp    string
		legacy   bool
		retryErr error
		explain  bool
	}{
		{"past with own path", fmt.Sprintf("&oe=%X", past.Unix()), false, gone, false},
		{"past URL fallback", fmt.Sprintf("&oe=%X", past.Unix()), true, gone, true},
		{"future", fmt.Sprintf("&oe=%X", future.Unix()), true, gone, false},
		{"absent", "", true, gone, false},
		{"invalid", "&oe=invalid", true, gone, false},
		{"transient retry", fmt.Sprintf("&oe=%X", past.Unix()), true, errors.New("retry timed out"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			path := "/v/f/SECRETFILENAME.enc?oh=SECRETTOKEN" + tc.stamp
			// Old messages retry even without a past stamp, so all cases
			// exercise the same NOT_FOUND path rather than the fresh guard.
			seedDocumentRow(t, ms, "GONE1", path, time.Now().Add(-2*cdnFreshWindow))
			if _, err := ms.db.Exec("UPDATE messages SET url=?, direct_path=? WHERE id=? AND chat_jid=?",
				mediaURLFromDirectPath(path), path, "GONE1", mediaTestChat); err != nil {
				t.Fatal(err)
			}
			if tc.legacy {
				if _, err := ms.db.Exec("UPDATE messages SET direct_path=NULL WHERE id=? AND chat_jid=?", "GONE1", mediaTestChat); err != nil {
					t.Fatal(err)
				}
			}
			transfers := 0
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, _ string) (int64, error) {
				transfers++
				if msg.GetDirectPath() != path {
					t.Errorf("download path = %q, want %q", msg.GetDirectPath(), path)
				}
				return 0, whatsmeow.ErrMediaDownloadFailedWith403
			}
			retry := &retryRecorder{err: tc.retryErr}
			b.mediaRetryDownload = retry.retry
			_, _, _, _, err := b.downloadMedia(context.Background(), "GONE1", mediaTestChat)
			if err == nil || transfers != 1 || retry.calls != 1 || !errors.Is(err, tc.retryErr) {
				t.Fatalf("transfers=%d retries=%d err=%v", transfers, retry.calls, err)
			}
			if errors.Is(err, errMediaUnavailable) != errors.Is(tc.retryErr, errMediaUnavailable) {
				t.Fatalf("media_unavailable classification changed: %v", err)
			}
			if tc.explain {
				for _, want := range []string{"this row has no direct path for the file", "legacy link expired on 2020-01-02 (UTC)", "request_history(chat_jid, before_message_id=<id>)", "then download_media after the replay arrives", "the phone decides whether it re-delivers the locator", "ask for the file again"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q lacks %q", err, want)
					}
				}
			} else if want := "failed to download media: " + tc.retryErr.Error(); err.Error() != want {
				t.Errorf("error = %q, want unchanged %q", err, want)
			}
			for _, secret := range []string{"SECRETTOKEN", "SECRETFILENAME", path, mediaCDNHost} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error exposed %q", secret)
				}
			}
			if tc.retryErr == gone {
				b.Connected = func() bool { return true }
				const token = "test-token-0123456789"
				rec := httptest.NewRecorder()
				body := fmt.Sprintf(`{"message_id":"GONE1","chat_jid":%q}`, mediaTestChat)
				b.newRESTMux(8080, token).ServeHTTP(rec, downloadRequest(token, body))
				var response apiError
				if decodeErr := json.Unmarshal(rec.Body.Bytes(), &response); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if rec.Code != http.StatusInternalServerError || response.Error.Code != "media_unavailable" || response.Error.Message != "Failed to download media: "+err.Error() {
					t.Fatalf("REST error: %d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestHistorySyncRepairsMissingDirectPathWithoutReplacingExisting(t *testing.T) {
	for _, syncType := range []waHistorySync.HistorySync_HistorySyncType{
		waHistorySync.HistorySync_INITIAL_BOOTSTRAP, waHistorySync.HistorySync_ON_DEMAND,
	} {
		t.Run(syncType.String(), func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newConcurrentTestStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.MediaAutoDownload = false
			ts := time.Now().Add(-time.Hour)
			legacy := fixtureDocument()
			legacy.DirectPath = nil
			if err := persistMessage(ms, "REPAIR1", phonePN.String(), phonePN.String(), ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, "REPAIR1"), true, b.Log); err != nil {
				t.Fatal(err)
			}
			if got := storedDirectPath(t, ms, "REPAIR1", phonePN.String()); got.Valid {
				t.Fatalf("legacy row already has a direct path: %v", got)
			}
			replay := func(doc *waE2E.DocumentMessage) {
				b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
					SyncType: syncType.Enum(),
					Conversations: []*waHistorySync.Conversation{{
						ID: proto.String(phonePN.String()),
						Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
							Key:              &waCommon.MessageKey{ID: proto.String("REPAIR1"), FromMe: proto.Bool(false)},
							MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
							Message:          &waE2E.Message{DocumentMessage: doc},
						}}},
					}},
				}})
			}
			repaired := fixtureDocument()
			repaired.DirectPath = proto.String("/o1/v/CURRENT.enc?oe=7FFFFFFF&oh=TOKEN")
			repaired.FileLength = proto.Uint64(0) // Empty files still have complete credentials.
			replay(repaired)
			if got := storedDirectPath(t, ms, "REPAIR1", phonePN.String()); got.String != repaired.GetDirectPath() {
				t.Fatalf("history did not repair missing path: %v", got)
			}
			transfers := 0
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, rel string) (int64, error) {
				transfers++
				if msg.GetDirectPath() != repaired.GetDirectPath() {
					t.Errorf("download asked for %q, want repaired locator", msg.GetDirectPath())
				}
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error {
					_, err := f.Write([]byte("document"))
					return err
				})
			}
			if ok, _, _, _, err := b.downloadMedia(context.Background(), "REPAIR1", phonePN.String()); err != nil || !ok || transfers != 1 {
				t.Fatalf("repaired download: ok=%v transfers=%d err=%v", ok, transfers, err)
			}
			// A later history replay must not roll back a locator refreshed by
			// a download, nor mix that locator with another snapshot's keys.
			replacement := fixtureDocument()
			replacement.DirectPath = proto.String("/o1/v/OTHER.enc?oe=7FFFFFFF")
			replacement.MediaKey = []byte(strings.Repeat("z", 32))
			replacement.FileSHA256 = []byte(strings.Repeat("h", 32))
			replay(replacement)
			if got := storedDirectPath(t, ms, "REPAIR1", phonePN.String()); got.String != repaired.GetDirectPath() {
				t.Errorf("history replaced existing path: %v", got)
			}
			var key, hash []byte
			if err := ms.db.QueryRow("SELECT media_key,file_sha256 FROM messages WHERE id=? AND chat_jid=?", "REPAIR1", phonePN.String()).Scan(&key, &hash); err != nil {
				t.Fatal(err)
			}
			if string(key) != string(repaired.MediaKey) || string(hash) != string(repaired.FileSHA256) {
				t.Error("history mixed a preserved path with another snapshot's credentials")
			}
			// Complete credentials for the same locator can still refresh the
			// snapshot: preserving the path must not freeze the entire row.
			refresh := fixtureDocument()
			refresh.DirectPath = repaired.DirectPath
			refresh.MediaKey = []byte(strings.Repeat("r", 32))
			refresh.FileSHA256 = []byte(strings.Repeat("n", 32))
			refresh.FileEncSHA256 = []byte(strings.Repeat("c", 32))
			refresh.FileLength = proto.Uint64(5432)
			replay(refresh)
			var path string
			var encHash []byte
			var length uint64
			if err := ms.db.QueryRow("SELECT direct_path,media_key,file_sha256,file_enc_sha256,file_length FROM messages WHERE id=? AND chat_jid=?", "REPAIR1", phonePN.String()).Scan(&path, &key, &hash, &encHash, &length); err != nil {
				t.Fatal(err)
			}
			if path != repaired.GetDirectPath() || string(key) != string(refresh.MediaKey) || string(hash) != string(refresh.FileSHA256) || string(encHash) != string(refresh.FileEncSHA256) || length != refresh.GetFileLength() {
				t.Errorf("same-path history did not refresh complete media: path=%q key=%q hash=%q enc=%q length=%d", path, key, hash, encHash, length)
			}
			partial := fixtureDocument()
			partial.DirectPath = refresh.DirectPath
			partial.MediaKey, partial.FileSHA256, partial.FileEncSHA256, partial.FileLength = nil, nil, nil, nil
			replay(partial)
			if err := ms.db.QueryRow("SELECT direct_path,media_key,file_sha256,file_enc_sha256,file_length FROM messages WHERE id=? AND chat_jid=?", "REPAIR1", phonePN.String()).Scan(&path, &key, &hash, &encHash, &length); err != nil {
				t.Fatal(err)
			}
			if path != refresh.GetDirectPath() || string(key) != string(refresh.MediaKey) || string(hash) != string(refresh.FileSHA256) || string(encHash) != string(refresh.FileEncSHA256) || length != refresh.GetFileLength() {
				t.Error("incomplete replay blanked a stored media snapshot")
			}
		})
	}
}

func TestHistoryRepairLIDReplayDownload(t *testing.T) {
	for _, tc := range []struct {
		name, requestChat, replayChat                                              string
		complete, peer, failAlias, rollbackAlias, ownTwin, nonMedia, discoveryFail bool
		badTimestamp                                                               string
		syncType                                                                   *waHistorySync.HistorySync_HistorySyncType
	}{
		{name: "LID replay", requestChat: phonePN.String(), replayChat: phoneLID.String()},
		{name: "phone replay", requestChat: phoneLID.String(), replayChat: phonePN.String()},
		{name: "complete alias preserved", requestChat: phonePN.String(), replayChat: phoneLID.String(), complete: true},
		{name: "peer cannot repair alias", requestChat: phonePN.String(), replayChat: phonePN.String(), peer: true},
		{name: "alias abort preserves canonical chunk", requestChat: phonePN.String(), replayChat: phonePN.String(), failAlias: true},
		{name: "alias rollback preserves canonical chunk", requestChat: phonePN.String(), replayChat: phonePN.String(), rollbackAlias: true},
		{name: "null alias timestamp", requestChat: phonePN.String(), replayChat: phonePN.String(), badTimestamp: "null"},
		{name: "garbled alias timestamp", requestChat: phonePN.String(), replayChat: phonePN.String(), badTimestamp: "garbled"},
		{name: "different authorship", requestChat: phonePN.String(), replayChat: phonePN.String(), ownTwin: true},
		{name: "non media twin", requestChat: phonePN.String(), replayChat: phonePN.String(), nonMedia: true},
		{name: "discovery read canceled after canonical commit", requestChat: phonePN.String(), replayChat: phoneLID.String(), discoveryFail: true},
		{name: "pair time does not repair twins", requestChat: phonePN.String(), replayChat: phonePN.String(), syncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum()},
		{name: "recent does not repair twins", requestChat: phonePN.String(), replayChat: phonePN.String(), syncType: waHistorySync.HistorySync_RECENT.Enum()},
		{name: "full does not repair twins", requestChat: phonePN.String(), replayChat: phonePN.String(), syncType: waHistorySync.HistorySync_FULL.Enum()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newConcurrentTestStore(t)
			client := newTestClientWithSelf(&mockLIDStore{
				lidByPN: map[types.JID]types.JID{phonePN: phoneLID},
				pnByLID: map[types.JID]types.JID{phoneLID: phonePN},
			}, selfPhone)
			log := installRecordingLogger(t)
			b := testBridge(t, client, ms, log)
			b.MediaAutoDownload = false
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.discoveryFail {
				b.historyBatchWriter = func(write func(*messageBatch) error) error {
					err := ms.BatchContext(ctx, write)
					if err == nil {
						cancel()
					}
					return err
				}
			}
			ts := time.Now().Add(-time.Hour).Truncate(time.Second)
			legacy := fixtureDocument()
			legacy.DirectPath = nil
			if tc.complete {
				legacy.DirectPath = proto.String("/o1/v/PRESERVED.enc?oe=7FFFFFFF")
			}
			if err := persistMessage(ms, "REPAIR1", phoneLID.String(), phoneLID.String(), ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, "REPAIR1"), true, b.Log); err != nil {
				t.Fatal(err)
			}
			if _, err := ms.db.Exec("UPDATE messages SET content='archived caption' WHERE id=? AND chat_jid=?", "REPAIR1", phoneLID.String()); err != nil {
				t.Fatal(err)
			}
			if tc.ownTwin {
				if _, err := ms.db.Exec("UPDATE messages SET is_from_me=1 WHERE id=? AND chat_jid=?", "REPAIR1", phoneLID.String()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.nonMedia {
				if _, err := ms.db.Exec("UPDATE messages SET media_type='',filename=NULL,url=NULL,media_key=NULL,file_sha256=NULL,file_enc_sha256=NULL,file_length=NULL WHERE id=? AND chat_jid=?", "REPAIR1", phoneLID.String()); err != nil {
					t.Fatal(err)
				}
				// Another eligible media twin makes discovery succeed, so the
				// per-row non-media guard is exercised independently.
				if err := persistMessage(ms, "NEW1", phoneLID.String(), phoneLID.String(), ts, false, extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, "NEW1"), false, b.Log); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failAlias {
				if _, err := ms.db.Exec(`CREATE TRIGGER deny_alias BEFORE UPDATE ON messages
					WHEN OLD.id='REPAIR1' AND OLD.chat_jid LIKE '%@lid'
					BEGIN SELECT RAISE(ABORT, 'alias write refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rollbackAlias {
				if _, err := ms.db.Exec(`CREATE TRIGGER rollback_alias BEFORE UPDATE ON messages
					WHEN OLD.id='REPAIR1' AND OLD.chat_jid LIKE '%@lid'
					BEGIN SELECT RAISE(ROLLBACK, 'alias transaction refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			const other = "5511888888888@s.whatsapp.net"
			if err := persistMessage(ms, "REPAIR1", other, other, ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, "REPAIR1"), true, b.Log); err != nil {
				t.Fatal(err)
			}
			repaired := fixtureDocument()
			repaired.Caption = proto.String("replay caption")
			sends := 0
			mux := http.NewServeMux()
			registerHistoryEndpointWithSender(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, client,
				func() bool { return true }, ms, chatPolicy{}, func(_ context.Context, msg *waE2E.Message) error {
					anchor := msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
					sends++
					if sends == 2 {
						if anchor.GetChatJID() != phonePN.String() {
							t.Fatalf("wrong twin: %v", anchor)
						}
						return nil
					}
					if anchor.GetChatJID() != phoneLID.String() {
						t.Fatalf("request lost stored identity: %v", anchor)
					}
					if tc.badTimestamp != "" {
						var value any
						if tc.badTimestamp != "null" {
							value = tc.badTimestamp
						}
						if _, err := ms.db.Exec("UPDATE messages SET timestamp=? WHERE id=? AND chat_jid=?", value, "REPAIR1", phoneLID.String()); err != nil {
							t.Fatal(err)
						}
					}
					syncType := tc.syncType
					if syncType == nil {
						syncType = waHistorySync.HistorySync_ON_DEMAND.Enum()
					}
					b.handleHistorySyncWithSharesContext(ctx, &events.HistorySync{Data: &waHistorySync.HistorySync{
						SyncType: syncType,
						Conversations: []*waHistorySync.Conversation{{
							ID: proto.String(tc.replayChat),
							Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
								Key:              &waCommon.MessageKey{ID: proto.String("REPAIR1"), FromMe: proto.Bool(false)},
								MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
								Message:          &waE2E.Message{DocumentMessage: repaired},
							}}, {Message: &waWeb.WebMessageInfo{
								Key:              &waCommon.MessageKey{ID: proto.String("NEW1"), FromMe: proto.Bool(false)},
								MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
								Message:          &waE2E.Message{DocumentMessage: repaired},
							}}},
						}},
					}}, false, tc.peer)
					return nil
				})
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"chat_jid":%q,"before_message_id":"REPAIR1"}`, tc.requestChat)
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
			}
			wantPath := repaired.GetDirectPath()
			noRepair := tc.complete || tc.peer || tc.failAlias || tc.rollbackAlias || tc.ownTwin || tc.nonMedia || tc.discoveryFail || tc.badTimestamp != "" || tc.syncType != nil
			if noRepair {
				wantPath = legacy.GetDirectPath()
			}
			if got := storedDirectPath(t, ms, "REPAIR1", phoneLID.String()); got.String != wantPath {
				t.Fatalf("replay did not preserve/repair selected LID row: got=%v want=%q", got, wantPath)
			}
			if got := storedDirectPath(t, ms, "REPAIR1", other); got.String != legacy.GetDirectPath() {
				t.Fatal("replay crossed a different chat's message key")
			}
			var content, senderServer, stamp string
			if err := ms.db.QueryRow("SELECT content,sender_server,COALESCE(CAST(timestamp AS TEXT),'') FROM messages WHERE id=? AND chat_jid=?", "REPAIR1", phoneLID.String()).Scan(&content, &senderServer, &stamp); err != nil {
				t.Fatal(err)
			}
			wantStamp := dbTime(ts)
			if tc.badTimestamp != "" {
				wantStamp = tc.badTimestamp
				if wantStamp == "null" {
					wantStamp = ""
				}
			}
			if content != "archived caption" || senderServer != types.HiddenUserServer || stamp != wantStamp {
				t.Fatalf("alias non-media metadata changed: content=%q server=%q stamp=%q", content, senderServer, stamp)
			}
			var aliases, canonical int
			wantAliases := 0
			if tc.nonMedia {
				wantAliases = 1
			}
			if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE id=? AND chat_jid=?", "NEW1", phoneLID.String()).Scan(&aliases); err != nil || aliases != wantAliases {
				t.Fatalf("created a new alias row: count=%d err=%v", aliases, err)
			}
			if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE id=? AND chat_jid=?", "REPAIR1", phonePN.String()).Scan(&canonical); err != nil {
				t.Fatal(err)
			}
			if canonical != 1 || storedDirectPath(t, ms, "REPAIR1", phonePN.String()).String != repaired.GetDirectPath() || storedDirectPath(t, ms, "NEW1", phonePN.String()).String != repaired.GetDirectPath() {
				t.Fatal("optional alias problem lost the canonical target or its neighbor")
			}
			if tc.failAlias || tc.rollbackAlias || tc.badTimestamp != "" {
				if !strings.Contains(log.String(), "[WARN] Failed optional history media alias repair") {
					t.Fatalf("missing repair warning: %s", log.String())
				}
			}
			if tc.discoveryFail && !strings.Contains(log.String(), "[WARN] Failed optional history media alias discovery") {
				t.Fatalf("missing discovery warning: %s", log.String())
			}
			if noRepair && !tc.complete {
				return
			}
			transfers := 0
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, rel string) (int64, error) {
				transfers++
				if msg.GetDirectPath() != wantPath {
					t.Errorf("download locator=%q want=%q", msg.GetDirectPath(), wantPath)
				}
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write([]byte("document")); return err })
			}
			ok, _, _, path, err := b.downloadMedia(context.Background(), "REPAIR1", phoneLID.String())
			if err != nil || !ok || transfers != 1 {
				t.Fatalf("original LID download failed: ok=%v transfers=%d err=%v", ok, transfers, err)
			}
			rel, err := filepath.Rel(b.StoreRoot.Name(), path)
			if err != nil {
				t.Fatal(err)
			}
			if data, err := b.StoreRoot.ReadFile(rel); err != nil || string(data) != "document" {
				t.Fatalf("downloaded artifact=%q err=%v", data, err)
			}
		})
	}
}

func TestHistorySyncRepairsIncompleteMediaWithAnExistingPath(t *testing.T) {
	for _, missing := range []string{"key", "sha256", "enc_sha256"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newConcurrentTestStore(t)
			b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
			b.MediaAutoDownload = false
			ts := time.Now().Add(-time.Hour)
			partial := fixtureDocument()
			switch missing {
			case "key":
				partial.MediaKey = nil
			case "sha256":
				partial.FileSHA256 = nil
			case "enc_sha256":
				partial.FileEncSHA256 = nil
			}
			if err := persistMessage(ms, "PARTIAL1", phonePN.String(), phonePN.String(), ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: partial}, ts, "PARTIAL1"), true, b.Log); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := b.downloadMedia(context.Background(), "PARTIAL1", phonePN.String()); !errors.Is(err, errMediaUnavailable) {
				t.Fatalf("partial snapshot: err=%v, want media_unavailable", err)
			}
			complete := fixtureDocument()
			complete.DirectPath = proto.String("/o1/v/COMPLETE.enc?oe=7FFFFFFF")
			b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
				SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(),
				Conversations: []*waHistorySync.Conversation{{
					ID: proto.String(phonePN.String()),
					Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
						Key:              &waCommon.MessageKey{ID: proto.String("PARTIAL1"), FromMe: proto.Bool(false)},
						MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
						Message:          &waE2E.Message{DocumentMessage: complete},
					}}},
				}},
			}})
			transfers := 0
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, rel string) (int64, error) {
				transfers++
				if msg.GetDirectPath() != complete.GetDirectPath() || !reflect.DeepEqual(msg.GetMediaKey(), complete.GetMediaKey()) || !reflect.DeepEqual(msg.GetFileSHA256(), complete.GetFileSHA256()) || !reflect.DeepEqual(msg.GetFileEncSHA256(), complete.GetFileEncSHA256()) {
					t.Error("repaired download did not use the complete snapshot")
				}
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error {
					_, err := f.Write([]byte("document"))
					return err
				})
			}
			if ok, _, _, _, err := b.downloadMedia(context.Background(), "PARTIAL1", phonePN.String()); err != nil || !ok || transfers != 1 {
				t.Fatalf("complete history did not repair partial snapshot: ok=%v transfers=%d err=%v", ok, transfers, err)
			}
		})
	}
}

type historyRepairCountingLIDs struct {
	mockLIDStore
	calls int
}

func (s *historyRepairCountingLIDs) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	if pn == phonePN {
		s.calls++
	} // exclude the pre-existing self-identity lookup
	return s.mockLIDStore.GetLIDForPN(ctx, pn)
}

func TestHistoryRepairAliasDiscoveryOncePerChat(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	lids := &historyRepairCountingLIDs{mockLIDStore: mockLIDStore{lidByPN: map[types.JID]types.JID{phonePN: phoneLID}}}
	b := testBridge(t, newTestClientWithSelf(lids, selfPhone), ms, testLogger())
	b.MediaAutoDownload = false
	ts := time.Now().Add(-time.Hour)
	legacy := fixtureDocument()
	legacy.DirectPath = nil
	var rows []*waHistorySync.HistorySyncMsg
	for i := 0; i <= historyBatchMessages; i++ {
		id := fmt.Sprintf("CHUNK%d", i)
		if i == 0 || i == historyBatchMessages {
			if err := persistMessage(ms, id, phoneLID.String(), phoneLID.String(), ts, false,
				extractMessage(&waE2E.Message{DocumentMessage: legacy}, ts, id), false, b.Log); err != nil {
				t.Fatal(err)
			}
		}
		rows = append(rows, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(false)},
			MessageTimestamp: proto.Uint64(uint64(ts.Unix())), //nolint:gosec // test fixture
			Message:          &waE2E.Message{DocumentMessage: fixtureDocument()},
		}})
	}
	b.handleHistorySyncWithShares(&events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_ON_DEMAND.Enum(),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(phonePN.String()), Messages: rows}},
	}}, false, false)
	if lids.calls != 1 {
		t.Fatalf("chat alias SDK reads=%d, want one for two committed chunks", lids.calls)
	}
	for _, id := range []string{"CHUNK0", fmt.Sprintf("CHUNK%d", historyBatchMessages)} {
		if storedDirectPath(t, ms, id, phoneLID.String()).String != fixtureDirectPath {
			t.Fatalf("chunk target %s not repaired", id)
		}
	}
}

func TestHistoryRepairAliasDiscoveryScope(t *testing.T) {
	ms := newTestMessageStore(t)
	ts := time.Now().Add(-time.Hour)
	other := "999888777@lid"
	for chat, id := range map[string]string{phoneLID.String(): "OTHER1", other: "TARGET1"} {
		if err := persistMessage(ms, id, chat, chat, ts, false,
			extractMessage(&waE2E.Message{DocumentMessage: fixtureDocument()}, ts, id), false, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"TARGET1", false}, {"MISSING1", false}, {"OTHER1", true}} {
		found, err := ms.historyMediaAliasExists(context.Background(), phoneLID.String(), []storedMessage{{ID: tc.id}})
		if err != nil || found != tc.want {
			t.Fatalf("discovery %s: found=%v err=%v want=%v", tc.id, found, err, tc.want)
		}
	}
}

func TestHistoryReplayPreservesCompleteNewsletterSnapshot(t *testing.T) {
	const chat = "12345@newsletter"
	ms := newTestMessageStore(t)
	ts := time.Now()
	if err := ms.StoreChat(chat, "", ts); err != nil {
		t.Fatal(err)
	}
	stored := storedMessage{
		ID: "CHANNEL1", ChatJID: chat, Timestamp: ts, MediaType: "document",
		URL: fixtureMediaURL, FileSHA256: []byte(strings.Repeat("s", 32)), FileLength: uint64(0),
		Media: messageMediaOptions{directPath: fixtureDirectPath},
	}
	if !mediaComplete(chat, stored.URL, stored.Media.directPath, stored.MediaKey, stored.FileSHA256, stored.FileEncSHA256) {
		t.Fatal("unencrypted newsletter fixture is not complete")
	}
	if err := ms.StoreMessage(stored); err != nil {
		t.Fatal(err)
	}
	replay := stored
	replay.Media.directPath = "/o1/v/OTHER.enc"
	replay.FileSHA256 = []byte(strings.Repeat("n", 32))
	if err := ms.Batch(func(batch *messageBatch) error {
		batch.historyReplay = true
		return batch.StoreMessage(replay)
	}); err != nil {
		t.Fatal(err)
	}
	var path string
	var hash []byte
	var length uint64
	if err := ms.db.QueryRow("SELECT direct_path,file_sha256,file_length FROM messages WHERE id=? AND chat_jid=?", stored.ID, chat).Scan(&path, &hash, &length); err != nil {
		t.Fatal(err)
	}
	if path != fixtureDirectPath || !reflect.DeepEqual(hash, stored.FileSHA256) || length != 0 {
		t.Error("history replaced a complete unencrypted newsletter snapshot")
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
