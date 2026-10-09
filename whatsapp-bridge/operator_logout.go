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
	var idle, wiped bool
	if err := b.Store.db.QueryRow("SELECT COALESCE((SELECT logged_out FROM operator_state WHERE id=1),0), COALESCE((SELECT local_session_wiped FROM operator_state WHERE id=1),0)").Scan(&idle, &wiped); err != nil {
		return err
	}
	if idle {
		b.operatorLogout.Store(true)
		b.operatorSessionWiped.Store(wiped && !b.isPaired())
		b.pairingState = "logged_out_by_operator" // Before any consumers start.
		if b.operatorPairing != nil {
			b.operatorPairing.state.State = "logged_out_by_operator"
		} else {
			b.Log.Warnf("State logged_out_by_operator: re-enable WHATSAPP_OPERATOR_BIND and POST /operator/v1/pairing/restart to leave idle")
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
	// Leave time for the terminal webhook and response within the listener's
	// 15-second write timeout, while ignoring cancellation by the caller.
	actionCtx, finish := context.WithTimeout(context.WithoutCancel(r.Context()), 12*time.Second)
	defer finish()
	if (p.b.operatorLogout.Load() && p.b.operatorSessionWiped.Load()) || (!p.b.operatorLogout.Load() && !p.paired()) {
		writeErrorCode(w, 409, "not_paired", "No linked device to unlink")
		return
	}
	// Park dial paths before the SDK disconnects. Persist before wiping:
	// a crash cannot accidentally start a new QR flow.
	ctx, cancel := context.WithTimeout(actionCtx, time.Second)
	_, err := p.b.Store.db.ExecContext(ctx, "INSERT INTO operator_state(id,logged_out,local_session_wiped) VALUES (1,1,0) ON CONFLICT(id) DO UPDATE SET logged_out=1,local_session_wiped=0")
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
	ctx, cancel = context.WithTimeout(actionCtx, time.Second)
	archivesDrained := p.b.cancelArchiveSessionReads(ctx)
	cancel()
	if !archivesDrained {
		writeErrorCode(w, 503, "archive_busy", "Session archive readers are still closing; retry operator logout")
		return
	}
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
	drainTimeout := p.b.logoutDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = time.Second
	}
	ctx, cancel = context.WithTimeout(actionCtx, drainTimeout)
	locked := p.b.drainOperatorClients(ctx)
	cancel()
	if !locked {
		writeErrorCode(w, 503, "client_busy", "Client operations are still running; retry operator logout")
		return
	}
	// A callback admitted before retirement may have finished by clearing the
	// connection state. Reassert idle after draining it, before deleting keys.
	p.mu.Lock()
	p.invalidateLocked("logged_out_by_operator")
	p.mu.Unlock()
	p.b.connectionMu.Lock()
	p.b.pairingState = "logged_out_by_operator"
	p.b.connectionChangedLocked()
	p.b.connectionMu.Unlock()
	ctx, cancel = context.WithTimeout(actionCtx, 5*time.Second)
	logoutErr := p.b.performLogout(ctx)
	// The pinned SDK unlinks remotely before Delete; this error identifies
	// successful unlink followed by a local failure that our wipe can retry.
	serverUnlinked := logoutErr == nil || strings.HasPrefix(logoutErr.Error(), "error deleting data from store:")
	cancel()
	p.client.Disconnect()
	// A server refusal or caller cancellation must never skip local destruction.
	ctx, cancel = context.WithTimeout(actionCtx, 5*time.Second)
	err = p.b.wipeOperatorSession(ctx)
	snapshotsRemoved, snapshotErr := removeSessionSnapshots(p.b.SnapshotDir)
	if err == nil {
		err = snapshotErr
	}
	if err == nil {
		_, err = p.b.Store.db.ExecContext(ctx, "UPDATE operator_state SET local_session_wiped=1 WHERE id=1")
	}
	cancel()
	if err == nil {
		p.b.operatorSessionWiped.Store(true)
		p.b.runtimePaired.Store(false)
	}
	p.b.clientGate.Unlock()
	p.b.historyVoteSessionGate.Unlock()
	if err != nil {
		writeJSON(w, 500, map[string]any{"server_unlinked": serverUnlinked, "local_session_wiped": false, "session_snapshots_removed": snapshotsRemoved})
		p.b.Log.Errorf("Operator logout local session wipe failed; instance remains parked")
		return
	}
	if body.After == "exit" {
		ctx, cancel = context.WithTimeout(actionCtx, time.Second)
		_, err := p.b.Store.db.ExecContext(ctx, "DELETE FROM operator_state WHERE id=1")
		cancel()
		if err != nil {
			p.b.Log.Warnf("Operator logout exit retains durable idle state")
		}
	}
	p.b.recipientNumbers.clear()
	p.b.notifyConnection("logged_out", "operator", true, true)
	p.b.Log.Infof("Operator logout removed session snapshots: count=%d", snapshotsRemoved)
	response := fmt.Sprintf("{\"server_unlinked\":%t,\"local_session_wiped\":true,\"session_snapshots_removed\":%d}\n", serverUnlinked, snapshotsRemoved)
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

// A queued RWMutex writer would block new readers indefinitely. TryLock keeps
// the operator request bounded while existing admitted requests drain.
func (b *Bridge) drainOperatorClients(ctx context.Context) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if b.clientGate.TryLock() {
			if b.historyVoteSessionGate.TryLock() {
				return true
			}
			b.clientGate.Unlock()
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (b *Bridge) wipeOperatorSession(ctx context.Context) error {
	if b.wipeSession != nil {
		return b.wipeSession(ctx)
	}
	client := b.currentClient()
	if client == nil || client.Store == nil {
		return errors.New("session unavailable")
	}
	if b.sessionDB == nil {
		return errors.New("session database unavailable")
	}
	// Every session connection has secure_delete=ON from its DSN, including
	// Logout's own Delete. Remove historical free pages and truncate the WAL
	// after VACUUM, which itself writes a new WAL in WAL mode.
	if client.Store.ID != nil {
		if err := client.Store.Delete(ctx); err != nil {
			return err
		}
	}
	if _, err := b.sessionDB.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	var busy, pages, checkpointed int
	if err := b.sessionDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("session checkpoint busy")
	}
	return nil
}
