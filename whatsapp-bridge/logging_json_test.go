package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONLogger_WritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	l := newJSONLogger("bridge", "INFO", &buf)
	l.now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	l.Debugf("hidden %d", 1)
	l.Infof("hello %q", "world")
	l.Sub("Client").Warnf("sub")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	var first map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 is not JSON: %v", err)
	}
	want := map[string]string{"ts": "2026-09-04T12:00:00.000Z", "level": "INFO", "module": "bridge", "msg": `hello "world"`}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("%s = %q, want %q", k, first[k], v)
		}
	}
	var second map[string]string
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second["module"] != "bridge/Client" || second["level"] != "WARN" {
		t.Errorf("sub logger line = %v", second)
	}
}

func TestJSONLogsEnabled(t *testing.T) {
	for in, want := range map[string]bool{"": false, "text": false, "json": true, " JSON ": true} {
		if got := jsonLogsEnabled(in); got != want {
			t.Errorf("jsonLogsEnabled(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewLoggerSet_JSONImplementsSub(t *testing.T) {
	bridge, client, db := newLoggerSet("DEBUG", true)
	for _, l := range []any{bridge, client, db} {
		if _, ok := l.(*jsonLogger); !ok {
			t.Errorf("logger %T is not a jsonLogger", l)
		}
	}
	bridge, _, _ = newLoggerSet("DEBUG", false)
	if _, ok := bridge.(*jsonLogger); ok {
		t.Errorf("text format returned a jsonLogger")
	}
}
