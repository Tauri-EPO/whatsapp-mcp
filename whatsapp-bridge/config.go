package main

// Parse the whole environment before startup opens the store, a listener or
// an outbox. Filesystem failures are handled later, with startup's defers.
import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const bridgePortEnv = "WHATSAPP_BRIDGE_PORT"

type bridgeConfig struct {
	Switches                     bridgeSwitches
	Port                         int
	Bind, AllowedHosts           string
	MediaRetention               time.Duration
	StatusMedia                  bool
	RosterSync, SessionKeepalive time.Duration
	ReadOnly                     readOnlyPolicy
	Tools                        toolPolicy
	MediaMaxBytes                uint64
	MediaRoots                   string
	DeviceName                   string
	LogLevel                     string
	JSONLogs                     bool
}

func loadBridgeConfig() (bridgeConfig, error) { return parseBridgeConfig(os.Getenv) }

func parseBridgeConfig(getenv func(string) string) (bridgeConfig, error) {
	cfg := bridgeConfig{
		Port: 8080, AllowedHosts: getenv(bridgeAllowedHostsEnv),
		DeviceName: strings.TrimSpace(getenv("WHATSAPP_DEVICE_NAME")),
		MediaRoots: getenv("WHATSAPP_MEDIA_ROOTS"),
		LogLevel:   resolveLogLevel(getenv(logLevelEnv)), JSONLogs: jsonLogsEnabled(getenv(logFormatEnv)),
	}
	var problems []string
	collect := func(err error) {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	var err error
	cfg.Switches, err = parseBridgeSwitches(getenv)
	collect(err)
	if value := getenv(bridgePortEnv); value != "" {
		cfg.Port, err = strconv.Atoi(value)
		if err != nil || cfg.Port < 1 || cfg.Port > 65535 {
			collect(fmt.Errorf("invalid WHATSAPP_BRIDGE_PORT=%q: expected 1-65535", configValue(value)))
		}
	}
	cfg.Bind, err = resolveBridgeBind(getenv(bridgeBindEnv))
	collect(err)
	cfg.MediaRetention, err = resolveMediaRetention(getenv(mediaRetentionEnv))
	collect(err)
	cfg.StatusMedia, err = resolveStatusAutoDownload(getenv(mediaAutoDownloadStatusEnv))
	collect(err)
	cfg.RosterSync, err = resolveGroupRosterSync(getenv(groupRosterSyncEnv))
	collect(err)
	cfg.SessionKeepalive, err = resolveSessionKeepalive(getenv(sessionKeepaliveEnv))
	collect(err)
	cfg.ReadOnly, err = parseReadOnly(getenv(readOnlyEnv))
	collect(err)
	cfg.MediaMaxBytes, err = resolveMediaMaxBytes(getenv(mediaMaxBytesEnv))
	collect(err)
	_, err = resolveMediaRootsValue(cfg.MediaRoots, true)
	collect(err)
	collect(validateBridgeToken(getenv("WHATSAPP_BRIDGE_TOKEN")))
	// The valid-name appendix is long; put it after every other variable.
	cfg.Tools, err = newToolPolicy(getenv(allowToolsEnv), getenv(denyToolsEnv))
	collect(err)
	if len(problems) > 0 {
		return bridgeConfig{}, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

// configValue bounds echoed invalid input, without changing what parsers read.
func configValue(value string) string {
	runes := []rune(value)
	if len(runes) > 80 {
		return string(runes[:80]) + "...(truncated)"
	}
	return value
}
