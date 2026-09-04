package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// recordingLogger captures bridgeLog output so tests can assert on it
// without redirecting os.Stdout.
type recordingLogger struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (r *recordingLogger) log(level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.buf, "[%s] "+msg+"\n", append([]any{level}, args...)...)
}

func (r *recordingLogger) Warnf(msg string, args ...any)  { r.log("WARN", msg, args...) }
func (r *recordingLogger) Errorf(msg string, args ...any) { r.log("ERROR", msg, args...) }
func (r *recordingLogger) Infof(msg string, args ...any)  { r.log("INFO", msg, args...) }
func (r *recordingLogger) Debugf(msg string, args ...any) { r.log("DEBUG", msg, args...) }
func (r *recordingLogger) Sub(string) waLog.Logger        { return r }

func (r *recordingLogger) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// installRecordingLogger swaps bridgeLog for the duration of the test.
func installRecordingLogger(t *testing.T) *recordingLogger {
	t.Helper()
	prev := bridgeLog
	rec := &recordingLogger{}
	bridgeLog = rec
	t.Cleanup(func() { bridgeLog = prev })
	return rec
}

func TestResolveLogLevel(t *testing.T) {
	cases := map[string]string{
		"":        "INFO",
		"debug":   "DEBUG",
		" Info ":  "INFO",
		"warn":    "WARN",
		"WARNING": "WARN",
		"error":   "ERROR",
		"trace":   "INFO", // unknown values fall back to the default
	}
	for in, want := range cases {
		if got := resolveLogLevel(in); got != want {
			t.Errorf("resolveLogLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInitLoggingSetsBridgeLogger(t *testing.T) {
	t.Setenv(logLevelEnv, "debug")
	prev := bridgeLog
	t.Cleanup(func() { bridgeLog = prev })

	bridge, client, db := initLogging()
	if bridge == nil || client == nil || db == nil {
		t.Fatal("initLogging returned a nil logger")
	}
	if bridgeLog != bridge {
		t.Fatal("initLogging did not install the package logger")
	}
}
