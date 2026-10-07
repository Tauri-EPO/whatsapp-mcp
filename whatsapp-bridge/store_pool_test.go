package main

import (
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

// A burst of concurrent writers and readers on an on-disk WAL store must
// neither fail nor open more connections than the bound. Unbounded, the same
// burst failed hundreds of writes with SQLITE_BUSY.
func TestBurstStaysWithinThePoolBound(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.db.Close() }()

	const chats, perChat, readers = 16, 40, 4
	var wg sync.WaitGroup
	var failures, peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if n := int64(ms.db.Stats().OpenConnections); n > peak.Load() {
					peak.Store(n)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for c := 0; c < chats; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			chat := fmt.Sprintf("5511999990%03d@s.whatsapp.net", c)
			if err := ms.StoreChat(chat, "Alice", time.Now()); err != nil {
				failures.Add(1)
				return
			}
			for i := 0; i < perChat; i++ {
				if err := ms.StoreMessage(fmt.Sprintf("M%d", i), chat, "5511999999999", "hello", time.Now(),
					false, "", "", "", nil, nil, nil, 0, ""); err != nil {
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
	wg.Wait()
	close(stop)
	<-done

	if n := failures.Load(); n != 0 {
		t.Errorf("%d operations failed under the burst", n)
	}
	if n := peak.Load(); n > messagesPoolConns {
		t.Errorf("peak open connections = %d, bound is %d", n, messagesPoolConns)
	}
	var stored int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != chats*perChat {
		t.Errorf("stored %d messages, want %d", stored, chats*perChat)
	}
}
