//go:build !windows

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestOperatorSocketRealHealthAndStreamingExport(t *testing.T) {
	b, _ := archiveFixture(t, true)
	path := filepath.Join(t.TempDir(), "operator.sock")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	server, err := startOperatorServer(operatorConfig{Bind: "127.0.0.1", Port: port, Socket: path, Token: fakeOperatorToken, AllowedHosts: "localhost"},
		operatorRoutes{health: b.handleHealth(), export: b.handleExport()}, waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0660 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		t.Fatalf("socket permissions: %v %v", info, err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, tc := range []struct {
		route, host, token string
		status             int
	}{
		{"health", "localhost", fakeOperatorToken, 200}, {"health", "localhost", "", 401},
		{"health", "outside.example.test", fakeOperatorToken, 403}, {"export", "localhost", fakeOperatorToken, 200},
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://localhost/operator/v1/"+tc.route, nil)
		r.Host = tc.host
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %s %v", tc.route, resp.StatusCode, body, readErr)
		}
		if tc.route == "export" {
			archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
			if err != nil || len(archive.File) == 0 {
				t.Fatalf("streamed export invalid: %v", err)
			}
		}
	}
	// TCP continues to answer with the same handler while Unix is enabled.
	r, _ := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/operator/v1/health", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestOperatorSocketRejectsUnsafePathsAndHandlesStale(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(store, "socket"), "relative.sock"} {
		if validateOperatorSocket(path, store, root) == nil {
			t.Fatal("unsafe path accepted")
		}
	}
	path := filepath.Join(root, "operator.sock")
	if err := os.Symlink(store, path); err != nil {
		t.Fatal(err)
	}
	if _, err := listenOperatorSocket(path); err == nil {
		t.Fatal("symlink accepted")
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenOperatorSocket(path); err == nil {
		t.Fatal("regular file accepted")
	}
	_ = os.Remove(path)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := listenOperatorSocketOwned(path, os.Geteuid()+1); err == nil {
		t.Fatal("foreign-owned socket accepted")
	}
	l, err := listenOperatorSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if _, err := listenOperatorSocket(path); err == nil {
		t.Fatal("live socket replaced")
	}
}

func TestOperatorSocketPreservesHomeResolutionError(t *testing.T) {
	t.Setenv("HOME", "")
	err := validateOperatorSocket(filepath.Join(t.TempDir(), "operator.sock"), t.TempDir(), t.TempDir())
	if err == nil || strings.Contains(err.Error(), "outside") || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("original HOME diagnostic lost: %v", err)
	}
}
