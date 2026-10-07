package main

// The SQLite databases, and the files the driver keeps next to them, are
// owner-only: created that way on a fresh store, tightened on one an earlier
// release left readable by group or others (issue #491).

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func skipWithoutModeBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
}

// modesOf maps each existing file among name, name-wal, name-shm to its mode.
func modesOf(t *testing.T, name string) map[string]os.FileMode {
	t.Helper()
	modes := map[string]os.FileMode{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(name + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[filepath.Base(name)+suffix] = info.Mode().Perm()
	}
	return modes
}

// requireOwnerOnly fails unless the database and both of its WAL siblings exist
// and none of them grants anything to group or others.
func requireOwnerOnly(t *testing.T, name string) {
	t.Helper()
	modes := modesOf(t, name)
	if len(modes) != 3 {
		t.Fatalf("%s: want the database, its -wal and its -shm on disk, got %v", filepath.Base(name), modes)
	}
	for file, mode := range modes {
		if mode != 0o600 {
			t.Errorf("%s is %04o, want 0600", file, mode)
		}
	}
}

func TestDatabasesAreCreatedOwnerOnly(t *testing.T) {
	skipWithoutModeBits(t)
	t.Setenv(storeDirEnv, filepath.Join(t.TempDir(), "store"))
	installRecordingLogger(t)

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	if err := ms.StoreChat(mediaTestChat, "Test", time.Now()); err != nil { // a write: the WAL and its index exist now
		t.Fatal(err)
	}
	requireOwnerOnly(t, messagesDBPath())

	// The session database, opened the way main() opens it.
	privateDatabase(whatsmeowDBPath())
	container, err := sqlstore.New(context.Background(), "sqlite", sqliteURI(whatsmeowDBPath(), sqliteWriterOptions), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	defer func() { _ = container.Close() }()
	requireOwnerOnly(t, whatsmeowDBPath())
}

// Without privateDatabase the driver's default applies. This is the control for
// the test above: it pins what the bridge is protecting against, and fails the
// day the driver starts creating private files by itself.
func TestTheDriverAloneCreatesAReadableDatabase(t *testing.T) {
	skipWithoutModeBits(t)
	path := filepath.Join(t.TempDir(), "plain.db")
	container, err := sqlstore.New(context.Background(), "sqlite", sqliteURI(path, sqliteWriterOptions), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	defer func() { _ = container.Close() }()
	if mode := modesOf(t, path)["plain.db"]; mode&0o077 == 0 {
		t.Fatalf("the driver created the database %04o by itself; privateDatabase may no longer be needed", mode)
	}
}

func TestExistingLooseDatabaseIsTightenedAtStartup(t *testing.T) {
	skipWithoutModeBits(t)
	t.Setenv(storeDirEnv, filepath.Join(t.TempDir(), "store"))
	rec := installRecordingLogger(t)

	// A store an earlier release created: the data is there, the mode is the driver's.
	first, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	if err := first.StoreChat(mediaTestChat, "kept across the restart", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if err := os.Chmod(messagesDBPath(), 0o644); err != nil { //nolint:gosec // the looser mode an older bridge left behind is the fixture
		t.Fatal(err)
	}
	if strings.Contains(rec.String(), "Tightened") {
		t.Fatalf("a fresh store had nothing to tighten:\n%s", rec.String())
	}

	second, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore on the existing store: %v", err)
	}
	defer func() { _ = second.Close() }()
	if mode := modesOf(t, messagesDBPath())["messages.db"]; mode != 0o600 {
		t.Errorf("messages.db is %04o after the restart, want 0600", mode)
	}
	if !strings.Contains(rec.String(), "[INFO] Tightened "+messagesDBPath()+" to 0600") {
		t.Errorf("want an INFO line naming the file, got:\n%s", rec.String())
	}
	var name string
	if err := second.db.QueryRow("SELECT name FROM chats WHERE jid = ?", mediaTestChat).Scan(&name); err != nil || name != "kept across the restart" {
		t.Errorf("the archive did not survive the chmod: name=%q err=%v", name, err)
	}
}

func TestSecureDatabaseFiles(t *testing.T) {
	skipWithoutModeBits(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	write := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(db, 0o644)
	write(db+"-wal", 0o664)
	write(db+"-shm", 0o640)
	write(db+"-journal", 0o600) // already private: not reported
	write(db+".bak", 0o644)     // not a file SQLite keeps next to the database: left alone

	tightened, err := secureDatabaseFiles(db)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{db, db + "-wal", db + "-shm"}; !slices.Equal(tightened, want) {
		t.Errorf("tightened = %v, want %v", tightened, want)
	}
	for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		if info, _ := os.Stat(name); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %04o, want 0600", filepath.Base(name), info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(db + ".bak"); info.Mode().Perm() != 0o644 {
		t.Errorf("an unrelated file was re-moded to %04o", info.Mode().Perm())
	}

	// A database that does not exist yet is created empty and private, and
	// nothing is reported: there was nothing loose.
	fresh := filepath.Join(dir, "fresh.db")
	if tightened, err := secureDatabaseFiles(fresh); err != nil || len(tightened) != 0 {
		t.Fatalf("fresh database: tightened=%v err=%v", tightened, err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Errorf("fresh database: info=%v err=%v, want an empty 0600 file", info, err)
	}

	// A path that cannot be created is an error for the caller to log, not a panic.
	if _, err := secureDatabaseFiles(filepath.Join(dir, "missing", "x.db")); err == nil {
		t.Error("want an error for a database in a directory that does not exist")
	}
}
