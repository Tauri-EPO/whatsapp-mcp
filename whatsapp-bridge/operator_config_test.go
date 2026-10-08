package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeOperatorToken = "5bb46abd5967c7ca28b4df05606834ebe5f01266e25c595c73d8295a7e985569"

func operatorTestValues(overrides map[string]string) func(string) string {
	values := map[string]string{operatorBindEnv: "127.0.0.1", operatorTokenEnv: fakeOperatorToken, "WHATSAPP_BRIDGE_TOKEN": "fake-data-plane-token-0123456789abcdef"} //nolint:gosec // Deliberately fake credentials for separate-token refusal tests.
	for key, value := range overrides {
		values[key] = value
	}
	return func(name string) string { return values[name] }
}

func noOperatorLookup(context.Context, string) ([]net.IPAddr, error) {
	return nil, errors.New("unexpected DNS lookup")
}

func TestOperatorConfigDefaultsOff(t *testing.T) {
	cfg, err := parseOperatorConfig(func(string) string { return "" }, noOperatorLookup)
	if err != nil || cfg.Bind != "" || cfg.Token != "" || cfg.Port != 8090 {
		t.Fatalf("off=%t port=%d err=%v", cfg.Bind == "", cfg.Port, err)
	}
	defaults, err := parseBridgeConfig(func(string) string { return "" })
	if err != nil || !defaults.PairingStdout {
		t.Fatal("default QR stdout behavior changed")
	}
	disabled, err := parseBridgeConfig(func(name string) string {
		if name == pairingStdoutEnv {
			return "false"
		}
		return ""
	})
	if err != nil || disabled.PairingStdout {
		t.Fatal("QR stdout opt-out was ignored")
	}
}

func TestOperatorConfigRefusesUnsafeBindsTokensAndPorts(t *testing.T) {
	for _, values := range []map[string]string{
		{operatorBindEnv: "0.0.0.0"}, {operatorBindEnv: "::"}, {operatorBindEnv: "[::]"}, {operatorBindEnv: "*"}, {operatorBindEnv: "127.0.0.1:8090"},
		{operatorTokenEnv: ""}, {operatorTokenEnv: "short-secret"}, {operatorTokenEnv: strings.Repeat("a", 64)}, {operatorTokenEnv: "change-me-0123456789abcdefghijklmnop"},
		{operatorTokenEnv: "fake-token-0123456789abcdef\nwith-space"}, {operatorTokenEnv: fakeOperatorToken, "WHATSAPP_BRIDGE_TOKEN": fakeOperatorToken},
		{operatorTokenEnv: strings.Repeat("12345678", 8)}, {operatorTokenEnv: strings.Repeat("abcdefgh", 8)},
		{operatorPortEnv: "0"}, {operatorPortEnv: "65536"}, {operatorPortEnv: "bad"}, {operatorAllowedHostsEnv: "*"}, {operatorAllowedHostsEnv: "*.example.test"},
	} {
		if _, err := parseOperatorConfig(operatorTestValues(values), noOperatorLookup); err == nil {
			t.Fatal("unsafe operator configuration accepted")
		}
	}
	for _, bind := range []string{"127.0.0.1", "::1", "[::1]", "192.0.2.10"} {
		cfg, err := parseOperatorConfig(operatorTestValues(map[string]string{operatorBindEnv: bind}), noOperatorLookup)
		if err != nil || cfg.Bind == "" {
			t.Fatalf("explicit bind refused: %v", err)
		}
	}
}

func TestOperatorTokenRequiresEncodedRandomBytes(t *testing.T) {
	bytes, err := hex.DecodeString(fakeOperatorToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{fakeOperatorToken, base64.RawURLEncoding.EncodeToString(bytes)} {
		if err := validateOperatorToken(token); err != nil {
			t.Fatalf("fake encoded credential refused: %v", err)
		}
	}
	for _, token := range []string{
		base64.RawURLEncoding.EncodeToString(bytes[:31]),
		base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), bytes...), bytes...)),
		fakeOperatorToken[:20] + "\n" + fakeOperatorToken[20:],
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("abcdefgh", 4))),
	} {
		if err := validateOperatorToken(token); err == nil || !strings.Contains(err.Error(), "openssl rand -hex 32") {
			t.Fatal("weak encoding accepted or generation guidance missing")
		}
	}
}

func TestOperatorConfigurationDefaultsPrivateAndRefusesPortCollision(t *testing.T) {
	cfg, err := parseBridgeConfig(operatorTestValues(nil))
	if err != nil || cfg.PairingStdout {
		t.Fatalf("operator default exposes pairing stdout: %v", err)
	}
	if _, err := parseBridgeConfig(operatorTestValues(map[string]string{operatorPortEnv: "8080"})); err == nil {
		t.Fatal("default data port collision accepted")
	}
	if _, err := parseBridgeConfig(operatorTestValues(map[string]string{operatorPortEnv: "8081", bridgePortEnv: "8081"})); err == nil {
		t.Fatal("custom data port collision accepted")
	}
}

func TestOperatorConfigResolvesOnlyOneAddress(t *testing.T) {
	for _, addresses := range [][]net.IPAddr{
		nil, {{IP: net.ParseIP("0.0.0.0")}}, {{IP: net.ParseIP("192.0.2.10")}, {IP: net.ParseIP("192.0.2.11")}},
	} {
		lookup := func(context.Context, string) ([]net.IPAddr, error) { return addresses, nil }
		if _, err := parseOperatorConfig(operatorTestValues(map[string]string{operatorBindEnv: "operator.example.test"}), lookup); err == nil {
			t.Fatal("ambiguous or unsafe resolution accepted")
		}
	}
	lookup := func(ctx context.Context, hostname string) ([]net.IPAddr, error) {
		if _, ok := ctx.Deadline(); !ok || hostname != "operator.example.test" {
			t.Fatal("resolution not bounded or wrong hostname")
		}
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil
	}
	cfg, err := parseOperatorConfig(operatorTestValues(map[string]string{operatorBindEnv: "operator.example.test"}), lookup)
	if err != nil || cfg.Bind != "192.0.2.10" {
		t.Fatalf("single resolved bind=%s err=%v", cfg.Bind, err)
	}
}

func TestOperatorTokenFileIsBoundedPrivateAndDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(path, []byte(fakeOperatorToken+"\n"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	values := operatorTestValues(map[string]string{operatorTokenEnv: "", operatorTokenFileEnv: path})
	cfg, err := parseOperatorConfig(values, noOperatorLookup)
	if err != nil || cfg.Token != fakeOperatorToken {
		t.Fatalf("private token file refused: %v", err)
	}
	if err := cfg.refuseBridgeToken(fakeOperatorToken); err == nil {
		t.Fatal("effective fallback bridge token accepted on operator")
	}
	if _, err := parseOperatorConfig(operatorTestValues(map[string]string{operatorTokenFileEnv: path}), noOperatorLookup); err == nil {
		t.Fatal("two token sources accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // Deliberately unsafe fixture mode; the next assertion requires refusal.
			t.Fatal(err)
		}
		if _, err := readOperatorToken(path); err == nil {
			t.Fatal("group-readable token accepted")
		}
		if err := os.Chmod(path, storeFileMode); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "operator-token-link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readOperatorToken(link); err == nil {
			t.Fatal("symlink token accepted")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 258)), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := readOperatorToken(path); err == nil {
		t.Fatal("oversized token file accepted")
	}
}
