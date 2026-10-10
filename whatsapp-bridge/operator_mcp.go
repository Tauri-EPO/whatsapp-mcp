package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Fixed validated destination, no ambient proxy, no redirects, no user URLs.
func newMCPAdminClient(address ...string) *http.Client {
	transport := &http.Transport{Proxy: nil}
	if len(address) > 0 && address[0] != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address[0])
		}
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func mcpAdminRead(r *http.Request, client *http.Client, token, route string, host ...string) (json.RawMessage, error) {
	destination := "127.0.0.1:8091"
	if len(host) > 0 && host[0] != "" {
		destination = host[0]
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+destination+"/admin/v1/"+route, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || response.StatusCode != 200 || len(body) > 64<<10 || response.Header.Get("Content-Encoding") != "" || !json.Valid(body) {
		return nil, errors.New("MCP admin response unavailable")
	}
	return body, nil
}

func withMCPAdmin(routes operatorRoutes, token string, client *http.Client, host ...string) operatorRoutes {
	routes.transcriptionUsage = func(w http.ResponseWriter, r *http.Request) {
		body, err := mcpAdminRead(r, client, token, "transcription/usage", host...)
		if err != nil {
			writeErrorCode(w, 503, "mcp_admin_unavailable", "MCP transcription usage unavailable")
			return
		}
		writeJSON(w, 200, body)
	}
	health := routes.health
	routes.health = func(w http.ResponseWriter, r *http.Request) {
		// Keep bridge health independent of an MCP outage; null means unknown.
		if health == nil {
			writeError(w, 503, "Bridge health unavailable")
			return
		}
		capture := &operatorHealthCapture{header: http.Header{}}
		health(capture, r)
		var payload map[string]any
		if json.Unmarshal(capture.body, &payload) != nil {
			writeError(w, 503, "Bridge health unavailable")
			return
		}
		payload["last_mcp_call_at"] = nil
		payload["ok"] = true
		if body, err := mcpAdminRead(r, client, token, "health", host...); err == nil {
			var state struct {
				LastCall *time.Time `json:"last_mcp_call_at"`
			}
			if json.Unmarshal(body, &state) == nil {
				payload["last_mcp_call_at"] = state.LastCall
			}
		}
		writeJSON(w, 200, payload)
	}
	return routes
}

func resolveSplitMCPAdmin(bind string, lookup func(context.Context, string) ([]net.IPAddr, error)) (string, string, error) {
	name := "mcp-admin." + strings.TrimPrefix(bind, splitBridgeAlias+".")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addresses, err := lookup(ctx, name)
	if err != nil || len(addresses) != 1 || addresses[0].Zone != "" {
		return "", "", errors.New("split MCP admin must resolve to exactly one agent IPv4 address")
	}
	ip := addresses[0].IP
	if ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return "", "", errors.New("split MCP admin resolved to an unsafe address")
	}
	return net.JoinHostPort(name, "8091"), net.JoinHostPort(ip.String(), "8091"), nil
}

type operatorHealthCapture struct {
	header http.Header
	body   []byte
}

func (w *operatorHealthCapture) Header() http.Header { return w.header }
func (w *operatorHealthCapture) WriteHeader(_ int)   {}
func (w *operatorHealthCapture) Write(body []byte) (int, error) {
	w.body = append(w.body, body...)
	return len(body), nil
}
