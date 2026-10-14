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

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const fakeWebhookSecret = "0123456789abcdef-Fake-Webhook-Secret"

func configureWebhookFields(t *testing.T, server *httptest.Server, destination string, fields map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"url": destination, "events": []string{"message.received"}, "secret": fakeWebhookSecret, "header_prefix": "X-Example"}
	for name, value := range fields {
		body[name] = value
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return webhookOperatorRequest(t, server, "PUT", "/operator/v1/webhook", string(data))
}

func webhookBarrier(t *testing.T, b *Bridge, receipts <-chan signedWebhookReceipt) {
	t.Helper()
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "test", Text: "filter barrier"}, true)
	r := receiveWebhook(t, receipts)
	var event runtimeWebhookEvent
	if err := json.Unmarshal(r.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "test" || event.Text != "filter barrier" {
		t.Fatalf("unexpected delivery before barrier: %s", r.body)
	}
}

func TestRuntimeWebhookConnectionSubscriptionsAndSignatures(t *testing.T) {
	for _, subscribed := range []bool{false, true} {
		t.Run(strconv.FormatBool(subscribed), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			selected := []string{"message.received"}
			if subscribed {
				selected = append(selected, "connection.changed")
			}
			if code, data := configureWebhookFields(t, server, destination, map[string]any{"events": selected}); code != 200 {
				t.Fatalf("configure=%d %s", code, data)
			}
			b.connectionEventWait = func(context.Context, time.Duration) bool { return true }
			lastID := ""
			for _, transition := range []struct {
				state, reason string
				event         any
			}{
				{"connected", "authenticated", &events.Connected{}},
				{"disconnected", "transport_lost", &events.Disconnected{}},
				{"connected", "authenticated", &events.Connected{}},
				{"logged_out", "operator", nil},
				{"pairing_required", "unpaired", nil},
				{"paired", "pairing_succeeded", nil},
			} {
				if transition.event != nil {
					b.handleEvent(transition.event, make(chan bool, 1))
				} else {
					b.notifyConnection(transition.state, transition.reason, true, false)
				}
				b.connectionEvents.Wait()
				if !subscribed {
					webhookBarrier(t, b, receipts)
					continue
				}
				r := receiveWebhook(t, receipts)
				if !verifyWebhookReceipt(r, time.Now()) || r.header.Get("X-Bridge-Token") != "" {
					t.Fatal("connection signature failed or bridge token escaped")
				}
				var payload connectionPayload
				if err := json.Unmarshal(r.body, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.State != transition.state || payload.Reason != transition.reason || payload.Type != "connection" || payload.At.IsZero() || payload.At.Location() != time.UTC {
					t.Fatalf("connection payload=%s", r.body)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(r.body, &fields); err != nil {
					t.Fatal(err)
				}
				var id, event string
				if err := json.Unmarshal(fields["event_id"], &id); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(fields["event"], &event); err != nil {
					t.Fatal(err)
				}
				if id == "" || id == lastID || id != r.header.Get("X-Example-Id") || event != "connection.changed" {
					t.Fatalf("connection identity=%s", r.body)
				}
				lastID = id
				delete(fields, "event_id")
				delete(fields, "event")
				legacy, _ := json.Marshal(payload)
				var expected map[string]json.RawMessage
				if err := json.Unmarshal(legacy, &expected); err != nil {
					t.Fatal(err)
				}
				actual, _ := json.Marshal(fields)
				want, _ := json.Marshal(expected)
				if !bytes.Equal(actual, want) {
					t.Fatalf("runtime added fields to legacy connection payload: %s", r.body)
				}
			}
		})
	}
}

func TestRuntimeWebhookReceiverFiltersAndDeploymentFloor(t *testing.T) {
	for _, filter := range []struct {
		field, env string
		chat       types.JID
		fromMe     bool
	}{
		{"forward_self", forwardSelfEnv, phonePN, true},
		{"forward_channels", webhookForwardChannelsEnv, types.NewJID("example", types.NewsletterServer), false},
		{"forward_broadcasts", webhookForwardBroadcastsEnv, types.NewJID("example", types.BroadcastServer), false},
	} {
		for _, deploy := range []string{"", "true", "false"} {
			for _, override := range []string{"omitted", "true", "false"} {
				t.Run(filter.field+"/deploy="+deploy+"/receiver="+override, func(t *testing.T) {
					b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
					getenv := func(name string) string {
						if name == filter.env {
							return deploy
						}
						return ""
					}
					switches, err := parseBridgeSwitches(getenv)
					if err != nil {
						t.Fatal(err)
					}
					b.ForwardSelf, b.ForwardChannels, b.ForwardBroadcasts = switches.ForwardSelf, switches.ForwardChannels, switches.ForwardBroadcasts
					options, err := parseRuntimeWebhookOptions(getenv)
					if err != nil {
						t.Fatal(err)
					}
					b.RuntimeWebhook.options.DeniedFilters = options.DeniedFilters
					fields := map[string]any{}
					if override != "omitted" {
						fields[filter.field] = override == "true"
					}
					code, data := configureWebhookFields(t, server, destination, fields)
					if deploy == "false" && override == "true" {
						if code != 400 || b.RuntimeWebhook.configured() {
							t.Fatalf("deployment floor widened: %d %s", code, data)
						}
						return
					}
					if code != 200 {
						t.Fatalf("configure=%d %s", code, data)
					}
					want := deploy == "true" || (deploy == "" && filter.fromMe)
					if override != "omitted" {
						want = override == "true"
					}
					for _, path := range []string{"/operator/v1/webhook", "/operator/v1/health"} {
						code, snapshot := webhookOperatorRequest(t, server, "GET", path, "")
						needle := `"` + filter.field + `":` + strconv.FormatBool(want)
						if code != 200 || !bytes.Contains(snapshot, []byte(needle)) || !bytes.Contains(snapshot, []byte(`"events":["message.received"]`)) || bytes.Contains(snapshot, []byte(fakeWebhookSecret)) {
							t.Fatalf("snapshot=%d %s", code, snapshot)
						}
					}
					b.handleMessage(buildTextMessage(filter.chat, phonePN, types.EmptyJID, types.EmptyJID, filter.fromMe, "receiver filtered message"))
					if want {
						_ = receiveWebhook(t, receipts)
					} else {
						webhookBarrier(t, b, receipts)
					}
					// Receiver opt-in cannot widen the legacy sender's deployment filter.
					if deploy != "true" && !filter.fromMe && b.webhookChatAllowed(b.Policy, filter.chat, filter.fromMe) {
						t.Fatal("legacy feed filter widened")
					}
				})
			}
		}
	}
}

func TestRuntimeWebhookReceiverChoicesPreserveLegacyDelivery(t *testing.T) {
	for _, tc := range []struct {
		field         string
		chat          types.JID
		fromMe, optIn bool
	}{
		{"forward_self", phonePN, true, false},
		{"forward_channels", types.NewJID("example", types.NewsletterServer), false, false},
		{"forward_broadcasts", types.NewJID("example", types.BroadcastServer), false, false},
		{"forward_channels", types.NewJID("example", types.NewsletterServer), false, true},
		{"forward_broadcasts", types.NewJID("example", types.BroadcastServer), false, true},
	} {
		t.Run(tc.field+"/opt-in="+strconv.FormatBool(tc.optIn), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			legacy := make(chan WebhookPayload, 4)
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload WebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				legacy <- payload
				w.WriteHeader(204)
			}))
			defer receiver.Close()
			b.Webhook.enabled, b.Webhook.url, b.Webhook.client = true, receiver.URL, receiver.Client()
			b.ForwardSelf, b.ForwardChannels, b.ForwardBroadcasts = !tc.optIn, !tc.optIn, !tc.optIn
			if code, data := configureWebhookFields(t, server, destination, map[string]any{tc.field: tc.optIn}); code != 200 {
				t.Fatalf("configure=%d %s", code, data)
			}
			b.handleMessage(buildTextMessage(tc.chat, phonePN, types.EmptyJID, types.EmptyJID, tc.fromMe, "independent receiver filters"))
			if tc.optIn {
				_ = receiveWebhook(t, receipts)
				if len(legacy) != 0 {
					t.Fatal("runtime opt-in widened legacy delivery")
				}
			} else {
				webhookBarrier(t, b, receipts)
				if len(legacy) != 1 {
					t.Fatal("runtime opt-out narrowed legacy delivery")
				}
				<-legacy
			}
			b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "legacy control"))
			if len(legacy) != 1 {
				t.Fatal("legacy receiver control not delivered")
			}
			<-legacy
		})
	}
}

func TestRuntimeWebhookOptInStillHonorsChatAndStatusPolicy(t *testing.T) {
	for _, chat := range []types.JID{types.NewJID("example", types.NewsletterServer), types.NewJID("example", types.BroadcastServer), types.StatusBroadcastJID} {
		b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
		if code, data := configureWebhookFields(t, server, destination, map[string]any{"forward_channels": true, "forward_broadcasts": true}); code != 200 {
			t.Fatalf("configure=%d %s", code, data)
		}
		b.handleMessage(buildTextMessage(types.StatusBroadcastJID, phonePN, types.EmptyJID, types.EmptyJID, false, "deploy-only status"))
		webhookBarrier(t, b, receipts)
		b.Policy = parseChatPolicy(mgGroup)
		b.handleMessage(buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "denied chat"))
		webhookBarrier(t, b, receipts)
	}
}

func TestRuntimeWebhookConnectionSharesQueueAndDebounce(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	if code, data := configureWebhookFields(t, server, destination, map[string]any{"events": []string{"message.received", "connection.changed"}}); code != 200 {
		t.Fatalf("configure=%d %s", code, data)
	}
	b.notifyConnection("connected", "authenticated", true, false)
	b.connectionEvents.Wait()
	_ = receiveWebhook(t, receipts)
	entered := make(chan struct{})
	b.connectionEventWait = func(ctx context.Context, _ time.Duration) bool { close(entered); <-ctx.Done(); return false }
	b.notifyConnection("disconnected", "transport_lost", false, false)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no debounce")
	}
	b.notifyConnection("connected", "authenticated", true, false)
	b.connectionEvents.Wait()
	webhookBarrier(t, b, receipts)
	rw := b.RuntimeWebhook
	rw.mu.Lock()
	g := rw.generation
	rw.mu.Unlock()
	g.delivery <- struct{}{}
	rw.enqueue(runtimeWebhookEvent{Event: "message.received"}, true)
	deadline := time.Now().Add(time.Second)
	for len(g.queue) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(g.queue) != 0 {
		t.Fatal("worker did not start blocked delivery")
	}
	for i := range 256 {
		if i%2 == 0 {
			b.queueRuntimeConnection(connectionPayload{Type: "connection", State: "paired"})
		} else {
			rw.enqueue(runtimeWebhookEvent{Event: "message.received"}, true)
		}
	}
	b.queueRuntimeConnection(connectionPayload{Type: "connection", State: "logged_out"})
	if s := rw.snapshot(); s.Queued != 256 || s.Dropped != 1 {
		t.Fatalf("mixed queue escaped shared bound: %+v", s)
	}
	_, _ = webhookOperatorRequest(t, server, "DELETE", "/operator/v1/webhook", "")
	<-g.delivery
}

func TestRuntimeWebhookNewFieldsStrictValidationAndConnectionOnly(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	for _, field := range []string{"forward_self", "forward_channels", "forward_broadcasts", "FORWARD_SELF", "FORWARD_CHANNELS", "FORWARD_BROADCASTS"} {
		for _, value := range []any{nil, "true", 1, []bool{true}, map[string]bool{"value": true}} {
			if code, data := configureWebhookFields(t, server, destination, map[string]any{field: value}); code != 400 {
				t.Fatalf("invalid %s accepted=%d %s", field, code, data)
			}
		}
	}
	for _, fields := range []map[string]any{
		{"forward_status": true}, {"unknown": false}, {"events": []string{}},
		{"events": []string{"unknown.event"}}, {"events": []string{"connection.changed", "connection.changed"}},
	} {
		if code, data := configureWebhookFields(t, server, destination, fields); code != 400 {
			t.Fatalf("invalid fields accepted=%d %s", code, data)
		}
	}
	if code, data := configureWebhookFields(t, server, destination, map[string]any{"events": []string{"connection.changed"}}); code != 200 {
		t.Fatalf("connection-only=%d %s", code, data)
	}
	b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "unsubscribed message"))
	b.notifyConnection("connected", "authenticated", true, false)
	b.connectionEvents.Wait()
	r := receiveWebhook(t, receipts)
	if !verifyWebhookReceipt(r, time.Now()) || !bytes.Contains(r.body, []byte(`"event":"connection.changed"`)) {
		t.Fatalf("unsubscribed message preceded connection: %s", r.body)
	}
}

type signedWebhookReceipt struct {
	header            http.Header
	body              []byte
	host, target, sni string
	remote            string
}

func runtimeWebhookFixture(t *testing.T, status *atomic.Int32, hooks ...func(http.ResponseWriter, *http.Request)) (*Bridge, *httptest.Server, string, <-chan signedWebhookReceipt, *recordingLogger) {
	t.Helper()
	receipts := make(chan signedWebhookReceipt, 128)
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receipts <- signedWebhookReceipt{header: r.Header.Clone(), body: body, host: r.Host, target: r.RequestURI, sni: r.TLS.ServerName, remote: r.RemoteAddr}
		if len(hooks) != 0 {
			hooks[0](w, r)
			return
		}
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
	b.RuntimeWebhook.options.FailureLimit = 1
	configureWebhookHTTP(t, server, destination)
	b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "retry text"))
	first := receiveWebhook(t, receipts)
	for range 4 {
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
	if code != 200 || !bytes.Contains(health, []byte(`"consecutive_failures":1`)) || !bytes.Contains(health, []byte(`"reason":"http_503"`)) || b.RuntimeWebhook.snapshot().Enabled {
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
	if code != 200 || !bytes.Contains(body, []byte(`"delivered":true`)) {
		t.Fatalf("reenable=%d %s", code, body)
	}
	_ = receiveWebhook(t, receipts)
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	_ = receiveWebhook(t, receipts)
	deadline = time.Now().Add(time.Second)
	for b.RuntimeWebhook.snapshot().LastSuccess == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.RuntimeWebhook.snapshot().LastSuccess == nil {
		t.Fatal("message success not recorded")
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
	if !strings.Contains(logger.String(), "Operator PUT /operator/v1/settings") {
		t.Fatal("PUT on a mutation route was not audited")
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
	for _, body := range []string{"{}", `{"unknown":true}`, strings.Repeat("x", 4097), `{} {}`, `{"url":"` + destination + `","events":["unknown.event"],"secret":"` + fakeWebhookSecret + `"}`} {
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
	rw.enqueue(event, true)
	deadline := time.Now().Add(time.Second)
	for len(g.queue) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(g.queue) != 0 {
		t.Fatal("worker did not dequeue first delivery")
	}
	for range 258 {
		rw.enqueue(event, true)
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
				b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "test", Text: "filter barrier", Timestamp: time.Now()}, true)
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
	if err != nil || o.Enabled || o.AllowPorts || o.FailureLimit != 50 {
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
			wantFailures := 1
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
		b.queueRuntimeMessage(true, runtimeWebhookEvent{MessageID: "FAKE-ID", Chat: phonePN.String(), Type: category, Stored: true, Timestamp: time.Now()})
		r := receiveWebhook(t, receipts)
		if bytes.Contains(r.body, []byte("media_reference")) {
			t.Fatalf("non-file reference=%s", r.body)
		}
	}
	b.queueRuntimeMessage(true, runtimeWebhookEvent{MessageID: "FAKE/ID", Chat: phonePN.String(), Type: "image", Stored: true, Timestamp: time.Now()})
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
		b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text", Timestamp: time.Now()}, true)
		done := make(chan struct{})
		go func() { defer close(done); webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "") }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("failure-limit deadlock")
		}
		waitWebhookDrop(t, b.RuntimeWebhook)
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

// Coordinator review regressions: the receiver is real TLS, without a paired phone.
func TestRuntimeWebhookReviewCallerCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			configureWebhookHTTP(t, server, destination)
			rw := b.RuntimeWebhook
			g := rw.generation
			d, err := webhookDelivery(runtimeWebhookEvent{Event: "message.received", Type: "text"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			}
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); _, _ = rw.attempt(ctx, g, d) }()
			_ = receiveWebhook(t, receipts)
			if !deadline {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("attempt did not cancel")
			}
			if s := rw.snapshot(); s.Failures != 0 || !s.Enabled || s.Reason != "" {
				t.Fatalf("local cancellation counted as receiver failure: %+v", s)
			}
		})
	}
}

func TestRuntimeWebhookReviewFailureCountsEventsAndPermanent4xx(t *testing.T) {
	for _, code := range []int32{400, 401, 404, 408, 429, 503} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			var status atomic.Int32
			status.Store(code)
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
			configureWebhookHTTP(t, server, destination)
			b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "event failure"))
			want := 1
			if code == 408 || code == 429 || code == 503 {
				want = 5
			}
			for range want {
				_ = receiveWebhook(t, receipts)
			}
			waitWebhookDrop(t, b.RuntimeWebhook)
			if s := b.RuntimeWebhook.snapshot(); s.Failures != 1 || !s.Enabled {
				t.Fatalf("attempts counted instead of one event: %+v", s)
			}
			select {
			case r := <-receipts:
				t.Fatalf("permanent 4xx retried: %s", r.body)
			default:
			}
		})
	}
}

func waitWebhookDrop(t *testing.T, rw *runtimeWebhook) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for rw.snapshot().Dropped == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rw.snapshot().Dropped == 0 {
		t.Fatal("event did not settle")
	}
}

func TestRuntimeWebhookReviewRestartTolerance(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
	o, err := parseRuntimeWebhookOptions(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	b.RuntimeWebhook.options.FailureLimit = o.FailureLimit
	configureWebhookHTTP(t, server, destination)
	for range 2 {
		b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
		for range 5 {
			_ = receiveWebhook(t, receipts)
		}
	}
	status.Store(204)
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	_ = receiveWebhook(t, receipts)
	deadline := time.Now().Add(time.Second)
	for b.RuntimeWebhook.snapshot().LastSuccess == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := b.RuntimeWebhook.snapshot(); !s.Enabled || s.Failures != 0 || s.LastSuccess == nil {
		t.Fatalf("short restart disabled receiver: %+v", s)
	}
	if o.FailureLimit <= 10 {
		t.Fatalf("default still only %d events", o.FailureLimit)
	}
}

func TestRuntimeWebhookReviewDisableAndDropsObservable(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	b, server, destination, _, logger := runtimeWebhookFixture(t, &status)
	b.RuntimeWebhook.options.FailureLimit = 1
	configureWebhookHTTP(t, server, destination+"/?token=fake-query-secret")
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	waitWebhookDrop(t, b.RuntimeWebhook)
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	if s := b.RuntimeWebhook.snapshot(); s.Enabled || s.Dropped != 2 {
		t.Fatalf("disabled drop not counted: %+v", s)
	}
	status.Store(204)
	configureWebhookHTTP(t, server, destination)
	_, _ = webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	metrics := httptest.NewServer(b.handleMetrics())
	defer metrics.Close()
	resp, err := metrics.Client().Get(metrics.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"whatsapp_bridge_runtime_webhook_deliveries_total 1", "whatsapp_bridge_runtime_webhook_failures_total 1", "whatsapp_bridge_runtime_webhook_dropped_total 2"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("missing metric %s", want)
		}
	}
	for _, want := range []string{"[ERROR] Runtime webhook automatically disabled", "[ERROR] Runtime webhook discarded"} {
		if !strings.Contains(logger.String(), want) {
			t.Errorf("missing log %s", want)
		}
	}
	for _, secret := range []string{destination, "fake-query-secret", fakeWebhookSecret} {
		if bytes.Contains(data, []byte(secret)) || strings.Contains(logger.String(), secret) {
			t.Fatal("receiver or secret in metrics/logs")
		}
	}
}

func TestRuntimeWebhookReviewSenderNamespace(t *testing.T) {
	for _, sender := range []types.JID{phonePN, phoneLID} {
		t.Run(sender.Server, func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			configureWebhookHTTP(t, server, destination)
			b.handleMessage(buildTextMessage(phonePN, sender, types.EmptyJID, types.EmptyJID, false, "sender namespace"))
			var event runtimeWebhookEvent
			if err := json.Unmarshal(receiveWebhook(t, receipts).body, &event); err != nil {
				t.Fatal(err)
			}
			if event.Sender != sender.String() {
				t.Fatalf("sender lost namespace: %s", event.Sender)
			}
		})
	}
}

func TestRuntimeWebhookReviewReactionTargetAndRemoval(t *testing.T) {
	for _, emoji := range []string{"+1", ""} {
		t.Run(strconv.FormatBool(emoji == ""), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			configureWebhookHTTP(t, server, destination)
			msg := buildTextMessage(phonePN, phoneLID, types.EmptyJID, types.EmptyJID, false, "")
			msg.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("FAKE-TARGET")}, Text: proto.String(emoji)}}
			b.handleMessage(msg)
			var event map[string]any
			if err := json.Unmarshal(receiveWebhook(t, receipts).body, &event); err != nil {
				t.Fatal(err)
			}
			if event["reaction_to_message_id"] != "FAKE-TARGET" || event["reaction_removed"] != (emoji == "") || event["sender"] != phoneLID.String() {
				t.Fatalf("incomplete reaction: %+v", event)
			}
		})
	}
}

func TestRuntimeWebhookReviewSyntheticEventIsIndependent(t *testing.T) {
	var status atomic.Int32
	status.Store(400)
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
	b.RuntimeWebhook.options.FailureLimit = 1
	configureWebhookHTTP(t, server, destination)
	code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(body, []byte(`"delivered":false`)) {
		t.Fatalf("test result=%d %s", code, body)
	}
	var event runtimeWebhookEvent
	if err := json.Unmarshal(receiveWebhook(t, receipts).body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Event != "webhook.test" {
		t.Fatalf("test impersonates message: %s", event.Event)
	}
	if s := b.RuntimeWebhook.snapshot(); !s.Enabled || s.Failures != 0 || s.LastSuccess != nil {
		t.Fatalf("test changed receiver health: %+v", s)
	}
	// A successful test must not reset an existing production failure episode.
	b.RuntimeWebhook.mu.Lock()
	b.RuntimeWebhook.state.Failures = 1
	b.RuntimeWebhook.state.Reason = "http_503"
	b.RuntimeWebhook.mu.Unlock()
	status.Store(204)
	_, _ = webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if s := b.RuntimeWebhook.snapshot(); s.Failures != 1 || s.Reason != "http_503" || s.LastSuccess != nil {
		t.Fatalf("test reset receiver health: %+v", s)
	}
}

func TestRuntimeWebhookReviewSnapshotRedactsQuery(t *testing.T) {
	_, server, destination, receipts, logger := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination+"/receiver/%2F?token=fake-query-secret")
	for _, path := range []string{"/operator/v1/webhook", "/operator/v1/health"} {
		code, data := webhookOperatorRequest(t, server, "GET", path, "")
		if code != 200 || bytes.Contains(data, []byte("fake-query-secret")) || bytes.Contains(data, []byte("token=")) {
			t.Fatalf("query leaked in %s: %s", path, data)
		}
		if !bytes.Contains(data, []byte(destination+"/receiver/%2F")) {
			t.Fatalf("safe URL lost path: %s", data)
		}
	}
	_, _ = webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if receiveWebhook(t, receipts).target != "/receiver/%2F?token=fake-query-secret" {
		t.Fatal("query removed from actual delivery")
	}
	if strings.Contains(logger.String(), "fake-query-secret") {
		t.Fatal("query leaked in log")
	}
}

func TestRuntimeWebhookReviewTransportReuseAndDeadAddress(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	var dials atomic.Int32
	b.RuntimeWebhook.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}, {IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dial := b.RuntimeWebhook.dial
	b.RuntimeWebhook.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if strings.HasPrefix(address, "[") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dial(ctx, network, address)
	}
	configureWebhookHTTP(t, server, destination)
	start := time.Now()
	for range 2 {
		_, _ = webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	}
	first, second := receiveWebhook(t, receipts), receiveWebhook(t, receipts)
	if time.Since(start) > time.Second {
		t.Fatal("dead address consumed more than one second")
	}
	if dials.Load() != 2 || first.remote != second.remote {
		t.Fatalf("transport not reused: %d dials, %s -> %s", dials.Load(), first.remote, second.remote)
	}
}

func TestRuntimeWebhookReviewQueueDeadlineDuringRequest(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	b.RuntimeWebhook.maxAge = 200 * time.Millisecond
	b.RuntimeWebhook.options.FailureLimit = 1
	configureWebhookHTTP(t, server, destination)
	b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "queue deadline"))
	_ = receiveWebhook(t, receipts)
	waitWebhookDrop(t, b.RuntimeWebhook)
	if s := b.RuntimeWebhook.snapshot(); s.Failures != 0 || !s.Enabled || s.Reason != "" {
		t.Fatalf("queue deadline counted against receiver: %+v", s)
	}
}

func TestRuntimeWebhookReviewSharedPolicyEvaluatedOnce(t *testing.T) {
	for _, mode := range []string{"unconfigured", "both", "disabled"} {
		for _, reaction := range []bool{false, true} {
			t.Run(mode+"/"+strconv.FormatBool(reaction), func(t *testing.T) {
				b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
				lids := &markerBudgetLIDs{mockLIDStore: mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}}}
				b.Client = newTestClient(lids)
				b.Policy = parseChatPolicy(phoneLID.String())
				if mode != "unconfigured" {
					configureWebhookHTTP(t, server, destination)
				}
				if mode == "disabled" {
					b.RuntimeWebhook.mu.Lock()
					b.RuntimeWebhook.state.Enabled = false
					b.RuntimeWebhook.mu.Unlock()
				}
				if mode == "both" {
					legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
					defer legacy.Close()
					b.Webhook.enabled, b.Webhook.url = true, legacy.URL
				}
				msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "shared policy")
				if reaction {
					msg.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("FAKE-TARGET")}, Text: proto.String("+1")}}
				}
				b.handleMessage(msg)
				want := int32(0)
				if mode != "unconfigured" {
					want = 1
				}
				if mode == "both" {
					_ = receiveWebhook(t, receipts)
				}
				if got := lids.lookups.Load(); got != want {
					t.Fatalf("policy lookups=%d want=%d", got, want)
				}
			})
		}
	}
}

func TestRuntimeWebhookReviewKeepAliveDiscardsBoundedBody(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ignored receiver response"))
	})
	configureWebhookHTTP(t, server, destination)
	for range 2 {
		_, _ = webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	}
	first, second := receiveWebhook(t, receipts), receiveWebhook(t, receipts)
	if first.remote != second.remote {
		t.Fatal("nonempty response prevented connection reuse")
	}
	// A pooled socket must not bypass the next complete DNS-answer check.
	b.RuntimeWebhook.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}
	code, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
	if code != 200 || !bytes.Contains(body, []byte(`"reason":"receiver_refused"`)) {
		t.Fatalf("pooled socket bypassed DNS: %s", body)
	}
	select {
	case <-receipts:
		t.Fatal("forbidden DNS reached pooled receiver")
	default:
	}
}

func TestRuntimeWebhookReviewReenableKeepsAdmissionAuthorization(t *testing.T) {
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination)
	rw := b.RuntimeWebhook
	rw.mu.Lock()
	rw.state.Enabled = false
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		// The message was denied before the operator re-enabled delivery.
		rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text", Text: "denied before reenable"}, false)
	}()
	<-started
	rw.state.Enabled = true
	rw.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("admission blocked")
	}
	rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text", Text: "authorized FIFO barrier"}, true)
	var event runtimeWebhookEvent
	if err := json.Unmarshal(receiveWebhook(t, receipts).body, &event); err != nil {
		t.Fatal(err)
	}
	if event.Text != "authorized FIFO barrier" {
		t.Fatal("reenable bypassed message authorization")
	}
}

func TestRuntimeWebhookReviewDisableCountsQueuedDrops(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	b, server, destination, _, _ := runtimeWebhookFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(400)
	})
	b.RuntimeWebhook.options.FailureLimit = 1
	configureWebhookHTTP(t, server, destination)
	b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	<-started
	for range 10 {
		b.RuntimeWebhook.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	}
	close(release)
	waitWebhookDrop(t, b.RuntimeWebhook)
	if s := b.RuntimeWebhook.snapshot(); s.Enabled || s.Failures != 1 || s.Dropped != 11 || s.Queued != 0 {
		t.Fatalf("queued discards not observed: %+v", s)
	}
}

func TestRuntimeWebhookRound2ConnectBudget(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(strconv.FormatBool(fallback), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			rw := b.RuntimeWebhook
			if fallback {
				rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
					return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}, {IP: net.ParseIP("8.8.8.8")}}, nil
				}
			}
			dial := rw.dial
			rw.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				if strings.HasPrefix(address, "[") {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				select {
				case <-time.After(450 * time.Millisecond):
					return dial(ctx, network, address)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			configureWebhookHTTP(t, server, destination)
			start := time.Now()
			_, body := webhookOperatorRequest(t, server, "POST", "/operator/v1/webhook/test", "")
			if !bytes.Contains(body, []byte(`"delivered":true`)) {
				t.Fatalf("slow sole/last address refused: %s", body)
			}
			if time.Since(start) > 3*time.Second || !verifyWebhookReceipt(receiveWebhook(t, receipts), time.Now()) {
				t.Fatal("connection exceeded total budget or signature failed")
			}
		})
	}
}

func TestRuntimeWebhookRound2FailureBeforeBackoffExpiry(t *testing.T) {
	var status atomic.Int32
	status.Store(503)
	b, server, destination, receipts, _ := runtimeWebhookFixture(t, &status)
	rw := b.RuntimeWebhook
	rw.maxAge, rw.backoff = 150*time.Millisecond, time.Second
	rw.options.FailureLimit = 2
	configureWebhookHTTP(t, server, destination)
	for n := uint64(1); n <= 2; n++ {
		rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
		_ = receiveWebhook(t, receipts)
		deadline := time.Now().Add(time.Second)
		for rw.dropped.Load() < n && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if s := rw.snapshot(); s.Failures != int(n) || s.Reason != "http_503" || s.Enabled != (n < 2) {
			t.Fatalf("real failure lost during age expiry: %+v", s)
		}
	}
	if rw.failures.Load() != 2 {
		t.Fatal("failed-event counter lost expired failures")
	}
}

func TestRuntimeWebhookRound2AcknowledgedSuccess(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(strconv.FormatBool(change), func(t *testing.T) {
			b, server, destination, receipts, _ := runtimeWebhookFixture(t, nil)
			configureWebhookHTTP(t, server, destination)
			rw := b.RuntimeWebhook
			g := rw.generation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := g.client.Transport
			g.client.Transport = mcpTestTransport(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err == nil {
					// The real TLS receiver has already acknowledged the request.
					if change {
						configureWebhookHTTP(t, server, destination)
					} else {
						cancel()
					}
				}
				return response, err
			})
			response := httptest.NewRecorder()
			b.handleRuntimeWebhookTest(response, httptest.NewRequest("POST", "/operator/v1/webhook/test", nil).WithContext(ctx))
			if !bytes.Contains(response.Body.Bytes(), []byte(`"delivered":true`)) || rw.deliveries.Load() != 1 || rw.dropped.Load() != 0 {
				t.Fatalf("acknowledgement lost: %s", response.Body.String())
			}
			_ = receiveWebhook(t, receipts)
			if rw.snapshot().LastSuccess != nil {
				t.Fatal("synthetic success changed production state")
			}
		})
	}
}

func TestRuntimeWebhookRound2PolicyAndDropLogRate(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(disabled), func(t *testing.T) {
			b, server, destination, _, logger := runtimeWebhookFixture(t, nil)
			if disabled {
				b.ForwardSelf = false
			}
			configureWebhookHTTP(t, server, destination)
			rw := b.RuntimeWebhook
			g := rw.generation
			g.delivery <- struct{}{}
			defer func() { <-g.delivery }()
			if disabled {
				rw.mu.Lock()
				rw.state.Enabled = false
				rw.mu.Unlock()
			} else {
				rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
				deadline := time.Now().Add(time.Second)
				for len(g.queue) != 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				for range 256 {
					rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
				}
			}
			for range 100 {
				rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, false)
			}
			if rw.dropped.Load() != 0 || strings.Contains(logger.String(), "Runtime webhook discarded") {
				t.Fatal("policy-withheld events counted or logged as loss")
			}
			remaining := 100
			if disabled {
				b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, true, "self withheld"))
				if rw.dropped.Load() != 0 {
					t.Fatal("disabled ingress bypassed self policy")
				}
				b.handleMessage(buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "allowed disabled ingress"))
				if rw.dropped.Load() != 1 {
					t.Fatal("policy-eligible disabled ingress was not counted")
				}
				remaining = 99
			}
			for range remaining {
				rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
			}
			if rw.dropped.Load() != 100 || strings.Count(logger.String(), "Runtime webhook discarded") != 1 {
				t.Fatalf("loss logging is unbounded: drops=%d logs=%s", rw.dropped.Load(), logger.String())
			}
			// Advance the interval without sleeping: the next line must include
			// the 99 suppressed losses and this new one.
			rw.mu.Lock()
			rw.lastDropLog = time.Now().Add(-time.Minute)
			rw.mu.Unlock()
			rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
			if rw.dropped.Load() != 101 || strings.Count(logger.String(), "Runtime webhook discarded") != 2 || !strings.Contains(logger.String(), "discarded 100 event(s)") {
				t.Fatal("interval log lost the suppressed count")
			}
			rw.remove()
			if !disabled {
				deadline := time.Now().Add(time.Second)
				for rw.dropped.Load() < 358 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if rw.dropped.Load() != 358 {
					t.Fatal("retired queue did not settle")
				}
			}
		})
	}
}

func TestRuntimeWebhookRound2ConfigurationCountsAbandoned(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			b, server, destination, receipts, logger := runtimeWebhookFixture(t, nil, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			configureWebhookHTTP(t, server, destination)
			rw := b.RuntimeWebhook
			rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
			_ = receiveWebhook(t, receipts)
			for range 10 {
				rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
			}
			if method == "PUT" {
				configureWebhookHTTP(t, server, destination)
			} else {
				if code, _ := webhookOperatorRequest(t, server, method, "/operator/v1/webhook", ""); code != 204 {
					t.Fatalf("delete=%d", code)
				}
			}
			deadline := time.Now().Add(time.Second)
			for rw.dropped.Load() < 11 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if rw.dropped.Load() != 11 || rw.failures.Load() != 0 || !strings.Contains(logger.String(), "configuration_changed") {
				t.Fatalf("abandoned events lost: drops=%d failures=%d logs=%s", rw.dropped.Load(), rw.failures.Load(), logger.String())
			}
		})
	}
}

func TestRuntimeWebhookRound2ReactionForwardLog(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		t.Run(strconv.FormatBool(reaction), func(t *testing.T) {
			b, server, destination, receipts, logger := runtimeWebhookFixture(t, nil)
			configureWebhookHTTP(t, server, destination)
			msg := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "forwarded text")
			if reaction {
				msg = buildReactionMessage(phonePN, phonePN, false, "FAKE-TARGET", "+1")
			}
			b.handleMessage(msg)
			_ = receiveWebhook(t, receipts)
			if strings.Contains(logger.String(), "not forwarded") {
				t.Fatalf("forwarded event logged as withheld: %s", logger.String())
			}
		})
	}
}

func TestRuntimeWebhookRound2RetiredWorkerOwnsLogger(t *testing.T) {
	b, server, destination, _, logger := runtimeWebhookFixture(t, nil)
	configureWebhookHTTP(t, server, destination)
	rw := b.RuntimeWebhook
	started, release := make(chan struct{}), make(chan struct{})
	rw.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		close(started)
		<-release
		return nil, errors.New("synthetic cancelled lookup")
	}
	rw.enqueue(runtimeWebhookEvent{Event: "message.received", Type: "text"}, true)
	<-started
	rw.remove()
	// The retired worker resumes after cleanup changes the sanctioned global.
	nextLogger := installRecordingLogger(t)
	close(release)
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logger.String(), "configuration_changed") && !strings.Contains(nextLogger.String(), "configuration_changed") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rw.dropped.Load() != 1 || !strings.Contains(logger.String(), "configuration_changed") || strings.Contains(nextLogger.String(), "Runtime webhook discarded") {
		t.Fatal("retired worker used another instance's logger")
	}
}
