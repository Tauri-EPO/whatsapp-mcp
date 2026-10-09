package main

// Bridge-wide logger.
//
// whatsmeow already logs through waLog; the bridge's own messages used to go
// through fmt.Printf with no level and no way to silence them, so
// `docker compose logs` mixed two formats. bridgeLog is the single logger
// for code paths that do not receive one explicitly (store, media download,
// REST handlers, webhook delivery). It is write-once configuration, set by
// initLogging() in run() before configuration validation from WHATSAPP_LOG_LEVEL, and Noop in tests.
//
// Deliberate exceptions that still write to stdout directly: the first-run
// token banner (auth.go) and the pairing QR code (printQRCode), which are
// meant to be read by a human, not parsed.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const logLevelEnv = "WHATSAPP_LOG_LEVEL"

var bridgeLog waLog.Logger = waLog.Noop

// resolveLogLevel maps WHATSAPP_LOG_LEVEL to a waLog level (default INFO).
func resolveLogLevel(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return strings.ToUpper(strings.TrimSpace(value))
	case "WARNING":
		return "WARN"
	case "":
		return "INFO"
	default:
		return "INFO"
	}
}

// textLogger formats each payload once and writes complete marked lines atomically.
type textLogger struct {
	module string
	min    int
	out    io.Writer
	mu     *sync.Mutex
	now    func() time.Time
	color  bool
}

const textLogMaxLine = 8 * 1024
const textLogTruncated = " [truncated]"
const textLogContinuation = "[continued] "

func newTextWriter(module, level string, out io.Writer, color bool) *textLogger {
	return &textLogger{module: module, min: levelRank[resolveLogLevel(level)], out: out, mu: &sync.Mutex{}, now: time.Now, color: color}
}

func newTextLogger(module, level string, color bool) waLog.Logger {
	return newTextWriter(module, level, os.Stdout, color)
}

func textModuleBound(module string) string {
	if len(module) <= 480 {
		return module
	}
	end := 480
	for end > 0 && !utf8.RuneStart(module[end]) {
		end--
	}
	return module[:end] + textLogTruncated
}

// The cap includes prefix, marker and newline, and never splits a UTF-8 rune.
func capTextLine(line string) string {
	if len(line)+1 <= textLogMaxLine {
		return line + "\n"
	}
	end := textLogMaxLine - 1 - len(textLogTruncated)
	for end > 0 && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[:end] + textLogTruncated + "\n"
}

func (l *textLogger) log(level, msg string, args ...any) {
	if levelRank[level] < l.min {
		return
	}
	message := fmt.Sprintf(msg, args...)
	prefix := l.now().Format("15:04:05.000") + " [" + textModuleBound(oneLine(l.module)) + " " + level + "] "
	if l.color {
		color := "\x1b[32m"
		if level == "WARN" {
			color = "\x1b[33m"
		}
		if level == "ERROR" {
			color = "\x1b[31m"
		}
		prefix = color + prefix + "\x1b[0m"
	}
	var rendered strings.Builder
	for index, part := range strings.Split(message, "\n") {
		marker := ""
		if index > 0 {
			marker = textLogContinuation
		}
		rendered.WriteString(capTextLine(prefix + marker + oneLine(part)))
	}
	l.mu.Lock()
	_, _ = io.WriteString(l.out, rendered.String())
	l.mu.Unlock()
}

func (l *textLogger) Warnf(msg string, args ...any)  { l.log("WARN", msg, args...) }
func (l *textLogger) Errorf(msg string, args ...any) { l.log("ERROR", msg, args...) }
func (l *textLogger) Infof(msg string, args ...any)  { l.log("INFO", msg, args...) }
func (l *textLogger) Debugf(msg string, args ...any) { l.log("DEBUG", msg, args...) }
func (l *textLogger) Sub(module string) waLog.Logger {
	return &textLogger{module: l.module + "/" + module, min: l.min, out: l.out, mu: l.mu, now: l.now, color: l.color}
}

// oneLine returns s with every character that could end the line, drive the
// terminal or reorder the text replaced by its Go escape (\n, \x1b, \u202e),
// and every byte that is not UTF-8 by \xNN. Ordinary text, accents, emoji and
// right-to-left scripts with their directional marks included, comes back
// unchanged; so does a tab. Literal backslashes are doubled so escapes can be
// distinguished from the original text.
func oneLine(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, unsafeInLogLine) < 0 && !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unsafeInLogLine(r):
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// unsafeInLogLine: control characters (line breaks, ESC, NUL, the C1 range
// with its own "next line"), the Unicode line and paragraph separators
// (U+2028, U+2029), and the bidirectional controls that reorder what follows.
func unsafeInLogLine(r rune) bool {
	if r == '\t' {
		return false
	}
	return unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || reordersText(r)
}

// reordersText: the bidirectional embeddings, overrides and isolates
// (U+202A to U+202E, U+2066 to U+2069), which make a terminal show text in
// another order than it was written. The directional marks ordinary
// right-to-left text carries (U+200E, U+200F, U+061C) are not among them: they
// are invisible hints next to a word, not a way to display other text.
func reordersText(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// initLogging creates the bridge, client and database loggers at the
// configured level and format (logging_json.go). Colour is enabled when stdout is a terminal.
func initLogging() (bridge, client, db waLog.Logger) {
	level := resolveLogLevel(os.Getenv(logLevelEnv))
	bridge, client, db = newLoggerSet(level, jsonLogsEnabled(os.Getenv(logFormatEnv)))
	bridgeLog = bridge
	return bridge, client, db
}
