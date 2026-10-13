package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runtimeWebhookOptions struct {
	Enabled, AllowPorts bool
	FailureLimit        int
}

func parseRuntimeWebhookOptions(getenv func(string) string) (runtimeWebhookOptions, error) {
	o := runtimeWebhookOptions{FailureLimit: 50}
	var err error
	o.Enabled, err = parseBoolEnv("WHATSAPP_WEBHOOK_RUNTIME", getenv("WHATSAPP_WEBHOOK_RUNTIME"), false)
	if err != nil {
		return o, err
	}
	o.AllowPorts, err = parseBoolEnv("WHATSAPP_WEBHOOK_RUNTIME_ALLOW_PORTS", getenv("WHATSAPP_WEBHOOK_RUNTIME_ALLOW_PORTS"), false)
	if err != nil {
		return o, err
	}
	if v := getenv("WHATSAPP_WEBHOOK_RUNTIME_FAILURE_LIMIT"); v != "" {
		o.FailureLimit, err = strconv.Atoi(v)
		if err != nil || o.FailureLimit < 1 || o.FailureLimit > 100 {
			return o, errors.New("WHATSAPP_WEBHOOK_RUNTIME_FAILURE_LIMIT must be 1-100")
		}
	}
	return o, nil
}

type runtimeWebhookConfig struct {
	URL          string   `json:"url"`
	Events       []string `json:"events"`
	Secret       string   `json:"secret"`
	HeaderPrefix string   `json:"header_prefix"`
}

type runtimeWebhookState struct {
	URL          string     `json:"url"`
	Enabled      bool       `json:"enabled"`
	Events       []string   `json:"events"`
	HeaderPrefix string     `json:"header_prefix"`
	Fingerprint  string     `json:"secret_fingerprint"`
	Failures     int        `json:"consecutive_failures"`
	LastSuccess  *time.Time `json:"last_success"`
	Reason       string     `json:"reason"`
	Queued       int        `json:"queued"`
	Dropped      uint64     `json:"dropped"`
}

type runtimeWebhookEvent struct {
	ID                  string    `json:"event_id"`
	Event               string    `json:"event"`
	MessageID           string    `json:"message_id,omitempty"`
	Chat                string    `json:"chat,omitempty"`
	Sender              string    `json:"sender,omitempty"`
	Type                string    `json:"type"`
	Text                string    `json:"text"`
	Timestamp           time.Time `json:"timestamp"`
	FromMe              bool      `json:"from_me"`
	Stored              bool      `json:"stored"`
	MediaReference      string    `json:"media_reference,omitempty"`
	ReactionToMessageID string    `json:"reaction_to_message_id,omitempty"`
	ReactionRemoved     *bool     `json:"reaction_removed,omitempty"`
}

type runtimeWebhookDelivery struct {
	body []byte
	id   string
	at   time.Time
	test bool
}
type runtimeWebhookGeneration struct {
	config runtimeWebhookConfig
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan runtimeWebhookDelivery
	// Serializes synthetic tests with background delivery, including failure state.
	delivery chan struct{}
	client   *http.Client
}

// Configuration and the bounded queue are process-local; replacing/removing the
// receiver cancels the old generation, so queued events never change destinations.
type runtimeWebhook struct {
	mu                            sync.Mutex
	ctx                           context.Context
	options                       runtimeWebhookOptions
	state                         runtimeWebhookState
	generation                    *runtimeWebhookGeneration
	lookup                        func(context.Context, string) ([]net.IPAddr, error)
	dial                          func(context.Context, string, string) (net.Conn, error)
	backoff                       time.Duration
	maxAge                        time.Duration
	tlsConfig                     *tls.Config
	deliveries, failures, dropped atomic.Uint64
}

func newRuntimeWebhook(ctx context.Context, options runtimeWebhookOptions) *runtimeWebhook {
	d := &net.Dialer{Timeout: 3 * time.Second}
	return &runtimeWebhook{ctx: ctx, options: options, lookup: net.DefaultResolver.LookupIPAddr, dial: d.DialContext, backoff: time.Second, maxAge: 5 * time.Minute}
}

func (w *runtimeWebhook) snapshot() runtimeWebhookState {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.state
	s.Events = append([]string{}, s.Events...)
	if w.generation != nil {
		s.Queued = len(w.generation.queue)
	}
	return s
}

func (w *runtimeWebhook) accepting() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.generation != nil && w.state.Enabled
}

// Caller holds mu. Logs and process-lifetime counters never carry receiver data.
func (w *runtimeWebhook) dropLocked(reason string, count uint64) {
	if count == 0 {
		return
	}
	w.state.Dropped += count
	w.dropped.Add(count)
	bridgeLog.Errorf("Runtime webhook discarded %d event(s): %s", count, reason)
}

// Restrict IPv6 to native global unicast: mapped IPv4 is checked as IPv4;
// translation/tunnel prefixes cannot hide a forbidden embedded IPv4 address.
func publicWebhookIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "168.63.129.16/32"} {
		if netip.MustParsePrefix(cidr).Contains(a) {
			return false
		}
	}
	if a.Is6() {
		return netip.MustParsePrefix("2000::/3").Contains(a) && !netip.MustParsePrefix("2001::/23").Contains(a) && !netip.MustParsePrefix("2001:db8::/32").Contains(a) && !netip.MustParsePrefix("2002::/16").Contains(a)
	}
	return true
}

func (w *runtimeWebhook) receiver(ctx context.Context, raw string) (*url.URL, []string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return nil, nil, errors.New("receiver requires an HTTPS URL without credentials or fragment")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || (!w.options.AllowPorts && n != 443) {
		return nil, nil, errors.New("receiver port is not allowed")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := w.lookup(ctx, u.Hostname())
	if err != nil || len(addresses) == 0 {
		return nil, nil, errors.New("receiver DNS lookup failed")
	}
	pinned := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.Zone != "" || !publicWebhookIP(address.IP) {
			return nil, nil, errors.New("receiver address is forbidden")
		}
		pinned = append(pinned, net.JoinHostPort(address.IP.String(), port))
	}
	return u, pinned, nil
}

func validWebhookPrefix(prefix string) bool {
	if len(prefix) < 3 || len(prefix) > 48 || !strings.HasPrefix(prefix, "X-") {
		return false
	}
	for _, c := range prefix {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func (w *runtimeWebhook) configure(ctx context.Context, cfg runtimeWebhookConfig) error {
	if len(cfg.Secret) < 32 || len(cfg.Secret) > 256 {
		return errors.New("secret must contain 32-256 bytes")
	}
	if len(cfg.Events) != 1 || cfg.Events[0] != "message.received" {
		return errors.New("events must be [message.received]")
	}
	if cfg.HeaderPrefix == "" {
		cfg.HeaderPrefix = "X-Webhook"
	}
	if !validWebhookPrefix(cfg.HeaderPrefix) {
		return errors.New("header_prefix must be an X- prefixed HTTP token of 3-48 characters")
	}
	u, _, err := w.receiver(ctx, cfg.URL)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generation != nil {
		w.generation.cancel()
	}
	child, cancel := context.WithCancel(w.ctx)
	g := &runtimeWebhookGeneration{config: cfg, ctx: child, cancel: cancel, queue: make(chan runtimeWebhookDelivery, 256), delivery: make(chan struct{}, 1)}
	g.client = w.client(g, u)
	sum := sha256.Sum256([]byte(cfg.Secret))
	u.RawQuery, u.ForceQuery = "", false
	w.state = runtimeWebhookState{URL: u.String(), Enabled: true, Events: append([]string{}, cfg.Events...), HeaderPrefix: cfg.HeaderPrefix, Fingerprint: hex.EncodeToString(sum[:8])}
	w.generation = g
	go w.run(g)
	return nil
}

func (w *runtimeWebhook) remove() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generation != nil {
		w.generation.cancel()
	}
	w.generation = nil
	w.state = runtimeWebhookState{}
}

func webhookDelivery(event runtimeWebhookEvent) (runtimeWebhookDelivery, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return runtimeWebhookDelivery{}, err
	}
	event.ID = hex.EncodeToString(id)
	body, err := json.Marshal(event)
	if err != nil || len(body) > 64<<10 {
		return runtimeWebhookDelivery{}, errors.New("event exceeds the 64 KiB payload limit")
	}
	return runtimeWebhookDelivery{body: body, id: event.ID, at: time.Now(), test: event.Event == "webhook.test"}, nil
}

func (w *runtimeWebhook) enqueue(event runtimeWebhookEvent, allowed bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generation == nil {
		return
	}
	if !w.state.Enabled {
		w.dropLocked("disabled", 1)
		return
	}
	if !allowed {
		return
	}
	d, err := webhookDelivery(event)
	if err != nil {
		w.dropLocked("payload_limit", 1)
		return
	}
	select {
	case w.generation.queue <- d:
	default:
		w.dropLocked("queue_full", 1)
	}
}

type runtimeWebhookAddresses struct{}

func (w *runtimeWebhook) client(g *runtimeWebhookGeneration, u *url.URL) *http.Client {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if w.tlsConfig != nil {
		tlsConfig = w.tlsConfig.Clone()
	}
	tlsConfig.ServerName = u.Hostname()
	transport := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 90 * time.Second,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Transport preserves request context values even when it detaches
			// cancellation for pooled dialing. This immutable list belongs to
			// the attempt whose complete DNS answer was validated below.
			pinned, _ := ctx.Value(runtimeWebhookAddresses{}).([]string)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			lastErr := errors.New("no validated receiver addresses")
			for _, address := range pinned {
				host, _, err := net.SplitHostPort(address)
				if err != nil || !publicWebhookIP(net.ParseIP(host)) {
					return nil, errors.New("receiver address is forbidden")
				}
				// A dead address cannot consume the whole connection budget.
				attemptCtx, stop := context.WithTimeout(ctx, min(300*time.Millisecond, 3*time.Second/time.Duration(len(pinned))))
				conn, err := w.dial(attemptCtx, network, address)
				stop()
				if err == nil {
					return conn, nil
				}
				lastErr = err
				if ctx.Err() != nil {
					break
				}
			}
			return nil, lastErr
		}}
	context.AfterFunc(g.ctx, transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (w *runtimeWebhook) deliver(ctx context.Context, g *runtimeWebhookGeneration, d runtimeWebhookDelivery) (bool, string) {
	u, pinned, err := w.receiver(ctx, g.config.URL)
	if err != nil {
		return false, "receiver_refused"
	}
	ctx = context.WithValue(ctx, runtimeWebhookAddresses{}, pinned)
	// The URL authority is an internal transport key, never a user-selected
	// network destination. Only the checked IP list above can choose a socket;
	// preserve the receiver's verified TLS name, Host and request target separately.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://runtime-webhook.invalid/", bytes.NewReader(d.body))
	if err != nil {
		return false, "request_failed"
	}
	req.Host = u.Host
	req.URL.Path, req.URL.RawPath = u.Path, u.RawPath
	req.URL.RawQuery, req.URL.ForceQuery = u.RawQuery, u.ForceQuery
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(g.config.Secret))
	_, _ = mac.Write([]byte(ts + "."))
	_, _ = mac.Write(d.body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(g.config.HeaderPrefix+"-Id", d.id)
	req.Header.Set(g.config.HeaderPrefix+"-Timestamp", ts)
	req.Header.Set(g.config.HeaderPrefix+"-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := g.client.Do(req)
	if err != nil {
		return false, "delivery_failed"
	}
	// Bound discarded response bytes; reaching EOF permits connection reuse.
	// The client's total timeout also bounds a receiver that stalls its body.
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "http_" + strconv.Itoa(resp.StatusCode)
	}
	return true, ""
}

func (w *runtimeWebhook) attempt(ctx context.Context, g *runtimeWebhookGeneration, d runtimeWebhookDelivery) (bool, string) {
	select {
	case g.delivery <- struct{}{}:
	case <-ctx.Done():
		return false, "cancelled"
	case <-g.ctx.Done():
		return false, "disabled"
	}
	defer func() { <-g.delivery }()
	w.mu.Lock()
	active := w.generation == g && w.state.Enabled
	w.mu.Unlock()
	if !active || g.ctx.Err() != nil {
		return false, "disabled"
	}
	ok, reason := w.deliver(ctx, g, d)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generation != g || g.ctx.Err() != nil {
		return false, "configuration_changed"
	}
	if ctx.Err() != nil {
		return false, "cancelled"
	}
	if ok {
		w.deliveries.Add(1)
		if d.test {
			return ok, reason
		}
		now := time.Now().UTC()
		w.state.LastSuccess = &now
		w.state.Failures = 0
		w.state.Reason = ""
	}
	return ok, reason
}

func (w *runtimeWebhook) run(g *runtimeWebhookGeneration) {
	for {
		select {
		case <-g.ctx.Done():
			return
		case d := <-g.queue:
			ctx, cancel := context.WithDeadline(g.ctx, d.at.Add(w.maxAge))
			delivered := false
			reason := "expired"
			for attempt := range 5 {
				if ctx.Err() != nil {
					break
				}
				var ok bool
				ok, reason = w.attempt(ctx, g, d)
				if ok {
					delivered = true
					break
				}
				status, _ := strconv.Atoi(strings.TrimPrefix(reason, "http_"))
				if attempt == 4 || (status >= 300 && status < 500 && status != 408 && status != 429) {
					break
				}
				timer := time.NewTimer(min(w.backoff*time.Duration(1<<attempt), 30*time.Second))
				select {
				case <-ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
			locallyCancelled := ctx.Err() != nil
			cancel()
			if !delivered {
				w.mu.Lock()
				if w.generation == g && g.ctx.Err() == nil {
					if locallyCancelled {
						reason = "expired"
					}
					w.dropLocked(reason, 1)
					if !locallyCancelled {
						w.failures.Add(1)
						w.state.Failures++
						w.state.Reason = reason
						if w.state.Failures >= w.options.FailureLimit {
							w.state.Enabled = false
							g.cancel()
							bridgeLog.Errorf("Runtime webhook automatically disabled after %d failed events", w.state.Failures)
						draining:
							for {
								select {
								case <-g.queue:
									w.dropLocked("disabled", 1)
								default:
									break draining
								}
							}
						}
					}
				}
				w.mu.Unlock()
			}
		}
	}
}

func (b *Bridge) handleRuntimeWebhook(w http.ResponseWriter, r *http.Request) {
	if b.RuntimeWebhook == nil {
		writeError(w, 404, "Runtime webhook is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, b.RuntimeWebhook.snapshot())
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var cfg runtimeWebhookConfig
		if err := decoder.Decode(&cfg); err != nil {
			writeError(w, 400, "Invalid webhook configuration")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, 400, "Expected one JSON object")
			return
		}
		if err := b.RuntimeWebhook.configure(r.Context(), cfg); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, b.RuntimeWebhook.snapshot())
	case http.MethodDelete:
		b.RuntimeWebhook.remove()
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, 405, "Method not allowed")
	}
}

func (b *Bridge) handleRuntimeWebhookTest(w http.ResponseWriter, r *http.Request) {
	rw := b.RuntimeWebhook
	if rw == nil {
		writeError(w, 404, "Runtime webhook is disabled")
		return
	}
	rw.mu.Lock()
	g := rw.generation
	enabled := rw.state.Enabled
	rw.mu.Unlock()
	if g == nil || !enabled {
		writeError(w, 409, "Configure an enabled webhook first")
		return
	}
	d, err := webhookDelivery(runtimeWebhookEvent{Event: "webhook.test", Type: "test", Text: "Synthetic webhook test", Timestamp: time.Now().UTC()})
	if err != nil {
		writeError(w, 500, "Could not create test event")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	ok, reason := rw.attempt(ctx, g, d)
	writeJSON(w, 200, map[string]any{"delivered": ok, "reason": reason, "event_id": d.id})
}

func (b *Bridge) queueRuntimeMessage(allowed bool, event runtimeWebhookEvent) {
	if b.RuntimeWebhook == nil {
		return
	}
	event.Event = "message.received"
	if event.Type == "" {
		event.Type = "text"
	} else if event.Stored && (event.Type == "image" || event.Type == "video" || event.Type == "audio" || event.Type == "document" || event.Type == "sticker") {
		event.MediaReference = "whatsapp://media/" + url.PathEscape(event.Chat) + "/" + url.PathEscape(event.MessageID)
	}
	event.Timestamp = event.Timestamp.UTC()
	b.RuntimeWebhook.enqueue(event, allowed)
}
