package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	purgeChat  = "5511999999999@s.whatsapp.net"
	purgeGroup = "120363012345678901@g.us"
	purgeToken = "test-token-0123456789"
)

// purgeFixture seeds three media rows (two cached) and one text row.
func purgeFixture(t *testing.T) (*Bridge, map[string]string) {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(nil, ms, installRecordingLogger(t))
	old := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	recent := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, chat := range []string{purgeChat, purgeGroup} {
		if err := ms.StoreChat(chat, "", recent); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(id, chat, mediaType string, ts time.Time, length uint64) {
		if err := ms.StoreMessage(id, chat, "x", "", ts, false, mediaType, "", "u", []byte("k"), []byte("s"), []byte("e"), length, ""); err != nil {
			t.Fatal(err)
		}
	}
	seed("OLDVID", purgeChat, "video", old, 5_000_000)
	seed("NEWIMG", purgeChat, "image", recent, 200_000)
	seed("GRPDOC", purgeGroup, "document", old, 50_000)
	if err := ms.StoreMessage("TXT", purgeChat, "x", "hello", recent, false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	write := func(id, chat, mediaType string, ts time.Time, size int) {
		dir := chatMediaDir(chat)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, mediaFileName(mediaType, ts, id))
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		files[id] = path
	}
	write("OLDVID", purgeChat, "video", old, 4096)
	write("GRPDOC", purgeGroup, "document", old, 512)
	return b, files
}

func purgeCall(t *testing.T, b *Bridge, body string) (int, MediaPurgeResponse) {
	t.Helper()
	mux := b.newRESTMux(8080, purgeToken)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/media/purge", strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+purgeToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp MediaPurgeResponse
	if rec.Code == http.StatusOK || rec.Code == http.StatusBadRequest || rec.Code == http.StatusInternalServerError {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
		}
	}
	return rec.Code, resp
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestMediaPurge_DryRunByDefaultRemovesNothing(t *testing.T) {
	b, files := purgeFixture(t)
	code, resp := purgeCall(t, b, `{"items":[{"message_id":"OLDVID","chat_jid":"`+purgeChat+`"}]}`)
	if code != http.StatusOK || !resp.Success || !resp.DryRun {
		t.Fatalf("%d %+v", code, resp)
	}
	if resp.PurgedFiles != 1 || resp.PurgedBytes != 4096 || resp.Items[0].File != filepath.Base(files["OLDVID"]) {
		t.Errorf("dry run report = %+v", resp)
	}
	if !fileExists(files["OLDVID"]) {
		t.Fatal("dry run removed the file")
	}
	if !strings.Contains(resp.Message, "dry_run=false") {
		t.Errorf("message should tell the caller how to really purge: %q", resp.Message)
	}
}

func TestMediaPurge_ItemsRemoveOnlyNamedFiles(t *testing.T) {
	b, files := purgeFixture(t)
	body := `{"dry_run": false, "items": [
		{"message_id":"OLDVID","chat_jid":"` + purgeChat + `"},
		{"message_id":"NEWIMG","chat_jid":"` + purgeChat + `"},
		{"message_id":"TXT","chat_jid":"` + purgeChat + `"},
		{"message_id":"NOPE","chat_jid":"` + purgeChat + `"},
		{"message_id":"","chat_jid":"` + purgeChat + `"}
	]}`
	code, resp := purgeCall(t, b, body)
	if code != http.StatusOK || resp.DryRun || resp.PurgedFiles != 1 || resp.PurgedBytes != 4096 || resp.Matched != 3 {
		t.Fatalf("%d %+v", code, resp)
	}
	byID := map[string]PurgeResult{}
	for _, item := range resp.Items {
		byID[item.MessageID] = item
	}
	if !byID["OLDVID"].Purged || byID["NEWIMG"].Reason != "not cached" || byID["TXT"].Reason != "not a media message" || byID["NOPE"].Reason != "message not found" || byID[""].Reason == "" {
		t.Errorf("items = %+v", resp.Items)
	}
	if fileExists(files["OLDVID"]) || !fileExists(files["GRPDOC"]) {
		t.Errorf("wrong files removed: OLDVID exists=%v GRPDOC exists=%v", fileExists(files["OLDVID"]), fileExists(files["GRPDOC"]))
	}
	// The row is untouched: download_media can rebuild the file later.
	if _, err := b.Store.MediaRow("OLDVID", purgeChat); err != nil {
		t.Errorf("row gone after purge: %v", err)
	}
	// Purging again reports "not cached" and no bytes.
	_, again := purgeCall(t, b, `{"dry_run": false, "items":[{"message_id":"OLDVID","chat_jid":"`+purgeChat+`"}]}`)
	if again.PurgedFiles != 0 || again.Items[0].Reason != "not cached" {
		t.Errorf("second purge = %+v", again)
	}
}

func TestMediaPurge_CriteriaForm(t *testing.T) {
	b, files := purgeFixture(t)
	// Older than 30 days: both old rows match, the cached ones are counted.
	code, resp := purgeCall(t, b, `{"older_than_days": 30}`)
	if code != http.StatusOK || resp.Matched != 2 || resp.PurgedFiles != 2 || resp.PurgedBytes != 4096+512 || !resp.DryRun {
		t.Fatalf("%d %+v", code, resp)
	}
	// Narrow by chat + type + size, real run.
	code, resp = purgeCall(t, b, `{"dry_run": false, "chat_jid": "`+purgeChat+`", "media_type": "video", "min_bytes": 1000000}`)
	if code != http.StatusOK || resp.Matched != 1 || resp.PurgedFiles != 1 || fileExists(files["OLDVID"]) || !fileExists(files["GRPDOC"]) {
		t.Fatalf("%d %+v", code, resp)
	}
	// Recent rows are not older than 30 days; nothing matches.
	_, resp = purgeCall(t, b, `{"older_than_days": 30, "media_type": "image"}`)
	if resp.Matched != 0 || len(resp.Items) != 0 {
		t.Errorf("image older than 30 days = %+v", resp)
	}
}

func TestMediaPurge_RejectsBadRequests(t *testing.T) {
	b, _ := purgeFixture(t)
	for name, body := range map[string]string{
		"invalid json":  `{`,
		"empty":         `{}`,
		"only dry_run":  `{"dry_run": false}`,
		"bad type":      `{"media_type": "hologram"}`,
		"negative days": `{"older_than_days": -1}`,
	} {
		if code, _ := purgeCall(t, b, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	// Wrong method is 405 through requireMethod.
	mux := b.newRESTMux(8080, purgeToken)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/media/purge", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+purgeToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	// No token is 401.
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/media/purge", strings.NewReader(`{"older_than_days": 1}`))
	req.Host = "127.0.0.1:8080"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}
}

func TestMediaPurge_AllowList(t *testing.T) {
	b, files := purgeFixture(t)
	b.Policy = parseChatPolicy(purgeChat)

	// Criteria over all chats silently skips denied ones.
	_, resp := purgeCall(t, b, `{"dry_run": false, "older_than_days": 30}`)
	if resp.Matched != 1 || resp.Items[0].MessageID != "OLDVID" || !fileExists(files["GRPDOC"]) {
		t.Errorf("criteria with policy = %+v", resp)
	}
	// Explicit denied chat is a 403; a denied item is reported, not deleted.
	if code, _ := purgeCall(t, b, `{"chat_jid": "`+purgeGroup+`"}`); code != http.StatusForbidden {
		t.Errorf("denied chat_jid = %d, want 403", code)
	}
	_, resp = purgeCall(t, b, `{"dry_run": false, "items":[{"message_id":"GRPDOC","chat_jid":"`+purgeGroup+`"}]}`)
	if resp.PurgedFiles != 0 || !strings.Contains(resp.Items[0].Reason, chatPolicyEnv) || !fileExists(files["GRPDOC"]) {
		t.Errorf("denied item = %+v", resp)
	}
}

func TestPurgeOne_RefusesPathsOutsideStore(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	res := purgeOne(mediaRow{ID: "X", ChatJID: "../../etc", MediaType: "image", Timestamp: time.Now()}, false)
	if res.Purged || res.Reason != "path outside the store directory" {
		t.Errorf("result = %+v", res)
	}
	res = purgeOne(mediaRow{ID: "X", ChatJID: "c@s.whatsapp.net", MediaType: "reaction"}, false)
	if res.Reason != "not a media message" {
		t.Errorf("reaction = %+v", res)
	}
}

func TestMediaPurge_InvalidatesStoreStats(t *testing.T) {
	b, _ := purgeFixture(t)
	before, mediaBefore, _ := b.storeStats.snapshot(time.Now())
	_, resp := purgeCall(t, b, `{"dry_run": false, "older_than_days": 30}`)
	if resp.PurgedFiles != 2 {
		t.Fatalf("%+v", resp)
	}
	after, mediaAfter, _ := b.storeStats.snapshot(time.Now())
	if after >= before || mediaAfter >= mediaBefore {
		t.Errorf("store stats not refreshed: %d/%d -> %d/%d", before, mediaBefore, after, mediaAfter)
	}
}
