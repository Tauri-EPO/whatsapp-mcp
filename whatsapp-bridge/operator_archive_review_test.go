package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"
)

func TestExportSkipsMalformedMediaAndUsesCanonicalChatDirectory(t *testing.T) {
	b, server := archiveFixture(t, false)
	for _, id := range []string{"bad..id", "bad/id", `bad\id`, "bad\nid"} {
		if _, err := b.Store.db.Exec("INSERT INTO messages(id,chat_jid,timestamp,media_type) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00','image')", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Store.db.Exec("INSERT INTO chats(jid,name) VALUES('legacy:chat','Alice'); INSERT INTO messages(id,chat_jid,timestamp,media_type) VALUES('NULL-TIME','111@s.whatsapp.net',NULL,'image'),('NULL-CHAT',NULL,'2026-10-01 00:00:00+00:00','image'),('BAD-TIME','111@s.whatsapp.net','invalid','image'); UPDATE messages SET chat_jid='legacy:chat' WHERE id='A'"); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRoot.Rename("111@s.whatsapp.net", "legacy_chat"); err != nil {
		t.Fatal(err)
	}
	response := archiveRequest(t, server, "GET", "/operator/v1/export?media=true")
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("status=%d err=%v", response.StatusCode, err)
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range archive.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = raw
	}
	var manifest struct {
		Skipped int `json:"media_skipped"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil || manifest.Skipped != 7 {
		t.Fatalf("skipped=%d err=%v", manifest.Skipped, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(files["messages.jsonl"]))
	var rows int
	for decoder.More() {
		var row map[string]any
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		rows++
		if row["id"] == "A" {
			ref, ok := row["media_ref"].(string)
			if !ok || !strings.HasPrefix(ref, "media/legacy_chat/") || string(files[ref]) != "cached sample bytes" {
				t.Fatal("canonical cached media missing")
			}
		} else if row["media_ref"] != nil {
			t.Fatal("malformed media has a reference")
		}
	}
	if rows != 8 {
		t.Fatalf("rows=%d", rows)
	}
}

func TestHistoryCompletionSurvivesOtherSyncTypes(t *testing.T) {
	b, _ := archiveFixture(t, false)
	b.historyProgress.update(waHistorySync.HistorySync_FULL, 100, 1, 1)
	for _, kind := range []waHistorySync.HistorySync_HistorySyncType{waHistorySync.HistorySync_ON_DEMAND, waHistorySync.HistorySync_PUSH_NAME, waHistorySync.HistorySync_NON_BLOCKING_DATA, waHistorySync.HistorySync_RECENT} {
		fixture := largeHistoryFixture(1)
		fixture.Data.SyncType = &kind
		fixture.Data.Progress = proto.Uint32(0)
		b.handleHistorySync(fixture)
		status := b.historyProgress.snapshot()
		if status.State != "complete" || status.Progress != 100 {
			t.Fatalf("kind=%s state=%+v", kind, status)
		}
	}
}

func TestSnapshotSpaceRetentionAndCancelledPublication(t *testing.T) {
	b, server := archiveFixture(t, true)
	b.snapshotSpace = func(*os.File) (uint64, error) { return 0, nil }
	response := archiveRequest(t, server, "POST", "/operator/v1/snapshot")
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 507 || !strings.Contains(string(body), "insufficient free space") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	entries, err := os.ReadDir(b.SnapshotDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("low-space files=%d err=%v", len(entries), err)
	}
	b.snapshotSpace = nil
	var previous []snapshotFile
	for range 3 {
		files, err := snapshotStores(t.Context(), b.StoreRoot, b.SnapshotDir, false, snapshotOptions{keep: 1})
		if err != nil || len(files) != 2 {
			t.Fatalf("files=%d err=%v", len(files), err)
		}
		for _, file := range previous {
			if _, err := os.Stat(filepath.Join(b.SnapshotDir, file.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old snapshot set retained")
			}
		}
		previous = files
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := snapshotStores(ctx, b.StoreRoot, b.SnapshotDir, false, snapshotOptions{keep: 1}); err == nil {
		t.Fatal("cancelled snapshot succeeded")
	}
	entries, err = os.ReadDir(b.SnapshotDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("cancelled snapshot left files=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatal("partial file retained after handled failure")
		}
	}
	available, err := snapshotFreeBytes(mustSnapshotDirectory(t, b.SnapshotDir))
	if err != nil || available == 0 {
		t.Fatalf("filesystem free space=%d err=%v", available, err)
	}
}

func mustSnapshotDirectory(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // Test-owned temporary snapshot directory.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestSnapshotLocationAndArchiveConfiguration(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store")
	media := filepath.Join(base, "outbox")
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	for _, dir := range []string{store, filepath.Join(store, "backups"), media, filepath.Join(media, "nested"), filepath.Join(base, defaultOutboxSubpath, "nested")} {
		if err := validateSnapshotLocation(dir, store, media); err == nil {
			t.Fatal("unsafe snapshot location accepted")
		}
	}
	_, err := parseBridgeConfig(func(name string) string {
		switch name {
		case storeDirEnv:
			return " " + store + " "
		case "WHATSAPP_SNAPSHOT_DIR":
			return filepath.Join(store, "backups")
		case "WHATSAPP_MEDIA_ROOTS":
			return media
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "outside store") {
		t.Fatal("whitespace store setting bypasses snapshot confinement")
	}
	if err := validateSnapshotLocation(filepath.Join(base, "backups"), store, media); err != nil {
		t.Fatal(err)
	}
	for key, values := range map[string][]string{"WHATSAPP_SNAPSHOT_KEEP": {"0", "-1", "10001"}, "WHATSAPP_OPERATOR_EXPORT_TIMEOUT_MIN": {"0", "1441", "invalid"}, "WHATSAPP_SNAPSHOT_SESSION": {"invalid"}} {
		for _, value := range values {
			if _, err := parseArchiveConfig(func(name string) string {
				if name == key {
					return value
				}
				return ""
			}); err == nil {
				t.Fatalf("invalid %s accepted", key)
			}
		}
	}
	cfg, err := parseArchiveConfig(func(string) string { return "" })
	if err != nil || cfg.Keep != 7 || cfg.Session || cfg.Timeout != 30*time.Minute {
		t.Fatal("unsafe archive defaults")
	}
	cfg, err = parseArchiveConfig(func(name string) string {
		switch name {
		case "WHATSAPP_SNAPSHOT_KEEP":
			return "2"
		case "WHATSAPP_OPERATOR_EXPORT_TIMEOUT_MIN":
			return "90"
		case "WHATSAPP_SNAPSHOT_SESSION":
			return "true"
		}
		return ""
	})
	if err != nil || cfg.Keep != 2 || !cfg.Session || cfg.Timeout != 90*time.Minute {
		t.Fatal("configured archive limits ignored")
	}
}

func TestRetentionFailurePreservesNewCompletedSnapshot(t *testing.T) {
	b, server := archiveFixture(t, true)
	if _, err := snapshotStores(t.Context(), b.StoreRoot, b.SnapshotDir, false); err != nil {
		t.Fatal(err)
	}
	files, err := snapshotStores(t.Context(), b.StoreRoot, b.SnapshotDir, false, snapshotOptions{keep: 1, prune: func(root *os.Root, dir *os.File, current string, keep int) error {
		if err := pruneSnapshotSets(root, dir, current, keep); err != nil {
			return err
		}
		return errors.New("injected directory sync failure after real pruning")
	}})
	if !errors.Is(err, errSnapshotRetention) {
		t.Fatalf("unexpected error=%v", err)
	}
	entries, readErr := os.ReadDir(b.SnapshotDir)
	if readErr != nil || len(entries) != 2 {
		t.Fatalf("retention failure destroyed completed snapshot: files=%d err=%v", len(entries), readErr)
	}
	if len(files) != 2 {
		t.Fatal("retention warning lost snapshot result")
	}
	b.snapshotPrune = func(*os.Root, *os.File, string, int) error { return errors.New("injected retention fault") }
	response := archiveRequest(t, server, "POST", "/operator/v1/snapshot")
	var result struct {
		Files   []snapshotFile
		Warning bool `json:"retention_warning"`
	}
	err = json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !result.Warning || len(result.Files) != 2 {
		t.Fatalf("retention result status=%d warning=%t files=%d err=%v", response.StatusCode, result.Warning, len(result.Files), err)
	}
}

func TestSnapshotSessionRequiresHTTPOptIn(t *testing.T) {
	b, server := archiveFixture(t, false)
	response := archiveRequest(t, server, "POST", "/operator/v1/snapshot?session=true")
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 403 || !strings.Contains(string(body), "WHATSAPP_SNAPSHOT_SESSION") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	directory := b.SnapshotDir
	b.SnapshotDir = ""
	response = archiveRequest(t, server, "POST", "/operator/v1/snapshot?session=true")
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("missing directory bypassed default-off session policy")
	}
	b.SnapshotDir = directory
	b.Archive.Session = true
	response = archiveRequest(t, server, "POST", "/operator/v1/snapshot?session=true")
	var result struct{ Files []snapshotFile }
	err := json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || len(result.Files) != 2 {
		t.Fatalf("opt-in snapshot status=%d files=%d err=%v", response.StatusCode, len(result.Files), err)
	}
}

func TestSnapshotCLIJSONStdout(t *testing.T) {
	if os.Getenv("W11_SNAPSHOT_CLI_TEST") == "true" {
		os.Args = []string{os.Args[0], "snapshot", "--out", os.Getenv("W11_SNAPSHOT_CLI_OUT"), "--session"}
		os.Exit(runCLI())
	}
	b, _ := archiveFixture(t, false)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSnapshotCLIJSONStdout$") //nolint:gosec // Runs this test binary with a fixed helper test name.
	cmd.Env = append(os.Environ(), "W11_SNAPSHOT_CLI_TEST=true", "W11_SNAPSHOT_CLI_OUT="+b.SnapshotDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("CLI failed: %v stderr=%s", err, stderr.String())
	}
	var result struct{ Files []snapshotFile }
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || len(result.Files) != 2 {
		t.Fatalf("stdout is not one JSON result: err=%v", err)
	}
	if !strings.Contains(stderr.String(), "Operator snapshot CLI:") {
		t.Fatal("CLI audit missing from stderr")
	}
}
