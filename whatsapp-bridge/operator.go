package main

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

type operatorRoutes struct {
	export, snapshot                               http.HandlerFunc
	health, ready, pairing                         http.HandlerFunc
	code, restart, passkeyResponse, passkeyConfirm http.HandlerFunc
	settings, logout                               http.HandlerFunc
	sendUsage, mcpToken                            http.HandlerFunc
}

type operatorBucket struct {
	tokens float64
	at     time.Time
}
type operatorLimiter struct {
	mu       sync.Mutex
	buckets  map[string]operatorBucket
	capacity float64
	now      func() time.Time
}

func newOperatorLimiter(capacity int) *operatorLimiter {
	return &operatorLimiter{buckets: map[string]operatorBucket{}, capacity: float64(capacity), now: time.Now}
}

func (l *operatorLimiter) take(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	bucket, exists := l.buckets[key]
	if !exists {
		for peer, value := range l.buckets {
			if now.Sub(value.at) > 10*time.Minute {
				delete(l.buckets, peer)
			}
		}
		if len(l.buckets) >= 1024 {
			return 60
		}
		bucket = operatorBucket{tokens: l.capacity, at: now}
	}
	bucket.tokens = min(l.capacity, bucket.tokens+now.Sub(bucket.at).Seconds()*l.capacity/60)
	bucket.at = now
	if bucket.tokens < 1 {
		l.buckets[key] = bucket
		return max(1, int(math.Ceil((1-bucket.tokens)*60/l.capacity)))
	}
	bucket.tokens--
	l.buckets[key] = bucket
	return 0
}

func operatorOriginAllowed(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	} // Native/backend clients still authenticate.
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host)
}

type operatorStatusWriter struct {
	http.ResponseWriter
	status  int
	onFlush func()
}

func (w *operatorStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *operatorStatusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *operatorStatusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	if w.onFlush != nil {
		w.onFlush()
	}
}

func (w *operatorStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newOperatorHandler(cfg operatorConfig, routes operatorRoutes, logger waLog.Logger) http.Handler {
	allowed, warning := buildHostAllowList(cfg.Port, cfg.Bind, cfg.AllowedHosts)
	if warning != "" {
		logger.Warnf("%s", strings.ReplaceAll(warning, bridgeAllowedHostsEnv, operatorAllowedHostsEnv))
	}
	mux := http.NewServeMux()
	mutations := map[string]bool{}
	for _, route := range []struct {
		name, method string
		handler      http.HandlerFunc
	}{
		{"health", "GET", routes.health}, {"ready", "GET", routes.ready}, {"pairing", "GET", routes.pairing},
		{"export", "GET", routes.export}, {"snapshot", "POST", routes.snapshot},
		{"pairing/code", "POST", routes.code}, {"pairing/restart", "POST", routes.restart},
		{"pairing/passkey/response", "POST", routes.passkeyResponse}, {"pairing/passkey/confirm", "POST", routes.passkeyConfirm},
		{"settings", "", routes.settings}, {"logout", "POST", routes.logout},
		{"send/usage", "GET", routes.sendUsage}, {"mcp-token", "", routes.mcpToken},
	} {
		path := "/operator/v1/" + route.name
		if route.method == http.MethodPost || route.name == "settings" || route.name == "mcp-token" {
			mutations[path] = true
		}
		handler := route.handler
		if handler == nil {
			handler = func(w http.ResponseWriter, _ *http.Request) {
				writeErrorCode(w, http.StatusServiceUnavailable, "operator_unavailable", "Operator pairing controller unavailable")
			}
		}
		if route.method == "" {
			mux.HandleFunc(path, handler)
		} else {
			mux.HandleFunc(path, requireMethod(route.method, handler))
		}
	}
	peers, token := newOperatorLimiter(120), newOperatorLimiter(60)
	limit := func(w http.ResponseWriter, delay int) {
		w.Header().Set("Retry-After", strconv.Itoa(delay))
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "Operator rate limit exceeded")
	}
	authenticated := withAuth(cfg.Token, allowed, func(w http.ResponseWriter, r *http.Request) {
		if !operatorOriginAllowed(r) {
			writeErrorCode(w, http.StatusForbidden, "origin_forbidden", "Operator Origin must match this listener")
			return
		}
		if delay := token.take("operator"); delay != 0 {
			limit(w, delay)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		out := &operatorStatusWriter{ResponseWriter: w}
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			peer = "unknown"
		}
		// Only fixed known route names are logged, never a URL/query or payload.
		if ((r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete) && mutations[r.URL.Path]) || (r.Method == http.MethodGet && r.URL.Path == "/operator/v1/export") {
			audited := false
			audit := func() {
				if audited {
					return
				}
				audited = true
				status := out.status
				if status == 0 {
					status = http.StatusInternalServerError
				}
				if status == 401 || status == 403 || status == 429 {
					logger.Debugf("Operator %s %s peer=%s outcome=%d", r.Method, r.URL.Path, peer, status)
				} else {
					logger.Infof("Operator %s %s peer=%s outcome=%d", r.Method, r.URL.Path, peer, status)
				}
			}
			defer audit()
			// Logout can os.Exit after flushing, before the middleware returns.
			out.onFlush = audit
		}
		if delay := peers.take(peer); delay != 0 {
			limit(out, delay)
			return
		}
		authenticated(out, r)
	})
}

func startOperatorServer(cfg operatorConfig, routes operatorRoutes, logger waLog.Logger) (*http.Server, error) {
	if cfg.Bind == "" {
		return nil, nil
	}
	listener, err := net.Listen("tcp", listenAddr(cfg.Bind, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("operator listener: %w", err)
	}
	server := &http.Server{Handler: newOperatorHandler(cfg, routes, logger), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Errorf("Operator listener stopped: %v", err)
		}
	}()
	logger.Infof("Operator listener enabled on %s", listener.Addr())
	return server, nil
}
