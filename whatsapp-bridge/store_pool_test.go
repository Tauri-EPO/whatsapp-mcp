package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every production sql.Open must be followed by an explicit pool bound, or a
// burst opens one connection per goroutine (issue #471).
func TestEverySQLOpenIsFollowedByAPoolBound(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	open := regexp.MustCompile(`\bsql\.Open\(`)
	sites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name) //nolint:gosec // the package's own source files, from a fixed glob
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !open.MatchString(line) {
				continue
			}
			sites++
			end := min(i+10, len(lines))
			if !strings.Contains(strings.Join(lines[i:end], "\n"), "boundPool(") {
				t.Errorf("%s:%d: sql.Open without a boundPool call in the next lines", name, i+1)
			}
		}
	}
	if sites < 3 {
		t.Fatalf("found %d sql.Open sites, expected the messages, contacts and session handles", sites)
	}
}

func TestMessageStorePoolsAreBounded(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.db.Close() }()
	if got := ms.db.Stats().MaxOpenConnections; got != messagesPoolConns {
		t.Errorf("messages.db MaxOpenConnections = %d, want %d", got, messagesPoolConns)
	}

	contacts, err := openWhatsmeowContactsDB(emptyWhatsmeowDB(t))
	if err != nil || contacts == nil {
		t.Fatalf("openWhatsmeowContactsDB = %v, %v", contacts, err)
	}
	defer func() { _ = contacts.Close() }()
	if got := contacts.Stats().MaxOpenConnections; got != contactsPoolConns {
		t.Errorf("contacts MaxOpenConnections = %d, want %d", got, contactsPoolConns)
	}
}

// emptyWhatsmeowDB creates an empty whatsapp.db in the test store so the
// read-only contacts handle has a file to open.
func emptyWhatsmeowDB(t *testing.T) string {
	t.Helper()
	path := whatsmeowDBPath()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionPoolIsBounded(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	db, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if got := db.Stats().MaxOpenConnections; got != sessionPoolConns {
		t.Errorf("session MaxOpenConnections = %d, want %d", got, sessionPoolConns)
	}
}

// A burst of concurrent writers and readers on an on-disk WAL store completes
// and stores its rows with the pool bounded. Unbounded, a burst this size
// opened a connection per goroutine and failed writes with SQLITE_BUSY; the
// writers use the live path's bounded busy retry so a busy StoreChat cannot
// skip all 40 of its messages (issue #580). Exhausting the pool first proves
// the burst queues behind its bound independently of SQLite's scheduling.
func TestBurstCompletesWithTheBoundedPool(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.db.Close() }()
	b := testBridge(t, nil, ms, bridgeLog)
	if got := ms.db.Stats().MaxOpenConnections; got != messagesPoolConns {
		t.Fatalf("burst pool bound = %d, want %d", got, messagesPoolConns)
	}
	// Pin every connection until every writer and reader is waiting for one.
	var held []*sql.Conn
	for i := 0; i < messagesPoolConns; i++ {
		conn, err := ms.db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		defer func() { _ = conn.Close() }()
	}
	waitsBefore := ms.db.Stats().WaitCount

	const chats, perChat, readers = 8, 40, 4
	var wg sync.WaitGroup
	var failures atomic.Int64
	for c := 0; c < chats; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			chat := fmt.Sprintf("5511999990%03d@s.whatsapp.net", c)
			if err := b.retryBusy(func() error { return ms.StoreChat(chat, "Alice", time.Now()) }); err != nil {
				failures.Add(1)
				return
			}
			for i := 0; i < perChat; i++ {
				if err := b.retryBusy(func() error {
					return ms.StoreMessage(fmt.Sprintf("M%d", i), chat, "5511999999999", "hello", time.Now(),
						false, "", "", "", nil, nil, nil, 0, "")
				}); err != nil {
					failures.Add(1)
				}
			}
		}(c)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var n int
				if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
					failures.Add(1)
				}
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for ms.db.Stats().WaitCount-waitsBefore < chats+readers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := ms.db.Stats()
	for _, conn := range held {
		_ = conn.Close()
	}
	wg.Wait()
	if stats.WaitCount-waitsBefore < chats+readers || stats.OpenConnections != messagesPoolConns {
		t.Errorf("burst did not queue behind the pool bound: %+v", stats)
	}

	var stored int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if n := failures.Load(); n != 0 || stored != chats*perChat {
		t.Errorf("%d operations failed, %d of %d messages stored", n, stored, chats*perChat)
	}
}
