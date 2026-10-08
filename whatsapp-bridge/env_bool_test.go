package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseBoolEnv(t *testing.T) {
	cases := []struct {
		raw     string
		def     bool
		want    bool
		wantErr bool
	}{
		{raw: "", def: true, want: true},
		{raw: "", def: false, want: false},
		{raw: "   ", def: true, want: true},
		{raw: "1", want: true},
		{raw: "true", want: true},
		{raw: "TRUE", want: true},
		{raw: " yes ", want: true},
		{raw: "On", want: true},
		{raw: "0", def: true, want: false},
		{raw: "false", def: true, want: false},
		{raw: "No", def: true, want: false},
		{raw: " OFF ", def: true, want: false},
		// A typo is never the default in disguise.
		{raw: "flase", def: true, wantErr: true},
		{raw: "treu", def: false, wantErr: true},
		{raw: "2", def: true, wantErr: true},
		{raw: "enabled", def: true, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseBoolEnv("SOME_SWITCH", tc.raw, tc.def)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "SOME_SWITCH") || !strings.Contains(err.Error(), tc.raw) {
				t.Errorf("parseBoolEnv(%q, default %v) = %v, %v; want an error naming the variable and the value", tc.raw, tc.def, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseBoolEnv(%q, default %v) = %v, %v; want %v", tc.raw, tc.def, got, err, tc.want)
		}
	}
}

// The four switches that used to fall back to their default on a value they
// could not read (issue #483): unset keeps them on, each accepted spelling is
// honoured, and a typo in any of them is an error main() stops on.
func TestParseBridgeSwitches(t *testing.T) {
	names := []string{forwardSelfEnv, mediaAutoDownloadEnv, webhookEnabledEnv, metricsEnv}
	field := func(sw bridgeSwitches, name string) bool {
		switch name {
		case forwardSelfEnv:
			return sw.ForwardSelf
		case mediaAutoDownloadEnv:
			return sw.MediaAutoDownload
		case webhookEnabledEnv:
			return sw.WebhookEnabled
		default:
			return sw.Metrics
		}
	}
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	all, err := parseBridgeSwitches(env(nil))
	if err != nil || all != (bridgeSwitches{ForwardSelf: true, MediaAutoDownload: true, WebhookEnabled: true, Metrics: true}) {
		t.Fatalf("unset = %+v, %v; want every switch on", all, err)
	}

	for _, name := range names {
		for _, off := range boolFalse {
			sw, err := parseBridgeSwitches(env(map[string]string{name: off}))
			if err != nil {
				t.Fatalf("%s=%s: %v", name, off, err)
			}
			for _, other := range names {
				if got, want := field(sw, other), other != name; got != want {
					t.Errorf("%s=%s: %s = %v, want %v", name, off, other, got, want)
				}
			}
		}
		for _, on := range boolTrue {
			if sw, err := parseBridgeSwitches(env(map[string]string{name: on})); err != nil || !field(sw, name) {
				t.Errorf("%s=%s = %+v, %v; want it on", name, on, sw, err)
			}
		}
		_, err := parseBridgeSwitches(env(map[string]string{name: "flase"}))
		if err == nil || !strings.Contains(err.Error(), name+"=") {
			t.Errorf("%s=flase: error %v, want one naming the variable", name, err)
		}
	}

	// The fifth switch is off unless asked for, and just as strict. It moves
	// alone: the four above stay on, and none of them turns it on.
	for _, on := range boolTrue {
		sw, err := parseBridgeSwitches(env(map[string]string{webhookForwardStatusEnv: on}))
		if err != nil || sw != (bridgeSwitches{ForwardSelf: true, MediaAutoDownload: true, WebhookEnabled: true, Metrics: true, ForwardStatus: true}) {
			t.Errorf("%s=%s = %+v, %v; want it on and nothing else changed", webhookForwardStatusEnv, on, sw, err)
		}
	}
	for _, off := range boolFalse {
		if sw, err := parseBridgeSwitches(env(map[string]string{webhookForwardStatusEnv: off})); err != nil || sw != all {
			t.Errorf("%s=%s = %+v, %v; want the defaults", webhookForwardStatusEnv, off, sw, err)
		}
	}
	for _, name := range names {
		if sw, err := parseBridgeSwitches(env(map[string]string{name: "off"})); err != nil || sw.ForwardStatus {
			t.Errorf("%s=off turned status forwarding on: %+v, %v", name, sw, err)
		}
	}
	if _, err := parseBridgeSwitches(env(map[string]string{webhookForwardStatusEnv: "treu"})); err == nil || !strings.Contains(err.Error(), webhookForwardStatusEnv+"=") {
		t.Errorf("%s=treu: error %v, want one naming the variable", webhookForwardStatusEnv, err)
	}

	// Two typos are reported together.
	_, err = parseBridgeSwitches(env(map[string]string{forwardSelfEnv: "flase", metricsEnv: "ture"}))
	if err == nil || !strings.Contains(err.Error(), forwardSelfEnv+"=") || !strings.Contains(err.Error(), metricsEnv+"=") {
		t.Errorf("two unreadable values: error %v, want both variables named", err)
	}
}

// newBridge is where the switches become behaviour: each one lands on its own
// field and on nothing else.
func TestNewBridgeAppliesEachSwitch(t *testing.T) {
	cases := map[string]bridgeSwitches{
		"forward self":        {ForwardSelf: true},
		"media auto-download": {MediaAutoDownload: true},
		"webhook":             {WebhookEnabled: true},
		"metrics":             {Metrics: true},
		"forward status":      {ForwardStatus: true},
		"none":                {},
	}
	for name, switches := range cases {
		b := newBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger(), "token-0123456789abcdef", nil, switches)
		t.Cleanup(func() { b.Shutdown(time.Second) })
		got := bridgeSwitches{
			ForwardSelf:       b.ForwardSelf,
			MediaAutoDownload: b.MediaAutoDownload,
			WebhookEnabled:    b.Webhook.Enabled(),
			Metrics:           b.MetricsEnabled,
			ForwardStatus:     b.ForwardStatus,
		}
		if got != switches {
			t.Errorf("%s: the bridge runs with %+v, want %+v", name, got, switches)
		}
	}
}

// Switch parsing also accepts the real environment reader.
func TestLoadBridgeSwitchesReadsTheEnvironment(t *testing.T) {
	t.Setenv(webhookEnabledEnv, "off")
	t.Setenv(metricsEnv, "treu")
	if _, err := parseBridgeSwitches(os.Getenv); err == nil || !strings.Contains(err.Error(), metricsEnv) {
		t.Fatalf("error = %v, want one naming %s", err, metricsEnv)
	}
	t.Setenv(metricsEnv, "")
	t.Setenv(forwardSelfEnv, "")
	t.Setenv(mediaAutoDownloadEnv, "")
	sw, err := parseBridgeSwitches(os.Getenv)
	if err != nil || sw.WebhookEnabled || !sw.Metrics || !sw.ForwardSelf || !sw.MediaAutoDownload {
		t.Fatalf("switches = %+v, %v", sw, err)
	}
}
