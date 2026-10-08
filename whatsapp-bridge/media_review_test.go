package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPurgeHTTPFinderRefusesSameChatLink(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			b := criteriaFixture(t)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			id := seedPurgeRows(t, b, purgeChat, "LINK", 1, base, false)[0]
			dir := chatMediaDir(purgeChat)
			name := mediaFileName("image", base, id, "")
			writeTestFile(t, filepath.Join(dir, "personal.txt"), "sibling bytes")
			symlinkOrSkip(t, "personal.txt", filepath.Join(dir, name))
			req := MediaPurgeRequest{ChatJID: purgeChat, DryRun: new(false)}
			if explicit {
				req.Items = []PurgeItem{{MessageID: id, ChatJID: purgeChat}}
			}
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			code, resp := purgeCall(t, b, string(body))
			if code != http.StatusOK || resp.Matched != 0 || resp.PurgedFiles != 0 || resp.PurgedBytes != 0 || len(resp.Items) != 1 || resp.Items[0].Reason != purgeReasonNotResolvable {
				t.Fatalf("status=%d response=%+v", code, resp)
			}
			if info, err := b.StoreRoot.Lstat(filepath.ToSlash(filepath.Join(purgeChat, name))); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("link changed: %v %v", info, err)
			}
			if data, err := b.StoreRoot.ReadFile(purgeChat + "/personal.txt"); err != nil || string(data) != "sibling bytes" {
				t.Fatalf("sibling changed: %q %v", data, err)
			}
		})
	}
}

func TestRetentionUsageIgnoreUserFilesAndPartialDownloads(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	if err := os.MkdirAll(chatMediaDir(purgeChat), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cache := mediaFileName("image", now.Add(-48*time.Hour), "CACHE1", "")
	names := []string{cache, "personal.txt", cache + ".part", "image_invalid_timestamp_ID.jpg"}
	for _, name := range names {
		file := filepath.Join(chatMediaDir(purgeChat), name)
		writeTestFile(t, file, "bytes")
		old := now.Add(-48 * time.Hour)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	root := storeRootAt(t, storeDir())
	store, media, files := storeUsage(root)
	if store != 20 || media != 5 || files != 1 {
		t.Fatalf("usage=%d/%d/%d", store, media, files)
	}
	removed, freed, failed := sweepMedia(root, 24*time.Hour, now)
	if removed != 1 || freed != 5 || failed != 0 {
		t.Fatalf("sweep=%d/%d/%d", removed, freed, failed)
	}
	for _, name := range names[1:] {
		if data, err := root.ReadFile(purgeChat + "/" + name); err != nil || string(data) != "bytes" {
			t.Fatalf("non-cache file changed: %q %v", data, err)
		}
	}
}

func TestPurgeExplicitRequestBounds(t *testing.T) {
	b := criteriaFixture(t)
	for _, count := range []int{1000, 1001} {
		items := make([]PurgeItem, count)
		for i := range items {
			items[i] = PurgeItem{MessageID: fmt.Sprintf("ABSENT%d", i), ChatJID: purgeChat}
		}
		body, err := json.Marshal(MediaPurgeRequest{Items: items})
		if err != nil {
			t.Fatal(err)
		}
		code, resp := purgeCall(t, b, string(body))
		if count == 1000 && (code != http.StatusOK || resp.Examined != count || len(resp.Items) != count) {
			t.Fatalf("boundary=%d %+v", code, resp)
		}
		if count == 1001 && (code != http.StatusBadRequest || resp.Examined != 0 || !strings.Contains(resp.Message, "1000")) {
			t.Fatalf("over limit: status=%d examined=%d message=%q", code, resp.Examined, resp.Message)
		}
	}
	body := `{"items":[{"message_id":"` + strings.Repeat("x", 1024*1024) + `","chat_jid":"` + purgeChat + `"}]}`
	code, resp := purgeCall(t, b, body)
	if code != http.StatusBadRequest || resp.Examined != 0 || !strings.Contains(resp.Message, "1048576") {
		t.Fatalf("oversized body: status=%d examined=%d message=%q", code, resp.Examined, resp.Message)
	}
}

func TestPurgeExplicitDuplicateIsReported(t *testing.T) {
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id := seedPurgeRows(t, b, purgeChat, "DUP", 1, base, true)[0]
	item := PurgeItem{MessageID: id, ChatJID: purgeChat}
	body, err := json.Marshal(MediaPurgeRequest{Items: []PurgeItem{item, item}, DryRun: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := purgeCall(t, b, string(body))
	if code != http.StatusOK || resp.Matched != 1 || resp.PurgedFiles != 1 || resp.Failed != 0 || resp.Examined != 2 || len(resp.Items) != 2 {
		t.Fatalf("status=%d response=%+v", code, resp)
	}
	duplicates := 0
	for _, result := range resp.Items {
		if result.Reason == "duplicate" && !result.Purged {
			duplicates++
		}
	}
	if duplicates != 1 {
		t.Fatalf("duplicates=%d", duplicates)
	}
}

func TestPurgeExplicitFailedRemovalIsCounted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block deletes on Windows")
	}
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedPurgeRows(t, b, purgeChat, "FAIL", 3, base, true)
	dir := chatMediaDir(purgeChat)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // deny writes but keep directory lookup
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore fixture directory
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("directory permissions are not enforced for this user")
	}
	items := make([]PurgeItem, len(ids))
	for i, id := range ids {
		items[i] = PurgeItem{MessageID: id, ChatJID: purgeChat}
	}
	body, err := json.Marshal(MediaPurgeRequest{Items: items, DryRun: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := purgeCall(t, b, string(body))
	if code != http.StatusOK || resp.Matched != 3 || resp.PurgedFiles != 0 || resp.Failed != 3 || resp.Truncated || len(resp.Items) != 3 || !strings.Contains(resp.Message, "could not be removed") {
		t.Fatalf("status=%d response=%+v", code, resp)
	}
	for _, item := range resp.Items {
		if item.Purged || !strings.HasPrefix(item.Reason, "remove failed:") {
			t.Fatalf("missing removal failure: %+v", item)
		}
	}
	if cachedCount(t, purgeChat) != 3 {
		t.Fatal("failed files must remain cached")
	}
}

func TestPartDocumentCacheLifecycle(t *testing.T) {
	storeInScratch(t)
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	ts := time.Now().Truncate(time.Second)
	name := mediaFileName("document", ts, "PART1", "model.part")
	if !strings.HasSuffix(name, "_PART1.part.bin") {
		t.Fatalf("ambiguous cache name: %q", name)
	}
	url, key, sha, enc, length := fullMediaInfo()
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("PART1", mediaTestChat, "x", "document", ts, false, "document", "model.part", url, key, sha, enc, length, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRoot.MkdirAll(mediaTestChat, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := strings.TrimSuffix(name, ".bin")
	writeTestFile(t, filepath.Join(chatMediaDir(mediaTestChat), legacy), "legacy bytes")
	var transfers atomic.Int32
	b.mediaTransfer = countingTransfer(&transfers)
	for i := 0; i < 2; i++ {
		ok, kind, found, _, err := b.downloadMedia(context.Background(), "PART1", mediaTestChat)
		if !ok || err != nil || kind != "document" || found != name {
			t.Fatalf("download%d: %v %s %s %v", i, ok, kind, found, err)
		}
	}
	if transfers.Load() != 1 {
		t.Fatalf("fetches=%d, want one download then zero on cache hit", transfers.Load())
	}
	writeTestFile(t, filepath.Join(chatMediaDir(mediaTestChat), name+".part"), "unfinished")
	_, media, files := storeUsage(b.StoreRoot)
	if media != int64(len("media bytes")) || files != 1 {
		t.Fatalf("completed and temp accounting=%d/%d", media, files)
	}
	if removed, _, failed := sweepMedia(b.StoreRoot, 24*time.Hour, time.Now()); removed != 0 || failed != 0 {
		t.Fatalf("fresh document swept: %d/%d", removed, failed)
	}
	if removed, freed, failed := sweepMedia(b.StoreRoot, 24*time.Hour, time.Now().Add(48*time.Hour)); removed != 1 || freed != int64(len("media bytes")) || failed != 0 {
		t.Fatalf("aged document sweep=%d/%d/%d", removed, freed, failed)
	}
	for _, untouched := range []string{legacy, name + ".part"} {
		if _, err := b.StoreRoot.Lstat(mediaTestChat + "/" + untouched); err != nil {
			t.Fatalf("legacy/temp must stay: %s %v", untouched, err)
		}
	}
}
