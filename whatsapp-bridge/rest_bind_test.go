package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveBridgeBind(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", defaultBridgeBind, false},
		{"  ", defaultBridgeBind, false},
		{"0.0.0.0", "0.0.0.0", false},
		{"::", "::", false},
		{"[::1]", "::1", false},
		{"bridge", "bridge", false},
		{"127.0.0.1:8080", "", true}, // port belongs to WHATSAPP_BRIDGE_PORT
		{"http://x", "", true},
	}
	for _, tc := range cases {
		got, err := resolveBridgeBind(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("resolveBridgeBind(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveBridgeBind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestListenAddr(t *testing.T) {
	if got := listenAddr("127.0.0.1", 8080); got != "127.0.0.1:8080" {
		t.Fatalf("listenAddr v4 = %q", got)
	}
	if got := listenAddr("::", 8080); got != "[::]:8080" {
		t.Fatalf("listenAddr v6 = %q", got)
	}
}

func TestBuildHostAllowListDefaultsToLoopback(t *testing.T) {
	list, warn := buildHostAllowList(8080, defaultBridgeBind, "")
	if warn != "" {
		t.Fatalf("unexpected warning for default config: %q", warn)
	}
	for _, h := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if !list.allows(h) {
			t.Errorf("loopback %q should be allowed", h)
		}
	}
	for _, h := range []string{"bridge:8080", "localhost", "127.0.0.1:9090", ""} {
		if list.allows(h) {
			t.Errorf("%q should be refused by the default list", h)
		}
	}
}

func TestBuildHostAllowListEntries(t *testing.T) {
	list, warn := buildHostAllowList(8080, "0.0.0.0", " bridge, Mcp.Example.COM:8443 ,[fd00::1]")
	if warn != "" {
		t.Fatalf("unexpected warning: %q", warn)
	}
	cases := map[string]bool{
		"bridge:8080":              true, // bare entry: any port
		"bridge":                   true, // bare entry: no port
		"BRIDGE:1234":              true,
		"mcp.example.com:8443":     true, // host:port entry: exact
		"mcp.example.com:8080":     false,
		"mcp.example.com":          false,
		"[fd00::1]:8080":           true,
		"127.0.0.1:8080":           true, // loopback always present
		"evil.example.com:8080":    false,
		"bridge.evil.example.com":  false,
		"bridge.evil.example:8080": false,
	}
	for host, want := range cases {
		if got := list.allows(host); got != want {
			t.Errorf("allows(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestBuildHostAllowListWarnings(t *testing.T) {
	list, warn := buildHostAllowList(8080, "0.0.0.0", "")
	if !strings.Contains(warn, bridgeAllowedHostsEnv) {
		t.Fatalf("non-loopback bind without hosts should warn, got %q", warn)
	}
	if list.allows("bridge:8080") {
		t.Fatal("non-loopback bind without hosts must stay loopback-only (fail-safe)")
	}

	list, warn = buildHostAllowList(8080, "0.0.0.0", "*")
	if !strings.Contains(warn, "any Host") {
		t.Fatalf("wildcard should warn, got %q", warn)
	}
	if !list.allows("anything.example:1") || !list.allows("") {
		t.Fatal("wildcard should accept any Host")
	}
}

func TestRESTMuxHonoursAllowedHosts(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.RESTBind, b.RESTAllowedHosts = "0.0.0.0", "bridge"
	mux := b.newRESTMux(8080, token)

	for host, want := range map[string]int{"bridge:8080": http.StatusOK, "other:8080": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/health", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: got %d, want %d", host, rec.Code, want)
		}
	}
}
