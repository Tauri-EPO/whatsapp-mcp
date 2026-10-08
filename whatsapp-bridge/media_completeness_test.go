package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// seedCompletenessRow stores a document row with exactly the given CDN
// columns, so a test can name the row that lacks one thing.
func seedCompletenessRow(t *testing.T, ms *MessageStore, id, url, directPath string, key, sha, enc []byte, length uint64) {
	t.Helper()
	ts := time.Now().Add(-time.Minute)
	if err := ms.StoreChat(mediaTestChat, "", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, mediaTestChat, "5511999999999@s.whatsapp.net", "", ts, false, "document", "empty.txt", url, key, sha, enc, length, ""); err != nil {
		t.Fatal(err)
	}
	if err := ms.SetDirectPath(id, mediaTestChat, directPath); err != nil {
		t.Fatal(err)
	}
}

func TestMediaComplete(t *testing.T) {
	k, s, e := []byte("k"), []byte("s"), []byte("e")
	cases := []struct {
		name            string
		url, directPath string
		key, sha, enc   []byte
		want            bool
	}{
		{"url and direct path", "u", "/p", k, s, e, true},
		{"url only", "u", "", k, s, e, true},
		{"direct path only", "", "/p", k, s, e, true},
		{"nowhere to ask", "", "", k, s, e, false},
		{"no key", "u", "/p", nil, s, e, false},
		{"no plaintext hash", "u", "/p", k, nil, e, false},
		{"no encrypted hash", "u", "/p", k, s, nil, false},
		{"nothing at all", "", "", nil, nil, nil, false},
	}
	for _, tc := range cases {
		if got := mediaComplete(tc.url, tc.directPath, tc.key, tc.sha, tc.enc); got != tc.want {
			t.Errorf("%s: mediaComplete = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// What makes a row downloadable is somewhere to ask, the key and the two
// hashes. A zero length and a missing url are not what "incomplete" means.
func TestDownloadMediaCompleteness(t *testing.T) {
	doc := fixtureDocument()
	key, sha, enc := doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256()
	cases := []struct {
		name            string
		url, directPath string
		key, sha, enc   []byte
		length          uint64
		wantPath        string // the path the transfer is asked for; "" = refused before any transfer
	}{
		{name: "an empty file: length 0, everything else there", url: fixtureMediaURL, directPath: fixtureDirectPath, key: key, sha: sha, enc: enc, length: 0, wantPath: fixtureDirectPath},
		{name: "an empty file on a row without a direct path", url: fixtureMediaURL, key: key, sha: sha, enc: enc, length: 0, wantPath: extractDirectPathFromURL(fixtureMediaURL)},
		{name: "a direct path and no url", directPath: fixtureDirectPath, key: key, sha: sha, enc: enc, length: 4321, wantPath: fixtureDirectPath},
		{name: "nowhere to ask: neither url nor direct path", key: key, sha: sha, enc: enc, length: 4321},
		{name: "no media key", url: fixtureMediaURL, directPath: fixtureDirectPath, sha: sha, enc: enc, length: 4321},
		{name: "no plaintext hash", url: fixtureMediaURL, directPath: fixtureDirectPath, key: key, enc: enc, length: 4321},
		{name: "no encrypted hash", url: fixtureMediaURL, directPath: fixtureDirectPath, key: key, sha: sha, length: 4321},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			seedCompletenessRow(t, ms, "ROW1", tc.url, tc.directPath, tc.key, tc.sha, tc.enc, tc.length)

			var asked []string
			b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
				asked = append(asked, msg.GetDirectPath())
				// An empty file: nothing to write but the file itself.
				return writeLikeDownloadToPath(relPath, make([]byte, tc.length))
			}
			retry := &retryRecorder{}
			b.mediaRetryDownload = retry.retry

			ok, _, _, absPath, err := b.downloadMedia(context.Background(), "ROW1", mediaTestChat)
			if tc.wantPath == "" {
				// Final, and decided before the network is touched.
				if ok || !errors.Is(err, errMediaUnavailable) || !strings.Contains(err.Error(), "incomplete media information") {
					t.Fatalf("ok=%v err=%v, want the permanent incomplete-information error", ok, err)
				}
				if len(asked) != 0 || retry.calls != 0 {
					t.Errorf("an incomplete row reached the network: transfers %v, retries %d", asked, retry.calls)
				}
				return
			}
			if err != nil || !ok {
				t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
			}
			if len(asked) != 1 || asked[0] != tc.wantPath {
				t.Errorf("transfer asked for %q, want [%q]", asked, tc.wantPath)
			}
			info, statErr := os.Stat(absPath)
			if statErr != nil || info.Size() != int64(tc.length) { //nolint:gosec // test lengths are tiny
				t.Fatalf("cached file: %v, size %v, want %d bytes", statErr, info, tc.length)
			}
			// The file is in the cache now, empty or not: a second call asks nobody.
			if ok, _, _, again, err := b.downloadMedia(context.Background(), "ROW1", mediaTestChat); err != nil || !ok || again != absPath || len(asked) != 1 {
				t.Errorf("second call: ok=%v err=%v path=%q transfers=%d, want the cached file and no transfer", ok, err, again, len(asked))
			}
		})
	}
}

func TestHandleDownloadServesAnEmptyFile(t *testing.T) {
	const token = "test-token-0123456789"
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	b.Connected = func() bool { return true }
	doc := fixtureDocument()
	seedCompletenessRow(t, ms, "EMPTY1", fixtureMediaURL, fixtureDirectPath, doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), 0)
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		return writeLikeDownloadToPath(relPath, nil)
	}

	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"message_id":"EMPTY1","chat_jid":%q}`, mediaTestChat)
	b.newRESTMux(8080, token).ServeHTTP(rec, downloadRequest(token, body))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("an empty file must download: %d %s", rec.Code, rec.Body.String())
	}
}

// A message that names its media by direct path alone is stored as it came:
// the path, and no url invented for it. The row says what the message said,
// which is what the refusal log of #452 reads.
func TestExtractMessageKeepsADirectPathOnlyMessageAsItCame(t *testing.T) {
	ts := time.Now()
	pathOnly := fixtureDocument()
	pathOnly.URL = nil
	ex := extractMessage(&waE2E.Message{DocumentMessage: pathOnly}, ts, "P1")
	if ex.directPath != fixtureDirectPath || ex.url != "" {
		t.Errorf("direct path only: url %q, path %q, want the path and no url", ex.url, ex.directPath)
	}
	if !mediaComplete(ex.url, ex.directPath, ex.mediaKey, ex.fileSHA, ex.fileEnc) {
		t.Errorf("a message with a direct path, a key and both hashes is complete")
	}
}

// liveMediaBridge is a bridge that caches media on arrival through the real
// downloadMedia, with the transfer faked: the tests below are about which
// messages reach it.
func liveMediaBridge(t *testing.T) (*Bridge, *MessageStore, *recordingLogger, *atomic.Int32, func() []string) {
	t.Helper()
	t.Setenv("WEBHOOK_ENABLED", "false")
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newConcurrentTestStore(t)
	log := installRecordingLogger(t)
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, log)
	b.MediaAutoDownload = true
	var transfers atomic.Int32
	var mu sync.Mutex
	var asked []string
	b.mediaTransfer = func(_ context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		mu.Lock()
		asked = append(asked, msg.GetDirectPath())
		mu.Unlock()
		n, err := writeLikeDownloadToPath(relPath, nil)
		transfers.Add(1)
		return n, err
	}
	return b, ms, log, &transfers, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// Live, a message that names only its direct path, and an empty file, go all
// the way to the transfer: neither is held back as "not downloadable", by the
// automatic download or by the gate behind it.
func TestHandleMessageCachesDirectPathOnlyAndEmptyMedia(t *testing.T) {
	pathOnly := fixtureDocument()
	pathOnly.URL = nil
	empty := fixtureDocument()
	empty.FileLength = proto.Uint64(0)

	for name, doc := range map[string]*waE2E.DocumentMessage{"direct path only": pathOnly, "empty file, no cap set": empty} {
		t.Run(name, func(t *testing.T) {
			b, ms, _, transfers, asked := liveMediaBridge(t)
			msg := buildImageMessage(phonePN, phonePN, false, "")
			msg.Info.ID = "AUTO1"
			msg.Message = &waE2E.Message{DocumentMessage: doc}
			b.handleMessage(msg)
			awaitCount(t, "transfers", 1, transfers.Load)
			if got := asked(); len(got) != 1 || got[0] != fixtureDirectPath {
				t.Errorf("transfer asked for %q, want the message's direct path", got)
			}
			var url string
			if err := ms.db.QueryRow("SELECT COALESCE(url, '') FROM messages WHERE id = ?", "AUTO1").Scan(&url); err != nil || url != doc.GetURL() {
				t.Errorf("stored url = %q (%v), want what the message carried: %q", url, err, doc.GetURL())
			}
			if got := storedDirectPath(t, ms, "AUTO1", phonePN.String()); got.String != fixtureDirectPath {
				t.Errorf("stored direct path = %q", got.String)
			}
		})
	}
}

// What is not fetched unasked: a message with no length to hold against
// WHATSAPP_MEDIA_MAX_BYTES (an empty file, or a sender that did not say), and a
// message that lacks something a download needs. Both rows are stored.
func TestHandleMessageDoesNotAutoDownloadWhatItCannotCheck(t *testing.T) {
	noLength := fixtureDocument()
	noLength.FileLength = nil
	empty := fixtureDocument()
	empty.FileLength = proto.Uint64(0)
	noHash := fixtureDocument()
	noHash.FileEncSHA256 = nil

	cases := []struct {
		name    string
		doc     *waE2E.DocumentMessage
		cap     uint64
		wantLog string
	}{
		{"no length declared, cap set", noLength, 1 << 20, "no length declared to check against WHATSAPP_MEDIA_MAX_BYTES=1048576"},
		{"length 0, cap set", empty, 1 << 20, "no length declared to check against WHATSAPP_MEDIA_MAX_BYTES=1048576"},
		{"no encrypted hash", noHash, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ms, log, transfers, _ := liveMediaBridge(t)
			b.MediaMaxBytes = tc.cap
			msg := buildImageMessage(phonePN, phonePN, false, "")
			msg.Info.ID = "HELD1"
			msg.Message = &waE2E.Message{DocumentMessage: tc.doc}
			b.handleMessage(msg)
			// Queuing happens inside handleMessage: nothing queued, nothing running.
			time.Sleep(50 * time.Millisecond)
			if b.autoDownloads.queued() != 0 || b.autoDownloads.running() != 0 || transfers.Load() != 0 {
				t.Errorf("a download was started: queued %d, running %d, transfers %d", b.autoDownloads.queued(), b.autoDownloads.running(), transfers.Load())
			}
			if tc.wantLog != "" && !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("log lacks %q:\n%s", tc.wantLog, log.String())
			}
			if strings.Contains(log.String(), "Auto-download failed") {
				t.Errorf("nothing should have been tried:\n%s", log.String())
			}
			var mediaType string
			if err := ms.db.QueryRow("SELECT media_type FROM messages WHERE id = ?", "HELD1").Scan(&mediaType); err != nil || mediaType != "document" {
				t.Errorf("the row must be stored all the same: %q (%v)", mediaType, err)
			}
		})
	}
}
