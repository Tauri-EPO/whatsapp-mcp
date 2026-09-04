package main

// Counters and a Prometheus text endpoint (GET /metrics), dependency-free.
// Unauthenticated like /api/version: it exposes counts and connection state,
// never message content. WHATSAPP_METRICS=false disables the route.

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const metricsEnv = "WHATSAPP_METRICS"

type metricsRegistry struct {
	messagesStored     atomic.Int64
	historyMessages    atomic.Int64
	messagesSent       atomic.Int64
	sendFailures       atomic.Int64
	mediaDownloads     atomic.Int64
	mediaDownloadFails atomic.Int64
	webhookFailures    atomic.Int64
	reconnects         atomic.Int64

	mu       sync.Mutex
	requests map[string]int64 // by status class: "2xx", "4xx", ...
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{requests: map[string]int64{}}
}

func (m *metricsRegistry) recordRequest(status int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.requests[fmt.Sprintf("%dxx", status/100)]++
	m.mu.Unlock()
}

// render writes the Prometheus text exposition for the bridge.
func (b *Bridge) renderMetrics() string {
	m := b.metrics
	connected, paired := false, false
	if b.Client != nil {
		connected = b.Client.IsConnected()
		paired = b.Client.Store != nil && b.Client.Store.ID != nil
	}
	storeBytes, mediaBytes, mediaFiles := int64(0), int64(0), 0
	if b.storeStats != nil {
		storeBytes, mediaBytes, mediaFiles = b.storeStats.snapshot(time.Now())
	}
	bool01 := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	var out []string
	add := func(name, help, kind string, value string) {
		out = append(out, "# HELP "+name+" "+help, "# TYPE "+name+" "+kind, name+" "+value)
	}
	add("whatsapp_bridge_up", "1 while the process serves requests.", "gauge", "1")
	add("whatsapp_bridge_connected", "1 while connected to WhatsApp.", "gauge", fmt.Sprint(bool01(connected)))
	add("whatsapp_bridge_paired", "1 while a WhatsApp session is paired.", "gauge", fmt.Sprint(bool01(paired)))
	add("whatsapp_bridge_uptime_seconds", "Seconds since the bridge started.", "gauge", fmt.Sprintf("%.0f", time.Since(b.startedAt).Seconds()))
	add("whatsapp_bridge_store_bytes", "Bytes under the store directory (databases + media).", "gauge", fmt.Sprint(storeBytes))
	add("whatsapp_bridge_media_bytes", "Bytes of cached media.", "gauge", fmt.Sprint(mediaBytes))
	add("whatsapp_bridge_media_files", "Cached media files.", "gauge", fmt.Sprint(mediaFiles))
	add("whatsapp_bridge_messages_stored_total", "Inbound/outbound messages written from live events.", "counter", fmt.Sprint(m.messagesStored.Load()))
	add("whatsapp_bridge_history_messages_total", "Messages written from history sync.", "counter", fmt.Sprint(m.historyMessages.Load()))
	add("whatsapp_bridge_messages_sent_total", "Successful /api/send calls.", "counter", fmt.Sprint(m.messagesSent.Load()))
	add("whatsapp_bridge_send_failures_total", "Failed /api/send calls.", "counter", fmt.Sprint(m.sendFailures.Load()))
	add("whatsapp_bridge_media_downloads_total", "Media files downloaded.", "counter", fmt.Sprint(m.mediaDownloads.Load()))
	add("whatsapp_bridge_media_download_failures_total", "Media downloads that failed.", "counter", fmt.Sprint(m.mediaDownloadFails.Load()))
	add("whatsapp_bridge_webhook_failures_total", "Outbound webhook POSTs that failed.", "counter", fmt.Sprint(m.webhookFailures.Load()))
	add("whatsapp_bridge_reconnects_total", "Reconnection attempts.", "counter", fmt.Sprint(m.reconnects.Load()))
	m.mu.Lock()
	classes := make([]string, 0, len(m.requests))
	for k := range m.requests {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	out = append(out, "# HELP whatsapp_bridge_http_requests_total REST requests by status class.", "# TYPE whatsapp_bridge_http_requests_total counter")
	for _, k := range classes {
		out = append(out, fmt.Sprintf("whatsapp_bridge_http_requests_total{class=%q} %d", k, m.requests[k]))
	}
	m.mu.Unlock()
	result := ""
	for _, l := range out {
		result += l + "\n"
	}
	return result
}

func (b *Bridge) handleMetrics() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.renderMetrics()))
	}
}
