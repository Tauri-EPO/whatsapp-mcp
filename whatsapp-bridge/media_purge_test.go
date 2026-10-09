package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	old := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	recent := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, chat := range []string{purgeChat, purgeGroup} {
		if err := ms.StoreChat(chat, "", recent); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(id, chat, mediaType string, ts time.Time, length uint64) {
		if err := ms.StoreMessage(storedMessage{
			ID:            id,
			ChatJID:       chat,
			Sender:        "x",
			Timestamp:     ts,
			MediaType:     mediaType,
			URL:           "u",
			MediaKey:      []byte("k"),
			FileSHA256:    []byte("s"),
			FileEncSHA256: []byte("e"),
			FileLength:    length,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("OLDVID", purgeChat, "video", old, 5_000_000)
	seed("NEWIMG", purgeChat, "image", recent, 200_000)
	seed("GRPDOC", purgeGroup, "document", old, 50_000)
	if err := ms.StoreMessage(storedMessage{
		ID:         "TXT",
		ChatJID:    purgeChat,
		Sender:     "x",
		Content:    "hello",
		Timestamp:  recent,
		FileLength: 0,
	}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	write := func(id, chat, mediaType string, ts time.Time, size int) {
		dir := chatMediaDir(chat)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, mediaFileName(mediaType, ts, id, ""))
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
	if code != http.StatusOK || resp.DryRun || resp.PurgedFiles != 1 || resp.PurgedBytes != 4096 || resp.Matched != 1 {
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
	code, resp = purgeCall(t, b, `{"dry_run": false, "chat_jid": "`+purgeChat+`", "media_type": "video", "min_bytes": 4096}`)
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
	dir := t.TempDir()
	t.Setenv(storeDirEnv, dir)
	root := storeRootAt(t, dir)
	res := purgeOne(root, mediaRow{ID: "X", ChatJID: "../../etc", MediaType: "image", Timestamp: time.Now()}, false)
	if res.Purged || res.Reason != "path outside the store directory" {
		t.Errorf("result = %+v", res)
	}
	res = purgeOne(root, mediaRow{ID: "X", ChatJID: "c@s.whatsapp.net", MediaType: "reaction"}, false)
	if res.Reason != "not a media message" {
		t.Errorf("reaction = %+v", res)
	}
	// No store root at all (the directory could not be opened at startup).
	res = purgeOne(nil, mediaRow{ID: "X", ChatJID: purgeChat, MediaType: "image", Timestamp: time.Now()}, false)
	if res.Purged || res.Reason != "store directory unavailable" {
		t.Errorf("nil root = %+v", res)
	}
}

// A cached name that is a symlink out of the store resolves outside the root,
// so os.Root refuses it and the purge says so — before this change os.Stat
// followed the link and reported the row as purged. Neither the link nor its
// target is touched.
func TestPurgeOne_RefusesSymlinkedCacheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(storeDirEnv, dir)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.bin")
	if err := os.WriteFile(secret, []byte("secret-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	chat := chatMediaDir(purgeChat)
	if err := os.MkdirAll(chat, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(chat, mediaFileName("image", ts, "LINK", ""))
	symlinkOrSkip(t, secret, link)

	res := purgeOne(storeRootAt(t, dir), mediaRow{ID: "LINK", ChatJID: purgeChat, MediaType: "image", Timestamp: ts}, false)
	if res.Purged || res.Reason != purgeReasonNotResolvable {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("the purge followed the symlink out of the store: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the symlink itself should be left in place: %v", err)
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

// criteriaFixture is a bridge with its own store directory and no seeded rows;
// seedPurgeRows adds them.
func criteriaFixture(t *testing.T) *Bridge {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	return testBridge(t, nil, ms, installRecordingLogger(t))
}

// seedPurgeRows stores n image rows in chat, one second apart from base, and
// writes the cached file of each when cached is true. It returns the ids.
func seedPurgeRows(t *testing.T, b *Bridge, chat, prefix string, n int, base time.Time, cached bool) []string {
	t.Helper()
	if err := b.Store.StoreChat(chat, "", base); err != nil {
		t.Fatal(err)
	}
	dir := chatMediaDir(chat)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s%04d", prefix, i)
		ts := base.Add(time.Duration(i) * time.Second)
		if err := b.Store.StoreMessage(storedMessage{
			ID:            id,
			ChatJID:       chat,
			Sender:        "x",
			Timestamp:     ts,
			MediaType:     "image",
			URL:           "u",
			MediaKey:      []byte("k"),
			FileSHA256:    []byte("s"),
			FileEncSHA256: []byte("e"),
			FileLength:    1024,
		}); err != nil {
			t.Fatal(err)
		}
		if cached {
			if err := os.WriteFile(filepath.Join(dir, mediaFileName("image", ts, id, "")), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	return ids
}

func cachedCount(t *testing.T, chat string) int {
	t.Helper()
	entries, err := os.ReadDir(chatMediaDir(chat))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func purgedIDs(resp MediaPurgeResponse) []string {
	var ids []string
	for _, it := range resp.Items {
		if it.Purged {
			ids = append(ids, it.MessageID)
		}
	}
	return ids
}

// Issue #445: the criteria form took the first 500 matching rows whether or not
// their file was still cached, so once those were purged the same call matched
// the same rows again and removed nothing. 300 uncached rows sit in front of
// 1200 cached ones; identical calls must walk past them, drain the chat in
// three real calls and then say there is nothing left.
func TestMediaPurge_CriteriaConvergesPastTheCap(t *testing.T) {
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "GONE", 300, base, false)
	seedPurgeRows(t, b, purgeChat, "KEEP", 1200, base.Add(time.Hour), true)
	body := `{"chat_jid": "` + purgeChat + `", "media_type": "image", "dry_run": false}`
	dry := `{"chat_jid": "` + purgeChat + `", "media_type": "image"}`

	// dry_run reports the set the next real call removes.
	_, preview := purgeCall(t, b, dry)
	if !preview.DryRun || preview.Matched != 500 || preview.PurgedFiles != 500 || preview.Remaining != -1 || preview.NextCursor == "" || preview.Examined != 800 || !preview.Truncated || cachedCount(t, purgeChat) != 1200 {
		t.Fatalf("dry run = %+v", preview)
	}

	wantPurged := []int{500, 500, 200}
	wantRemaining := []int{-1, -1, 0}
	cursor := ""
	for i := range wantPurged {
		callBody := strings.TrimSuffix(body, "}") + `,"cursor":"` + cursor + `"}`
		code, resp := purgeCall(t, b, callBody)
		if code != http.StatusOK || resp.PurgedFiles != wantPurged[i] || resp.Matched != wantPurged[i] || resp.Remaining != wantRemaining[i] || resp.Truncated != (wantRemaining[i] != 0) || resp.ScanTruncated {
			t.Fatalf("call %d: %d %+v", i+1, code, resp)
		}
		if i == 0 {
			got, want := purgedIDs(resp), purgedIDs(preview)
			if !slices.Equal(got, want) {
				t.Errorf("the real call removed a different set than its dry run (%d vs %d ids)", len(got), len(want))
			}
			if !strings.Contains(resp.Message, "cursor=next_cursor") {
				t.Errorf("message does not explain continuation: %q", resp.Message)
			}
		}
		cursor = resp.NextCursor
	}
	code, resp := purgeCall(t, b, body)
	if code != http.StatusOK || resp.Matched != 0 || resp.PurgedFiles != 0 || resp.Remaining != 0 || resp.Truncated || resp.ScanTruncated || len(resp.Items) != 0 {
		t.Fatalf("fourth call = %d %+v", code, resp)
	}
	if n := cachedCount(t, purgeChat); n != 0 {
		t.Errorf("%d files still cached", n)
	}
	var rows int
	if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, purgeChat).Scan(&rows); err != nil || rows != 1500 {
		t.Errorf("rows = %d, %v; the purge must keep every row", rows, err)
	}
}

// The scan has a ceiling so a huge archive cannot turn one call into an
// unbounded walk of the disk; reaching it is reported, not hidden behind a
// plain "nothing matched".
func TestMediaPurge_CriteriaScanIsBounded(t *testing.T) {
	b := criteriaFixture(t)
	b.PurgeScanLimit = 100
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "GONE", 250, base, false)
	seedPurgeRows(t, b, purgeChat, "KEEP", 10, base.Add(time.Hour), true)

	_, resp := purgeCall(t, b, `{"chat_jid": "`+purgeChat+`", "dry_run": false}`)
	if resp.PurgedFiles != 0 || !resp.ScanTruncated || !resp.Truncated || resp.Examined != 100 || resp.NextCursor == "" || !strings.Contains(resp.Message, "cursor=next_cursor") {
		t.Fatalf("scan limit not reported: %+v", resp)
	}
	// With the default ceiling the same call walks past the uncached rows.
	b.PurgeScanLimit = 0
	_, resp = purgeCall(t, b, `{"chat_jid": "`+purgeChat+`", "dry_run": false}`)
	if resp.PurgedFiles != 10 || resp.ScanTruncated || resp.Truncated {
		t.Fatalf("default limit = %+v", resp)
	}
}

// A cached name the store root refuses is neither purged nor a slot in the
// budget: it is counted and listed so the operator sees why it stays, and the
// files behind it are still reached.
func TestMediaPurge_CriteriaCountsUnreachableRows(t *testing.T) {
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedPurgeRows(t, b, purgeChat, "ROW", 3, base, true)
	dir := chatMediaDir(purgeChat)
	linked := filepath.Join(dir, mediaFileName("image", base, ids[0], ""))
	outside := filepath.Join(t.TempDir(), "secret.bin")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(linked); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, linked)

	_, resp := purgeCall(t, b, `{"chat_jid": "`+purgeChat+`", "dry_run": false}`)
	if resp.PurgedFiles != 2 || resp.Matched != 2 || resp.Unreachable != 1 || resp.Truncated {
		t.Fatalf("response = %+v", resp)
	}
	var listed bool
	for _, it := range resp.Items {
		if it.MessageID == ids[0] && !it.Purged && it.Reason == purgeReasonNotResolvable {
			listed = true
		}
	}
	if !listed {
		t.Errorf("the refused row is not explained in items: %+v", resp.Items)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the purge followed the symlink out of the store: %v", err)
	}
}

// Deny path: rows of a chat outside WHATSAPP_ALLOWED_CHATS are not examined,
// not counted in remaining and their files are never touched, however many of
// them match.
func TestMediaPurge_CriteriaRespectsAllowListInRemaining(t *testing.T) {
	b := criteriaFixture(t)
	b.Policy = parseChatPolicy(purgeChat)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "OK", 520, base, true)
	seedPurgeRows(t, b, purgeGroup, "NO", 600, base, true)

	_, resp := purgeCall(t, b, `{"media_type": "image", "dry_run": false}`)
	if resp.PurgedFiles != 500 || resp.Remaining != -1 || !resp.Truncated || resp.NextCursor == "" {
		t.Fatalf("first call = %+v", resp)
	}
	_, resp = purgeCall(t, b, `{"media_type": "image", "dry_run": false, "cursor":"`+resp.NextCursor+`"}`)
	if resp.PurgedFiles != 20 || resp.Remaining != 0 || resp.Truncated {
		t.Fatalf("second call = %+v", resp)
	}
	if n := cachedCount(t, purgeGroup); n != 600 {
		t.Errorf("denied chat lost files: %d left of 600", n)
	}
	if code, _ := purgeCall(t, b, `{"chat_jid": "`+purgeGroup+`", "dry_run": false}`); code != http.StatusForbidden {
		t.Errorf("denied chat_jid = %d, want 403", code)
	}
}

// A selected file the real call cannot remove stays cached and would head the
// next call again; that must not read as "repeat while truncated".
func TestMediaPurge_CriteriaFailedRemovalIsNotAnEndlessLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block deletes on Windows")
	}
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "RO", 3, base, true)
	dir := chatMediaDir(purgeChat)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory needs the x bit; read-only is what makes the removal fail
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore the directory so t.TempDir can delete it
	// Running as root, permissions do not apply and the removal would succeed.
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("directory permissions are not enforced for this user")
	}

	_, resp := purgeCall(t, b, `{"chat_jid": "`+purgeChat+`", "dry_run": false}`)
	if resp.PurgedFiles != 0 || resp.Failed != 3 || resp.Truncated || !strings.Contains(resp.Message, "could not be removed") {
		t.Fatalf("response = %+v", resp)
	}
}

// Rows of a denied chat are not probed, so they do not use the scan ceiling.
func TestMediaPurge_CriteriaDeniedRowsDoNotUseTheScanBudget(t *testing.T) {
	b := criteriaFixture(t)
	b.PurgeScanLimit = 50
	b.Policy = parseChatPolicy(purgeChat)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeGroup, "NO", 200, base, true)
	seedPurgeRows(t, b, purgeChat, "OK", 5, base.Add(time.Hour), true)

	_, resp := purgeCall(t, b, `{"media_type": "image", "dry_run": false}`)
	if resp.PurgedFiles != 5 || resp.ScanTruncated || resp.Truncated {
		t.Fatalf("response = %+v", resp)
	}
}

// Without a store directory every probe would fail the same way; say so once.
func TestMediaPurge_CriteriaWithoutStoreRootFailsFast(t *testing.T) {
	b := criteriaFixture(t)
	seedPurgeRows(t, b, purgeChat, "ROW", 3, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true)
	b.StoreRoot = nil
	code, resp := purgeCall(t, b, `{"chat_jid": "`+purgeChat+`", "dry_run": false}`)
	if code != http.StatusInternalServerError || resp.Success || !strings.Contains(resp.Message, "Store directory unavailable") {
		t.Fatalf("%d %+v", code, resp)
	}
}
