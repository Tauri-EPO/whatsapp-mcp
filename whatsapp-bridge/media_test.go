package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

const mediaTestChat = "5511999999999@s.whatsapp.net"

// seedMediaRow stores one message row with the given media columns and
// returns the timestamp used, so tests can rebuild the expected filename.
func seedMediaRow(t *testing.T, ms *MessageStore, id, mediaType, url string, key, sha, encSHA []byte, length uint64) time.Time {
	t.Helper()
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	if err := ms.StoreChat(mediaTestChat, "Test", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(id, mediaTestChat, "5511999999999", "caption", ts, false, mediaType, "", url, key, sha, encSHA, length, ""); err != nil {
		t.Fatal(err)
	}
	return ts
}

func fullMediaInfo() (string, []byte, []byte, []byte, uint64) {
	return "https://mmg.whatsapp.net/v/t62.7118-24/abc_n.enc?ccb=11-4&oh=x&oe=y", []byte("key"), []byte("sha"), []byte("enc"), 1234
}

func TestDownloadMedia_Errors(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, installRecordingLogger(t))

	if _, _, _, _, err := b.downloadMedia(context.Background(), "missing", mediaTestChat); err == nil || !strings.Contains(err.Error(), "failed to find message") {
		t.Errorf("unknown message: err = %v", err)
	}

	seedMediaRow(t, ms, "text-only", "", "", nil, nil, nil, 0)
	if _, _, _, _, err := b.downloadMedia(context.Background(), "text-only", mediaTestChat); err == nil || !strings.Contains(err.Error(), "not a media message") {
		t.Errorf("text message: err = %v", err)
	}

	// Media row whose CDN fields were never captured (old rows, view-once
	// placeholders): refuse before touching the network.
	seedMediaRow(t, ms, "no-keys", "image", "https://mmg.whatsapp.net/v/x", nil, nil, nil, 0)
	if _, _, _, _, err := b.downloadMedia(context.Background(), "no-keys", mediaTestChat); err == nil || !strings.Contains(err.Error(), "incomplete media information") {
		t.Errorf("incomplete row: err = %v", err)
	}

	url, key, sha, enc, n := fullMediaInfo()
	seedMediaRow(t, ms, "odd-type", "hologram", url, key, sha, enc, n)
	if _, _, _, _, err := b.downloadMedia(context.Background(), "odd-type", mediaTestChat); err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Errorf("unsupported type: err = %v", err)
	}

	// Complete row but no WhatsApp client: the download itself fails and the
	// failure counter moves; no .part file is left behind.
	seedMediaRow(t, ms, "img-1", "image", url, key, sha, enc, n)
	ok, _, _, _, err := b.downloadMedia(context.Background(), "img-1", mediaTestChat)
	if ok || err == nil || !strings.Contains(err.Error(), "failed to download media") {
		t.Errorf("download failure: ok=%v err=%v", ok, err)
	}
	if got := b.metrics.mediaDownloadFails.Load(); got != 1 {
		t.Errorf("mediaDownloadFails = %d, want 1", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(storePath(mediaTestChat), "*.part"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestDownloadMedia_ReturnsCachedFileWithoutClient(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, installRecordingLogger(t))

	// Filenames must match what extractMediaInfo produces on arrival:
	// <type>_<yyyymmdd_hhmmss>_<message id><ext>.
	cases := map[string]string{"image": ".jpg", "video": ".mp4", "audio": ".ogg", "sticker": ".webp", "document": ""}
	for mediaType, ext := range cases {
		id := "cached-" + mediaType
		ts := seedMediaRow(t, ms, id, mediaType, "", nil, nil, nil, 0) // no CDN info: only the cache can serve it
		want := mediaType + "_" + ts.Format("20060102_150405") + "_" + id + ext
		dir := storePath(mediaTestChat)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, want), []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}

		ok, gotType, gotName, gotPath, err := b.downloadMedia(context.Background(), id, mediaTestChat)
		if err != nil || !ok || gotType != mediaType || gotName != want {
			t.Errorf("%s: ok=%v type=%q name=%q err=%v (want name %q)", mediaType, ok, gotType, gotName, err, want)
		}
		if !filepath.IsAbs(gotPath) || filepath.Base(gotPath) != want {
			t.Errorf("%s: path = %q, want absolute path ending in %q", mediaType, gotPath, want)
		}
	}
	if got := b.metrics.mediaDownloads.Load(); got != 0 {
		t.Errorf("cache hits must not count as downloads, got %d", got)
	}
}

func TestDownloadMedia_ChatDirSanitisesColons(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, installRecordingLogger(t))
	chat := "5511999999999:12@s.whatsapp.net" // device-suffixed JID
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	if err := ms.StoreChat(chat, "", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("m", chat, "x", "", ts, false, "image", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _ = b.downloadMedia(context.Background(), "m", chat)
	if _, err := os.Stat(storePath("5511999999999_12@s.whatsapp.net")); err != nil {
		t.Errorf("chat directory with ':' replaced by '_' not created: %v", err)
	}
}

func downloadRequest(token, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/download", strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleDownload(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(nil, newTestMessageStore(t), installRecordingLogger(t))
	mux := b.newRESTMux(8080, token)
	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, downloadRequest(token, body))
		return rec
	}

	// Not connected: 503 with the JSON envelope, before the body is parsed.
	b.Connected = func() bool { return false }
	if rec := do(`{"message_id":"m","chat_jid":"c@s.whatsapp.net"}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not connected") {
		t.Errorf("disconnected: %d %s", rec.Code, rec.Body.String())
	}

	b.Connected = func() bool { return true }
	if rec := do(`{bad json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(`{"message_id":"m"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "required") {
		t.Errorf("missing chat_jid: %d %s", rec.Code, rec.Body.String())
	}

	var gotID, gotChat string
	b.DownloadMedia = func(ctx context.Context, messageID, chatJID string) (bool, string, string, string, error) {
		gotID, gotChat = messageID, chatJID
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("download context must carry a deadline")
		}
		return false, "", "", "", errors.New("CDN says 410")
	}
	rec := do(`{"message_id":"m1","chat_jid":"c@s.whatsapp.net"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "CDN says 410") {
		t.Errorf("download failure: %d %s", rec.Code, rec.Body.String())
	}
	if gotID != "m1" || gotChat != "c@s.whatsapp.net" {
		t.Errorf("handler passed (%q, %q) to DownloadMedia", gotID, gotChat)
	}

	b.DownloadMedia = func(ctx context.Context, messageID, chatJID string) (bool, string, string, string, error) {
		return true, "image", "image_20260904_150405_m1.jpg", "/app/store/c@s.whatsapp.net/image_20260904_150405_m1.jpg", nil
	}
	rec = do(`{"message_id":"m1","chat_jid":"c@s.whatsapp.net"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("success: %d %s", rec.Code, rec.Body.String())
	}
	var resp DownloadMediaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.Filename != "image_20260904_150405_m1.jpg" || !strings.HasSuffix(resp.Path, "m1.jpg") || !strings.Contains(resp.Message, "image") {
		t.Errorf("response = %+v", resp)
	}
}

func TestMediaDownloader_ImplementsDownloadableMessage(t *testing.T) {
	d := &MediaDownloader{URL: "u", DirectPath: "/v/p", MediaKey: []byte("k"), FileLength: 9, FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), MediaType: whatsmeow.MediaImage}
	if d.GetURL() != "u" || d.GetDirectPath() != "/v/p" || string(d.GetMediaKey()) != "k" || d.GetFileLength() != 9 || string(d.GetFileSHA256()) != "s" || string(d.GetFileEncSHA256()) != "e" || d.GetMediaType() != whatsmeow.MediaImage {
		t.Errorf("accessors do not return the stored fields: %+v", d)
	}
}

func TestMediaFileName_DocumentExtension(t *testing.T) {
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	cases := map[string]string{
		"Report Q3.pdf":          ".pdf",
		"contract.DOCX":          ".docx",
		"archive.tar.gz":         ".gz",
		"noext":                  "",
		"":                       "",
		"evil.p/df":              "", // separators never reach the disk
		"x.abcdefghijklm":        "", // longer than 10 chars: not an extension
		"weird.ex\u00e9":         "", // non-ASCII
		"trailing space.pdf   ":  ".pdf",
		"../../../../etc/passwd": "",
		"photo.jpg":              ".jpg", // a document can carry any type
	}
	for name, ext := range cases {
		got := mediaFileName("document", ts, "ABC", name)
		if got != "document_20260904_150405_ABC"+ext {
			t.Errorf("%q: got %q, want ext %q", name, got, ext)
		}
	}
	// Other types ignore the original name.
	if got := mediaFileName("image", ts, "ABC", "photo.png"); got != "image_20260904_150405_ABC.jpg" {
		t.Errorf("image: %q", got)
	}
	if got := legacyMediaFileName("document", ts, "ABC"); got != "document_20260904_150405_ABC" {
		t.Errorf("legacy: %q", got)
	}
}

func TestDownloadMedia_DocumentsUseAndFallBackOnLegacyName(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, installRecordingLogger(t))
	ts := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	if err := ms.StoreChat(mediaTestChat, "Test", ts); err != nil {
		t.Fatal(err)
	}
	seed := func(id string) {
		if err := ms.StoreMessage(id, mediaTestChat, "x", "", ts, false, "document", "Report Q3.pdf", "", nil, nil, nil, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	dir := storePath(mediaTestChat)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	// New shape: the cached file carries the sender's extension.
	seed("NEW")
	if err := os.WriteFile(filepath.Join(dir, "document_20260904_150405_NEW.pdf"), []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, _, name, path, err := b.downloadMedia(context.Background(), "NEW", mediaTestChat)
	if err != nil || !ok || name != "document_20260904_150405_NEW.pdf" || filepath.Base(path) != name {
		t.Errorf("new name: ok=%v name=%q path=%q err=%v", ok, name, path, err)
	}

	// Legacy shape: cached before documents kept an extension; still served.
	seed("OLD")
	if err := os.WriteFile(filepath.Join(dir, "document_20260904_150405_OLD"), []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, _, name, path, err = b.downloadMedia(context.Background(), "OLD", mediaTestChat)
	if err != nil || !ok || name != "document_20260904_150405_OLD" || filepath.Base(path) != name {
		t.Errorf("legacy name: ok=%v name=%q path=%q err=%v", ok, name, path, err)
	}

	// Purge finds the legacy file too.
	res := purgeOne(mediaRow{ID: "OLD", ChatJID: mediaTestChat, MediaType: "document", Timestamp: ts, Filename: "Report Q3.pdf"}, false)
	if !res.Purged || res.File != "document_20260904_150405_OLD" {
		t.Errorf("purge legacy: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "document_20260904_150405_OLD")); !os.IsNotExist(err) {
		t.Errorf("legacy file still there")
	}
}
