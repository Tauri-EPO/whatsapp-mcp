package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const mcpTokenKey = "auth.mcp_token" //nolint:gosec // Database key name, not a credential.

type mcpTokenState struct {
	Current            string    `json:"current"`
	Previous           string    `json:"previous"`
	PreviousValidUntil time.Time `json:"previous_valid_until"`
}

func tokenHash(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validTokenHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == sha256.Size
}

func readMCPToken(ctx context.Context, tx *sql.Tx) (mcpTokenState, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM runtime_settings WHERE key=?", mcpTokenKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || raw == "null" {
		return mcpTokenState{}, nil
	}
	if err != nil {
		return mcpTokenState{}, err
	}
	var state mcpTokenState
	if json.Unmarshal([]byte(raw), &state) != nil || !validTokenHash(state.Current) || (state.Previous != "" && !validTokenHash(state.Previous)) {
		return state, errors.New("invalid saved MCP authentication state")
	}
	return state, nil
}

func (b *Bridge) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "DELETE" {
		w.Header().Set("Allow", "POST, DELETE")
		writeError(w, 405, "Use POST or DELETE")
		return
	}
	var body struct {
		SHA256             string    `json:"sha256"`
		Token              *string   `json:"token,omitempty"`
		PreviousValidUntil time.Time `json:"previous_valid_until"`
	}
	now := b.sendClock()
	if r.Method == "POST" {
		if !operatorDecode(w, r, &body) {
			return
		}
		body.SHA256 = strings.ToLower(body.SHA256)
		if !validTokenHash(body.SHA256) || (body.Token != nil && (len(*body.Token) < 16 || subtle.ConstantTimeCompare([]byte(tokenHash(*body.Token)), []byte(body.SHA256)) != 1)) {
			writeErrorCode(w, 400, "invalid_request", "Expected a SHA256 hash matching the optional token")
			return
		}
		body.Token = nil
		if body.PreviousValidUntil.After(now.Add(24 * time.Hour)) {
			body.PreviousValidUntil = now.Add(24 * time.Hour)
		}
		if body.PreviousValidUntil.Before(now) {
			body.PreviousValidUntil = now
		}
	}
	state, version, err := b.saveMCPToken(r.Context(), body.SHA256, body.PreviousValidUntil, r.Method == "DELETE")
	if err != nil {
		writeErrorCode(w, 503, "settings_unavailable", "MCP authentication state could not be saved")
		return
	}
	prefix := func(hash string) string {
		if len(hash) > 8 {
			return hash[:8]
		}
		return hash
	}
	b.Log.Infof("MCP token rotation current=%s previous=%s previous_valid_until=%s version=%d", prefix(state.Current), prefix(state.Previous), state.PreviousValidUntil.Format(time.RFC3339), version)
	writeJSON(w, 200, map[string]any{"success": true, "version": version, "previous_valid_until": state.PreviousValidUntil})
}

func (b *Bridge) saveMCPToken(ctx context.Context, hash string, until time.Time, clear bool) (mcpTokenState, int64, error) {
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return mcpTokenState{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readMCPToken(ctx, tx)
	// DELETE remains a recovery operation even for incompatible persisted state.
	if err != nil && !clear {
		return state, 0, err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0)+1 FROM runtime_settings").Scan(&version); err != nil {
		return state, 0, err
	}
	raw := "null"
	if !clear {
		previous := state.Current
		if previous == "" {
			previous = b.MCPEnvHash
		}
		state = mcpTokenState{hash, previous, until}
		encoded, _ := json.Marshal(state)
		raw = string(encoded)
	} else {
		state = mcpTokenState{}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value,updated_at,version) VALUES (?,?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at,version=excluded.version`, mcpTokenKey, raw, dbTime(b.sendClock()), version)
	if err != nil {
		return state, 0, err
	}
	return state, version, tx.Commit()
}
