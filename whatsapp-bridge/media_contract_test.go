package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPurgeItemsBudgetCountsCachedFiles(t *testing.T) {
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedPurgeRows(t, b, purgeChat, "ITEM", 600, base, false)
	items := make([]PurgeItem, 0, len(ids))
	for i, id := range ids {
		items = append(items, PurgeItem{MessageID: id, ChatJID: purgeChat})
		if i >= 500 {
			writeTestFile(t, filepath.Join(chatMediaDir(purgeChat), mediaFileName("image", base.Add(time.Duration(i)*time.Second), id, "")), "cached")
		}
	}
	body, err := json.Marshal(MediaPurgeRequest{Items: items, DryRun: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := purgeCall(t, b, string(body))
	if code != http.StatusOK || resp.Matched != 100 || resp.PurgedFiles != 100 || resp.Truncated || resp.Remaining != 0 || resp.Examined != 600 || len(resp.Items) != 600 {
		t.Fatalf("600 names, 100 cached: status=%d response=%+v", code, resp)
	}
	if cachedCount(t, purgeChat) != 0 {
		t.Fatal("cached tail was not removed")
	}
}

func TestPurgeMinBytesUsesActualCachedSize(t *testing.T) {
	b := criteriaFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedPurgeRows(t, b, purgeChat, "SIZE", 5, base, false)
	lengths := []any{nil, 0, 1, 9999999, 9999999}
	for i, id := range ids {
		if _, err := b.Store.db.Exec("UPDATE messages SET file_length=? WHERE id=? AND chat_jid=?", lengths[i], id, purgeChat); err != nil {
			t.Fatal(err)
		}
		if i == 4 {
			continue
		} // declared large, absent: never a purge candidate
		size := 2048
		if i == 3 {
			size = 8
		} // declared large, cached small: excluded
		writeTestFile(t, filepath.Join(chatMediaDir(purgeChat), mediaFileName("image", base.Add(time.Duration(i)*time.Second), id, "")), strings.Repeat("x", size))
	}
	body := `{"chat_jid":"` + purgeChat + `","min_bytes":1024}`
	_, preview := purgeCall(t, b, body)
	_, actual := purgeCall(t, b, strings.TrimSuffix(body, "}")+`,"dry_run":false}`)
	if preview.Matched != 3 || actual.Matched != 3 || actual.PurgedBytes != 3*2048 || fmt.Sprint(purgedIDs(preview)) != fmt.Sprint(purgedIDs(actual)) || cachedCount(t, purgeChat) != 1 {
		t.Fatalf("preview=%+v actual=%+v", preview, actual)
	}
}

func TestPurgeCursorContinuesPastUncachedPrefix(t *testing.T) {
	b := criteriaFixture(t)
	b.PurgeScanLimit = 100
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "PREFIX", 250, base, false)
	seedPurgeRows(t, b, purgeChat, "TAIL", 10, base.Add(time.Hour), true)
	cursor := ""
	for i := 0; i < 3; i++ {
		body, err := json.Marshal(MediaPurgeRequest{ChatJID: purgeChat, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		code, preview := purgeCall(t, b, string(body))
		if code != http.StatusOK {
			t.Fatalf("status=%d response=%+v", code, preview)
		}
		if i < 2 {
			if preview.PurgedFiles != 0 || preview.Examined != 100 || !preview.ScanTruncated || preview.NextCursor == "" || preview.NextCursor == cursor {
				t.Fatalf("call=%d response=%+v", i, preview)
			}
			cursor = preview.NextCursor
			continue
		}
		if preview.PurgedFiles != 10 || preview.Truncated || preview.Examined != 60 {
			t.Fatalf("tail preview=%+v", preview)
		}
		body, err = json.Marshal(MediaPurgeRequest{ChatJID: purgeChat, Cursor: cursor, DryRun: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		_, actual := purgeCall(t, b, string(body))
		if !slices.Equal(purgedIDs(preview), purgedIDs(actual)) || cachedCount(t, purgeChat) != 0 {
			t.Fatalf("actual differs: %+v", actual)
		}
	}
}

func TestPurgeCursorKeepsSameIDsFromDifferentChats(t *testing.T) {
	b := criteriaFixture(t)
	b.PurgeScanLimit = 1
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedPurgeRows(t, b, purgeChat, "SAME", 1, base, true)
	seedPurgeRows(t, b, purgeGroup, "SAME", 1, base, true)
	cursor := ""
	chats := make(map[string]bool)
	for i := 0; i < 2; i++ {
		body, err := json.Marshal(MediaPurgeRequest{MediaType: "image", Cursor: cursor, DryRun: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		_, resp := purgeCall(t, b, string(body))
		if resp.PurgedFiles != 1 || len(resp.Items) != 1 || chats[resp.Items[0].ChatJID] {
			t.Fatalf("lost or repeated chat: %+v", resp)
		}
		chats[resp.Items[0].ChatJID] = true
		cursor = resp.NextCursor
	}
	if cachedCount(t, purgeChat) != 0 || cachedCount(t, purgeGroup) != 0 {
		t.Fatal("a tied row was skipped")
	}
}

// Real messages.db, its cursor index and its FTS triggers; no router or disk
// mock. A prefix larger than the scan cap must not strand the cached tail.
func TestPurgeCursorDrains150000Rows(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b := testBridge(t, nil, store, installRecordingLogger(t))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.StoreChat(purgeChat, "Alice", base); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(chatMediaDir(purgeChat), 0o700); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare("INSERT INTO messages(id,chat_jid,timestamp,media_type) VALUES(?,?,?,'image')")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	for i := 0; i < 150000; i++ {
		id := fmt.Sprintf("SCAN%06d", i)
		ts := base.Add(time.Duration(i) * time.Second)
		if _, err := stmt.Exec(id, purgeChat, dbTime(ts)); err != nil {
			t.Fatal(err)
		}
		if i >= 120000 {
			writeTestFile(t, filepath.Join(chatMediaDir(purgeChat), mediaFileName("image", ts, id, "")), "x")
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cursor := ""
	total, examined, calls := 0, 0, 0
	for {
		body, err := json.Marshal(MediaPurgeRequest{ChatJID: purgeChat, Cursor: cursor, DryRun: new(false)})
		if err != nil {
			t.Fatal(err)
		}
		code, resp := purgeCall(t, b, string(body))
		calls++
		if code != http.StatusOK || resp.Examined > purgeMaxScan || resp.PurgedFiles > purgeMaxFiles || calls > 63 {
			t.Fatalf("call=%d code=%d response=%+v", calls, code, resp)
		}
		total += resp.PurgedFiles
		examined += resp.Examined
		if calls == 1 && (!resp.ScanTruncated || resp.PurgedFiles != 0 || resp.Examined != 100000) {
			t.Fatalf("prefix response=%+v", resp)
		}
		if !resp.Truncated {
			break
		}
		if resp.NextCursor == "" || resp.NextCursor == cursor || resp.Remaining != -1 {
			t.Fatalf("no progress: %+v", resp)
		}
		cursor = resp.NextCursor
	}
	if total != 30000 || examined != 150000 || cachedCount(t, purgeChat) != 0 {
		t.Fatalf("purged=%d examined=%d", total, examined)
	}
	var rows int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 150000 {
		t.Fatalf("rows=%d error=%v", rows, err)
	}
	t.Logf("150000 rows / 120000 uncached prefix: %d calls, %d probes, %d files removed in %s", calls, examined, total, time.Since(start))
}

func TestRetentionDayBoundaries(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 0, "0": 0, "30": 30 * 24 * time.Hour, "106751": 106751 * 24 * time.Hour} {
		got, err := resolveMediaRetention(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{"106752", "9999999", "9223372036854775807"} {
		got, err := resolveMediaRetention(raw)
		if err == nil || got != 0 || !strings.Contains(err.Error(), mediaRetentionEnv) || !strings.Contains(err.Error(), raw) {
			t.Fatalf("%q: %s %v", raw, got, err)
		}
	}
}

func TestRetentionAndUsageRefuseNestedAndUnsafeNames(t *testing.T) {
	for _, nested := range []string{"nested", "name\ncontrol"} {
		t.Run(fmt.Sprintf("%q", nested), func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			path := filepath.Join(chatMediaDir(purgeChat), nested, "image.jpg")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, path, "nested bytes")
			root := storeRootAt(t, storeDir())
			_, bytes, files := storeUsage(root)
			removed, freed, failed := sweepMedia(root, time.Hour, time.Now().Add(24*time.Hour))
			if bytes != 0 || files != 0 || removed != 0 || freed != 0 || failed != 0 {
				t.Fatalf("usage=%d/%d retention=%d/%d/%d", bytes, files, removed, freed, failed)
			}
			if data, err := root.ReadFile(filepath.ToSlash(filepath.Join(purgeChat, nested, "image.jpg"))); err != nil || string(data) != "nested bytes" {
				t.Fatalf("nested file changed: %q %v", data, err)
			}
		})
	}
}

func TestRetentionOverflowStopsStartupBeforeIO(t *testing.T) {
	for _, raw := range []string{"106752", "9999999", "9223372036854775807"} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv(storeDirEnv, "store")
			t.Setenv(mediaRetentionEnv, raw)
			exit, output := captureStartupOutput(t, run)
			if exit != 1 || !strings.Contains(output, mediaRetentionEnv) || !strings.Contains(output, raw) || strings.Count(output, "\n") != 1 {
				t.Fatalf("exit=%d output=%q", exit, output)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("startup IO entries=%d error=%v", len(entries), err)
			}
		})
	}
}
