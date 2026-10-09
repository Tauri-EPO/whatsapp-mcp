package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDatabasePoolWaitsRemainVisibleThroughHTTP(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	session, err := openSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Exec("CREATE TABLE fixture(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.waDB == nil {
		t.Fatal("real read-only contacts pool missing")
	}
	b := testBridge(t, nil, store, testLogger())
	b.sessionDB = session
	server := httptest.NewServer(b.newRESTMux(8080, "fake-metrics-fixture-token"))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = time.Second
	readMetrics := func() string {
		t.Helper()
		response, err := client.Get(server.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			t.Fatal("metrics blocked or failed")
		}
		return string(data)
	}
	for _, pool := range []struct {
		name string
		db   *sql.DB
	}{{"messages", store.db}, {"session", session}, {"contacts", store.waDB}} {
		t.Run(pool.name, func(t *testing.T) {
			pool.db.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			held, err := pool.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Close() }()
			before := pool.db.Stats().WaitCount
			done := make(chan error, 1)
			go func() { var value int; done <- pool.db.QueryRowContext(ctx, "SELECT 1").Scan(&value) }()
			for pool.db.Stats().WaitCount == before && ctx.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			if ctx.Err() != nil {
				t.Fatal("query did not queue on held real pool")
			}
			body := readMetrics()
			for _, want := range []string{fmt.Sprintf("whatsapp_bridge_db_in_use{pool=%q} 1", pool.name), fmt.Sprintf("whatsapp_bridge_db_wait_total{pool=%q} %d", pool.name, before+1)} {
				if !strings.Contains(body, want+"\n") {
					t.Fatal("HTTP omitted held pool or queued waiter")
				}
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			prefix := fmt.Sprintf("whatsapp_bridge_db_wait_seconds_total{pool=%q} ", pool.name)
			_, tail, ok := strings.Cut(readMetrics(), prefix)
			value, _, _ := strings.Cut(tail, "\n")
			seconds, err := strconv.ParseFloat(value, 64)
			if !ok || err != nil || seconds <= 0 {
				t.Fatal("released real wait duration was not exposed")
			}
		})
	}
}
