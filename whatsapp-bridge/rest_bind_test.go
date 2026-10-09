package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitBridgeBindPinsOnlyDistinctLocalAddress(t *testing.T) {
	_, local, err := net.ParseCIDR("192.0.2.10/24")
	if err != nil {
		t.Fatal(err)
	}
	local.IP = net.ParseIP("192.0.2.10")
	for _, tc := range []struct {
		name, bind, hosts, operator string
		ips                         []string
		lookupErr, localErr         bool
		want                        string
	}{
		{"split", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"192.0.2.10"}, false, false, "192.0.2.10"},
		{"split without operator", "bridge-agent", "bridge-agent:8080", "", []string{"192.0.2.10"}, false, false, "192.0.2.10"},
		{"duplicate DNS", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"192.0.2.10", "192.0.2.10"}, false, false, "192.0.2.10"},
		{"loopback", "127.0.0.1", "", "192.0.2.20", nil, false, false, "127.0.0.1"},
		{"legacy off operator", "0.0.0.0", "", "", nil, false, false, "0.0.0.0"},
		{"wildcard v4", "0.0.0.0", "bridge-agent:8080", "192.0.2.20", nil, false, false, ""},
		{"wildcard v6", "::", "bridge-agent:8080", "192.0.2.20", nil, false, false, ""},
		{"arbitrary hostname", "bridge", "bridge-agent:8080", "192.0.2.20", nil, false, false, ""},
		{"explicit IP", "192.0.2.10", "bridge-agent:8080", "192.0.2.20", nil, false, false, ""},
		{"missing alias", "bridge-agent", "bridge-agent:8080", "192.0.2.20", nil, true, false, ""},
		{"empty DNS", "bridge-agent", "bridge-agent:8080", "192.0.2.20", nil, false, false, ""},
		{"multiple IPs", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"192.0.2.10", "192.0.2.20"}, false, false, ""},
		{"operator IP", "bridge-agent", "bridge-agent:8080", "192.0.2.10", []string{"192.0.2.10"}, false, false, ""},
		{"nonlocal IP", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"192.0.2.30"}, false, false, ""},
		{"DNS wildcard", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"0.0.0.0"}, false, false, ""},
		{"DNS loopback", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"127.0.0.1"}, false, false, ""},
		{"DNS multicast", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"224.0.0.1"}, false, false, ""},
		{"interface error", "bridge-agent", "bridge-agent:8080", "192.0.2.20", []string{"192.0.2.10"}, false, true, ""},
		{"host wildcard", "bridge-agent", "*", "192.0.2.20", []string{"192.0.2.10"}, false, false, ""},
		{"host missing port", "bridge-agent", "bridge-agent", "192.0.2.20", []string{"192.0.2.10"}, false, false, ""},
		{"host extras", "bridge-agent", "bridge-agent:8080,example.test", "192.0.2.20", []string{"192.0.2.10"}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(_ context.Context, name string) ([]net.IPAddr, error) {
				if name != splitBridgeAlias {
					t.Fatalf("unexpected lookup %s", name)
				}
				if tc.lookupErr {
					return nil, errors.New("DNS unavailable")
				}
				var addresses []net.IPAddr
				for _, ip := range tc.ips {
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP(ip)})
				}
				return addresses, nil
			}
			interfaces := func() ([]net.Addr, error) {
				if tc.localErr {
					return nil, errors.New("interfaces unavailable")
				}
				return []net.Addr{local}, nil
			}
			got, err := resolveSplitBridgeBind(tc.bind, tc.hosts, 8080, tc.operator, lookup, interfaces)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

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
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
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
