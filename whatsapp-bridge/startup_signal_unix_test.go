//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOperatorStartupHelper(t *testing.T) {
	if os.Getenv("WAMCP_TEST_OPERATOR_STARTUP") != "1" {
		return
	}
	os.Exit(run())
}

func TestOperatorStartupRefusesEffectiveStoredBridgeTokenBeforeDatabaseEffects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".bridge-token"), []byte(fakeOperatorToken+"\n"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOperatorStartupHelper$") //nolint:gosec // Re-executes this test binary with a private fake store.
	cmd.Env = append(os.Environ(), "WAMCP_TEST_OPERATOR_STARTUP=1", storeDirEnv+"="+dir, "WHATSAPP_BRIDGE_TOKEN=", operatorBindEnv+"=127.0.0.1", operatorTokenEnv+"="+fakeOperatorToken, operatorTokenFileEnv+"=", "WHATSAPP_MEDIA_ROOTS="+t.TempDir())
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "effective WHATSAPP_BRIDGE_TOKEN") {
		t.Fatalf("equal stored-token refusal: err=%v output=%s", err, output)
	}
	if strings.Contains(string(output), fakeOperatorToken) {
		t.Fatal("operator startup diagnostic exposed the token")
	}
	for _, file := range []string{"whatsapp.db", "messages.db"} {
		if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
			t.Fatal("equal tokens were refused after database effects")
		}
	}
}

func TestStartupBlockedSignalHelper(t *testing.T) {
	if os.Getenv("WAMCP_TEST_BLOCKED_SIGNAL") != "1" {
		return
	}
	cfg, err := loadBridgeConfig()
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(runBridge(cfg))
}

func TestSIGTERMDuringBlockedStartupExitsCleanly(t *testing.T) {
	for _, code := range []int{403, 402} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeConnectionProblem(root, classifyConnectionProblem(code, 0, time.Hour, time.Now())); err != nil {
				t.Fatal(err)
			}
			_ = root.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_, port, _ := net.SplitHostPort(address)
			_ = listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupBlockedSignalHelper$") //nolint:gosec // Re-executes only this test binary with a private fake store.
			cmd.Env = append(os.Environ(), "WAMCP_TEST_BLOCKED_SIGNAL=1", storeDirEnv+"="+dir, "WHATSAPP_BRIDGE_TOKEN=fake-startup-signal-token-0123456789", bridgePortEnv+"="+port, bridgeBindEnv+"=127.0.0.1", "WEBHOOK_ENABLED=false", "WHATSAPP_MEDIA_ROOTS="+t.TempDir(), logLevelEnv+"=INFO")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() { _ = cmd.Process.Kill() }()
			client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
			defer client.CloseIdleConnections()
			for {
				req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/api/health", nil)
				req.Header.Set("Authorization", "Bearer fake-startup-signal-token-0123456789")
				response, err := client.Do(req)
				if err == nil {
					_ = response.Body.Close()
					if response.StatusCode == http.StatusOK {
						break
					}
				}
				select {
				case err := <-done:
					t.Fatalf("startup exited before health: %v\n%s", err, &output)
				case <-ctx.Done():
					t.Fatal("blocked startup did not serve health")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("SIGTERM exit=%v\n%s", err, &output)
				}
			case <-ctx.Done():
				t.Fatal("SIGTERM did not stop blocked startup")
			}
			if strings.Contains(output.String(), "context canceled") || strings.Contains(output.String(), "QR code") {
				t.Fatal("shutdown logged an error or drew a QR")
			}
			if !strings.Contains(output.String(), "Shutting down during connection startup") {
				t.Fatal("clean startup shutdown not observed")
			}
			if ctx.Err() != nil {
				t.Fatalf("shutdown exceeded deadline: %v", ctx.Err())
			}
		})
	}
}

func TestStartupStatusPurgeFailureStillServesHealth(t *testing.T) {
	for _, mode := range []string{"read-only", "runtime-deny", "refused-path", "signal"} {
		t.Run(mode, func(t *testing.T) {
			b := newSettingsBridge(t)
			victim := seedOperatorMedia(t, b, "START", "status@broadcast", "image", time.Hour, 4)
			if mode == "runtime-deny" {
				_, err := b.Store.db.Exec(`INSERT INTO runtime_settings(key,value,updated_at,version) VALUES('tools.deny','["purge_media"]',?,1)`, dbTime(time.Now()))
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "refused-path" {
				if err := os.Remove(victim); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, "../messages.db", victim)
			}
			if mode == "signal" {
				tx, err := b.Store.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				stamp := time.Now().UTC().Truncate(time.Second)
				for i := range 6000 {
					id := fmt.Sprintf("SIGNAL%05d", i)
					_, err := tx.Exec("INSERT INTO messages(id,chat_jid,sender,timestamp,media_type) VALUES(?,?,?,?,?)", id, "status@broadcast", purgeChat, dbTime(stamp), "image")
					if err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
					leaf := mediaFileName("image", stamp, id, "")
					if _, err := writeMediaFile(b.StoreRoot, "status@broadcast/"+leaf, func(f *os.File) error { _, err := f.Write([]byte("x")); return err }); err != nil {
						_ = tx.Rollback()
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeConnectionProblem(b.StoreRoot, classifyConnectionProblem(403, 0, time.Hour, time.Now())); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_, port, _ := net.SplitHostPort(address)
			_ = listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupBlockedSignalHelper$") //nolint:gosec // Re-executes this test binary with a private fake store and a persisted dial block.
			readOnly := "false"
			if mode == "read-only" {
				readOnly = "true"
			}
			cmd.Env = append(os.Environ(), "WAMCP_TEST_BLOCKED_SIGNAL=1", storeDirEnv+"="+storeDir(), "WHATSAPP_BRIDGE_TOKEN=fake-startup-signal-token-0123456789", bridgePortEnv+"="+port, bridgeBindEnv+"=127.0.0.1", "WEBHOOK_ENABLED=false", "WHATSAPP_MEDIA_ROOTS="+t.TempDir(), logLevelEnv+"=INFO", purgeStatusOnStartEnv+"=true", readOnlyEnv+"="+readOnly, denyToolsEnv+"=", allowToolsEnv+"=")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if mode == "signal" {
				writer := &startupPurgeSignalWriter{output: &output, signal: func() { _ = cmd.Process.Signal(syscall.SIGTERM) }}
				cmd.Stdout, cmd.Stderr = writer, writer
			}

			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() { _ = cmd.Process.Kill() }()

			if mode == "signal" {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("signal exit: %v\n%s", err, &output)
					}
				case <-ctx.Done():
					t.Fatal("signal did not cancel startup purge")
				}
				if !strings.Contains(output.String(), "Status startup purge starting") || !strings.Contains(output.String(), "Shutting down during connection startup") {
					t.Fatal(output.String())
				}
				applied, err := migrationApplied(b.Store.db, statusPurgeMarker)
				if err != nil || applied {
					t.Fatal("signal failed to cancel purge", applied, err)
				}
				return
			}
			client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
			defer client.CloseIdleConnections()
			for {
				req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/api/health", nil)
				req.Header.Set("Authorization", "Bearer fake-startup-signal-token-0123456789")
				response, err := client.Do(req)
				if err == nil {
					_ = response.Body.Close()
					if response.StatusCode == 200 {
						break
					}
				}
				select {
				case err := <-done:
					t.Fatalf("startup exited: %v\n%s", err, &output)
				case <-ctx.Done():
					t.Fatal("startup did not serve health")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("exit: %v\n%s", err, &output)
				}
			case <-ctx.Done():
				t.Fatal("startup shutdown hung")
			}
			if !strings.Contains(output.String(), "WARN") || !strings.Contains(output.String(), "Status startup purge incomplete; retry next startup") || strings.Contains(output.String(), "QR code") {
				t.Fatal(output.String())
			}
			applied, err := migrationApplied(b.Store.db, statusPurgeMarker)
			if err != nil || applied {
				t.Fatal("failed purge marked complete", applied, err)
			}
		})
	}
}

type startupPurgeSignalWriter struct {
	output *bytes.Buffer
	signal func()
	sent   bool
}

func (w *startupPurgeSignalWriter) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if !w.sent && strings.Contains(w.output.String(), "Status startup purge starting") {
		w.sent = true
		w.signal()
	}
	return n, err
}
