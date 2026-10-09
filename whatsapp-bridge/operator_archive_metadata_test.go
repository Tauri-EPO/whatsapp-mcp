package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

func TestExportPreservesNoteHashRevocationAndCurrentLocation(t *testing.T) {
	b, server := archiveFixture(t, true)
	hash := bytes.Repeat([]byte{1}, 32)
	if _, err := b.Store.db.Exec("UPDATE messages SET file_sha256=?,deleted_at='2026-10-02 00:00:00+00:00',view_once=1 WHERE id='A'", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,timestamp,content,media_type,location) VALUES('LOCATION','111@s.whatsapp.net','2026-10-01 00:00:00+00:00','initial position','location','{"latitude":0.75,"longitude":0.5,"live":true}')`); err != nil {
		t.Fatal(err)
	}
	response := archiveRequest(t, server, "GET", "/operator/v1/export")
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		if file.Name != "messages.jsonl" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Close() }()
		decoder := json.NewDecoder(reader)
		seen := 0
		for decoder.More() {
			var row map[string]any
			if err := decoder.Decode(&row); err != nil {
				t.Fatal(err)
			}
			switch row["id"] {
			case "A":
				if row["sha256"] != strings.Repeat("01", 32) || row["deleted_at"] != "2026-10-02 00:00:00+00:00" || row["view_once"] != float64(1) {
					t.Fatalf("archive metadata lost: %v", row)
				}
				seen++
			case "LOCATION":
				location, ok := row["location"].(map[string]any)
				if !ok || location["latitude"] != 0.75 {
					t.Fatal("current location payload lost")
				}
				seen++
			}
		}
		if seen != 2 {
			t.Fatalf("metadata rows=%d", seen)
		}
	}
}

func TestStreamingZIPAbortsChangedCache(t *testing.T) {
	passes := 0
	walk := func(emit func(archiveEntry) error) error {
		passes++
		value := "original"
		if passes > 1 {
			value = "changed"
		}
		return emit(archiveEntry{"media/sample", func(out io.Writer) (int64, error) { _, err := io.WriteString(out, value); return 1, err }})
	}
	var out bytes.Buffer
	if _, err := streamArchive(&out, walk, exportManifestForTest()); err == nil {
		t.Fatal("cache mutation accepted")
	}
	if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err == nil {
		t.Fatal("changed stream produced a valid ZIP")
	}
}

func exportManifestForTest() func(io.Writer, archiveWalk) (int64, error) {
	return func(out io.Writer, walk archiveWalk) (int64, error) {
		return 0, walk(func(entry archiveEntry) error { _, err := entry.write(out); return err })
	}
}

func TestExportClientCancellationReleasesReaderAndLimit(t *testing.T) {
	b, _ := archiveFixture(t, false)
	tx, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err := tx.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00',?)", fmtInt(int64(i)), strings.Repeat("x", 4096)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	handler := b.handleExport()
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{export: func(w http.ResponseWriter, r *http.Request) { defer close(done); handler(w, r) }}, b.Log))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/operator/v1/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled export kept its reader")
	}
	if b.exportBusy.Load() {
		t.Fatal("cancelled export retained admission slot")
	}
}

func TestOperatorLogoutCancelsExportAndWipesRealSession(t *testing.T) {
	b := newSettingsBridge(t)
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	boundPool(db, messagesPoolConns)
	b.sessionDB = db
	container := sqlstore.NewWithDB(db, "sqlite", testLogger())
	defer func() { _ = container.Close() }()
	if err := container.Upgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	device := container.NewDevice()
	jid := types.NewJID("5511999999999", types.DefaultUserServer)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := append([]byte(nil), device.NoiseKey.Priv[:]...)
	b.Client = newRuntimeClient(device, testLogger())
	// The offline test never contacts WhatsApp; local destruction is the real path.
	b.logoutClient = func(context.Context) error { return errors.New("offline unlink") }
	b.SnapshotDir = t.TempDir()
	b.Archive.Session = true
	b.operatorPairing = newOperatorPairing(b.ctx, b, b.Client, nil, b.isPaired, b.Connected, io.Discard, make(chan bool, 1))
	if _, err := b.Store.db.Exec("INSERT INTO chats(jid,name) VALUES('111@s.whatsapp.net','Alice')"); err != nil {
		t.Fatal(err)
	}
	tx, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err := tx.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00',?)", fmtInt(int64(i)), strings.Repeat("x", 4096)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, b.operatorRoutes(), b.Log))
	defer server.Close()
	sessionSnapshot := archiveRequest(t, server, "POST", "/operator/v1/snapshot?session=true")
	var backup struct{ Files []snapshotFile }
	if err := json.NewDecoder(sessionSnapshot.Body).Decode(&backup); err != nil {
		t.Fatal(err)
	}
	_ = sessionSnapshot.Body.Close()
	if sessionSnapshot.StatusCode != 200 || len(backup.Files) != 2 {
		t.Fatal("session snapshot failed before logout")
	}
	for _, file := range backup.Files {
		if !strings.HasSuffix(file.Name, "-whatsapp.db") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(b.SnapshotDir, file.Name))
		if err != nil || !bytes.Contains(raw, key) {
			t.Fatal("session snapshot did not contain test credential")
		}
		if err := os.WriteFile(filepath.Join(b.SnapshotDir, file.Name+".partial"), raw, 0o600); err != nil { //nolint:gosec // Generated snapshot name in the test's private directory.
			t.Fatal(err)
		}
	}
	export := archiveRequest(t, server, "GET", "/operator/v1/export")
	defer func() { _ = export.Body.Close() }()
	if _, err := io.ReadFull(export.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	// Confirm the reader is actually pinned, then leave the HTTP client stalled.
	if _, err := db.Exec("CREATE TABLE archive_logout_probe(value TEXT); INSERT INTO archive_logout_probe VALUES('sample')"); err != nil {
		t.Fatal(err)
	}
	var busy, pages, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &pages, &checkpointed); err != nil || pages <= checkpointed {
		t.Fatalf("export reader was not pinned: pages=%d checkpointed=%d err=%v", pages, checkpointed, err)
	}
	req, _ := http.NewRequest("POST", server.URL+"/operator/v1/logout", strings.NewReader(`{"after":"idle"}`))
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	start := time.Now()
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(result), `"local_session_wiped":true`) {
		t.Fatalf("logout blocked by export: status=%d body=%s err=%v", response.StatusCode, result, err)
	}
	if !strings.Contains(string(result), `"server_unlinked":false`) || !strings.Contains(string(result), `"session_snapshots_removed":2`) {
		t.Fatal("offline logout did not report both snapshot deletions")
	}
	entries, err := os.ReadDir(b.SnapshotDir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "-messages.db") {
		t.Fatal("logout retained session snapshot or deleted data snapshot")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("logout waited for the stalled export client")
	}
	devices, err := container.GetAllDevices(t.Context())
	if err != nil || len(devices) != 0 {
		t.Fatal("SDK session survived logout")
	}
	for _, name := range []string{"whatsapp.db", "whatsapp.db-wal"} {
		data, err := b.StoreRoot.ReadFile(name)
		if err != nil || bytes.Contains(data, key) {
			t.Fatalf("credential cleanup failed: file=%s err=%v", name, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for b.exportBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.exportBusy.Load() {
		t.Fatal("logout kept export admission occupied")
	}
	b.SnapshotDir = t.TempDir()
	b.Archive.Session = true
	snapshot := archiveRequest(t, server, "POST", "/operator/v1/snapshot?session=true")
	_ = snapshot.Body.Close()
	if snapshot.StatusCode != 503 {
		t.Fatal("session snapshot admitted after logout parked session access")
	}
}
