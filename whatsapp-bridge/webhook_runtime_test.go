package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const fakeWebhookSecret = "0123456789abcdef-Fake-Webhook-Secret"

type signedWebhookReceipt struct {
	header            http.Header
	body              []byte
	host, target, sni string
}

func runtimeWebhookFixture(t *testing.T, status *atomic.Int32) (*Bridge, *httptest.Server, string, <-chan signedWebhookReceipt, *recordingLogger) {
	t.Helper()
	receipts := make(chan signedWebhookReceipt, 32)
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receipts <- signedWebhookReceipt{header: r.Header.Clone(), body: body, host: r.Host, target: r.RequestURI, sni: r.TLS.ServerName}
		if status != nil {
			w.Header().Set("Location", "https://10.0.0.1/metadata")
			w.WriteHeader(int(status.Load()))
		} else {
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(receiver.Close)
	logger := installRecordingLogger(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), logger)
	b.Webhook.enabled = false
	b.MediaAutoDownload = false
	rw := newRuntimeWebhook(b.ctx, runtimeWebhookOptions{Enabled: true, AllowPorts: true, FailureLimit: 3})
	rw.backoff = time.Millisecond
	rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	rw.tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	rw.dial = func(ctx context.Context, network, pinned string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(receiver.Listener.Addr().String())
		if pinned != net.JoinHostPort("8.8.8.8", port) {
			t.Errorf("connection was not pinned: %s", pinned)
		}
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}
	b.RuntimeWebhook = rw
	b.operatorPairing = &operatorPairing{b: b, paired: func() bool { return false }, connected: func() bool { return false }, now: time.Now}
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, b.operatorRoutes(), logger))
	t.Cleanup(server.Close)
	u, _ := url.Parse(receiver.URL)
	u.Host = net.JoinHostPort("example.com", u.Port())
	return b, server, u.String(), receipts, logger
}

func webhookOperatorRequest(t *testing.T, server *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func configureWebhookHTTP(t *testing.T, server *httptest.Server, destination string) {
	t.Helper()
	body, _ := json.Marshal(runtimeWebhookConfig{URL: destination, Events: []string{"message.received"}, Secret: fakeWebhookSecret, HeaderPrefix: "X-Example"}) //nolint:gosec // Synthetic operator PUT input intentionally supplies a fake secret; responses never serialize it.
	if code, data := webhookOperatorRequest(t, server, "PUT", "/operator/v1/webhook", string(body)); code != 200 {
		t.Fatalf("configure=%d %s", code, data)
	}
}

// Matches the documented receiver check: freshness, raw bytes, constant-time MAC.
func verifyWebhookReceipt(receipt signedWebhookReceipt, now time.Time) bool {
	ts := receipt.header.Get("X-Example-Timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Unix()-n > 300 || n-now.Unix() > 300 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(fakeWebhookSecret))
	_, _ = mac.Write([]byte(ts + "."))
	_, _ = mac.Write(receipt.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(receipt.header.Get("X-Example-Signature")))
}

func receiveWebhook(t *testing.T, receipts <-chan signedWebhookReceipt) signedWebhookReceipt {
	t.Helper()
	select {
	case r := <-receipts:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no runtime delivery")
		return signedWebhookReceipt{}
	}
}

func TestRuntimeWebhookReceivedMessageSignatureAndReferences(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination)
	msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "runtime text")
	b.handleMessage(msg)
	r := receiveWebhook(t, receipts)
	if !verifyWebhookReceipt(r, time.Now()) || r.header.Get("X-Bridge-Token") != "" {
		t.Fatal("signature failed or bridge token escaped")
	}
	var event runtimeWebhookEvent
	if err := json.Unmarshal(r.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.MessageID != msg.Info.ID || event.Chat != phonePN.String() || event.Text != "runtime text" || event.Type != "text" || !event.Timestamp.Equal(msg.Info.Timestamp) || event.ID != r.header.Get("X-Example-Id") {
		t.Fatalf("payload=%s", r.body)
	}
	if verifyWebhookReceipt(r, time.Now().Add(301*time.Second)) {
		t.Fatal("stale signature verified")
	}
	r.body = append(r.body, ' ')
	if verifyWebhookReceipt(r, time.Now()) {
		t.Fatal("modified body verified")
	}
	b.handleMessage(buildImageMessage(phonePN, phonePN, false, "image caption"))
	r = receiveWebhook(t, receipts)
	if err := json.Unmarshal(r.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "image" || event.MediaReference == "" || bytes.Contains(r.body, []byte("mediaBase64")) {
		t.Fatalf("media payload=%s", r.body)
	}
}

func TestRuntimeWebhookReceiverDenyClasses(t *testing.T) {
	for _, tc := range []struct{ name, address string }{
		{"loopback v4", "127.0.0.1"}, {"loopback v6", "::1"}, {"private v4", "10.1.2.3"}, {"private v6", "fd00::1"},
		{"link-local v4", "169.254.1.2"}, {"link-local v6", "fe80::1"}, {"metadata v4", "169.254.169.254"}, {"metadata v6", "fd00:ec2::254"},
		{"metadata azure", "168.63.129.16"}, {"CGNAT", "100.64.1.2"}, {"multicast v4", "224.0.0.1"}, {"multicast v6", "ff02::1"},
		{"unspecified v4", "0.0.0.0"}, {"unspecified v6", "::"}, {"mapped private", "::ffff:10.1.2.3"}, {"translation", "64:ff9b::a01:203"}, {"6to4", "2002:0a01:0203::"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rw := newRuntimeWebhook(context.Background(), runtimeWebhookOptions{})
			rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
				return []net.IPAddr{{IP: net.ParseIP(tc.address)}}, nil
			}
			if _, _, err := rw.receiver(context.Background(), "https://example.com/path"); err == nil {
				t.Fatal("forbidden address accepted")
			}
		})
	}
	rw := newRuntimeWebhook(context.Background(), runtimeWebhookOptions{})
	rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	for _, raw := range []string{"http://example.com", "https://example.com:444", "https://user:secret@example.com", "https://example.com/#fragment"} {
		if _, _, err := rw.receiver(context.Background(), raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
	}
	if _, _, err := rw.receiver(context.Background(), "https://example.com"); err == nil {
		t.Fatal("mixed DNS answer accepted")
	}
}

func TestRuntimeWebhookDNSRebindingAndRedirect(t *testing.T) {
	for _, mode := range []string{"rebind", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var status atomic.Int32
			status.Store(302)
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
			var queries, dials atomic.Int32
			lookup := b.RuntimeWebhook.lookup
			dial := b.RuntimeWebhook.dial
			b.RuntimeWebhook.lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
				if queries.Add(1) > 1 && mode == "rebind" {
					return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
				}
				return lookup(ctx, host)
			}
			b.RuntimeWebhook.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				return dial(ctx, network, address)
			}
			configureWebhookHTTP(t, server, destination)
			code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
			if code != 200 || !bytes.Contains(body, []byte(`"delivered":false`)) {
				t.Fatalf("test=%d %s", code, body)
			}
			if mode == "rebind" && dials.Load() != 0 {
				t.Fatal("private rebound destination was dialed")
			}
			if mode == "redirect" {
				_ = receiveWebhook(t, receipts)
				if dials.Load() != 1 {
					t.Fatal("redirect followed")
				}
			}
		})
	}
}

func TestRuntimeWebhookRetriesDisableHealthAndReenable(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	b, server, destination, receipts, logger := runtimeWebhookFixture(t, &status)
	configureWebhookHTTP(t, server, destination)
	b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "retry text"))
	first := receiveWebhook(t, receipts)
	for range 2 {
		next := receiveWebhook(t, receipts)
		if !bytes.Equal(next.body, first.body) || next.header.Get("X-Example-Id") != first.header.Get("X-Example-Id") {
			t.Fatal("retry changed event identity")
		}
	}
	deadline := time.Now().Add(time.Second)
	for b.RuntimeWebhook.snapshot().Enabled && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	code, health := webhookOperatorRequest(t, server, "GET", "/operator/v1/health", "")
	if code != 200 || !bytes.Contains(health, []byte(`"consecutive_failures":3`)) || !bytes.Contains(health, []byte(`"reason":"http_503"`)) || b.RuntimeWebhook.snapshot().Enabled {
		t.Fatalf("health=%s", health)
	}
	_, get := webhookOperatorRequest(t, server, "GET", "/operator/v1/webhook", "")
	metrics := httptest.NewRecorder()
	b.handleMetrics()(metrics, httptest.NewRequest("GET", "/metrics", nil))
	for _, data := range [][]byte{health, get, metrics.Body.Bytes(), []byte(logger.String())} {
		if bytes.Contains(data, []byte(fakeWebhookSecret)) {
			t.Fatal("secret leaked")
		}
	}
	status.Store(204)
	configureWebhookHTTP(t, server, destination)
	code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(body, []byte(`"delivered":true`)) || b.RuntimeWebhook.snapshot().LastSuccess == nil {
		t.Fatalf("reenable=%d %s", code, body)
	}
	_, _ = webhookOperatorRequest(t, server, "DELETE", "/operator/v1/webhook", "")
	if s := b.RuntimeWebhook.snapshot(); s.Enabled || s.URL != "" || s.Queued != 0 {
		t.Fatalf("delete=%+v", s)
	}
}

func TestRuntimeWebhookRoutesOptInAuthAndValidation(t *testing.T) {
	b, server, destination, _, logger := runtimeWebhookFixture(t, nil)
	// A nil runtime dependency leaves all four methods absent, behind operator auth.
	b.RuntimeWebhook.remove()
	b.RuntimeWebhook = nil
	off := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, b.operatorRoutes(), logger))
	defer off.Close()
	if code, _ := webhookOperatorRequest(t, off, "PUT", "/operator/v1/settings", "{}"); code != 405 {
		t.Fatalf("legacy method=%d", code)
	}
	if strings.Contains(logger.String(), "Operator PUT /operator/v1/settings") {
		t.Fatal("opt-in off changed legacy PUT auditing")
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/operator/v1/webhook"}, {"PUT", "/operator/v1/webhook"}, {"DELETE", "/operator/v1/webhook"}, {"POST", "/operator/v1/webhook/test"}} {
		if code, _ := webhookOperatorRequest(t, off, tc.method, tc.path, "{}"); code != 404 {
			t.Fatalf("off %s=%d", tc.method, code)
		}
	}
	// Restore a runtime receiver before exercising the already registered routes.
	b.RuntimeWebhook = newRuntimeWebhook(b.ctx, runtimeWebhookOptions{Enabled: true, FailureLimit: 3})
	for _, tc := range []struct {
		method, host, origin, token string
		want                        int
	}{
		{"PUT", "", "", "", 401}, {"DELETE", "evil.example.test", "", fakeOperatorToken, 403},
		{"PUT", "", "https://evil.example.test", fakeOperatorToken, 403},
	} {
		req, _ := http.NewRequest(tc.method, server.URL+"/operator/v1/webhook", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Origin", tc.origin)
		if tc.origin == "" {
			req.Header.Del("Origin")
		}
		if tc.host != "" {
			req.Host = tc.host
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("denied=%d", resp.StatusCode)
		}
	}
	for _, body := range []string{"{}", `{"unknown":true}`, strings.Repeat("x", 4097), `{} {}`, `{"url":"` + destination + `","events":["connection.changed"],"secret":"` + fakeWebhookSecret + `"}`} {
		if code, _ := webhookOperatorRequest(t, server, "PUT", "/operator/v1/webhook", body); code != 400 {
			t.Fatalf("invalid configuration=%d", code)
		}
	}
	if code, _ := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", ""); code != 409 {
		t.Fatalf("unconfigured test=%d", code)
	}
	if code, _ := webhookOperatorRequest(t, server, "PATCH", "/operator/v1/webhook", "{}"); code != 405 {
		t.Fatalf("method=%d", code)
	}
	if !strings.Contains(logger.String(), "Operator PUT /operator/v1/webhook") {
		t.Fatal("PUT not audited")
	}
	for _, prefix := range []string{"Authorization", "X-Bad\r\nHeader", "X-", strings.Repeat("X", 49)} {
		if validWebhookPrefix(prefix) {
			t.Fatal("unsafe header prefix accepted")
		}
	}
}

func TestRuntimeWebhookQueueBoundAndDeleteCancels(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination)
	rw := b.RuntimeWebhook
	rw.mu.Lock()
	g := rw.generation
	rw.mu.Unlock()
	g.delivery <- struct{}{}
	event := runtimeWebhookEvent{Event: "message.received", Type: "text", Timestamp: time.Now()}
	rw.enqueue(event)
	deadline := time.Now().Add(time.Second)
	for len(g.queue) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(g.queue) != 0 {
		t.Fatal("worker did not dequeue first delivery")
	}
	for range 258 {
		rw.enqueue(event)
	}
	if s := rw.snapshot(); s.Queued != 256 || s.Dropped != 2 {
		t.Fatalf("queue bound=%+v", s)
	}
	if code, _ := webhookOperatorRequest(t, server, "DELETE", "/operator/v1/webhook", ""); code != 204 {
		t.Fatalf("delete=%d", code)
	}
	<-g.delivery
	if s := rw.snapshot(); s.Enabled || s.Queued != 0 {
		t.Fatalf("deleted state=%+v", s)
	}
	select {
	case <-receipts:
		t.Fatal("deleted generation delivered")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRuntimeWebhookSharedForwardFilters(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		chat                     types.JID
		policy                   string
		feed, self, fromMe, want bool
	}{
		{"conversation", phonePN, "", false, false, false, true},
		{"denied conversation", phonePN, mgGroup, false, false, false, false},
		{"self off", phonePN, "", false, false, true, false},
		{"self on", phonePN, "", false, true, true, true},
		{"status off", types.StatusBroadcastJID, "", false, false, false, false},
		{"status on", types.StatusBroadcastJID, "", true, false, false, true},
		{"status denied", types.StatusBroadcastJID, mgGroup, true, false, false, false},
		{"channel off", types.NewJID("example", types.NewsletterServer), "", false, false, false, false},
		{"channel on", types.NewJID("example", types.NewsletterServer), "", true, false, false, true},
		{"channel denied", types.NewJID("example", types.NewsletterServer), mgGroup, true, false, false, false},
		{"broadcast off", types.NewJID("example", types.BroadcastServer), "", false, false, false, false},
		{"broadcast on", types.NewJID("example", types.BroadcastServer), "", true, false, false, true},
		{"broadcast denied", types.NewJID("example", types.BroadcastServer), mgGroup, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			b.Policy = parseChatPolicy(tc.policy)
			b.ForwardSelf = tc.self
			b.ForwardStatus, b.ForwardChannels, b.ForwardBroadcasts = tc.feed, tc.feed, tc.feed
			configureWebhookHTTP(t, server, destination)
			b.handleMessage(buildTextMessage(tc.chat, phonePN, types.EmptyJID, types.EmptyJID, tc.fromMe, "filtered message"))
			if tc.want {
				_ = receiveWebhook(t, receipts)
			} else {
				// A control event queued after handleMessage is a FIFO barrier:
				// any incorrectly admitted message must reach this receiver first.
				b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "test", Text: "filter barrier", Timestamp: time.Now()})
				receipt := receiveWebhook(t, receipts)
				var event runtimeWebhookEvent
				if err := json.Unmarshal(receipt.body, &event); err != nil {
					t.Fatal(err)
				}
				if event.Type != "test" || event.Text != "filter barrier" {
					t.Fatalf("filtered message reached receiver: %s", receipt.body)
				}
			}
		})
	}
}
func TestRuntimeWebhookStartupOptions(t *testing.T) {
	o, err := parseRuntimeWebhookOptions(func(string) string { return "" })
	if err != nil || o.Enabled || o.AllowPorts || o.FailureLimit != 10 {
		t.Fatalf("defaults=%+v %v", o, err)
	}
	for key, value := range map[string]string{"WHATSAPP_WEBHOOK_RUNTIME": "maybe", "WHATSAPP_WEBHOOK_RUNTIME_ALLOW_PORTS": "maybe", "WHATSAPP_WEBHOOK_RUNTIME_FAILURE_LIMIT": "0"} {
		if _, err := parseRuntimeWebhookOptions(func(k string) string {
			if k == key {
				return value
			}
			return ""
		}); err == nil {
			t.Fatalf("invalid %s accepted", key)
		}
	}
}

func TestRuntimeWebhookAttemptAndAgeCaps(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			var status atomic.Int32
			status.Store(503)
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
			b.RuntimeWebhook.options.FailureLimit = 100
			if expired {
				b.RuntimeWebhook.maxAge = time.Nanosecond
			}
			configureWebhookHTTP(t, server, destination)
			b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "bounded retry"))
			if !expired {
				for range 5 {
					_ = receiveWebhook(t, receipts)
				}
			}
			deadline := time.Now().Add(time.Second)
			for b.RuntimeWebhook.snapshot().Dropped == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			s := b.RuntimeWebhook.snapshot()
			wantFailures := 5
			if expired {
				wantFailures = 0
			}
			if s.Dropped != 1 || s.Failures != wantFailures || !s.Enabled {
				t.Fatalf("cap state=%+v", s)
			}
			select {
			case <-receipts:
				t.Fatal("attempt or age cap exceeded")
			default:
			}
		})
	}
}

func TestRuntimeWebhookNonFileMessagesHaveNoMediaReference(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination)
	for _, category := range []string{"poll", "location", "live_location", "reaction", ""} {
		b.queueRuntimeMessage(phonePN, phonePN, false, runtimeWebhookEvent{MessageID: "FAKE-ID", Chat: phonePN.String(), Type: category, Stored: true, Timestamp: time.Now()})
		r := receiveWebhook(t, receipts)
		if bytes.Contains(r.body, []byte("media_reference")) {
			t.Fatalf("non-file reference=%s", r.body)
		}
	}
	b.queueRuntimeMessage(phonePN, phonePN, false, runtimeWebhookEvent{MessageID: "FAKE/ID", Chat: phonePN.String(), Type: "image", Stored: true, Timestamp: time.Now()})
	r := receiveWebhook(t, receipts)
	var event runtimeWebhookEvent
	if err := json.Unmarshal(r.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.MediaReference != "whatsapp://media/"+phonePN.String()+"/FAKE%2FID" {
		t.Fatalf("unescaped resource identity=%s", event.MediaReference)
	}
}

func TestRuntimeWebhookFailureLimitConcurrentTestAndQueue(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	b, server, destination, _, _ := runtimeWebhookFixture(t, &status)
	b.RuntimeWebhook.options.FailureLimit = 1
	// Compete with the queue consumer repeatedly; disabling must never hold state
	// locked on a blocking receive while health/configuration are waiting.
	for range 10 {
		configureWebhookHTTP(t, server, destination)
		b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text", Timestamp: time.Now()})
		done := make(chan struct{})
		go func() { defer close(done); webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "") }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("failure-limit deadlock")
		}
		if b.RuntimeWebhook.snapshot().Enabled {
			t.Fatal("failure did not disable")
		}
	}
}

func TestRuntimeWebhookFallsBackAcrossPinnedPublicAddresses(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	var queries, dials atomic.Int32
	b.RuntimeWebhook.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		queries.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dial := b.RuntimeWebhook.dial
	b.RuntimeWebhook.dial = func(ctx context.Context, network, pinned string) (net.Conn, error) {
		dials.Add(1)
		host, _, err := net.SplitHostPort(pinned)
		if err != nil {
			return nil, err
		}
		if host == "1.1.1.1" {
			return nil, errors.New("synthetic unreachable public receiver")
		}
		return dial(ctx, network, pinned)
	}
	configureWebhookHTTP(t, server, destination)
	code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(body, []byte(`"delivered":true`)) {
		t.Fatalf("fallback=%d %s", code, body)
	}
	if queries.Load() != 2 || dials.Load() != 2 || !verifyWebhookReceipt(receiveWebhook(t, receipts), time.Now()) {
		t.Fatal("fallback re-resolved DNS, skipped a pinned address, or failed signing")
	}
}

func TestRuntimeWebhookPinnedTransportPreservesHostSNIAndEscapedTarget(t *testing.T) {
	_, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	origin, err := url.Parse(destination)
	if err != nil {
		t.Fatal(err)
	}
	destination += "/receiver/%2F?q=one%2Ftwo&empty="
	configureWebhookHTTP(t, server, destination)
	code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(body, []byte(`"delivered":true`)) {
		t.Fatalf("transport=%d %s", code, body)
	}
	r := receiveWebhook(t, receipts)
	if r.host != origin.Host || r.sni != origin.Hostname() || r.target != "/receiver/%2F?q=one%2Ftwo&empty=" || !verifyWebhookReceipt(r, time.Now()) {
		t.Fatalf("receiver identity/target changed: host=%s sni=%s target=%s", r.host, r.sni, r.target)
	}
}
