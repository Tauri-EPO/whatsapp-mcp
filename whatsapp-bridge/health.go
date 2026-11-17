package main

import (
	"net/http"
	"net/url"
	"time"
)

// Liveness (/api/health) and readiness (/api/ready). REST starts before
// pairing, so health is 200 as soon as the listener is up and ready is 200
// only while paired and connected.

// handleHealth serves GET /api/health.
func (b *Bridge) handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := b.healthStatus()
		status["webhook"] = b.webhookStatus()
		writeJSON(w, http.StatusOK, status)
	}
}

// Only the authenticated REST health response gets this view. The private
// operator health keeps its existing snapshot, including operator-only fields.
type webhookStatusView struct {
	runtimeWebhookFilters
	Configured  bool       `json:"configured"`
	Enabled     bool       `json:"enabled"`
	Events      []string   `json:"events"`
	AuthSet     bool       `json:"authorization_set"`
	URL         string     `json:"url"`
	LastSuccess *time.Time `json:"last_success"`
	Failures    int        `json:"consecutive_failures"`
	Reason      string     `json:"reason"`
	Queued      int        `json:"queued"`
	Dropped     uint64     `json:"dropped"`
}

func (b *Bridge) webhookStatus() webhookStatusView {
	if b.RuntimeWebhook == nil {
		return webhookStatusView{Events: []string{}, Reason: "runtime webhook disabled"}
	}
	s := b.RuntimeWebhook.snapshot()
	if s.URL == "" {
		return webhookStatusView{Events: []string{}, Reason: "runtime webhook has no destination"}
	}
	return webhookStatusView{
		runtimeWebhookFilters: s.runtimeWebhookFilters, Events: s.Events, AuthSet: s.AuthSet,
		Configured: true, Enabled: s.Enabled, URL: maskedWebhookURL(s.URL),
		LastSuccess: s.LastSuccess, Failures: s.Failures, Reason: s.Reason,
		Queued: s.Queued, Dropped: s.Dropped,
	}
}

// Never retain userinfo, query or fragment. Short paths are hidden completely
// so the suffix cannot disclose the entire path.
func maskedWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	path := []rune(u.EscapedPath())
	suffix := ""
	if len(path) > 4 {
		suffix = string(path[len(path)-4:])
	}
	return u.Scheme + "://" + u.Host + "/…" + suffix
}

// handleReady serves GET /api/ready.
func (b *Bridge) handleReady() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := b.healthStatus()
		code := http.StatusOK
		if status["status"] != "ok" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, status)
	}
}
