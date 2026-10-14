package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func TestWhatsAppProxyValidationAndPrivateFile(t *testing.T) {
	for _, raw := range []string{"ftp://fake:secret@proxy.example.test", "http://", "http://fake:secret@proxy.example.test:0", "http://fake:secret@proxy.example.test/path", "socks5://proxy.example.test", "http://proxy.example.test/?token=secret"} {
		_, err := parseWhatsAppProxy(func(key string) string {
			if key == "WHATSAPP_PROXY_URL" {
				return raw
			}
			return ""
		})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), raw) {
			t.Fatalf("invalid proxy was accepted or disclosed: %v", err)
		}
	}
	for _, raw := range []string{"http://proxy.example.test", "https://fake:secret@proxy.example.test:8443", "socks5://fake:secret@proxy.example.test:1080"} {
		if _, err := parseWhatsAppProxy(func(key string) string {
			if key == "WHATSAPP_PROXY_URL" {
				return raw
			}
			return ""
		}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "proxy-url")
	if err := os.WriteFile(path, []byte("http://fake:secret@proxy.example.test:8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	get := func(key string) string {
		if key == "WHATSAPP_PROXY_URL_FILE" {
			return path
		}
		return ""
	}
	if _, err := parseWhatsAppProxy(get); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil { //nolint:gosec // Deliberately permissive fixture must be refused by the parser.
		t.Fatal(err)
	}
	if _, err := parseWhatsAppProxy(get); err == nil {
		t.Fatal("permissive proxy file accepted")
	}
	if u, err := parseWhatsAppProxy(func(string) string { return "" }); u != nil || err != nil {
		t.Fatal("unset changed behavior")
	}
}

func TestWhatsAppProxyWebsocketAndRealMediaDownload(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.Host)
		mu.Unlock()
		if r.Header.Get("Proxy-Authorization") != "Basic ZmFrZTpzZWNyZXQ=" {
			t.Error("proxy credentials missing")
		}
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "synthetic media bytes")
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("fake", "secret")
	rec := &recordingLogger{}
	client := makeRuntimeClient(newTestClient(&mockLIDStore{}).Store, rec, proxyClientFactory(nil, u))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.ConnectContext(ctx); err == nil {
		t.Fatal("fake proxy unexpectedly connected WhatsApp")
	}
	paired := types.NewJID("10000000001", types.DefaultUserServer)
	client.Store.ID = &paired
	if err := client.ConnectContext(ctx); err == nil {
		t.Fatal("fake proxy unexpectedly connected paired websocket")
	}
	//nolint:staticcheck // Test-only SDK media entrypoint bypasses paired CDN discovery, retaining the real HTTP consumer.
	data, err := client.DangerousInternals().DownloadAndDecrypt(ctx, "http://media.example.test/fake", nil, whatsmeow.MediaImage, nil, nil)
	if err != nil || string(data) != "synthetic media bytes" {
		t.Fatalf("media result=%q error=%v", data, err)
	}
	mu.Lock()
	got := strings.Join(paths, "\n")
	mu.Unlock()
	if strings.Count(got, "CONNECT web.whatsapp.com:443") != 2 || !strings.Contains(got, "GET media.example.test") {
		t.Fatalf("SDK traffic did not use proxy: %s", got)
	}
	l := proxySafeLogger{Logger: rec, proxyURL: u}
	l.Sub("test").Errorf("failed %s %s %s", u, "fake", "secret")
	b := testBridge(t, client, newTestMessageStore(t), rec)
	b.ProxyEnabled = true
	h := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/operator/v1/health", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	newOperatorHandler(operatorConfig{Token: fakeOperatorToken, AllowedHosts: "localhost"}, operatorRoutes{health: b.handleHealth()}, rec).ServeHTTP(h, r)
	if h.Code != 200 || !strings.Contains(h.Body.String(), `"proxy_enabled":true`) {
		t.Fatalf("health: %d %s", h.Code, h.Body)
	}
	for _, secret := range []string{u.String(), "fake", "secret", u.Host} {
		if strings.Contains(rec.String()+h.Body.String(), secret) {
			t.Fatalf("proxy data disclosed: %q", secret)
		}
	}
}

func TestProxyLoggerPreservesNetworkErrorRedaction(t *testing.T) {
	u, _ := url.Parse("http://fake:secret@proxy.example.test:8080")
	rec := &recordingLogger{}
	l := proxySafeLogger{Logger: sdkSafeLogger{rec}, proxyURL: u}
	private := "https://media.example.test/private-history-path?hash=private-hash"
	l.Sub("Socket").Warnf("Failed to download media: %v", &url.Error{Op: "Get", URL: private, Err: errors.New("private-network-detail")})
	for _, value := range []string{private, "private-history-path", "private-hash", "private-network-detail"} {
		if strings.Contains(rec.String(), value) {
			t.Fatalf("network error disclosed %q: %s", value, rec.String())
		}
	}
}
