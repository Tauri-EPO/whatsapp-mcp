package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Parse once, before startup effects. Diagnostics never echo proxy values.
func parseWhatsAppProxy(getenv func(string) string) (*url.URL, error) {
	if getenv("WHATSAPP_PROXY_URL") == "" && getenv("WHATSAPP_PROXY_URL_FILE") == "" {
		return nil, nil
	}
	raw, err := mediaSecret(getenv, "WHATSAPP_PROXY_URL", "WHATSAPP_PROXY_URL_FILE")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	invalid := errors.New("WHATSAPP_PROXY_URL: expected socks5:// or http(s):// host with optional credentials and port")
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") || u.Hostname() == "" ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		strings.ContainsAny(u.Hostname(), " /\\") || strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, invalid
		}
	}
	if u.Scheme == "socks5" && u.Port() == "" {
		return nil, invalid
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if u.User.Username() == "" || len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, invalid
		}
	}
	return u, nil
}

// Wrap the factory so operator restart/logout handoffs retain the same policy.
func proxyClientFactory(base runtimeClientFactory, proxyURL *url.URL) runtimeClientFactory {
	if proxyURL == nil {
		return base
	}
	if base == nil {
		base = whatsmeow.NewClient
	}
	return func(device *store.Device, logger waLog.Logger) *whatsmeow.Client {
		client := base(device, proxySafeLogger{Logger: logger, proxyURL: proxyURL})
		if client == nil {
			return nil
		}
		// The pinned SDK applies this to pre/post-login websocket and media
		// upload/download only, never to any bridge-owned HTTP client.
		// net/http's explicit Proxy URL also supports socks5. This avoids the
		// SDK SOCKS helper retaining ProxyFromEnvironment on its cloned transport.
		client.SetProxy(http.ProxyURL(proxyURL))
		return client
	}
}

type proxySafeLogger struct {
	waLog.Logger
	proxyURL *url.URL
}

func (l proxySafeLogger) clean(msg string, args ...any) string {
	text := fmt.Sprintf(msg, sdkSafeArgs(args)...)
	values := []string{l.proxyURL.String(), l.proxyURL.Host}
	if l.proxyURL.User != nil {
		password, _ := l.proxyURL.User.Password()
		values = append(values, l.proxyURL.User.String(), l.proxyURL.User.Username(), password)
	}
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[proxy redacted]")
		}
	}
	return text
}

func (l proxySafeLogger) Warnf(msg string, args ...any) { l.Logger.Warnf("%s", l.clean(msg, args...)) }
func (l proxySafeLogger) Errorf(msg string, args ...any) {
	l.Logger.Errorf("%s", l.clean(msg, args...))
}
func (l proxySafeLogger) Infof(msg string, args ...any) { l.Logger.Infof("%s", l.clean(msg, args...)) }
func (l proxySafeLogger) Debugf(msg string, args ...any) {
	l.Logger.Debugf("%s", l.clean(msg, args...))
}
func (l proxySafeLogger) Sub(module string) waLog.Logger {
	return proxySafeLogger{l.Logger.Sub(module), l.proxyURL}
}
