package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreDirDefaultsAndEnv(t *testing.T) {
	t.Setenv(storeDirEnv, "")
	if got := storeDir(); got != "store" {
		t.Fatalf("default storeDir = %q", got)
	}
	dir := t.TempDir()
	t.Setenv(storeDirEnv, dir)
	if got := storeDir(); got != filepath.Clean(dir) {
		t.Fatalf("storeDir = %q, want %q", got, dir)
	}
	if got := tokenFilePath(); got != filepath.Join(dir, ".bridge-token") {
		t.Fatalf("tokenFilePath = %q", got)
	}
	if got := sqliteURI(messagesDBPath(), "a=1"); got != "file:"+filepath.ToSlash(filepath.Join(dir, "messages.db"))+"?a=1" {
		t.Fatalf("sqliteURI = %q", got)
	}
}

// NewMessageStore and the token/lock helpers must honour WHATSAPP_STORE_DIR
// instead of the working directory.
func TestStoreDirIsUsedByStoreTokenAndLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(storeDirEnv, dir)
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	t.Chdir(t.TempDir()) // a different cwd: nothing may land here

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	if _, err := os.Stat(filepath.Join(dir, "messages.db")); err != nil {
		t.Fatalf("messages.db not in store dir: %v", err)
	}
	if _, _, err := loadOrCreateBridgeToken(); err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bridge-token")); err != nil {
		t.Fatalf(".bridge-token not in store dir: %v", err)
	}
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(filepath.Join(dir, ".bridge.lock")); err != nil {
		t.Fatalf(".bridge.lock not in store dir: %v", err)
	}
	if _, err := os.Stat("store"); !os.IsNotExist(err) {
		t.Fatal("a store/ directory was created in the working directory")
	}
}
