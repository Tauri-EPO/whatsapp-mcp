//go:build unix

package main

// The SQLite databases, and the files the driver keeps next to them, are
// owner-only: created that way on a fresh store, tightened on one an earlier
// release left readable by group or others (issue #491). Unix only: there are
// no such bits to look at on Windows.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// usualUmask runs the test under umask 022, whatever the shell that started
// `go test` uses: these tests are about what the driver and the bridge do with
// the default every container and most hosts have. Tests of this package do not
// run in parallel, so changing the process umask for one of them is safe.
func usualUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// permOf is the permission bits of an existing file.
func permOf(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// requireOwnerOnly fails unless the database and both of its WAL siblings exist
// and none of them grants anything to group or others.
func requireOwnerOnly(t *testing.T, name string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if mode := permOf(t, name+suffix); mode != 0o600 {
			t.Errorf("%s%s is %04o, want 0600", filepath.Base(name), suffix, mode)
		}
	}
}

func TestDatabasesAreCreatedOwnerOnly(t *testing.T) {
	usualUmask(t)
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
	usualUmask(t)
	path := filepath.Join(t.TempDir(), "plain.db")
	container, err := sqlstore.New(context.Background(), "sqlite", sqliteURI(path, sqliteWriterOptions), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New: %v", err)
	}
	defer func() { _ = container.Close() }()
	if mode := permOf(t, path); mode&0o077 == 0 {
		t.Fatalf("the driver created the database %04o by itself; privateDatabase may no longer be needed", mode)
	}
}

func TestExistingLooseDatabaseIsTightenedAtStartup(t *testing.T) {
	usualUmask(t)
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
	if mode := permOf(t, messagesDBPath()); mode != 0o600 {
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
	usualUmask(t)
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

	tightened, loose, err := secureDatabaseFiles(db)
	if err != nil || len(loose) != 0 {
		t.Fatalf("loose=%v err=%v", loose, err)
	}
	if want := []string{db, db + "-wal", db + "-shm"}; !slices.Equal(tightened, want) {
		t.Errorf("tightened = %v, want %v", tightened, want)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if mode := permOf(t, db+suffix); mode != 0o600 {
			t.Errorf("x.db%s is %04o, want 0600", suffix, mode)
		}
	}
	if mode := permOf(t, db+".bak"); mode != 0o644 {
		t.Errorf("an unrelated file was re-moded to %04o", mode)
	}

	// A database that does not exist yet is created empty and private, and
	// nothing is reported: there was nothing loose.
	fresh := filepath.Join(dir, "fresh.db")
	if tightened, loose, err := secureDatabaseFiles(fresh); err != nil || len(tightened)+len(loose) != 0 {
		t.Fatalf("fresh database: tightened=%v loose=%v err=%v", tightened, loose, err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 0 {
		t.Errorf("fresh database: info=%v err=%v, want an empty 0600 file", info, err)
	}

	// A path that cannot be created is an error for the caller to log, not a panic.
	if _, _, err := secureDatabaseFiles(filepath.Join(dir, "missing", "x.db")); err == nil {
		t.Error("want an error for a database in a directory that does not exist")
	}
}

// chmod and a plain create follow a symlink, so a link under one of the names
// would have its target, wherever it is, re-moded or created. Only regular
// files are touched.
func TestSecureDatabaseFilesLeavesLinksAndDirectoriesAlone(t *testing.T) {
	usualUmask(t)
	dir := t.TempDir()
	outside := t.TempDir()
	db := filepath.Join(dir, "x.db")
	target := filepath.Join(outside, "someone-elses.conf")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil { //nolint:gosec // a file other accounts read on purpose is the fixture
		t.Fatal(err)
	}
	// The database name is a dangling link, a sibling is a link to a file
	// outside the store, another sibling is a directory.
	missing := filepath.Join(outside, "created-through-the-link.db")
	symlinkOrSkip(t, missing, db)
	symlinkOrSkip(t, target, db+"-wal")
	if err := os.Mkdir(db+"-journal", 0o755); err != nil { //nolint:gosec // a directory under the journal name is the fixture
		t.Fatal(err)
	}

	tightened, loose, err := secureDatabaseFiles(db)
	if err != nil || len(tightened)+len(loose) != 0 {
		t.Fatalf("tightened=%v loose=%v err=%v, want nothing touched", tightened, loose, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("the dangling link had its target created: %v", err)
	}
	if mode := permOf(t, target); mode != 0o644 {
		t.Errorf("a file outside the store was re-moded to %04o through the link", mode)
	}
	if mode := permOf(t, db+"-journal"); mode != 0o755 {
		t.Errorf("a directory was re-moded to %04o", mode)
	}
}

// The deny path itself: on a store whose directory an operator created
// world-searchable (a bind mount made with mkdir), another user of the machine
// cannot read the archive. The process has to be root, and allowed to become
// that other user, so this runs in the Docker gates and is skipped where tests
// run unprivileged.
func TestAnotherUserCannotReadTheDatabases(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a command as another user")
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat to read the file with")
	}
	usualUmask(t)
	// A directory chain anyone may walk, ending in a 0755 store.
	base, err := os.MkdirTemp("/tmp", "wamcp-mode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	store := filepath.Join(base, "store")
	for _, dir := range []string{base, store} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the world-searchable directory is the fixture
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // same: MkdirTemp made it 0700
			t.Fatal(err)
		}
	}
	t.Setenv(storeDirEnv, store)
	installRecordingLogger(t)
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	if err := ms.StoreChat(mediaTestChat, "Test", time.Now()); err != nil {
		t.Fatal(err)
	}

	// asNobody reports whether uid 65534 was refused the file. An error that is
	// not cat's own exit status means the command could not be run as that
	// user at all (no CAP_SETUID, a single-uid namespace): nothing to conclude.
	asNobody := func(path string) (refused bool) {
		t.Helper()
		cmd := exec.Command(cat, path) //nolint:gosec // a fixed binary reading a file this test created
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		err := cmd.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Skipf("cannot run a command as another user here: %v", err)
		}
		return err != nil
	}
	// The control: the same user does read a file that is open to others, so a
	// refusal below is the mode and not the directory or the fixture.
	readable := filepath.Join(store, "readable.txt")
	if err := os.WriteFile(readable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readable, 0o644); err != nil { //nolint:gosec // the readable file is the control
		t.Fatal(err)
	}
	if asNobody(readable) {
		t.Fatal("the other user cannot even read a 0644 file here: the test proves nothing")
	}
	for _, name := range []string{"messages.db", "messages.db-wal", "messages.db-shm"} {
		if !asNobody(filepath.Join(store, name)) {
			t.Errorf("another user read %s", name)
		}
	}
}
