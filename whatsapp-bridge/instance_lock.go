package main

// Single-instance lock.
//
// WhatsApp allows one live connection per linked device. Two bridges sharing
// a store therefore evict each other in a loop (<stream:error><conflict
// type="replaced"/>): each reconnect kicks the other out, neither reliably
// persists incoming messages, and /api/health on whichever survives still
// says connected:true. It is easy to hit by accident — a service manager
// already runs the bridge and someone starts `./whatsapp-bridge` in a shell
// "just to check".
//
// The fix is to claim an exclusive OS-level lock on store/.bridge.lock before
// touching the session database or dialling WhatsApp, and refuse to start if
// it is held. The lock belongs to the open file description, so the kernel
// releases it when the holder exits or crashes: a dead bridge can never lock
// its own restart out, and no stale-lockfile cleanup is needed. The holder's
// PID is written into the file purely so the refusal message can name it.
//
// Platform specifics (flock vs LockFileEx) live in instance_lock_unix.go and
// instance_lock_windows.go.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The lock file lives at instanceLockPath() inside the store directory (see
// store_dir.go).

// errInstanceLocked is returned by acquireInstanceLock when another process
// already holds the lock.
var errInstanceLocked = errors.New("another whatsapp-bridge already holds this store")

// instanceLock is a held single-instance lock. Release it with Release; it is
// also released automatically by the OS when the process exits.
type instanceLock struct {
	f *os.File
}

// acquireInstanceLock opens (creating if needed) the lock file at path and
// takes an exclusive, non-blocking lock on it. On success the holder's PID is
// written into the file. If another process holds the lock, the returned
// error wraps errInstanceLocked and, when readable, names the holder's PID.
func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := lockFileExclusive(f); err != nil {
		holder := readLockHolder(f)
		_ = f.Close()
		if holder != "" {
			return nil, fmt.Errorf("%w (pid %s, lock file %s)", errInstanceLocked, holder, path)
		}
		return nil, fmt.Errorf("%w (lock file %s)", errInstanceLocked, path)
	}
	// Record who holds it. Best effort: the lock, not the content, is what
	// enforces exclusivity.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &instanceLock{f: f}, nil
}

// Release drops the lock and closes the file. Safe to call on a nil receiver.
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}

// readLockHolder returns the PID recorded in the lock file, or "" if it cannot
// be read or parsed.
func readLockHolder(f *os.File) string {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if n == 0 && err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(buf[:n]))
	if _, convErr := strconv.Atoi(pid); convErr != nil {
		return ""
	}
	return pid
}
