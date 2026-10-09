package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Fixed loopback destination, no ambient proxy, no redirects, no user URLs.
func newMCPAdminClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func mcpAdminRead(r *http.Request, client *http.Client, token, route string) (json.RawMessage, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://127.0.0.1:8091/admin/v1/"+route, nil)
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

func withMCPAdmin(routes operatorRoutes, token string, client *http.Client) operatorRoutes {
	routes.transcriptionUsage = func(w http.ResponseWriter, r *http.Request) {
		body, err := mcpAdminRead(r, client, token, "transcription/usage")
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
		if body, err := mcpAdminRead(r, client, token, "health"); err == nil {
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
