package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Liveness (/api/health) and readiness (/api/ready). REST starts before
// pairing, so health is 200 as soon as the listener is up and ready is 200
// only while paired and connected.

// handleHealth serves GET /api/health.
func (b *Bridge) handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := b.healthStatus()
		status["webhook"] = b.webhookStatus().compact()
		writeJSON(w, http.StatusOK, status)
	}
}

// The detailed view is served only by the tool-policy-protected webhook read.
// Health carries the four-field compact projection; operator health is unchanged.
type webhookCompactStatus struct {
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	Failures   int    `json:"consecutive_failures"`
	Reason     string `json:"reason"`
}

type webhookStatusView struct {
	runtimeWebhookFilters
	Configured   bool              `json:"configured"`
	Enabled      bool              `json:"enabled"`
	Events       []string          `json:"events"`
	AuthSet      bool              `json:"authorization_set"`
	AllowedChats webhookChatFilter `json:"allowed_chats"`
	URL          string            `json:"url"`
	LastSuccess  *time.Time        `json:"last_success"`
	Failures     int               `json:"consecutive_failures"`
	Reason       string            `json:"reason"`
	Queued       int               `json:"queued"`
	Dropped      uint64            `json:"dropped"`
}

func (s webhookStatusView) compact() webhookCompactStatus {
	return webhookCompactStatus{Configured: s.Configured, Enabled: s.Enabled, Failures: s.Failures, Reason: s.Reason}
}

func (b *Bridge) webhookStatus() webhookStatusView {
	if b.RuntimeWebhook == nil {
		return webhookStatusView{Events: []string{}, AllowedChats: allChatsFilter, Reason: "runtime webhook disabled"}
	}
	s := b.RuntimeWebhook.snapshot()
	if s.URL == "" {
		return webhookStatusView{Events: []string{}, AllowedChats: allChatsFilter, Reason: "runtime webhook has no destination"}
	}
	return webhookStatusView{
		runtimeWebhookFilters: s.runtimeWebhookFilters, Events: s.Events, AuthSet: s.AuthSet, AllowedChats: s.AllowedChats,
		Configured: true, Enabled: s.Enabled, URL: maskedWebhookURL(s.URL),
		LastSuccess: s.LastSuccess, Failures: s.Failures, Reason: safeWebhookReason(s.Reason),
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
	path := []rune(strings.TrimPrefix(u.EscapedPath(), "/"))
	suffix := ""
	if len([]rune(strings.TrimPrefix(u.Path, "/"))) >= 12 {
		suffix = string(path[len(path)-4:])
	}
	return u.Scheme + "://" + u.Host + "/…" + suffix
}

// Reasons are codes, never receiver error text. Keep unknown future values
// private until explicitly supported by the agent-facing contract.
func safeWebhookReason(reason string) string {
	switch reason {
	case "", "receiver_refused", "request_failed", "delivery_failed", "cancelled", "disabled", "configuration_changed", "expired", "payload_limit", "queue_full":
		return reason
	}
	if len(reason) == 8 && strings.HasPrefix(reason, "http_") {
		valid := true
		for _, c := range reason[5:] {
			valid = valid && c >= '0' && c <= '9'
		}
		if valid {
			return reason
		}
	}
	return "runtime webhook reason unavailable"
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
