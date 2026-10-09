package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ingestChatsEnv = "TRANSCRIBE_ON_INGEST_CHATS"
const transcriptionCapEnv = "TRANSCRIBE_MONTHLY_MAX_MINUTES"
const transcriptionScopeEnv = "TRANSCRIBE_CAP_SCOPE"
const runtimeSettingsSchema = `CREATE TABLE IF NOT EXISTS runtime_settings (
	key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMP NOT NULL,
	version INTEGER NOT NULL CHECK(version > 0)
)`

type runtimeSetting struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}
type runtimeSettingsSnapshot struct {
	Version  int64                     `json:"version"`
	Settings map[string]runtimeSetting `json:"settings"`
	policy   toolPolicy
}
type settingDefinition struct {
	key, env     string
	defaultValue any
	parse        func(json.RawMessage) (any, error)
	parseEnv     func(string) (any, error)
}

func parseIngestChats(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = "all"
	}
	if value != "all" && value != "direct" {
		return "", errors.New("expected all or direct")
	}
	return value, nil
}

// Add a definition here when a consumer introduces another runtime knob.
// Security boundaries (read-only and chat allow-list) remain deploy-time only.
func settingDefinitions() []settingDefinition {
	capValue := func(raw json.RawMessage) (any, error) {
		var value float64
		if len(raw) == 0 || raw[0] == '"' || string(raw) == "null" || json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 525600 {
			return nil, errors.New("expected minutes in 0..525600")
		}
		return value, nil
	}
	scope := func(value string) (any, error) {
		if value != "ingest" && value != "all" {
			return nil, errors.New("expected ingest or all")
		}
		return value, nil
	}
	tools := func(raw json.RawMessage) (any, error) {
		var names []string
		if json.Unmarshal(raw, &names) != nil || names == nil {
			return nil, errors.New("expected an array of tool names")
		}
		for _, name := range names {
			if name == "" || strings.Contains(name, ",") || name != strings.TrimSpace(name) {
				return nil, errors.New("expected tool names")
			}
		}
		policy, err := newToolPolicy(strings.Join(names, ","), "")
		if err != nil {
			return nil, errors.New("unknown tool name")
		}
		return sortedNames(policy.allow), nil
	}
	toolEnv := func(raw string) (any, error) {
		return sortedNames(parseToolList(raw)), nil // Config validates names once, last.
	}
	return []settingDefinition{
		sendSetting("send.rate_per_minute", sendRateMinuteEnv),
		sendSetting("send.rate_per_day", sendRateDayEnv),
		sendSetting("send.new_chats_per_day", sendNewChatsEnv),
		sendSetting("send.min_interval_ms", sendIntervalEnv),

		{"transcription.monthly_max_minutes", transcriptionCapEnv, nil, capValue, func(raw string) (any, error) {
			value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				return nil, errors.New("expected non-negative minutes")
			}
			return capValue(json.RawMessage(strconv.FormatFloat(value, 'f', -1, 64)))
		}},
		{"transcription.cap_scope", transcriptionScopeEnv, "ingest", func(raw json.RawMessage) (any, error) {
			var value string
			if json.Unmarshal(raw, &value) != nil {
				return nil, errors.New("expected ingest or all")
			}
			return scope(value)
		}, func(raw string) (any, error) { return scope(strings.TrimSpace(raw)) }},
		{"tools.allow", allowToolsEnv, []string{}, tools, toolEnv},
		{"tools.deny", denyToolsEnv, []string{}, tools, toolEnv},
		{"transcription.ingest_chats", ingestChatsEnv, "all", func(raw json.RawMessage) (any, error) {
			var value string
			if json.Unmarshal(raw, &value) != nil || value == "" {
				return nil, errors.New("expected all or direct")
			}
			return parseIngestChats(value)
		}, func(raw string) (any, error) { return parseIngestChats(raw) }},
	}
}

func sendSetting(key, env string) settingDefinition {
	parse := func(raw json.RawMessage) (any, error) {
		var value int64
		if json.Unmarshal(raw, &value) != nil || value < 0 || value > 2147483647 {
			return nil, errors.New("expected an integer from 0 to 2147483647; 0 disables")
		}
		return value, nil
	}
	return settingDefinition{key, env, int64(0), parse, func(raw string) (any, error) {
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
		if err != nil || value < 0 {
			return nil, errors.New("expected a nonnegative 32-bit integer")
		}
		return value, nil
	}}
}

func runtimeDefaults(getenv func(string) string) (map[string]runtimeSetting, error) {
	out := map[string]runtimeSetting{}
	for _, def := range settingDefinitions() {
		setting := runtimeSetting{Value: def.defaultValue, Source: "default"}
		if raw := getenv(def.env); strings.TrimSpace(raw) != "" {
			var err error
			setting.Value, err = def.parseEnv(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", def.env, err)
			}
			setting.Source = "env"
		}
		out[def.key] = setting
	}
	return out, nil
}

type settingsReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRuntimeSettings(ctx context.Context, db settingsReader, defaults map[string]runtimeSetting, warn func(string, int64)) (runtimeSettingsSnapshot, error) {
	out := runtimeSettingsSnapshot{Settings: map[string]runtimeSetting{}}
	closedAllow := false
	for key, value := range defaults {
		out.Settings[key] = value
	}
	rows, err := db.QueryContext(ctx, "SELECT key, value, version FROM runtime_settings")
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	definitions := map[string]settingDefinition{}
	for _, def := range settingDefinitions() {
		definitions[def.key] = def
	}
	for rows.Next() {
		var key, raw string
		var version int64
		if err := rows.Scan(&key, &raw, &version); err != nil {
			return out, err
		}
		out.Version = max(out.Version, version)
		def, known := definitions[key]
		if !known || raw == "null" {
			continue
		}
		var value any
		var err error
		if key == "tools.allow" || key == "tools.deny" {
			var names []string
			err = json.Unmarshal([]byte(raw), &names)
			if err == nil && names == nil {
				err = errors.New("expected tool array")
			}
			valid := map[string]bool{}
			known := knownTools()
			for _, name := range names {
				if name == "" || name != strings.TrimSpace(name) || strings.Contains(name, ",") {
					err = errors.New("invalid tool array")
				} else if !known[name] {
					warn(key, version)
				} else {
					valid[name] = true
				}
			}
			value = sortedNames(valid)
			if key == "tools.allow" && len(names) > 0 && len(valid) == 0 {
				closedAllow = true
			}
		} else {
			value, err = def.parse(json.RawMessage(raw))
		}
		if err != nil {
			warn(key, version)
			if key == "tools.allow" {
				closedAllow = len(defaults[key].Value.([]string)) == 0
			}
			continue // Recover at env/default; PATCH null can always clear the row.
		}
		out.Settings[key] = runtimeSetting{Value: value, Source: "runtime"}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	applyRuntimeToolFloor(&out, defaults, closedAllow)
	applyRuntimeSendCeiling(&out, defaults)

	capKey, scopeKey := "transcription.monthly_max_minutes", "transcription.cap_scope"
	if ceiling, ok := defaults[capKey].Value.(float64); ok {
		value, valid := out.Settings[capKey].Value.(float64)
		if !valid || value > ceiling {
			out.Settings[capKey] = defaults[capKey]
		}
	}
	if defaults[scopeKey].Value == "all" {
		out.Settings[scopeKey] = defaults[scopeKey]
	}
	return out, nil
}

// Deployment send budgets can only be tightened by runtime settings.
func applyRuntimeSendCeiling(out *runtimeSettingsSnapshot, defaults map[string]runtimeSetting) {
	for _, key := range []string{"send.rate_per_minute", "send.rate_per_day", "send.new_chats_per_day", "send.min_interval_ms"} {
		deploy := defaults[key]
		setting := out.Settings[key]
		env, runtime := deploy.Value.(int64), setting.Value.(int64)
		if env > 0 && (runtime == 0 || (key == "send.min_interval_ms" && runtime < env) || (key != "send.min_interval_ms" && runtime > env)) {
			out.Settings[key] = deploy
		}
	}
}

// Deployment policy is the permanent ceiling of runtime capability. Empty
// runtime lists can remove their own restriction, never the deployment floor.
func applyRuntimeToolFloor(out *runtimeSettingsSnapshot, defaults map[string]runtimeSetting, closedAllow bool) {
	allowSetting, denySetting := out.Settings["tools.allow"], out.Settings["tools.deny"]
	allow := parseToolList(strings.Join(allowSetting.Value.([]string), ","))
	envAllow := parseToolList(strings.Join(defaults["tools.allow"].Value.([]string), ","))
	if len(envAllow) > 0 {
		if len(allow) == 0 && !closedAllow {
			allow = envAllow
		} else {
			for name := range allow {
				if !envAllow[name] {
					delete(allow, name)
				}
			}
			closedAllow = len(allow) == 0
		}
	}
	deny := parseToolList(strings.Join(denySetting.Value.([]string), ","))
	labels := map[string]string{}
	for name := range deny {
		labels[name] = denyToolsEnv + " (deploy) lists it"
		if denySetting.Source == "runtime" {
			labels[name] = "runtime tools.deny lists it"
		}
	}
	for _, name := range defaults["tools.deny"].Value.([]string) {
		deny[name] = true
		labels[name] = denyToolsEnv + " (deploy) lists it"
	}
	if closedAllow && len(allow) == 0 {
		for name := range knownTools() {
			deny[name] = true
			if labels[name] == "" {
				labels[name] = "runtime tools.allow has no readable tools inside the deploy floor"
			}
		}
		denySetting.Source = "runtime"
	}
	allowSetting.Value, denySetting.Value = sortedNames(allow), sortedNames(deny)
	out.Settings["tools.allow"], out.Settings["tools.deny"] = allowSetting, denySetting
	label := allowToolsEnv + " (deploy)"
	if allowSetting.Source == "runtime" {
		label = "runtime tools.allow within " + allowToolsEnv + " (deploy)"
	}
	out.policy = toolPolicy{allow: allow, deny: deny, allowLabel: label, denyLabels: labels}
}

func (b *Bridge) warnSavedSetting(key string, version int64) {
	b.settingsWarnMu.Lock()
	defer b.settingsWarnMu.Unlock()
	if b.settingsWarned == nil {
		b.settingsWarned = map[string]int64{}
	}
	if b.settingsWarned[key] == version {
		return
	}
	b.settingsWarned[key] = version
	b.Log.Warnf("Saved runtime setting key=%s version=%d is incompatible; applying safe deploy fallback", key, version)
}

func (b *Bridge) settingsSnapshot(ctx context.Context) (runtimeSettingsSnapshot, error) {
	return readRuntimeSettings(ctx, b.Store.db, b.RuntimeDefaults, b.warnSavedSetting)
}

func (b *Bridge) handleRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPatch {
		w.Header().Set("Allow", "GET, PATCH")
		writeError(w, 405, "Use GET or PATCH")
		return
	}
	if r.Method == http.MethodPatch {
		var body map[string]json.RawMessage
		if !operatorDecode(w, r, &body) {
			return
		}
		if len(body) == 0 {
			writeError(w, 400, "Expected settings object with at least one key")
			return
		}
		definitions := map[string]settingDefinition{}
		for _, def := range settingDefinitions() {
			definitions[def.key] = def
		}
		keys := make([]string, 0, len(body))
		for key, raw := range body {
			def, known := definitions[key]
			if !known {
				writeError(w, 400, "Unknown runtime setting key: "+configValue(key))
				return
			}
			if string(raw) != "null" {
				value, err := def.parse(raw)
				if err != nil {
					writeError(w, 400, key+": "+err.Error())
					return
				}
				body[key], _ = json.Marshal(value)
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if err := b.patchRuntimeSettings(r.Context(), body, keys); err != nil {
			writeErrorCode(w, 503, "settings_unavailable", "Runtime settings could not be saved")
			return
		}
	}
	snapshot, err := b.settingsSnapshot(r.Context())
	if err != nil {
		writeErrorCode(w, 503, "settings_unavailable", "Runtime settings could not be read")
		return
	}
	writeJSON(w, 200, snapshot)
}

func (b *Bridge) patchRuntimeSettings(ctx context.Context, values map[string]json.RawMessage, keys []string) error {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	before, err := readRuntimeSettings(ctx, tx, b.RuntimeDefaults, b.warnSavedSetting)
	if err != nil {
		return err
	}
	version := before.Version + 1
	for _, key := range keys {
		// A null tombstone preserves the clock even when the last override clears.
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at,version) VALUES (?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at,version=excluded.version`,
			key, string(values[key]), dbTime(time.Now()), version); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, key := range keys {
		source := "runtime"
		if string(values[key]) == "null" {
			source = b.RuntimeDefaults[key].Source
		}
		b.Log.Infof("Runtime setting key=%s source=%s->%s version=%d", key, before.Settings[key].Source, source, version)
	}
	return nil
}

func (b *Bridge) runtimeToolGuard(h http.HandlerFunc) http.HandlerFunc {
	if b.RuntimeDefaults == nil {
		return b.Tools.guard(h)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if b.operatorLogout.Load() {
			writeErrorCode(w, 503, "operator_idle", "Device is parked by operator logout")
			return
		}
		snapshot, err := b.settingsSnapshot(r.Context())
		if err != nil {
			writeErrorCode(w, 503, "settings_unavailable", "Runtime policy unavailable")
			return
		}
		snapshot.policy.guard(h)(w, r)
	}
}
