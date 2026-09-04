package main

// Bridge-wide logger.
//
// whatsmeow already logs through waLog; the bridge's own messages used to go
// through fmt.Printf with no level and no way to silence them, so
// `docker compose logs` mixed two formats. bridgeLog is the single logger
// for code paths that do not receive one explicitly (store, media download,
// REST handlers, webhook delivery). It is write-once configuration, set by
// initLogging() in main() from WHATSAPP_LOG_LEVEL, and Noop in tests.
//
// Deliberate exceptions that still write to stdout directly: the first-run
// token banner (auth.go) and the pairing QR code (printQRCode), which are
// meant to be read by a human, not parsed.

import (
	"os"
	"strings"

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

// initLogging creates the bridge, client and database loggers at the
// configured level. Colour is enabled when stdout is a terminal.
func initLogging() (bridge, client, db waLog.Logger) {
	level := resolveLogLevel(os.Getenv(logLevelEnv))
	color := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		color = true
	}
	bridgeLog = waLog.Stdout("Bridge", level, color)
	return bridgeLog, waLog.Stdout("Client", level, color), waLog.Stdout("Database", "INFO", color)
}
