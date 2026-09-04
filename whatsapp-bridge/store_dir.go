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
	"os"
	"path/filepath"
	"strings"
)

const storeDirEnv = "WHATSAPP_STORE_DIR"
const defaultStoreDir = "store"

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

func whatsmeowDBPath() string  { return storePath("whatsapp.db") }
func messagesDBPath() string   { return storePath("messages.db") }
func tokenFilePath() string    { return storePath(".bridge-token") }
func instanceLockPath() string { return storePath(".bridge.lock") }
