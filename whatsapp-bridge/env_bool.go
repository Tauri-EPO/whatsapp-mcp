package main

// Boolean switches read from the environment.
//
// One parser for all of them (issue #483): 1/true/yes/on and 0/false/no/off,
// any case, surrounding space ignored. Unset or empty means the switch's
// default. Anything else is an error naming the variable, and main() refuses
// to start on it: WHATSAPP_MEDIA_AUTODOWNLOAD=flase used to mean "true" and
// keep filling the disk, WEBHOOK_ENABLED=fasle kept posting every message,
// with nothing in the log to say the value had been ignored.

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	boolTrue  = []string{"1", "true", "yes", "on"}
	boolFalse = []string{"0", "false", "no", "off"}
)

// parseBoolEnv reads the value of the switch called name; def is what unset
// means.
func parseBoolEnv(name, raw string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return def, nil
	}
	for _, t := range boolTrue {
		if v == t {
			return true, nil
		}
	}
	for _, f := range boolFalse {
		if v == f {
			return false, nil
		}
	}
	return def, fmt.Errorf("%s=%q is not a boolean; use one of %s", name,
		raw, strings.Join(append(append([]string{}, boolTrue...), boolFalse...), ", "))
}

// The names are constants ending in Env on purpose: that spelling is how
// tests/test_env_docs.py finds the variables the bridge reads.
const (
	forwardSelfEnv    = "FORWARD_SELF"
	webhookEnabledEnv = "WEBHOOK_ENABLED"
)

// bridgeSwitches are the four switches that are on unless turned off. main()
// loads them once, before anything is opened, and hands them to newBridge.
type bridgeSwitches struct {
	ForwardSelf       bool // FORWARD_SELF: self-sent messages reach the webhook
	MediaAutoDownload bool // WHATSAPP_MEDIA_AUTODOWNLOAD: cache inbound media on arrival
	WebhookEnabled    bool // WEBHOOK_ENABLED: outbound webhooks at all
	Metrics           bool // WHATSAPP_METRICS: serve GET /metrics
}

// loadBridgeSwitches reads the four from the environment.
func loadBridgeSwitches() (bridgeSwitches, error) {
	return parseBridgeSwitches(os.Getenv)
}

// parseBridgeSwitches reads all four and reports every value it cannot read
// in one error, so an env file with two typos costs one restart, not two.
func parseBridgeSwitches(getenv func(string) string) (bridgeSwitches, error) {
	var sw bridgeSwitches
	var unreadable []string
	for _, s := range []struct {
		name string
		dst  *bool
	}{
		{forwardSelfEnv, &sw.ForwardSelf},
		{mediaAutoDownloadEnv, &sw.MediaAutoDownload},
		{webhookEnabledEnv, &sw.WebhookEnabled},
		{metricsEnv, &sw.Metrics},
	} {
		value, err := parseBoolEnv(s.name, getenv(s.name), true)
		if err != nil {
			unreadable = append(unreadable, err.Error())
		}
		*s.dst = value
	}
	if len(unreadable) > 0 {
		// One line: this goes to the log as it is.
		return bridgeSwitches{}, errors.New(strings.Join(unreadable, "; "))
	}
	return sw, nil
}
