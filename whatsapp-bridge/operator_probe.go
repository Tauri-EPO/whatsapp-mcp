package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// The smoke client reads its credential inside the bridge container. Nothing
// secret travels in argv, a temporary file, or the output of this probe.
func operatorStatusProbe(getenv func(string) string, out io.Writer) int {
	cfg, err := parseOperatorConfig(getenv, net.DefaultResolver.LookupIPAddr)
	if err != nil {
		return 1
	}
	if cfg.Bind == "" {
		_, _ = fmt.Fprintln(out, "disabled")
		return 0
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))+"/operator/v1/pairing", nil)
	if err != nil {
		return 1
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Token)
	// Loopback Hosts are always admitted by this listener's explicit list.
	// Dial only the resolved interface, without a second DNS lookup or a
	// wildcard Host exception when the configured bind was a Docker alias.
	request.Host = net.JoinHostPort("localhost", strconv.Itoa(cfg.Port))
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 1
	}
	defer func() { _ = response.Body.Close() }()
	var state struct {
		State string `json:"state"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state) != nil {
		return 1
	}
	switch state.State {
	case "starting", "awaiting_qr", "code_issued", "completing", "paired", "connected", "expired", "logged_out", "logged_out_by_operator", "passkey_required", "passkey_submitted", "passkey_confirm", "passkey_failed":
		_, _ = fmt.Fprintln(out, state.State)
		return 0
	default:
		return 1
	}
}
