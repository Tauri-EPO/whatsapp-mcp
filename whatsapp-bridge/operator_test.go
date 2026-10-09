package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func operatorHTTPFixture(t *testing.T) (*httptest.Server, *bytes.Buffer, *int) {
	t.Helper()
	var audit bytes.Buffer
	calls := new(int)
	routes := operatorRoutes{
		health: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]string{"status": "awaiting_pairing"})
		},
		ready: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 503, map[string]string{"status": "awaiting_pairing"})
		},
		pairing: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, map[string]string{"qr": "FAKE-QR-CREDENTIAL"})
		},
		code: func(w http.ResponseWriter, _ *http.Request) {
			*calls++
			writeJSON(w, 200, map[string]string{"code": "FAKE-CODE-CREDENTIAL"})
		},
	}
	cfg := operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}
	server := httptest.NewServer(newOperatorHandler(cfg, routes, newJSONLogger("operator-test", "INFO", &audit)))
	t.Cleanup(server.Close)
	return server, &audit, calls
}

func TestOperatorHTTPAuthenticatesAndHasNoDataPlaneRoutes(t *testing.T) {
	server, _, calls := operatorHTTPFixture(t)
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/operator/v1/health", "", 401}, {"/operator/v1/health", "fake-data-plane-token-0123456789abcdef", 401},
		{"/operator/v1/health", fakeOperatorToken, 200}, {"/operator/v1/ready", fakeOperatorToken, 503},
		{"/operator/v1/pairing", fakeOperatorToken, 200}, {"/api/send", fakeOperatorToken, 404}, {"/api/download", fakeOperatorToken, 404},
		{"/operator/v1/logout", fakeOperatorToken, 404}, {"/operator/v1/settings", fakeOperatorToken, 404}, {"/operator/v1/transcription/usage", fakeOperatorToken, 404},
	} {
		req, _ := http.NewRequest("GET", server.URL+tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.status || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("route=%s status=%d", tc.path, response.StatusCode)
		}
	}
	if *calls != 0 {
		t.Fatal("read or denied request reached a pairing mutation")
	}
	dataHosts, _ := buildHostAllowList(8080, "127.0.0.1", "")
	dataAuth := withAuth("fake-data-plane-token-0123456789abcdef", dataHosts, func(http.ResponseWriter, *http.Request) { t.Fatal("operator token reached the data plane") })
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/send", nil)
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	w := httptest.NewRecorder()
	dataAuth(w, req)
	if w.Code != 401 {
		t.Fatalf("operator token on data plane=%d", w.Code)
	}
}

func TestOperatorHTTPRefusesHostOriginAndAuditsWithoutCredentials(t *testing.T) {
	server, audit, calls := operatorHTTPFixture(t)
	for _, tc := range []struct {
		host, origin string
		status       int
	}{
		{"evil.example.test", "", 403}, {"", "https://evil.example.test", 403}, {"", "null", 403}, {"", server.URL, 200}, {"", "", 200},
	} {
		req, _ := http.NewRequest("POST", server.URL+"/operator/v1/pairing/code?secret=FAKE-QUERY-CREDENTIAL", strings.NewReader(`{"phone":"5511999999999","secret":"FAKE-BODY-CREDENTIAL"}`))
		req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("operator origin/host status=%d want=%d", response.StatusCode, tc.status)
		}
	}
	if *calls != 2 {
		t.Fatalf("accepted mutations=%d", *calls)
	}
	if strings.Count(audit.String(), "Operator POST") != 2 || !strings.Contains(audit.String(), "peer=127.0.0.1") {
		t.Fatalf("audit lines=%s", audit)
	}
	for _, secret := range []string{fakeOperatorToken, "FAKE-QUERY-CREDENTIAL", "FAKE-BODY-CREDENTIAL", "FAKE-QR-CREDENTIAL", "FAKE-CODE-CREDENTIAL", "5511999999999"} {
		if strings.Contains(audit.String(), secret) {
			t.Fatal("operator audit exposed credential or payload")
		}
	}
}

func TestOperatorLimiterIgnoresForwardedClientsAndBoundsPeers(t *testing.T) {
	server, _, _ := operatorHTTPFixture(t)
	denied := 0
	for index := range 200 {
		req, _ := http.NewRequest("GET", server.URL+"/operator/v1/health", nil)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", index+1))
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode == 429 {
			denied++
			if response.Header.Get("Retry-After") == "" {
				t.Fatal("operator rate limit has no retry delay")
			}
		} else if response.StatusCode != 401 {
			t.Fatalf("guess status=%d", response.StatusCode)
		}
	}
	if denied == 0 {
		t.Fatal("spoofed forwarding headers bypassed operator rate limit")
	}
	limiter := newOperatorLimiter(1)
	for index := range 1024 {
		if limiter.take(fmt.Sprint(index)) != 0 {
			t.Fatal("fresh peer refused below the bound")
		}
	}
	if limiter.take("overflow") == 0 || len(limiter.buckets) != 1024 {
		t.Fatal("operator peer state was not bounded")
	}
}

func TestOperatorListenerOffHasNoSideEffect(t *testing.T) {
	server, err := startOperatorServer(operatorConfig{}, operatorRoutes{}, waLog.Noop)
	if err != nil || server != nil {
		t.Fatal("disabled operator listener was started")
	}
}
