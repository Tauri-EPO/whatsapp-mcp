//go:build unix

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPrivateProcessUmaskIncludesSQLiteSidecars(t *testing.T) {
	if os.Getenv("WAMCP_UMASK_FIXTURE") == "1" {
		syscall.Umask(0o022) // Establish a permissive inherited mask in this isolated child.
		privateProcessUmask()
		root := os.Getenv("WAMCP_UMASK_DIRECTORY")
		if err := os.Mkdir(filepath.Join(root, "directory"), 0o777); err != nil { //nolint:gosec // Deliberately permissive request: process umask must restrict it.
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "loose-file"), []byte("fake bytes"), 0o666); err != nil { //nolint:gosec // Deliberately permissive request: process umask must restrict it.
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(root, "unprotected.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE fixture(value TEXT); INSERT INTO fixture VALUES('fake bytes')"); err != nil {
			t.Fatal(err)
		}

		journal, err := sql.Open("sqlite", filepath.Join(root, "journal.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = journal.Close() }()
		journal.SetMaxOpenConns(1)
		if _, err := journal.Exec("PRAGMA journal_mode=DELETE; CREATE TABLE fixture(value TEXT)"); err != nil {
			t.Fatal(err)
		}
		tx, err := journal.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec("INSERT INTO fixture VALUES('fake bytes')"); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"directory", "loose-file", "unprotected.db", "unprotected.db-wal", "unprotected.db-shm", "journal.db", "journal.db-journal"} {
			info, err := os.Stat(filepath.Join(root, name)) //nolint:gosec // Child receives this test's generated t.TempDir and fixed object names only.
			if err != nil || info.Mode().Perm()&0o077 != 0 {
				t.Fatalf("creation mask did not protect %s", name)
			}
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestPrivateProcessUmaskIncludesSQLiteSidecars$") //nolint:gosec // Test binary running this isolated process-wide umask fixture.
	cmd.Env = append(os.Environ(), "WAMCP_UMASK_FIXTURE=1", "WAMCP_UMASK_DIRECTORY="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("umask child: %v\n%s", err, out)
	}
}
