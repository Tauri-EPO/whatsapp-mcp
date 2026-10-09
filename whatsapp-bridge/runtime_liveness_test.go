package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeStatusHTTPRemainsResponsiveDuringDownloadAndPendingHandoff(t *testing.T) {
	first := newTestClient(&mockLIDStore{})
	b := testBridge(t, first, newTestMessageStore(t), testLogger())
	b.bindRuntimeClient()
	b.Connected = func() bool { return true } // Only the external paired socket is replaced.
	reconnect := make(chan bool, 1)
	b.installClient(first, false, reconnect)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	b.DownloadMedia = func(ctx context.Context, _, _ string) (bool, string, string, string, error) {
		close(entered)
		select {
		case <-unblock:
			return false, "", "", "", errors.New("fake blocked transfer released")
		case <-ctx.Done():
			return false, "", "", "", ctx.Err()
		}
	}
	const token = "fake-data-plane-token-0123456789abcdef" //nolint:gosec // Public fake HTTP credential for the authenticated deny-path fixture.
	server := httptest.NewServer(b.runtimeRESTHandler(8080, token))
	t.Cleanup(server.Close)
	request := func(client *http.Client, method, path, bearer, host, body string) (int, error) {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Host = host
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode, err
	}
	download := &http.Client{Timeout: 10 * time.Second}
	downloadDone := make(chan error, 1)
	go func() {
		_, err := request(download, "POST", "/api/download", token, "127.0.0.1:8080", `{"message_id":"BLOCKED","chat_jid":"111@s.whatsapp.net"}`)
		downloadDone <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("authenticated download did not enter its runtime handler")
	}
	handoffDone := make(chan struct{})
	go func() { b.installClient(newTestClient(&mockLIDStore{}), false, reconnect); close(handoffDone) }()
	// The download owns a read lock. TryRLock refuses only after the writer
	// is pending, establishing the old healthcheck blockage without a sleep.
	for b.clientGate.TryRLock() {
		b.clientGate.RUnlock()
		if ctx.Err() != nil {
			t.Fatal("handoff writer did not become pending")
		}
		runtime.Gosched()
	}
	probe := &http.Client{Timeout: time.Second}
	for _, item := range []struct {
		method, path, bearer, host string
		want                       int
	}{
		{"GET", "/api/health", token, "127.0.0.1:8080", 200},
		{"GET", "/api/ready", token, "127.0.0.1:8080", 503},
		{"GET", "/api/version", "", "127.0.0.1:8080", 200},
		{"GET", "/metrics", "", "127.0.0.1:8080", 200},
		{"GET", "/api/health", "", "127.0.0.1:8080", 401},
		{"POST", "/api/health", token, "127.0.0.1:8080", 405},
		{"GET", "/api/health", token, "example.invalid", 403},
	} {
		code, err := request(probe, item.method, item.path, item.bearer, item.host, "")
		if err != nil || code != item.want {
			t.Fatalf("state endpoint %s: code=%d want=%d err=%v", item.path, code, item.want, err)
		}
	}
	release()
	select {
	case <-handoffDone:
	case <-ctx.Done():
		t.Fatal("released download still blocked handoff")
	}
	if err := <-downloadDone; err != nil {
		t.Fatal(err)
	}
}
