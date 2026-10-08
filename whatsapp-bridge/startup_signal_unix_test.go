//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
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
