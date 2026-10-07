package main

// Store directory resolution.
//
// Everything the bridge persists — whatsapp.db (session), messages.db, the
// downloaded media tree, .bridge-token and .bridge.lock — lives under one
// directory. It used to be hard-wired to "store/" relative to the working
// directory, which silently created a second, empty store (and a second
// pairing) whenever the binary was started from another folder. The
// directory is now WHATSAPP_STORE_DIR (absolute or relative), defaulting to
// "./store" so existing setups keep working.

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const storeDirEnv = "WHATSAPP_STORE_DIR"
const defaultStoreDir = "store"

// storeDirMode is the mode of every directory the bridge creates in the store,
// the store itself included: owner only, because they hold the session keys and
// the whole archive. It applies to directories created from now on; one that
// already exists keeps the mode it has.
const storeDirMode os.FileMode = 0o700

// storeFileMode is the mode of the two SQLite databases and of the files the
// driver keeps next to them: owner only, like the media files and the token.
const storeFileMode os.FileMode = 0o600

// privateDatabase makes the SQLite database at path owner-only before the
// driver opens it, and says so in the log when it had to change something.
//
// The driver creates a database 0644 (minus the umask) and gives the
// write-ahead log and the shared-memory file the mode the database has at that
// moment (issue #491). So the database is created here, empty and 0600, when it
// does not exist yet, and a database an earlier release left readable by group
// or others is tightened together with whatever -wal / -shm / -journal a
// previous run left behind. The directory is not touched: it keeps the mode it
// has (storeDirMode). Both processes run as one user in the images, so the MCP
// server reads the files as before.
//
// A failure is logged and startup goes on: the open that follows either works
// or reports its own error.
func privateDatabase(path string) {
	tightened, loose, err := secureDatabaseFiles(path)
	if len(tightened) > 0 {
		bridgeLog.Infof("Tightened %s to %04o: group or others could read it (both processes must run as the same user)", strings.Join(tightened, ", "), storeFileMode)
	}
	if len(loose) > 0 {
		bridgeLog.Warnf("%s stays readable by group or others: this file system does not keep Unix permissions (a bind mount from a Windows or macOS host, CIFS, exFAT)", strings.Join(loose, ", "))
	}
	if err != nil {
		bridgeLog.Warnf("Could not make %s owner-only: %v", path, err)
	}
}

// secureDatabaseFiles creates path with storeFileMode when it is missing and
// removes group and other access from it and from its SQLite siblings. It
// returns the files whose mode it changed, and the ones that are still loose
// after the chmod because the file system ignored it.
//
// Only regular files are touched, and nothing is opened through a name that
// already exists: chmod and a plain create follow a symlink to wherever it
// points, so a link or a directory under one of these names is left alone.
func secureDatabaseFiles(path string) (tightened, loose []string, err error) {
	if runtime.GOOS == "windows" {
		// No owner/group/other bits to set there; every file would read as loose.
		return nil, nil, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, storeFileMode) //nolint:gosec // path is the operator-configured database inside the store directory
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return nil, nil, err
		}
	case !errors.Is(err, fs.ErrExist):
		return nil, nil, err
	}
	for _, name := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		info, err := os.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return tightened, loose, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&^storeFileMode == 0 {
			continue
		}
		if err := os.Chmod(name, storeFileMode); err != nil {
			return tightened, loose, err
		}
		if after, err := os.Lstat(name); err == nil && after.Mode().Perm()&^storeFileMode != 0 {
			loose = append(loose, name)
			continue
		}
		tightened = append(tightened, name)
	}
	return tightened, loose, nil
}

// storeDir returns the configured store directory (not cleaned or created).
func storeDir() string {
	if v := strings.TrimSpace(os.Getenv(storeDirEnv)); v != "" {
		return filepath.Clean(v)
	}
	return defaultStoreDir
}

// storePath joins elem onto the store directory.
func storePath(elem ...string) string {
	return filepath.Join(append([]string{storeDir()}, elem...)...)
}

// openStoreRoot opens the store directory as an os.Root. Everything that walks,
// measures or deletes inside the store, the inbound media write path and the
// read that feeds an image to the webhook go through that handle
// (Bridge.StoreRoot):
// the kernel resolves each path component within the directory and refuses any
// component that leaves it, including a symlink swapped in between the check and
// the syscall. That is a control, where a filepath.Rel comparison on a name we
// looked up earlier is only a check. The directory must already exist — main()
// creates it first.
func openStoreRoot() (*os.Root, error) {
	return os.OpenRoot(storeDir())
}

// sqliteURI builds a `file:` DSN for a store file. SQLite URIs want forward
// slashes even on Windows.
func sqliteURI(path, query string) string {
	uri := "file:" + filepath.ToSlash(path)
	if query != "" {
		uri += "?" + query
	}
	return uri
}

// SQLite DSN options for every handle the bridge opens (modernc.org/sqlite
// syntax). WAL lets readers (the MCP server, our own contact lookups) proceed
// while a writer holds the file; the busy timeout turns "database is locked"
// into a short wait. _time_format=sqlite keeps time.Time columns in the text
// form the cgo driver used ("2006-01-02 15:04:05.999999999-07:00"), so
// existing stores and the MCP server's parser see no difference.
const (
	sqliteTimeFormat      = "_time_format=sqlite"
	sqliteWriterOptions   = "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&" + sqliteTimeFormat
	sqliteReadOnlyOptions = "mode=ro&_pragma=busy_timeout(5000)&" + sqliteTimeFormat
)

// Connection-pool bounds for every handle the bridge opens (issue #471).
// database/sql opens one connection per concurrent goroutine without a bound;
// each carries its own page cache, and SQLite has one WAL writer, so extra
// connections buy memory and SQLITE_BUSY, not throughput. Measured on an
// on-disk WAL store with 32 goroutines storing 150 messages each plus 8
// readers: unbounded peaked at 40 open connections and failed 300 to 2,300 of
// 4,800 writes with SQLITE_BUSY; 8 connections failed up to 153; 1 to 4
// failed none or a couple, and finished no slower. The floor is 2 (a rows
// cursor held while the loop writes, as the startup backfills do) and the
// choice is 4, which leaves room for a cursor, a transaction and a reader.
const (
	messagesPoolConns = 4 // messages.db, read and written by the bridge
	sessionPoolConns  = 4 // whatsapp.db, whatsmeow's own store
	contactsPoolConns = 2 // whatsapp.db opened read-only for contact lookups
)

// boundPool caps the open connections of db and keeps as many idle, so they
// are reused instead of closed and reopened.
func boundPool(db *sql.DB, conns int) {
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
}

// openSessionDB opens whatsmeow's session database (whatsapp.db) with its pool
// bounded; main hands it to sqlstore.NewWithDB.
func openSessionDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteURI(whatsmeowDBPath(), sqliteWriterOptions))
	if err != nil {
		return nil, err
	}
	boundPool(db, sessionPoolConns)
	return db, nil
}

func whatsmeowDBPath() string  { return storePath("whatsapp.db") }
func messagesDBPath() string   { return storePath("messages.db") }
func tokenFilePath() string    { return storePath(".bridge-token") }
func instanceLockPath() string { return storePath(".bridge.lock") }
