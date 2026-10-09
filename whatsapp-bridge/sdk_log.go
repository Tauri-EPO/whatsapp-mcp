package main

import (
	"errors"
	"fmt"
	"net/url"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// SDK download retries log their error before returning it to the bridge.
// net/http URL errors can expose a CDN path and its access parameters. Install
// this adapter once at startup, rather than changing a live client's logger.
type sdkSafeLogger struct{ waLog.Logger }

func sdkSafeArgs(args []any) []any {
	clean := append([]any(nil), args...)
	for i, arg := range clean {
		if err, ok := arg.(error); ok {
			var networkError *url.Error
			if errors.As(err, &networkError) {
				clean[i] = fmt.Errorf("%s request failed (%T)", networkError.Op, networkError.Err)
			}
		}
	}
	return clean
}

func (l sdkSafeLogger) Warnf(msg string, args ...any)  { l.Logger.Warnf(msg, sdkSafeArgs(args)...) }
func (l sdkSafeLogger) Errorf(msg string, args ...any) { l.Logger.Errorf(msg, sdkSafeArgs(args)...) }
func (l sdkSafeLogger) Infof(msg string, args ...any)  { l.Logger.Infof(msg, sdkSafeArgs(args)...) }
func (l sdkSafeLogger) Debugf(msg string, args ...any) { l.Logger.Debugf(msg, sdkSafeArgs(args)...) }
func (l sdkSafeLogger) Sub(module string) waLog.Logger { return sdkSafeLogger{l.Logger.Sub(module)} }
