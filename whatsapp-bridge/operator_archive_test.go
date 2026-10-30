package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func archiveFixture(t *testing.T, withNotes bool) (*Bridge, *httptest.Server) {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, waLog.Noop)
	b.Connected = func() bool { return true }
	b.SnapshotDir = filepath.Join(t.TempDir(), "snapshots")
	if _, err := ms.db.Exec("INSERT INTO chats(jid,name) VALUES('111@s.whatsapp.net','Alice'); INSERT INTO messages(id,chat_jid,sender,sender_server,timestamp,content,media_type,filename,quoted_message_id) VALUES('A','111@s.whatsapp.net','111','s.whatsapp.net','2026-10-01 00:00:00+00:00','export sample','document','file.txt','Q')"); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRoot.Mkdir("111@s.whatsapp.net", 0o700); err != nil {
		t.Fatal(err)
	}
	stamp, _ := parseDBTime("2026-10-01 00:00:00+00:00")
	f, err := b.StoreRoot.OpenFile("111@s.whatsapp.net/"+mediaFileName("document", stamp, "A", "file.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, "cached sample bytes")
	_ = f.Close()
	if withNotes {
		db, err := sql.Open("sqlite", sqliteURI(storePath("notes.db"), sqliteWriterOptions))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(`CREATE TABLE notes(target_type TEXT,target_id TEXT,key TEXT,value TEXT,updated_at TEXT,source TEXT,version INTEGER,PRIMARY KEY(target_type,target_id,key,version)); CREATE TABLE media_notes(sha256 TEXT,key TEXT,value TEXT,updated_at TEXT,PRIMARY KEY(sha256,key)); INSERT INTO notes VALUES('chat','111@s.whatsapp.net','summary','sample note','2026-10-01','','1'); INSERT INTO media_notes VALUES('fake-hash','transcript','sample transcription','2026-10-01')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE notes_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO notes_meta VALUES('ingest_sent_since','2026-10-01 00:00:00+00:00')`); err != nil {
			t.Fatal(err)
		}
	}
	session, err := sql.Open("sqlite", sqliteURI(storePath("whatsapp.db"), sqliteWriterOptions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT,their_jid TEXT,first_name TEXT,full_name TEXT,push_name TEXT,business_name TEXT,PRIMARY KEY(our_jid,their_jid)); CREATE TABLE session_credentials(key TEXT); INSERT INTO session_credentials VALUES('FAKE-SESSION-KEY-DO-NOT-EXPORT'); INSERT INTO whatsmeow_contacts VALUES('222@s.whatsapp.net','111@s.whatsapp.net','Alice','Alice','Alice','')`); err != nil {
		t.Fatal(err)
	}
	routes := operatorRoutes{export: b.handleExport(), snapshot: b.handleSnapshot()}
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, routes, waLog.Noop))
	t.Cleanup(server.Close)
	return b, server
}

func archiveRequest(t *testing.T, server *httptest.Server, method, route string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSnapshotPreservesSentResetIntentAndPrunesItWithNotes(t *testing.T) {
	for _, value := range []string{"fake-pending-reset", ""} {
		t.Run(fmt.Sprintf("bytes-%d", len(value)), func(t *testing.T) {
			b, _ := archiveFixture(t, true)
			if err := os.WriteFile(storePath(sentResetMarker), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			source, err := openArchiveDB(b.StoreRoot, "notes.db", false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			var before string
			if err := source.QueryRow("SELECT group_concat(sql) FROM sqlite_master").Scan(&before); err != nil {
				t.Fatal(err)
			}
			var previous []snapshotFile
			for range 2 {
				files, err := snapshotStores(t.Context(), b.StoreRoot, b.SnapshotDir, false, snapshotOptions{keep: 1})
				if err != nil || len(files) != 3 {
					t.Fatalf("files=%v err=%v", files, err)
				}
				found := false
				for _, file := range files {
					if !strings.HasSuffix(file.Name, sentResetMarker) {
						continue
					}
					found = true
					path := filepath.Join(b.SnapshotDir, file.Name)
					data, err := os.ReadFile(path) //nolint:gosec // Server-generated snapshot filename in the test's private directory.
					if err != nil || string(data) != value {
						t.Fatalf("marker bytes changed err=%v", err)
					}
					hash := sha256.Sum256(data)
					info, err := os.Stat(path)
					if err != nil || file.SHA256 != hex.EncodeToString(hash[:]) || file.Size != int64(len(value)) {
						t.Fatalf("invalid marker metadata err=%v", err)
					}
					if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
						t.Fatal("marker is not private")
					}
				}
				if !found {
					t.Fatal("missing reset intent")
				}
				for _, old := range previous {
					if _, err := os.Stat(filepath.Join(b.SnapshotDir, old.Name)); !os.IsNotExist(err) {
						t.Fatal("old snapshot set retained")
					}
				}
				previous = files
			}
			var after string
			if err := source.QueryRow("SELECT group_concat(sql) FROM sqlite_master").Scan(&after); err != nil || before != after {
				t.Fatal("source notes schema changed")
			}
			var transcript string
			if err := source.QueryRow("SELECT value FROM media_notes WHERE key='transcript'").Scan(&transcript); err != nil || transcript != "sample transcription" {
				t.Fatal("source notes contents changed")
			}
			if data, err := os.ReadFile(storePath(sentResetMarker)); err != nil || string(data) != value {
				t.Fatal("source marker changed")
			}
		})
	}
}

func TestSnapshotRejectsUnsafeSentResetIntent(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversized", "non-ascii", "orphan"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := archiveFixture(t, kind != "orphan")
			path := storePath(sentResetMarker)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("notes.db", path)
				if err != nil && runtime.GOOS == "windows" {
					t.Skip("symlink creation unavailable")
				}
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "oversized":
				err = os.WriteFile(path, bytes.Repeat([]byte{'x'}, 129), 0o600)
			case "non-ascii":
				err = os.WriteFile(path, []byte{255}, 0o600)
			default:
				err = os.WriteFile(path, []byte("fake-reset"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotStores(t.Context(), b.StoreRoot, b.SnapshotDir, false); err == nil {
				t.Fatal("unsafe marker accepted")
			}
			entries, err := os.ReadDir(b.SnapshotDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed snapshot left artifacts err=%v", err)
			}
		})
	}
}

func TestOperatorExportArchiveHashesCountsNotesMediaAndSecrets(t *testing.T) {
	_, server := archiveFixture(t, true)
	for _, media := range []bool{false, true} {
		response := archiveRequest(t, server, "GET", fmt.Sprintf("/operator/v1/export?media=%t", media))
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("export status=%d err=%v", response.StatusCode, err)
		}
		archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, f := range archive.File {
			reader, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			files[f.Name] = data
		}
		var manifest struct {
			Files []struct {
				Name   string
				SHA256 string
				Size   uint64
				Count  int64
			}
		}
		if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Files) != len(files)-1 {
			t.Fatalf("manifest files=%d archive=%d", len(manifest.Files), len(files))
		}
		for _, entry := range manifest.Files {
			data, exists := files[entry.Name]
			hash := sha256.Sum256(data)
			if !exists || entry.SHA256 != hex.EncodeToString(hash[:]) || entry.Size != uint64(len(data)) {
				t.Fatalf("hash mismatch for %s", entry.Name)
			}
			if entry.Name == "messages.jsonl" && entry.Count != 1 {
				t.Fatalf("messages count=%d", entry.Count)
			}
		}
		for _, name := range []string{"chats.json", "contacts.json", "notes.jsonl", "transcriptions.jsonl", "notes_meta.jsonl"} {
			if len(files[name]) < 3 {
				t.Fatalf("missing %s", name)
			}
		}
		for _, secret := range []string{fakeOperatorToken, "FAKE-SESSION-KEY-DO-NOT-EXPORT", "media_key", "session_credentials"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatal("credential material included in archive")
			}
		}
		var row map[string]any
		if err := json.Unmarshal(files["messages.jsonl"], &row); err != nil {
			t.Fatal(err)
		}
		if media {
			ref, ok := row["media_ref"].(string)
			if !ok || string(files[ref]) != "cached sample bytes" {
				t.Fatal("media reference missing or wrong")
			}
		} else if row["media_ref"] != nil || len(files) != 8 {
			t.Fatal("media included by default")
		}
	}
}

func TestOperatorExportSentResetIntent(t *testing.T) {
	for _, kind := range []string{"present", "empty", "absent", "symlink", "directory", "oversized", "public", "non-ascii", "orphan"} {
		t.Run(kind, func(t *testing.T) {
			b, server := archiveFixture(t, kind != "orphan")
			value := []byte("fake-pending-reset")
			if kind == "empty" {
				value = []byte{}
			}
			markerPath := storePath(sentResetMarker)
			var err error
			switch kind {
			case "absent":
			case "symlink":
				err = os.Symlink("notes.db", markerPath)
				if err != nil && runtime.GOOS == "windows" {
					t.Skip("symlink creation unavailable")
				}
			case "directory":
				err = os.Mkdir(markerPath, 0o700)
			default:
				if kind == "oversized" {
					value = bytes.Repeat([]byte{'x'}, 129)
				}
				if kind == "non-ascii" {
					value = []byte{255}
				}
				err = os.WriteFile(markerPath, value, 0o600)
				if err == nil && kind == "public" {
					if runtime.GOOS == "windows" {
						t.Skip("POSIX permissions")
					}
					err = os.Chmod(markerPath, 0o644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			response := archiveRequest(t, server, "GET", "/operator/v1/export")
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			valid := kind == "present" || kind == "empty" || kind == "absent"
			if !valid {
				if response.StatusCode != 503 {
					t.Fatalf("unsafe export status=%d", response.StatusCode)
				}
				return
			}
			if response.StatusCode != 200 {
				t.Fatalf("export status=%d", response.StatusCode)
			}
			archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range archive.File {
				if file.Name != sentResetMarker {
					continue
				}
				found = true
				if file.Mode().Perm() != 0o600 {
					t.Fatal("export marker is not owner-only")
				}
				reader, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				_ = reader.Close()
				if err != nil || !bytes.Equal(data, value) {
					t.Fatalf("marker changed err=%v", err)
				}
			}
			if found != (kind != "absent") {
				t.Fatal("reset marker presence changed")
			}
			if kind != "absent" {
				data, err := b.StoreRoot.ReadFile(sentResetMarker)
				if err != nil || !bytes.Equal(data, value) {
					t.Fatal("source marker changed")
				}
			}
		})
	}
}

func TestOperatorArchiveRouteDenyPathsAndQueries(t *testing.T) {
	_, server := archiveFixture(t, false)
	for _, route := range []struct{ method, path string }{{"GET", "export"}, {"POST", "snapshot"}} {
		for _, tc := range []struct {
			token, host, origin string
			code                int
		}{{"", "", "", 401}, {"fake-data-plane-token-0123456789abcdef", "", "", 401}, {fakeOperatorToken, "evil.example.test", "", 403}, {fakeOperatorToken, "", "https://evil.example.test", 403}} {
			req, _ := http.NewRequest(route.method, server.URL+"/operator/v1/"+route.path, nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != tc.code {
				t.Fatalf("%s deny=%d want=%d", route.path, response.StatusCode, tc.code)
			}
		}
		for _, query := range []string{"out=../../escaped", "session=maybe", "media=true&media=false"} {
			response := archiveRequest(t, server, route.method, "/operator/v1/"+route.path+"?"+query)
			_ = response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatalf("unexpected query allowed for %s: %d", route.path, response.StatusCode)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(storeDir(), "notes.db")); !os.IsNotExist(err) {
		t.Fatal("lazy notes store created")
	}
}

func TestOperatorSnapshotUnderWALWriteLoadAndCLI(t *testing.T) {
	b, server := archiveFixture(t, true)
	committed, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		if _, err := committed.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00','pre-call commit')", fmt.Sprintf("BEFORE%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ctx.Err() == nil; i++ {
			if _, err := b.Store.db.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES(?, '111@s.whatsapp.net', '2026-10-01 00:00:00+00:00', 'load')", fmt.Sprintf("LOAD%d", i)); err != nil {
				failed.Store(true)
				return
			}
		}
	}()
	var before int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&before); err != nil {
		t.Fatal(err)
	}
	response := archiveRequest(t, server, "POST", "/operator/v1/snapshot")
	var result struct{ Files []snapshotFile }
	err = json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	cancel()
	<-done
	if err != nil || response.StatusCode != 200 || failed.Load() || !b.Connected() {
		t.Fatalf("snapshot status=%d err=%v writerFailed=%t", response.StatusCode, err, failed.Load())
	}
	if len(result.Files) != 2 {
		t.Fatalf("snapshot files=%d", len(result.Files))
	}
	for _, file := range result.Files {
		name := filepath.Join(b.SnapshotDir, file.Name)
		raw, err := os.ReadFile(name) //nolint:gosec // Server-generated snapshot filename in the test's private directory.
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if file.Size != int64(len(raw)) || file.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatal("snapshot hash/size mismatch")
		}
		db, err := sql.Open("sqlite", sqliteURI(name, sqliteReadOnlyOptions))
		if err != nil {
			t.Fatal(err)
		}
		var integrity string
		err = db.QueryRow("PRAGMA integrity_check").Scan(&integrity)
		if err != nil || integrity != "ok" {
			t.Fatalf("integrity=%s err=%v", integrity, err)
		}
		if strings.HasSuffix(file.Name, "messages.db") {
			var prior int
			if err := db.QueryRow("SELECT COUNT(*) FROM messages WHERE id LIKE 'BEFORE%'").Scan(&prior); err != nil || prior != 1000 {
				t.Fatalf("pre-call commits=%d err=%v", prior, err)
			}
			var rows int
			if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows < before {
				t.Fatalf("snapshot rows=%d before=%d err=%v", rows, before, err)
			}
		}
		_ = db.Close()
		info, _ := os.Stat(name)
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatal("snapshot mode not 0600")
		}
	}
	info, _ := os.Stat(b.SnapshotDir)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatal("snapshot directory not 0700")
	}
	var out bytes.Buffer
	if code := snapshotCLI([]string{"--out", filepath.Join(t.TempDir(), "cron"), "--session"}, &out); code != 0 {
		t.Fatalf("CLI code=%d output=%s", code, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Files) != 3 {
		t.Fatalf("CLI session snapshot err=%v files=%d", err, len(result.Files))
	}
}

func TestSnapshotRejectsSymlinkWorldWritableAndReadOnlyNotes(t *testing.T) {
	b, _ := archiveFixture(t, true)
	db, err := openArchiveDB(b.StoreRoot, "notes.db", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO media_notes VALUES('fake','summary','forbidden','2026-10-01')"); err == nil {
		t.Fatal("bridge wrote notes.db")
	}
	_ = db.Close()
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes and unprivileged symlinks exercised on Linux")
	}
	target := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o777); err != nil { //nolint:gosec // Deliberately unsafe mode exercises the snapshot deny path.
		t.Fatal(err)
	}
	if _, err := snapshotStores(t.Context(), b.StoreRoot, target, false); err == nil {
		t.Fatal("world-writable snapshot dir accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(b.SnapshotDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotStores(t.Context(), b.StoreRoot, link, false); err == nil {
		t.Fatal("symlink snapshot dir accepted")
	}
}

func TestOperatorExportMissingNotesAndConcurrentLimit(t *testing.T) {
	b, server := archiveFixture(t, false)
	b.exportBusy.Store(true)
	response := archiveRequest(t, server, "GET", "/operator/v1/export")
	_ = response.Body.Close()
	if response.StatusCode != 429 {
		t.Fatalf("concurrent export=%d", response.StatusCode)
	}
	b.exportBusy.Store(false)
	response = archiveRequest(t, server, "GET", "/operator/v1/export")
	_, err := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("missing notes export status=%d err=%v", response.StatusCode, err)
	}
	if _, err := os.Stat(storePath("notes.db")); !os.IsNotExist(err) {
		t.Fatal("export created notes.db")
	}
}

type heapSampleWriter struct {
	bytes int64
	peak  uint64
}

func (w *heapSampleWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	if w.bytes%(1<<20) < int64(len(p)) {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		if stats.HeapAlloc > w.peak {
			w.peak = stats.HeapAlloc
		}
	}
	return len(p), nil
}

func TestOperatorExportLargeStoreHasBoundedHeap(t *testing.T) {
	b, server := archiveFixture(t, false)
	var peaks []uint64
	for _, count := range []int{2000, 20000} {
		tx, err := b.Store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare("INSERT OR REPLACE INTO messages(id,chat_jid,timestamp,content) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00',?)")
		if err != nil {
			t.Fatal(err)
		}
		for i := range count {
			if _, err := stmt.Exec(fmt.Sprintf("LARGE%08d", i), strings.Repeat("x", 2048)); err != nil {
				t.Fatal(err)
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		sink := &heapSampleWriter{}
		response := archiveRequest(t, server, "GET", "/operator/v1/export")
		_, err = io.Copy(sink, response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("large export status=%d err=%v", response.StatusCode, err)
		}
		peaks = append(peaks, sink.peak)
		t.Logf("rows=%d streamed_bytes=%d peak_heap=%d", count, sink.bytes, sink.peak)
	}
	if peaks[1] > peaks[0]+16<<20 {
		t.Fatalf("heap grew with 10x archive: %v", peaks)
	}
}

func TestStreamingZIPMemoryIndependentOfMediaEntryCount(t *testing.T) {
	// Many tiny files catch the standard ZIP writer's retained central directory.
	for _, count := range []int{10, 100000} {
		walk := func(emit func(archiveEntry) error) error {
			for i := range count {
				if err := emit(archiveEntry{fmt.Sprintf("media/%08d", i), func(out io.Writer) (int64, error) { _, err := io.WriteString(out, "sample"); return 1, err }}); err != nil {
					return err
				}
			}
			return nil
		}
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		sink := &heapSampleWriter{}
		if _, err := streamArchive(sink, walk, exportManifest(time.Unix(0, 0))); err != nil {
			t.Fatal(err)
		}
		if sink.peak > before.HeapAlloc+16<<20 {
			t.Fatalf("ZIP metadata heap grew: entries=%d before=%d peak=%d", count, before.HeapAlloc, sink.peak)
		}
	}
}
