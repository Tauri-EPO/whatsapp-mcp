//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestUnlinkRecoveryRunBridgeHelper(t *testing.T) {
	if os.Getenv("WAMCP_TEST_UNLINK_RECOVERY") != "1" {
		return
	}
	factory := func(device *store.Device, logger waLog.Logger) *whatsmeow.Client {
		client := whatsmeow.NewClient(device, logger)
		transport := &http.Client{Transport: mcpTestTransport(func(req *http.Request) (*http.Response, error) {
			outcome := "fresh"
			if device.ID != nil {
				outcome = "revoked"
			}
			if err := os.WriteFile(storePath(".fake-startup-dial"), []byte(outcome), storeFileMode); err != nil {
				return nil, err
			}
			return nil, errors.New("fake startup transport refusal")
		})}
		client.SetWebsocketHTTPClient(transport)
		client.SetPreLoginHTTPClient(transport)
		return client
	}
	cfg, err := loadBridgeConfig()
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(runBridgeWithClientFactory(cfg, factory))
}

func TestPendingUnlinkStartupRunsCleanupAndFreshDialWithoutOperator(t *testing.T) {
	b, container := revokedSessionFixture(t)
	id := b.currentClient().Store.ID.String()
	b.handleClientEvent(b.currentClient(), &events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}, make(chan bool, 1))
	saved, err := readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil || saved.RevokedDevice != id {
		t.Fatalf("real unlink path did not record identity: %+v %v", saved, err)
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
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnlinkRecoveryRunBridgeHelper$") //nolint:gosec // Runs this test binary with a fake SDK transport and private fixture.
	cmd.Env = append(os.Environ(), "WAMCP_TEST_UNLINK_RECOVERY=1", storeDirEnv+"="+storeDir(), "WHATSAPP_BRIDGE_TOKEN=fake-startup-signal-token-0123456789", bridgePortEnv+"="+port, bridgeBindEnv+"=127.0.0.1", "WEBHOOK_ENABLED=false", "WHATSAPP_MEDIA_ROOTS="+t.TempDir(), logLevelEnv+"=INFO", operatorBindEnv+"=", "WHATSAPP_PAIRING_STDOUT=false")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for {
		data, err := os.ReadFile(storePath(".fake-startup-dial"))
		if err == nil {
			if string(data) != "fresh" {
				t.Fatal("startup connected revoked credentials")
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("startup exited before recovery: %v\n%s", err, &output)
		case <-ctx.Done():
			<-done
			t.Fatalf("pending cleanup never resumed through runBridge: %s", &output)
		case <-time.After(10 * time.Millisecond):
		}
	}
	saved, err = readConnectionProblem(b.StoreRoot)
	if err != nil || saved == nil || !saved.SessionRemoved || saved.RevokedDevice != id {
		t.Fatalf("binary did not publish verified removal: %+v %v", saved, err)
	}
	devices, err := container.GetAllDevices(b.ctx)
	if err != nil || len(devices) != 0 {
		t.Fatal("binary left revoked session rows")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer fake-startup-signal-token-0123456789")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	err = json.NewDecoder(response.Body).Decode(&body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 200 || body["paired"] != false {
		t.Fatalf("binary recovery health=%v %v", body, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown=%v\n%s", err, &output)
		}
	case <-ctx.Done():
		t.Fatal("recovered binary did not shut down")
	}
	if strings.Contains(output.String(), "could not be initialized") || strings.Contains(output.String(), "QR sequence expired") {
		t.Fatalf("shutdown emitted false pairing errors: %s", &output)
	}
	if !errors.Is(ctx.Err(), nil) {
		t.Fatal("recovery proof used its timeout")
	}
}
