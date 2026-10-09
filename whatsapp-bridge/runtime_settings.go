package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const ingestChatsEnv = "TRANSCRIBE_ON_INGEST_CHATS"
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

func readRuntimeSettings(ctx context.Context, db settingsReader, defaults map[string]runtimeSetting) (runtimeSettingsSnapshot, error) {
	out := runtimeSettingsSnapshot{Settings: map[string]runtimeSetting{}}
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
		value, err := def.parse(json.RawMessage(raw))
		if err != nil {
			return out, fmt.Errorf("invalid saved setting %s", key)
		}
		out.Settings[key] = runtimeSetting{Value: value, Source: "runtime"}
	}
	return out, rows.Err()
}

func (b *Bridge) settingsSnapshot(ctx context.Context) (runtimeSettingsSnapshot, error) {
	return readRuntimeSettings(ctx, b.Store.db, b.RuntimeDefaults)
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
	before, err := readRuntimeSettings(ctx, tx, b.RuntimeDefaults)
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
		snapshot, err := b.settingsSnapshot(r.Context())
		if err != nil {
			writeErrorCode(w, 503, "settings_unavailable", "Runtime policy unavailable")
			return
		}
		allow := snapshot.Settings["tools.allow"].Value.([]string)
		deny := snapshot.Settings["tools.deny"].Value.([]string)
		policy, err := newToolPolicy(strings.Join(allow, ","), strings.Join(deny, ","))
		if err != nil {
			writeError(w, 503, "Runtime policy unavailable")
			return
		}
		policy.guard(h)(w, r)
	}
}
