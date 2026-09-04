package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAcquireInstanceLock_RecordsPIDAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bridge.lock")

	lock, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file records %q, want own pid %d", got, os.Getpid())
	}

	lock.Release()
	// Releasing must make the lock available again in the same process.
	again, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	again.Release()
	again.Release() // idempotent
	var nilLock *instanceLock
	nilLock.Release() // nil-safe
}

func TestAcquireInstanceLock_SecondHolderIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bridge.lock")

	lock, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lock.Release()

	// A second open file description on the same file (what a second bridge
	// process would have) must be refused while the first is held.
	second, err := acquireInstanceLock(path)
	if err == nil {
		second.Release()
		t.Fatal("second acquire succeeded; expected errInstanceLocked")
	}
	if !errors.Is(err, errInstanceLocked) {
		t.Fatalf("error = %v, want errInstanceLocked", err)
	}
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("refusal should name the holder pid; got %q", err)
	}
}

// TestAcquireInstanceLock_ReleasedWhenHolderDies runs a helper process that
// takes the lock and exits without releasing it, then checks the lock can be
// taken again: a crashed bridge must never lock its own restart out.
func TestAcquireInstanceLock_ReleasedWhenHolderDies(t *testing.T) {
	if os.Getenv("BRIDGE_LOCK_HELPER") != "" {
		if _, err := acquireInstanceLock(os.Getenv("BRIDGE_LOCK_PATH")); err != nil {
			os.Exit(3)
		}
		os.Exit(0) // exit while still holding the lock
	}

	path := filepath.Join(t.TempDir(), ".bridge.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireInstanceLock_ReleasedWhenHolderDies$")
	cmd.Env = append(os.Environ(), "BRIDGE_LOCK_HELPER=1", "BRIDGE_LOCK_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := acquireInstanceLock(path)
		if err == nil {
			lock.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock still held after holder exited: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
