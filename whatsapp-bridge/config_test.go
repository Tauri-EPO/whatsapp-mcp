package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func invalidStartupValues() map[string]string {
	return map[string]string{
		forwardSelfEnv: "typo", mediaAutoDownloadEnv: "typo", webhookEnabledEnv: "typo",
		metricsEnv: "typo", webhookForwardStatusEnv: "typo", mediaAutoDownloadStatusEnv: "typo",
		readOnlyEnv: "typo", allowToolsEnv: "missing_allow", denyToolsEnv: "missing_deny",
		bridgePortEnv: "0", bridgeBindEnv: "http://localhost", mediaRetentionEnv: "-1",
		groupRosterSyncEnv: "-1", sessionKeepaliveEnv: "169", mediaMaxBytesEnv: "50MB",
		"WHATSAPP_MEDIA_ROOTS": "relative/outbox", "WHATSAPP_BRIDGE_TOKEN": "tiny-secret",
	}
}

func TestConfigRejectsEveryInvalidValueBeforeEffects(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	store := filepath.Join(t.TempDir(), "not-created")
	t.Setenv(storeDirEnv, store)
	for name, value := range invalidStartupValues() {
		t.Run(name, func(t *testing.T) {
			_, err := parseBridgeConfig(func(key string) string {
				if key == name {
					return value
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want %s", err, name)
			}
		})
	}
	for _, path := range []string{store, filepath.Join(homeDir, defaultOutboxSubpath)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("configuration created %s: %v", path, err)
		}
	}
}

func TestConfigReportsAllInvalidVariablesTogether(t *testing.T) {
	values := invalidStartupValues()
	_, err := parseBridgeConfig(func(name string) string { return values[name] })
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for name := range values {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
	if strings.ContainsAny(err.Error(), "\r\n") || strings.Contains(err.Error(), values["WHATSAPP_BRIDGE_TOKEN"]) {
		t.Fatalf("diagnostic should be one line and omit the token value: %v", err)
	}
}

func TestConfigDefaultsAndParsedValues(t *testing.T) {
	cfg, err := parseBridgeConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 || cfg.Bind != defaultBridgeBind || cfg.MediaMaxBytes != defaultMediaMaxBytes ||
		cfg.RosterSync != groupRosterSyncInterval || cfg.SessionKeepalive != sessionKeepaliveInterval ||
		cfg.MediaRoots != "" || cfg.ReadOnly.enabled {
		t.Fatalf("defaults = %+v", cfg)
	}
	values := map[string]string{
		mediaMaxBytesEnv: "0", mediaRetentionEnv: "2", mediaAutoDownloadStatusEnv: "yes",
		groupRosterSyncEnv: "0", sessionKeepaliveEnv: "0", readOnlyEnv: "on",
		allowToolsEnv: "send_message", denyToolsEnv: "send_file", bridgePortEnv: "9000",
		bridgeBindEnv: "::1", bridgeAllowedHostsEnv: "example.test",
		"WHATSAPP_MEDIA_ROOTS": t.TempDir(), "WEBHOOK_URL": "http://localhost:8769/whatsapp/webhook",
	}
	cfg, err = parseBridgeConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MediaMaxBytes != 0 || cfg.MediaRetention != 48*time.Hour || !cfg.StatusMedia ||
		cfg.RosterSync != 0 || cfg.SessionKeepalive != 0 || !cfg.ReadOnly.enabled ||
		!cfg.Tools.allowsTool("send_message") || cfg.Tools.allowsTool("send_file") ||
		cfg.Bind != "::1" || cfg.AllowedHosts != "example.test" || cfg.Port != 9000 || cfg.MediaRoots != values["WHATSAPP_MEDIA_ROOTS"] {
		t.Fatalf("parsed configuration = %+v", cfg)
	}
}

func TestConfigRejectsDurationOverflow(t *testing.T) {
	for _, name := range []string{mediaRetentionEnv, groupRosterSyncEnv} {
		_, err := parseBridgeConfig(func(key string) string {
			if key == name {
				return "9223372036854775807"
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("overflow accepted for %s: %v", name, err)
		}
	}
}

func TestRESTStartupReturnsListenerFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, nil, newTestMessageStore(t), testLogger())
	b.RESTBind = "127.0.0.1"
	if err := b.startRESTServer(port, "token-0123456789abcdef"); err == nil {
		t.Fatal("occupied listener was reported as a successful start")
	}
}

// Exercise startup's real stores and defers, stopping before any WhatsApp dial.
func TestStartupListenerFailureReleasesStore(t *testing.T) {
	previousLog := bridgeLog
	t.Cleanup(func() { bridgeLog = previousLog })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(storeDirEnv, t.TempDir())
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "token-0123456789abcdef")
	values := map[string]string{bridgePortEnv: portText, "WHATSAPP_MEDIA_ROOTS": t.TempDir(), "WHATSAPP_BRIDGE_TOKEN": "token-0123456789abcdef"}
	cfg, err := parseBridgeConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if exit := runBridge(cfg); exit != 1 {
		t.Fatalf("startup exit=%d, want 1", exit)
	}
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		t.Fatalf("startup kept the store lock: %v", err)
	}
	defer lock.Release()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("reopen store after refused startup: %v", err)
	}
	defer func() { _ = ms.Close() }()
	if err := ms.StoreChat("5511999999999@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
}
