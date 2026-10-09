package main

// Parse the whole environment before startup opens the store, a listener or
// an outbox. Filesystem failures are handled later, with startup's defers.
import (
	"errors"
	"fmt"
	"net"
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
	RuntimeDefaults              map[string]runtimeSetting
	MediaMaxBytes                uint64
	MediaRoots                   string
	DeviceName                   string
	LogLevel                     string
	JSONLogs                     bool
	Operator                     operatorConfig
	PairingStdout                bool
	History                      historyLimits
	SnapshotDir                  string
	Archive                      archiveConfig
	SendIncludeActions           bool
	MCPEnvHash                   string
	MCPFallbackBridge            bool
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
	cfg.History, err = parseHistoryLimits(getenv)
	collect(err)
	cfg.SnapshotDir = strings.TrimSpace(getenv("WHATSAPP_SNAPSHOT_DIR"))
	cfg.Archive, err = parseArchiveConfig(getenv)
	collect(err)
	store := strings.TrimSpace(getenv(storeDirEnv))
	if store == "" {
		store = defaultStoreDir
	}
	collect(validateSnapshotLocation(cfg.SnapshotDir, store, cfg.MediaRoots))
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
	cfg.Operator, err = parseOperatorConfig(getenv, net.DefaultResolver.LookupIPAddr)
	collect(err)
	if cfg.Operator.Bind != "" && !isLoopbackBind(cfg.Bind) {
		collect(errors.New("WHATSAPP_BRIDGE_BIND must remain loopback when WHATSAPP_OPERATOR_BIND is enabled"))
	}
	if cfg.Operator.Bind != "" && cfg.Operator.Port == cfg.Port {
		collect(errors.New("WHATSAPP_OPERATOR_PORT must differ from WHATSAPP_BRIDGE_PORT"))
	}
	if cfg.Operator.Bind != "" && (cfg.Port == 8091 || cfg.Operator.Port == 8091) {
		collect(errors.New("Bridge and operator ports must differ from the loopback MCP admin port 8091"))
	}
	cfg.PairingStdout, err = parseBoolEnv(pairingStdoutEnv, getenv(pairingStdoutEnv), cfg.Operator.Bind == "")
	collect(err)
	cfg.RuntimeDefaults, err = runtimeDefaults(getenv)
	collect(err)
	cfg.SendIncludeActions, err = parseBoolEnv("WHATSAPP_SEND_INCLUDE_ACTIONS", getenv("WHATSAPP_SEND_INCLUDE_ACTIONS"), false)
	collect(err)
	mcpToken := strings.TrimSpace(getenv("WHATSAPP_MCP_TOKEN"))
	switch strings.ToLower(mcpToken) {
	case "off", "none", "disabled":
	case "":
		host := strings.TrimSpace(getenv("WHATSAPP_MCP_HOST"))
		cfg.MCPFallbackBridge = host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1"
	default:
		if len(mcpToken) < 16 {
			collect(errors.New("WHATSAPP_MCP_TOKEN must have at least 16 characters"))
		} else {
			cfg.MCPEnvHash = tokenHash(mcpToken)
		}
	}
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
