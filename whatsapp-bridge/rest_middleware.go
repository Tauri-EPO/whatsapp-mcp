package main

// Shared REST plumbing: one JSON error shape, one method check, one request
// log line. Handlers used to answer failures three ways (SendMessageResponse,
// {"ok":false,"error":...}, plain-text http.Error) and re-implement the 405
// each; the bridge logged nothing per request, so /api/send left no trail.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// apiError is the failure body every endpoint answers with. `message` is kept
// at the top level for callers that only read that field.
type apiError struct {
	Success bool         `json:"success"`
	Message string       `json:"message"`
	Error   apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorCode maps an HTTP status to the code the MCP server understands
// (errors.py): invalid_argument, denied, not_found, bridge_unavailable, internal.
func errorCode(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusMethodNotAllowed:
		return "invalid_argument"
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return "denied"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable:
		return "bridge_unavailable"
	default:
		return "internal"
	}
}

// writeError answers a failure as JSON. Drop-in for http.Error.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Message: message, Error: apiErrorBody{Code: errorCode(status), Message: message}})
}

// requireMethod rejects everything but m with a JSON 405.
func requireMethod(m string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != m {
			w.Header().Set("Allow", m)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed; use "+m)
			return
		}
		h(w, r)
	}
}

// statusRecorder captures the status code for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// requestLog writes one line per request: method, path, status, duration,
// remote address and user agent (never bodies). Health probes go to DEBUG
// so a 30-second Docker healthcheck does not fill the log.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logf := bridgeLog.Infof
		if r.URL.Path == "/api/health" || r.URL.Path == "/api/ready" {
			logf = bridgeLog.Debugf
		}
		ua := r.UserAgent()
		if len(ua) > 80 {
			ua = ua[:80] + "…"
		}
		logf("%s %s → %d (%s) from=%s ua=%q", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), remoteHost(r.RemoteAddr), ua)
	})
}

func remoteHost(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// Per-endpoint deadlines for calls that go out to WhatsApp. Derived from the
// request context so a client that disconnects (or the server draining)
// cancels the WhatsApp call instead of leaving it running.
const (
	sendDeadline     = 60 * time.Second  // upload + send
	downloadDeadline = 120 * time.Second // may wait on media-retry from the sender's phone
	actionDeadline   = 30 * time.Second  // read receipts, reactions, presence, history requests
)

func requestContext(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
