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
	"os"
	"strconv"
	"strings"
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

// oneLineLogger keeps every message a text logger prints on one line.
//
// A message ID, a push name, a group subject are whatever the other side put
// in the stanza, and the bridge and whatsmeow print them with %s. In the text
// format a line break inside one of them started a second line that reads like
// a log entry of its own (issue #492). The JSON format never had the problem:
// its message is one JSON string. Wrapping the logger, instead of quoting at
// each call site, covers the lines nobody thought of and the ones whatsmeow
// writes.
//
// min is the level below which nothing is printed, so a DEBUG line is not
// formatted just to be dropped by the logger underneath.
type oneLineLogger struct {
	inner waLog.Logger
	min   int
}

func (l oneLineLogger) emit(level string, write func(string, ...any), msg string, args []any) {
	if levelRank[level] < l.min {
		return
	}
	write("%s", oneLine(fmt.Sprintf(msg, args...)))
}

func (l oneLineLogger) Warnf(msg string, args ...any)  { l.emit("WARN", l.inner.Warnf, msg, args) }
func (l oneLineLogger) Errorf(msg string, args ...any) { l.emit("ERROR", l.inner.Errorf, msg, args) }
func (l oneLineLogger) Infof(msg string, args ...any)  { l.emit("INFO", l.inner.Infof, msg, args) }
func (l oneLineLogger) Debugf(msg string, args ...any) { l.emit("DEBUG", l.inner.Debugf, msg, args) }
func (l oneLineLogger) Sub(module string) waLog.Logger {
	return oneLineLogger{inner: l.inner.Sub(module), min: l.min}
}

// newTextLogger is whatsmeow's stdout logger behind oneLineLogger.
func newTextLogger(module, level string, color bool) waLog.Logger {
	return oneLineLogger{inner: waLog.Stdout(module, level, color), min: levelRank[level]}
}

// oneLine returns s with every character that could end the line, drive the
// terminal or reorder the text replaced by its Go escape (\n, \x1b, \u202e),
// and every byte that is not UTF-8 by \xNN. Ordinary text, accents, emoji and
// right-to-left scripts with their directional marks included, comes back
// unchanged; so does a tab. A backslash is not doubled, so the output is for
// reading, not for decoding: it cannot be turned back into the original bytes.
func oneLine(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, unsafeInLogLine) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
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
