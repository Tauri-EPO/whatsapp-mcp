package main

// REST listener address and Host allow-list (issue #58).
//
// Defaults are unchanged and fail-safe: bind 127.0.0.1 and accept only the
// loopback Host spellings for the bound port. Two env vars relax that for
// deployments where the MCP server runs in another container or host:
//
//   WHATSAPP_BRIDGE_BIND          address to listen on (default 127.0.0.1;
//                                 0.0.0.0 / :: for every interface)
//   WHATSAPP_BRIDGE_ALLOWED_HOSTS comma-separated Host values, same
//                                 semantics as WHATSAPP_MCP_ALLOWED_HOSTS:
//                                 "host" matches host and host:<any port>,
//                                 "host:port" matches exactly, "*" accepts
//                                 any Host. Loopback spellings are always
//                                 included.
//
// Binding off loopback without an allow-list keeps the loopback-only list,
// so every non-loopback request is refused with 403 until the operator names
// the hosts clients will use. That is the safe failure: the bearer token still
// guards the API, but DNS-rebinding protection stays on by default.

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const (
	bridgeBindEnv         = "WHATSAPP_BRIDGE_BIND"
	bridgeAllowedHostsEnv = "WHATSAPP_BRIDGE_ALLOWED_HOSTS"
	defaultBridgeBind     = "127.0.0.1"
	allowAnyHost          = "*"
)

// hostAllowList is the compiled form of WHATSAPP_BRIDGE_ALLOWED_HOSTS.
type hostAllowList struct {
	any   bool
	exact map[string]struct{} // "host:port" entries (and the loopback set)
	hosts map[string]struct{} // bare "host" entries: match any port or none
}

// resolveBridgeBind returns the listen address from WHATSAPP_BRIDGE_BIND
// (default 127.0.0.1). An IPv6 literal may be given with or without brackets.
func resolveBridgeBind(value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return defaultBridgeBind, nil
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	if ip := net.ParseIP(v); ip != nil {
		return ip.String(), nil
	}
	if strings.ContainsAny(v, ":/ ") {
		return "", fmt.Errorf("invalid %s=%q: expected an IP address or hostname without port", bridgeBindEnv, value)
	}
	return v, nil
}

// listenAddr joins a bind address and port, bracketing IPv6 literals.
func listenAddr(bind string, port int) string {
	return net.JoinHostPort(bind, fmt.Sprint(port))
}

// isLoopbackBind reports whether bind only reaches the local machine.
func isLoopbackBind(bind string) bool {
	if strings.EqualFold(bind, "localhost") {
		return true
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

// buildHostAllowList compiles the Host allow-list for a listener on port.
// The loopback spellings are always present; allowedHosts is the raw env
// value. Returns the list and a non-empty warning when the configuration
// deserves operator attention (any-Host, or non-loopback bind without hosts).
func buildHostAllowList(port int, bind, allowedHosts string) (hostAllowList, string) {
	list := hostAllowList{exact: buildAllowedHosts(port), hosts: map[string]struct{}{}}
	var entries []string
	for _, raw := range strings.Split(allowedHosts, ",") {
		if e := strings.ToLower(strings.TrimSpace(raw)); e != "" {
			entries = append(entries, e)
		}
	}
	for _, e := range entries {
		if e == allowAnyHost {
			list.any = true
			return list, fmt.Sprintf("%s=* accepts any Host header; DNS-rebinding protection is off", bridgeAllowedHostsEnv)
		}
		if _, _, err := net.SplitHostPort(e); err == nil {
			list.exact[e] = struct{}{}
			continue
		}
		list.hosts[strings.TrimSuffix(strings.TrimPrefix(e, "["), "]")] = struct{}{}
	}
	if len(entries) == 0 && !isLoopbackBind(bind) {
		return list, fmt.Sprintf("bound to %s but %s is unset: only loopback Host headers are accepted, remote clients will get 403 until it names their host", bind, bridgeAllowedHostsEnv)
	}
	return list, ""
}

// allows reports whether a request Host header is acceptable.
func (l hostAllowList) allows(host string) bool {
	if l.any {
		return true
	}
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if _, ok := l.exact[h]; ok {
		return true
	}
	name := h
	if hp, _, err := net.SplitHostPort(h); err == nil {
		name = hp
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	_, ok := l.hosts[name]
	return ok
}

// loadRESTBindConfig reads the two env vars; the error names the offending
// variable so main() can fail fast before pairing.
func loadRESTBindConfig() (bind, allowedHosts string, err error) {
	bind, err = resolveBridgeBind(os.Getenv(bridgeBindEnv))
	if err != nil {
		return "", "", err
	}
	return bind, os.Getenv(bridgeAllowedHostsEnv), nil
}
