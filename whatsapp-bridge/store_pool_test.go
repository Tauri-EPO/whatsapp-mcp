package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// A store path with "#", "?" or "%" opens that very file with its options.
// Unescaped, "#" started a URI fragment: a test subdirectory named "...#01"
// truncated the path to a file shared across runs, without WAL or pragmas.
func TestSQLiteURIOpensPathsWithURIDelimiters(t *testing.T) {
	names := []string{"hash#01", "percent%41", "space and+plus"}
	if runtime.GOOS != "windows" {
		names = append(names, "question?mark")
	}
	for _, name := range names {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "messages.db")
		db, err := openMessagesDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var mode string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = db.Close()
		if mode != "wal" {
			t.Errorf("%s: journal_mode=%q, want wal (DSN options lost)", name, mode)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: database not at its path: %v", name, err)
		}
	}
}

// The connector's CheckNamedValue would shadow one the driver added in a
// later release; this fails first, so the connector can delegate to it.
func TestSQLiteDriverHasNoNamedValueChecker(t *testing.T) {
	base, err := sqlite.NewConnector(sqliteURI(filepath.Join(t.TempDir(), "probe.db"), ""))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := base.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, ok := conn.(driver.NamedValueChecker); ok {
		t.Fatal("modernc.org/sqlite connections now check named values: make canonicalTimeConn delegate to that checker")
	}
	if _, ok := conn.(sqliteConn); !ok {
		t.Fatal("modernc.org/sqlite connections no longer implement sqliteConn")
	}
}

// Every production sql.Open or sql.OpenDB must be followed by an explicit pool
// bound, or a burst opens one connection per goroutine (issue #471).
func TestEverySQLOpenIsFollowedByAPoolBound(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	open := regexp.MustCompile(`\bsql\.Open(DB)?\(`)
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
				t.Errorf("%s:%d: sql.Open/OpenDB without a boundPool call in the next lines", name, i+1)
			}
		}
	}
	if sites < 3 {
		t.Fatalf("found %d sql.Open sites, expected the messages, contacts and session handles", sites)
	}
}

// messages.db is written only through openMessagesDB, whose connection stores
// a bound time.Time in dbTime's spelling. A writer handle opened anywhere else
// (the messages writer options, or the store path with any writer options)
// would bring back the raw driver rendering. Read-only handles, such as the
// operator archive's, write nothing and are not checked.
func TestMessagesDBIsOpenedOnlyThroughOpenMessagesDB(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	definition := regexp.MustCompile(`^messagesWriterOptions\s*=`)
	uses := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name) //nolint:gosec // the package's own source files, from a fixed glob
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || definition.MatchString(trimmed) {
				continue
			}
			if strings.Contains(line, "messagesDBPath()") && strings.Contains(line, "WriterOptions") {
				t.Errorf("%s:%d: open messages.db with openMessagesDB (store_dir.go), which stores bound times in dbTime's spelling", name, i+1)
				continue
			}
			if !strings.Contains(line, "messagesWriterOptions") {
				continue
			}
			uses++
			if name != "store_dir.go" || !strings.Contains(line, "openMessagesDBWithOptions(path, messagesWriterOptions)") {
				t.Errorf("%s:%d: open messages.db with openMessagesDB (store_dir.go), which stores bound times in dbTime's spelling", name, i+1)
			}
		}
	}
	if uses != 1 {
		t.Fatalf("found %d uses of messagesWriterOptions, expected the one in openMessagesDB", uses)
	}
}

func TestMessageStorePoolsAreBounded(t *testing.T) {
	// The #471 measurement in store_dir.go found losses with eight connections.
	// Comparing Stats against the same constant alone cannot pin that choice.
	if messagesPoolConns != 4 {
		t.Fatalf("messages pool limit=%d, want measured limit 4", messagesPoolConns)
	}
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

// Saturating an on-disk WAL pool queues all callers behind its bound. One free
// connection then drains every row without SQLite writer contention; concurrent
// writers are exercised independently below.
func TestMessageStorePoolQueuesAndDrainsAllRows(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	acquireCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for i := 0; i < messagesPoolConns; i++ {
		conn, err := ms.db.Conn(acquireCtx)
		if err != nil {
			t.Fatalf("acquiring pool connection %d of %d: %v", i+1, messagesPoolConns, err)
		}
		held = append(held, conn)
	}
	waitsBefore := ms.db.Stats().WaitCount
	const chats, perChat, readers = 8, 40, 4
	var wg sync.WaitGroup
	var failures atomic.Int64
	var errorMu sync.Mutex
	firstErrors := make(map[string]error)
	recordError := func(kind string, err error) {
		if err != nil {
			failures.Add(1)
			errorMu.Lock()
			if firstErrors[kind] == nil {
				firstErrors[kind] = err
			}
			errorMu.Unlock()
		}
	}
	for c := 0; c < chats; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			chat := fmt.Sprintf("5511999990%03d@s.whatsapp.net", c)
			if err := ms.StoreChat(chat, "Alice", time.Now()); err != nil {
				recordError("chat", err)
				return
			}
			for i := 0; i < perChat; i++ {
				recordError("message", ms.StoreMessage(storedMessage{
					ID:         fmt.Sprintf("M%d", i),
					ChatJID:    chat,
					Sender:     "5511999999999",
					Content:    "hello",
					Timestamp:  time.Now(),
					FileLength: 0,
				}))
			}
		}(c)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var n int
				recordError("read", ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n))
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for ms.db.Stats().WaitCount-waitsBefore < chats+readers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := ms.db.Stats()
	available := held[len(held)-1]
	held = held[:len(held)-1] // each connection is closed exactly once
	_ = available.Close()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// This is a row-accounting proof, not a ten-second latency contract.
	// Reserve time to report stats and release held connections on deadlock.
	drainBudget := time.Minute
	if deadline, ok := t.Deadline(); ok {
		drainBudget = min(drainBudget, time.Until(deadline)/2)
	}
	select {
	case <-done:
	case <-time.After(drainBudget):
		t.Fatalf("queued drain did not finish: pool=%+v", ms.db.Stats())
	}
	if stats.WaitCount-waitsBefore < chats+readers || stats.OpenConnections != messagesPoolConns {
		t.Errorf("pool did not queue %d callers: waits=%d stats=%+v", chats+readers, stats.WaitCount-waitsBefore, stats)
	}
	var stored int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if n := failures.Load(); n != 0 || stored != chats*perChat {
		t.Errorf("failures=%d want 0; rows=%d want %d; first errors: chat=%v message=%v read=%v", n, stored, chats*perChat, firstErrors["chat"], firstErrors["message"], firstErrors["read"])
	}
}

// All four connections remain available to twelve simultaneous writers. The
// production budget may lose a row under contention; every such loss must be a
// reported BUSY error, and the stored rows must match that accounting exactly.
func TestConcurrentMessageWritersAccountForEveryBoundedRetryLoss(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for range 4 {
		conn, err := ms.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	connections = nil
	if stats := ms.db.Stats(); stats.OpenConnections != 4 || stats.InUse != 0 || stats.MaxOpenConnections != 4 {
		t.Fatalf("concurrent writers need four free bounded connections: %+v", stats)
	}
	const chats, perChat, writers = 8, 40, 12
	for c := 0; c < chats; c++ {
		if err := ms.StoreChat(fmt.Sprintf("5511999990%03d@s.whatsapp.net", c), "Alice", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, ms, rec)
	type lostRow struct {
		id, chat string
		err      error
	}
	losses := make(chan lostRow, chats*perChat)
	start, done := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			<-start
			for row := writer; row < chats*perChat; row += writers {
				chat := fmt.Sprintf("5511999990%03d@s.whatsapp.net", row/perChat)
				id := fmt.Sprintf("M%d", row%perChat)
				err := b.retryBusy(func() error {
					return ms.StoreMessage(storedMessage{
						ID:         id,
						ChatJID:    chat,
						Sender:     "5511999999999",
						Content:    "hello",
						Timestamp:  time.Now(),
						FileLength: 0,
					})
				})
				if err != nil {
					b.noteStoreFailure("message", id, chat, err)
					losses <- lostRow{id, chat, err}
				}
			}
		}(writer)
	}
	go func() { wg.Wait(); close(losses); close(done) }()
	close(start)
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("concurrent writers did not finish: pool=%+v", ms.db.Stats())
	}
	reported := 0
	errors := errorLines(rec.String())
	for loss := range losses {
		reported++
		if !isBusyError(loss.err) {
			t.Errorf("unexpected failure id=%s chat=%s: %v; want BUSY", loss.id, loss.chat, loss.err)
		}
		found := false
		for _, line := range errors {
			found = found || (strings.Contains(line, loss.id+" ") && strings.Contains(line, loss.chat) && strings.Contains(line, loss.err.Error()))
		}
		if !found {
			t.Errorf("unreported loss id=%s chat=%s error=%v; ERROR lines=%v", loss.id, loss.chat, loss.err, errors)
		}
	}
	var rows, indexed int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if rows != chats*perChat-reported || indexed != rows || len(errors) != reported || b.metrics.storeFailures.Load() != int64(reported) {
		t.Fatalf("rows=%d want %d; indexed=%d want %d; ERRORs=%d counter=%d want %d; errors=%v", rows, chats*perChat-reported, indexed, rows, len(errors), b.metrics.storeFailures.Load(), reported, errors)
	}
	t.Logf("stored=%d reported BUSY losses=%d; ERROR lines=%v", rows, reported, errors)
}
