package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	operatorBindEnv         = "WHATSAPP_OPERATOR_BIND"
	operatorPortEnv         = "WHATSAPP_OPERATOR_PORT"
	operatorTokenEnv        = "WHATSAPP_OPERATOR_TOKEN"
	operatorTokenFileEnv    = "WHATSAPP_OPERATOR_TOKEN_FILE"
	operatorAllowedHostsEnv = "WHATSAPP_OPERATOR_ALLOWED_HOSTS"
	pairingStdoutEnv        = "WHATSAPP_PAIRING_STDOUT"
)

type operatorConfig struct {
	Bind         string
	Port         int
	Token        string
	AllowedHosts string
}

// Configuration remains opt-in. Resolve a hostname once and bind the actual
// single address, so Docker aliases cannot accidentally widen this listener.
func parseOperatorConfig(getenv func(string) string, lookup func(context.Context, string) ([]net.IPAddr, error)) (operatorConfig, error) {
	cfg := operatorConfig{Port: 8090, AllowedHosts: getenv(operatorAllowedHostsEnv)}
	if value := getenv(operatorPortEnv); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return cfg, errors.New("WHATSAPP_OPERATOR_PORT must be 1-65535")
		}
		cfg.Port = port
	}
	bind := strings.TrimSpace(getenv(operatorBindEnv))
	if bind == "" {
		return cfg, nil
	}
	bind = strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
	if ip := net.ParseIP(bind); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return cfg, errors.New("WHATSAPP_OPERATOR_BIND must not be wildcard or multicast")
		}
		cfg.Bind = ip.String()
	} else {
		if strings.ContainsAny(bind, "*/ :\\") {
			return cfg, errors.New("WHATSAPP_OPERATOR_BIND requires one address or hostname, never a wildcard or port")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addresses, err := lookup(ctx, bind)
		if err != nil {
			return cfg, errors.New("WHATSAPP_OPERATOR_BIND hostname could not be resolved")
		}
		unique := map[string]bool{}
		for _, address := range addresses {
			if address.IP == nil || address.IP.IsUnspecified() || address.IP.IsMulticast() || address.Zone != "" {
				return cfg, errors.New("WHATSAPP_OPERATOR_BIND hostname resolved to an unsafe address")
			}
			unique[address.IP.String()] = true
		}
		if len(unique) != 1 {
			return cfg, errors.New("WHATSAPP_OPERATOR_BIND hostname must resolve to exactly one address on the operator network")
		}
		for address := range unique {
			cfg.Bind = address
		}
	}
	for _, entry := range strings.Split(cfg.AllowedHosts, ",") {
		if strings.Contains(entry, "*") {
			return cfg, errors.New("WHATSAPP_OPERATOR_ALLOWED_HOSTS must name hosts explicitly; wildcard is refused")
		}
	}
	token, path := getenv(operatorTokenEnv), strings.TrimSpace(getenv(operatorTokenFileEnv))
	if token != "" && path != "" {
		return cfg, errors.New("set only one of WHATSAPP_OPERATOR_TOKEN and WHATSAPP_OPERATOR_TOKEN_FILE")
	}
	if path != "" {
		var err error
		token, err = readOperatorToken(path)
		if err != nil {
			return cfg, err
		}
	}
	if err := validateOperatorToken(token); err != nil {
		return cfg, err
	}
	cfg.Token = strings.TrimSpace(token)
	if bridgeToken := strings.TrimSpace(getenv("WHATSAPP_BRIDGE_TOKEN")); bridgeToken != "" {
		if err := cfg.refuseBridgeToken(bridgeToken); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func validateOperatorToken(raw string) error {
	token := strings.TrimSpace(raw)
	refused := errors.New("WHATSAPP_OPERATOR_TOKEN requires at least 32 random bytes encoded as 64 hex or 43-256 unpadded base64url characters; generate one with openssl rand -hex 32")
	if len(token) > 256 || strings.ContainsAny(token, " \r\n\t\v\f") {
		return refused
	}
	for _, placeholder := range []string{"changeme", "change-me", "replace-me", "your-token", "password", "example"} {
		if strings.Contains(strings.ToLower(token), placeholder) {
			return refused
		}
	}
	var decoded []byte
	var err error
	if len(token) == 64 {
		decoded, err = hex.DecodeString(token)
	}
	if len(decoded) == 0 || err != nil {
		decoded, err = base64.RawURLEncoding.Strict().DecodeString(token)
	}
	if err != nil || len(decoded) < 32 {
		return refused
	}
	// This rejects obvious repetition; it cannot prove that an operator used
	// a cryptographic generator. The documented generator is still required.
	for period := 1; period <= len(decoded)/2; period++ {
		if len(decoded)%period == 0 && bytes.Equal(decoded, bytes.Repeat(decoded[:period], len(decoded)/period)) {
			return refused
		}
	}
	counts := map[byte]int{}
	for _, value := range decoded {
		counts[value]++
	}
	entropy := 0.0
	for _, count := range counts {
		frequency := float64(count) / float64(len(decoded))
		entropy -= frequency * math.Log2(frequency)
	}
	if len(counts) < 16 || entropy*float64(len(decoded)) < 128 {
		return refused
	}
	return nil
}

func (cfg operatorConfig) refuseBridgeToken(bridgeToken string) error {
	if cfg.Bind != "" && subtle.ConstantTimeCompare([]byte(cfg.Token), []byte(bridgeToken)) == 1 {
		return errors.New("WHATSAPP_OPERATOR_TOKEN must differ from the effective WHATSAPP_BRIDGE_TOKEN")
	}
	return nil
}

func readOperatorToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE must be a readable regular file, never a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE must be owner-only (0600)")
	}
	f, err := os.Open(path) //nolint:gosec // Operator-configured credential path, bounded and checked for replacement/symlinks below.
	if err != nil {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE could not be opened")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE changed while opening")
	}
	if runtime.GOOS != "windows" && opened.Mode().Perm()&0o077 != 0 {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE must remain owner-only (0600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 258))
	if err != nil || len(data) > 257 {
		return "", errors.New("WHATSAPP_OPERATOR_TOKEN_FILE could not be read within its size limit")
	}
	return strings.TrimSpace(string(data)), nil
}
