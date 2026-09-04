package main

// JSON log lines (WHATSAPP_LOG_FORMAT=json): one object per line with ts,
// level, module and msg, so `docker compose logs` can be shipped to Loki,
// Elastic or journald without a regex. Implements waLog.Logger like
// waLog.Stdout does, so the whatsmeow client uses it too.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const logFormatEnv = "WHATSAPP_LOG_FORMAT"

var levelRank = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

type jsonLogger struct {
	module string
	min    int
	out    io.Writer
	mu     *sync.Mutex
	now    func() time.Time
}

// newJSONLogger writes at or above level to out.
func newJSONLogger(module, level string, out io.Writer) *jsonLogger {
	return &jsonLogger{module: module, min: levelRank[resolveLogLevel(level)], out: out, mu: &sync.Mutex{}, now: time.Now}
}

func (l *jsonLogger) log(level, msg string, args ...any) {
	if levelRank[level] < l.min {
		return
	}
	line, _ := json.Marshal(map[string]string{
		"ts":     l.now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"level":  level,
		"module": l.module,
		"msg":    fmt.Sprintf(msg, args...),
	})
	l.mu.Lock()
	_, _ = l.out.Write(append(line, '\n'))
	l.mu.Unlock()
}

func (l *jsonLogger) Warnf(msg string, args ...any)  { l.log("WARN", msg, args...) }
func (l *jsonLogger) Errorf(msg string, args ...any) { l.log("ERROR", msg, args...) }
func (l *jsonLogger) Infof(msg string, args ...any)  { l.log("INFO", msg, args...) }
func (l *jsonLogger) Debugf(msg string, args ...any) { l.log("DEBUG", msg, args...) }
func (l *jsonLogger) Sub(module string) waLog.Logger {
	return &jsonLogger{module: l.module + "/" + module, min: l.min, out: l.out, mu: l.mu, now: l.now}
}

// jsonLogsEnabled reports WHATSAPP_LOG_FORMAT=json.
func jsonLogsEnabled(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "json")
}

// newLoggerSet builds the bridge/client/database loggers for the configured
// level and format.
func newLoggerSet(level string, jsonFormat bool) (bridge, client, db waLog.Logger) {
	if jsonFormat {
		return newJSONLogger("bridge", level, os.Stdout), newJSONLogger("client", level, os.Stdout), newJSONLogger("database", "INFO", os.Stdout)
	}
	color := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		color = true
	}
	return waLog.Stdout("Bridge", level, color), waLog.Stdout("Client", level, color), waLog.Stdout("Database", "INFO", color)
}
