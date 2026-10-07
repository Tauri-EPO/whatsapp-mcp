//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The deny path itself: on a store whose directory an operator created
// world-searchable (a bind mount made with mkdir), another user of the machine
// cannot read the archive. The process has to be root to become that other
// user, so this runs in the Docker gates and is skipped where tests run
// unprivileged.
func TestAnotherUserCannotReadTheDatabases(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a command as another user")
	}
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat to read the file with")
	}
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

	asNobody := func(path string) error {
		cmd := exec.Command(cat, path) //nolint:gosec // a fixed binary reading a file this test created
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		return cmd.Run()
	}
	// The control: the same user does read a file that is open to others, so a
	// refusal below is the mode and not the directory or the fixture.
	readable := filepath.Join(store, "readable.txt")
	if err := os.WriteFile(readable, []byte("x"), 0o644); err != nil { //nolint:gosec // the readable file is the control
		t.Fatal(err)
	}
	if err := asNobody(readable); err != nil {
		t.Fatalf("the other user cannot even read a 0644 file here (%v): the test proves nothing", err)
	}
	for _, name := range []string{"messages.db", "messages.db-wal", "messages.db-shm"} {
		if err := asNobody(filepath.Join(store, name)); err == nil {
			t.Errorf("another user read %s", name)
		}
	}
}
