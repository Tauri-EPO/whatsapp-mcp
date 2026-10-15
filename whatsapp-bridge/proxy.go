package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	return proxyClientFactoryTransport(base, proxyURL, http.DefaultTransport.(*http.Transport))
}

func proxyClientFactoryTransport(base runtimeClientFactory, proxyURL *url.URL, transport *http.Transport) runtimeClientFactory {
	if proxyURL == nil {
		return base
	}
	if base == nil {
		base = whatsmeow.NewClient
	}
	redactor := newProxyRedactor(proxyURL)
	return func(device *store.Device, logger waLog.Logger) *whatsmeow.Client {
		client := base(device, proxySafeLogger{Logger: logger, redactor: redactor})
		if client == nil {
			return nil
		}
		// The pinned SDK applies this to pre/post-login websocket and media
		// upload/download only, never to any bridge-owned HTTP client.
		// net/http's explicit Proxy URL also supports socks5. This avoids the
		// SDK SOCKS helper retaining ProxyFromEnvironment on its cloned transport.
		client.SetProxy(http.ProxyURL(proxyURL))
		// Sanitize at the HTTP boundary as well: bridge callers log returned
		// websocket errors through their own logger, outside the SDK adapter.
		h := proxyHTTPClient(proxyURL, transport)
		client.SetPreLoginHTTPClient(h)
		client.SetWebsocketHTTPClient(h)
		client.SetMediaHTTPClient(h)
		return client
	}
}

type proxySafeLogger struct {
	waLog.Logger
	redactor *strings.Replacer
}

func newProxyRedactor(u *url.URL) *strings.Replacer {
	values := []string{u.String(), u.Redacted(), u.Host, u.Hostname()}
	if u.User != nil {
		password, _ := u.User.Password()
		values = append(values, u.User.String()+"@", u.User.Username()+":"+password+"@")
		if len(password) >= 8 {
			values = append(values, password)
		}
	}
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		if value != "" {
			pairs = append(pairs, value, "[proxy]")
		}
	}
	return strings.NewReplacer(pairs...)
}

// Known production adapters expose their level without formatting a discarded
// message. Unknown injected loggers retain their existing emission behavior.
func proxyLogEnabled(logger waLog.Logger, level string) bool {
	switch l := logger.(type) {
	case *textLogger:
		return levelRank[level] >= l.min
	case *jsonLogger:
		return levelRank[level] >= l.min
	case sdkSafeLogger:
		return proxyLogEnabled(l.Logger, level)
	case privatePairingLogger:
		return level != "DEBUG" && proxyLogEnabled(l.Logger, level)
	default:
		return logger != waLog.Noop
	}
}

func (l proxySafeLogger) emit(level, msg string, args ...any) {
	if !proxyLogEnabled(l.Logger, level) {
		return
	}
	text := l.redactor.Replace(fmt.Sprintf(msg, sdkSafeArgs(args)...))
	switch level {
	case "WARN":
		l.Logger.Warnf("%s", text)
	case "ERROR":
		l.Logger.Errorf("%s", text)
	case "INFO":
		l.Logger.Infof("%s", text)
	case "DEBUG":
		l.Logger.Debugf("%s", text)
	}
}

func (l proxySafeLogger) Warnf(msg string, args ...any)  { l.emit("WARN", msg, args...) }
func (l proxySafeLogger) Errorf(msg string, args ...any) { l.emit("ERROR", msg, args...) }
func (l proxySafeLogger) Infof(msg string, args ...any)  { l.emit("INFO", msg, args...) }
func (l proxySafeLogger) Debugf(msg string, args ...any) { l.emit("DEBUG", msg, args...) }
func (l proxySafeLogger) Sub(module string) waLog.Logger {
	return proxySafeLogger{l.Logger.Sub(module), l.redactor}
}

type proxySafeTransport struct{ http.RoundTripper }

func proxyHTTPClient(u *url.URL, base *http.Transport) *http.Client {
	transport := base.Clone()
	transport.Proxy = http.ProxyURL(u)
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return conn, proxyRequestError{err}
		}
		return proxySafeConn{conn}, nil
	}
	return &http.Client{Transport: proxySafeTransport{transport}}
}

// The SDK also logs websocket/media stream errors after RoundTrip succeeds.
// Preserve net.Conn behavior while redacting peer addresses on reads/writes.
type proxySafeConn struct{ net.Conn }

func proxyStreamError(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return proxyRequestError{err}
}

func (c proxySafeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, proxyStreamError(err)
}

func (c proxySafeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, proxyStreamError(err)
}

// No destination, proxy DNS/IP address, credentials or CONNECT response can
// escape to callers. Unwrap preserves cancellation/timeout classification.
type proxyRequestError struct{ cause error }

func (e proxyRequestError) Error() string { return "WhatsApp proxy request failed: [proxy]" }
func (e proxyRequestError) Unwrap() error { return e.cause }

func (t proxySafeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.RoundTripper.RoundTrip(r)
	if err != nil {
		return response, proxyRequestError{err}
	}
	return response, nil
}
