package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const operatorLogoutSchema = `CREATE TABLE IF NOT EXISTS operator_state (
	id INTEGER PRIMARY KEY CHECK(id=1), logged_out INTEGER NOT NULL
)`

func (b *Bridge) restoreOperatorIdle() error {
	var idle bool
	if err := b.Store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM operator_state WHERE id=1 AND logged_out=1)").Scan(&idle); err != nil {
		return err
	}
	if idle {
		b.operatorLogout.Store(true)
		b.operatorSessionWiped.Store(!b.isPaired())
		b.pairingState = "logged_out_by_operator" // Before any consumers start.
		if b.operatorPairing != nil {
			b.operatorPairing.state.State = "logged_out_by_operator"
		}
	}
	return nil
}

func (p *operatorPairing) logout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		After string `json:"after"`
	}
	if !operatorDecode(w, r, &body) {
		return
	}
	if body.After == "" {
		body.After = "exit"
	}
	if body.After != "exit" && body.After != "idle" {
		writeError(w, 400, "after must be exit or idle")
		return
	}
	if !p.action.TryLock() {
		writeErrorCode(w, 409, "pairing_busy", "Another operator action is running")
		return
	}
	defer p.action.Unlock()
	if (p.b.operatorLogout.Load() && p.b.operatorSessionWiped.Load()) || (!p.b.operatorLogout.Load() && !p.paired()) {
		writeErrorCode(w, 409, "not_paired", "No linked device to unlink")
		return
	}
	// Park dial paths before the SDK disconnects. Persist before wiping:
	// a crash cannot accidentally start a new QR flow.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	_, err := p.b.Store.db.ExecContext(ctx, "INSERT INTO operator_state(id,logged_out) VALUES (1,1) ON CONFLICT(id) DO UPDATE SET logged_out=1")
	cancel()
	if err != nil {
		writeErrorCode(w, 503, "state_persistence_failed", "Operator idle state could not be saved")
		return
	}
	if !p.b.operatorLogout.Load() {
		p.b.operatorSessionWiped.Store(false)
	}
	p.b.operatorLogout.Store(true)
	p.b.operatorRetiredClient.Store(p.b.currentClient())
	p.mu.Lock()
	p.state.Generation++
	p.invalidateLocked("logged_out_by_operator")
	if p.cancelAttempt != nil {
		p.cancelAttempt()
	}
	p.mu.Unlock()
	p.b.connectionMu.Lock()
	p.b.pairingState = "logged_out_by_operator"
	p.b.connectionChangedLocked()
	p.b.connectionMu.Unlock()
	// Drain accepted REST/event consumers before the SDK mutates Device.ID and
	// its stores. Retired events return before taking this gate.
	p.b.clientGate.Lock()
	// A callback admitted before retirement may have finished by clearing the
	// connection state. Reassert idle after draining it, before deleting keys.
	p.mu.Lock()
	p.invalidateLocked("logged_out_by_operator")
	p.mu.Unlock()
	p.b.connectionMu.Lock()
	p.b.pairingState = "logged_out_by_operator"
	p.b.connectionChangedLocked()
	p.b.connectionMu.Unlock()
	ctx, cancel = context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	logoutErr := p.b.performLogout(ctx)
	// The pinned SDK unlinks remotely before Delete; this error identifies
	// successful unlink followed by a local failure that our wipe can retry.
	serverUnlinked := logoutErr == nil || strings.HasPrefix(logoutErr.Error(), "error deleting data from store:")
	cancel()
	p.client.Disconnect()
	// A server refusal or caller cancellation must never skip local destruction.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = p.b.wipeOperatorSession(ctx)
	cancel()
	if err == nil {
		p.b.operatorSessionWiped.Store(true)
		p.b.runtimePaired.Store(false)
	}
	p.b.clientGate.Unlock()
	if err != nil {
		writeJSON(w, 500, map[string]any{"server_unlinked": serverUnlinked, "local_session_wiped": false})
		p.b.Log.Errorf("Operator logout local session wipe failed; instance remains parked")
		return
	}
	if body.After == "exit" {
		if _, err := p.b.Store.db.Exec("DELETE FROM operator_state WHERE id=1"); err != nil {
			p.b.Log.Warnf("Operator logout exit retains durable idle state")
		}
	}
	p.b.recipientNumbers.clear()
	p.b.notifyConnection("logged_out", "operator", true, true)
	response := fmt.Sprintf("{\"server_unlinked\":%t,\"local_session_wiped\":true}\n", serverUnlinked)
	// Exit immediately after flushing: a fixed length avoids a truncated chunked body.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(response)))
	w.WriteHeader(200)
	_, _ = io.WriteString(w, response)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if body.After == "exit" {
		p.b.Exit("device logged out by operator", exitCodeLoggedOut)
	}
}

func (b *Bridge) performLogout(ctx context.Context) error {
	if b.logoutClient != nil {
		return b.logoutClient(ctx)
	}
	return b.currentClient().Logout(ctx)
}

func (b *Bridge) wipeOperatorSession(ctx context.Context) error {
	if b.wipeSession != nil {
		return b.wipeSession(ctx)
	}
	client := b.currentClient()
	if client == nil || client.Store == nil {
		return errors.New("session unavailable")
	}
	if client.Store.ID == nil {
		return nil
	} // Logout already deleted this device.
	return client.Store.Delete(ctx)
}
