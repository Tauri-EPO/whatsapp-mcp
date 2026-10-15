package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
	waLog "go.mau.fi/whatsmeow/util/log"
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != u.Host {
			t.Errorf("direct WhatsApp/media dial outside proxy: %s", address)
			return nil, errors.New("direct dial refused by test")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client := makeRuntimeClient(newTestClient(&mockLIDStore{}).Store, rec, proxyClientFactoryTransport(nil, u, transport))
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
	if strings.Count(got, "CONNECT web.whatsapp.com:443") < 1 || !strings.Contains(got, "GET media.example.test") {
		t.Fatalf("SDK traffic did not use proxy: %s", got)
	}
	l := proxySafeLogger{Logger: rec, redactor: newProxyRedactor(u)}
	l.Sub("test").Errorf("failed %s %s", u, "fake:secret@")
	b := testBridge(t, client, newTestMessageStore(t), rec)
	b.ProxyEnabled = true
	h := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/operator/v1/health", nil)
	r.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	newOperatorHandler(operatorConfig{Token: fakeOperatorToken, AllowedHosts: "localhost"}, operatorRoutes{health: b.handleHealth()}, rec).ServeHTTP(h, r)
	if h.Code != 200 || !strings.Contains(h.Body.String(), `"proxy_enabled":true`) {
		t.Fatalf("health: %d %s", h.Code, h.Body)
	}
	for _, secret := range []string{u.String(), "fake:secret@", u.Host} {
		if strings.Contains(rec.String()+h.Body.String(), secret) {
			t.Fatalf("proxy data disclosed: %q", secret)
		}
	}
}

func TestProxyLoggerPreservesNetworkErrorRedaction(t *testing.T) {
	u, _ := url.Parse("http://fake:secret@proxy.example.test:8080")
	rec := &recordingLogger{}
	l := proxySafeLogger{Logger: sdkSafeLogger{rec}, redactor: newProxyRedactor(u)}
	private := "https://media.example.test/private-history-path?hash=private-hash"
	l.Sub("Socket").Warnf("Failed to download media: %v", &url.Error{Op: "Get", URL: private, Err: errors.New("private-network-detail")})
	for _, value := range []string{private, "private-history-path", "private-hash", "private-network-detail"} {
		if strings.Contains(rec.String(), value) {
			t.Fatalf("network error disclosed %q: %s", value, rec.String())
		}
	}
}

func TestProxyShortCredentialsPreserveJIDs(t *testing.T) {
	u, _ := url.Parse("http://whatsapp:1@proxy:8080")
	rec := &recordingLogger{}
	l := proxySafeLogger{Logger: rec, redactor: newProxyRedactor(u)}
	l.Infof("15551234567@s.whatsapp.net %s whatsapp:1@proxy:8080", u)
	if !strings.Contains(rec.String(), "15551234567@s.whatsapp.net") || strings.Contains(rec.String(), "whatsapp:1@") || strings.Contains(rec.String(), "proxy:8080") {
		t.Fatal(rec.String())
	}
}

type proxyCountingStringer struct{ calls *int }

func (s proxyCountingStringer) String() string { *s.calls++; return "synthetic" }

func TestProxyDiscardedDebugDoesNotFormat(t *testing.T) {
	u, _ := url.Parse("http://proxy.example.test:8080")
	for _, logger := range []waLog.Logger{newTextWriter("test", "INFO", &bytes.Buffer{}, false), newJSONLogger("test", "INFO", &bytes.Buffer{}), privatePairingLogger{newTextWriter("test", "DEBUG", &bytes.Buffer{}, false)}, waLog.Noop} {
		calls := 0
		l := proxySafeLogger{Logger: sdkSafeLogger{logger}, redactor: newProxyRedactor(u)}
		l.Sub("Socket").Debugf("%s", proxyCountingStringer{&calls})
		if calls != 0 {
			t.Fatalf("discarded DEBUG formatted %d times", calls)
		}
	}
}

func TestProxyReturnedDialErrorsSafeForBridgeLogger(t *testing.T) {
	u, _ := url.Parse("http://fake:synthetic-secret@proxy.example.test:8080")
	for _, cause := range []error{
		&net.DNSError{Name: "proxy.example.test", Err: "synthetic DNS failure"},
		&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.19"), Port: 8080}, Err: errors.New("synthetic connect failure")},
	} {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return nil, cause }
		h := proxyHTTPClient(u, transport)
		client := whatsmeow.NewClient(newTestClient(&mockLIDStore{}).Store, waLog.Noop)
		client.SetPreLoginHTTPClient(h)
		client.SetMediaHTTPClient(h)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := client.ConnectContext(ctx)
		if err == nil {
			t.Fatal("dial failure accepted")
		}
		rec := &recordingLogger{}
		rec.Errorf("Failed to connect: %v", err) // The bridge's unwrapped logging path.
		//nolint:staticcheck // Exercise the actual SDK media HTTP consumer without pairing.
		_, err = client.DangerousInternals().DownloadAndDecrypt(ctx, "http://media.example.test/fake", nil, whatsmeow.MediaImage, nil, nil)
		cancel()
		rec.Errorf("Failed to download: %v", err)
		for _, private := range []string{u.String(), u.Hostname(), u.Host, "192.0.2.19", "synthetic-secret"} {
			if strings.Contains(rec.String(), private) {
				t.Fatalf("disclosed %q: %s", private, rec.String())
			}
		}
	}
}

type proxyResetConn struct {
	net.Conn
	response *strings.Reader
	ready    chan struct{}
	once     sync.Once
}

func (c *proxyResetConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.ready) })
	return len(p), nil
}
func (c *proxyResetConn) Read(p []byte) (int, error) {
	<-c.ready
	if c.response.Len() > 0 {
		return c.response.Read(p)
	}
	return 0, &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.19"), Port: 8080}, Err: errors.New("synthetic connection reset")}
}

func TestProxyEstablishedMediaStreamErrorIsRedacted(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	conn := &proxyResetConn{Conn: left, ready: make(chan struct{}), response: strings.NewReader("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nsynthetic")}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
	u, _ := url.Parse("http://fake:synthetic-secret@proxy.example.test:8080")
	h := proxyHTTPClient(u, base)
	response, err := h.Get("http://media.example.test/fake")
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("headers did not arrive: %v", err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err == nil {
		t.Fatal("reset stream unexpectedly completed")
	}
	rec := &recordingLogger{}
	rec.Errorf("media stream failed: %v", err)
	for _, value := range []string{u.String(), u.Hostname(), "192.0.2.19", "synthetic-secret"} {
		if strings.Contains(rec.String(), value) {
			t.Fatalf("stream disclosed %q: %s", value, rec.String())
		}
	}
}
