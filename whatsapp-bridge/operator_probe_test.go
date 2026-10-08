package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestOperatorStatusProbeUsesDirectHTTPAndPrintsNoCredentials(t *testing.T) {
	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls++ }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	for _, tc := range []struct {
		status int
		body   string
		want   string
		exit   int
	}{
		{200, `{"state":"passkey_required","qr":{"payload":"FAKE-QR-CREDENTIAL"},"confirmation_code":"FAKE-CODE"}`, "passkey_required\n", 0},
		{401, `{"state":"passkey_required"}`, "", 1},
		{200, `{"state":"forged\nlog"}`, "", 1},
		{302, `{"state":"passkey_required"}`, "", 1},
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Header.Get("Authorization") != "Bearer "+fakeOperatorToken || r.URL.Path != "/operator/v1/pairing" {
				t.Error("probe did not reach the authenticated state endpoint")
			}
			w.Header().Set("Location", proxy.URL)
			w.WriteHeader(tc.status)
			_, _ = fmt.Fprint(w, tc.body)
		}))
		_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		exit := operatorStatusProbe(operatorTestValues(map[string]string{operatorPortEnv: port}), &out)
		server.Close()
		if exit != tc.exit || out.String() != tc.want || calls != 1 {
			t.Fatalf("exit=%d output=%q calls=%d", exit, out.String(), calls)
		}
	}
	if proxyCalls != 0 {
		t.Fatal("probe followed a redirect or inherited proxy")
	}
}

func TestOperatorStatusProbeUsesAllowedHostOnExplicitInterface(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	cfg := operatorConfig{Bind: "127.0.0.2", Port: port, Token: fakeOperatorToken, AllowedHosts: "operator.example.test"}
	server := httptest.NewUnstartedServer(newOperatorHandler(cfg, operatorRoutes{pairing: func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"state": "awaiting_qr"})
	}}, testLogger()))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	var out bytes.Buffer
	if code := operatorStatusProbe(operatorTestValues(map[string]string{operatorBindEnv: "127.0.0.2", operatorPortEnv: strconv.Itoa(port), operatorAllowedHostsEnv: "operator.example.test"}), &out); code != 0 || out.String() != "awaiting_qr\n" {
		t.Fatalf("explicit-interface probe exit=%d output=%q", code, out.String())
	}
}
