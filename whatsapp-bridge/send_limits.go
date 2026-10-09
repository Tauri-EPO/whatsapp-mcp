package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	sendRateMinuteEnv = "WHATSAPP_SEND_RATE_PER_MINUTE"
	sendRateDayEnv    = "WHATSAPP_SEND_RATE_PER_DAY"
	sendNewChatsEnv   = "WHATSAPP_SEND_NEW_CHATS_PER_DAY"
	sendIntervalEnv   = "WHATSAPP_SEND_MIN_INTERVAL_MS"
)

const sendLimitsSchema = `
CREATE TABLE IF NOT EXISTS send_usage (
 id INTEGER PRIMARY KEY CHECK(id=1), day TEXT NOT NULL, today INTEGER NOT NULL,
 new_chats INTEGER NOT NULL, refusals INTEGER NOT NULL, tokens REAL NOT NULL,
 rate INTEGER NOT NULL, refill_ns INTEGER NOT NULL, last_send_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS send_contacts (chat_jid TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS send_refusals (reason TEXT PRIMARY KEY, total INTEGER NOT NULL);
`

type sendUsage struct {
	Day           string    `json:"day"`
	Today         int64     `json:"today"`
	DayLimit      int64     `json:"day_limit"`
	MinuteLimit   int64     `json:"minute_limit"`
	NewChatsToday int64     `json:"new_chats_today"`
	NewChatsLimit int64     `json:"new_chats_limit"`
	RefusalsToday int64     `json:"refusals_today"`
	MinIntervalMS int64     `json:"min_interval_ms"`
	ResetsAt      time.Time `json:"resets_at"`
}

type sendState struct {
	sendUsage
	tokens             float64
	rate, refill, last int64
}

type sendReservationFailure struct {
	cause         error
	limitsEnabled bool
}

func (e *sendReservationFailure) Error() string { return e.cause.Error() }
func (e *sendReservationFailure) Unwrap() error { return e.cause }

func sendLimitsEnabled(snapshot runtimeSettingsSnapshot) bool {
	for _, key := range []string{"send.rate_per_minute", "send.rate_per_day", "send.new_chats_per_day", "send.min_interval_ms"} {
		if setting, ok := snapshot.Settings[key]; ok && setting.Value.(int64) > 0 {
			return true
		}
	}
	return false
}

func (b *Bridge) sendClock() time.Time {
	if b.sendNow != nil {
		return b.sendNow().UTC()
	}
	return time.Now().UTC()
}

func (b *Bridge) readSendState(ctx context.Context, tx *sql.Tx, now time.Time) (sendState, error) {
	var s sendState
	err := tx.QueryRowContext(ctx, "SELECT day,today,new_chats,refusals,tokens,rate,refill_ns,last_send_ns FROM send_usage WHERE id=1").Scan(&s.Day, &s.Today, &s.NewChatsToday, &s.RefusalsToday, &s.tokens, &s.rate, &s.refill, &s.last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	if s.Day != now.Format("2006-01-02") {
		s.Day, s.Today, s.NewChatsToday, s.RefusalsToday = now.Format("2006-01-02"), 0, 0, 0
	}
	s.ResetsAt = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	if b.RuntimeDefaults != nil {
		snapshot, err := readRuntimeSettings(ctx, tx, b.RuntimeDefaults, b.warnSavedSetting)
		if err != nil {
			return s, err
		}
		s.MinuteLimit = snapshot.Settings["send.rate_per_minute"].Value.(int64)
		s.DayLimit = snapshot.Settings["send.rate_per_day"].Value.(int64)
		s.NewChatsLimit = snapshot.Settings["send.new_chats_per_day"].Value.(int64)
		s.MinIntervalMS = snapshot.Settings["send.min_interval_ms"].Value.(int64)
	}
	if s.refill == 0 || s.rate == 0 {
		s.tokens = float64(s.MinuteLimit)
	} else {
		s.tokens += math.Max(0, float64(now.UnixNano()-s.refill)/float64(time.Minute)) * float64(s.rate)
	}
	s.tokens = math.Min(s.tokens, float64(s.MinuteLimit))
	s.rate, s.refill = s.MinuteLimit, now.UnixNano()
	return s, nil
}

// Reserve before the network call. Uncertain delivery and archive failure must
// never refund a send: restarting cannot turn an ambiguous send into free budget.
func (b *Bridge) reserveSend(ctx context.Context, targets []string) (reason string, delay int, err error) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	known, enabled := false, false
	defer func() {
		if err != nil && known {
			err = &sendReservationFailure{cause: err, limitsEnabled: enabled}
		}
	}()
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// messagesWriterOptions uses BEGIN IMMEDIATE, so inbound/settings writes
	// cannot invalidate the reservation between reading and writing its state.
	now := b.sendClock()
	s, err := b.readSendState(ctx, tx, now)
	if err != nil {
		return "", 0, err
	}
	known, enabled = true, s.MinuteLimit > 0 || s.DayLimit > 0 || s.NewChatsLimit > 0 || s.MinIntervalMS > 0
	unique := map[string]bool{}
	identities := map[string]bool{}
	var fresh int64
	for _, target := range targets {
		if unique[target] {
			continue
		}
		unique[target] = true
		twin := ""
		if jid, e := types.ParseJID(target); e == nil && (jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer) {
			alt, e := lookupAltJID(ctx, b.currentClient(), jid)
			if e != nil && !errors.Is(e, errLIDStoreUnavailable) {
				return "", 0, e
			}
			twin = alt.String()
		}
		if identities[target] || (twin != "" && identities[twin]) {
			continue
		}
		identities[target] = true
		if twin != "" {
			identities[twin] = true
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE chat_jid IN (?,?) UNION ALL SELECT 1 FROM send_contacts WHERE chat_jid IN (?,?))`, target, twin, target, twin).Scan(&exists); err != nil {
			return "", 0, err
		}
		if !exists {
			fresh++
		}
	}
	count := int64(len(targets))
	refuse := func(name string, seconds float64) {
		if reason == "" {
			reason, delay = name, max(1, int(math.Ceil(seconds)))
		}
	}
	if (s.DayLimit > 0 && count > s.DayLimit) || (s.MinuteLimit > 0 && count > s.MinuteLimit) || (s.NewChatsLimit > 0 && fresh > s.NewChatsLimit) {
		reason = "limit_exceeds_batch"
	}
	if s.DayLimit > 0 && s.Today+count > s.DayLimit {
		refuse("per_day", s.ResetsAt.Sub(now).Seconds())
	}
	if s.NewChatsLimit > 0 && s.NewChatsToday+fresh > s.NewChatsLimit {
		refuse("new_chats_per_day", s.ResetsAt.Sub(now).Seconds())
	}
	if s.MinuteLimit > 0 && s.tokens < float64(count) {
		refuse("per_minute", (float64(count)-s.tokens)*60/float64(s.MinuteLimit))
	}
	if s.MinIntervalMS > 0 && s.last > 0 {
		remaining := time.Duration(s.MinIntervalMS)*time.Millisecond - now.Sub(time.Unix(0, s.last))
		if remaining > 0 {
			refuse("min_interval", remaining.Seconds())
		}
	}
	if reason != "" {
		s.RefusalsToday++
		if _, err := tx.ExecContext(ctx, `INSERT INTO send_refusals(reason,total) VALUES (?,1) ON CONFLICT(reason) DO UPDATE SET total=total+1`, reason); err != nil {
			return "", 0, err
		}
	} else {
		s.Today += count
		s.NewChatsToday += fresh
		s.tokens -= float64(count)
		s.last = now.UnixNano()
		for target := range unique {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO send_contacts(chat_jid) VALUES (?)", target); err != nil {
				return "", 0, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO send_usage VALUES (1,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET day=excluded.day,today=excluded.today,new_chats=excluded.new_chats,refusals=excluded.refusals,tokens=excluded.tokens,rate=excluded.rate,refill_ns=excluded.refill_ns,last_send_ns=excluded.last_send_ns`, s.Day, s.Today, s.NewChatsToday, s.RefusalsToday, s.tokens, s.rate, s.refill, s.last)
	if err != nil {
		return "", 0, err
	}
	return reason, delay, tx.Commit()
}

func (b *Bridge) allowSend(w http.ResponseWriter, r *http.Request, targets ...string) bool {
	if b.Store == nil && b.RuntimeDefaults == nil {
		return true
	}
	ctx, cancel := requestContext(r, sendDeadline)
	defer cancel()
	settingsCtx := ctx
	// Read-only settings checks do not acquire the WAL writer. With budgets off,
	// accounting must not change whether an otherwise valid send can proceed.
	var snapshot runtimeSettingsSnapshot
	var err error
	if b.RuntimeDefaults != nil {
		snapshot, err = b.settingsSnapshot(ctx)
	}
	if err != nil {
		writeErrorCode(w, 503, "send_usage_unavailable", "Send limits unavailable; do not send")
		return false
	}
	enabled := sendLimitsEnabled(snapshot)
	if !enabled {
		var countCancel context.CancelFunc
		ctx, countCancel = context.WithTimeout(ctx, 100*time.Millisecond)
		defer countCancel()
	}
	reason, delay, err := b.reserveSend(ctx, targets)
	if err != nil {
		var failure *sendReservationFailure
		if errors.As(err, &failure) {
			enabled = failure.limitsEnabled
		} else if b.RuntimeDefaults != nil {
			// No reservation snapshot was obtained. Recheck the live policy
			// outside the shorter best-effort counting deadline before allowing.
			current, checkErr := b.settingsSnapshot(settingsCtx)
			enabled = checkErr != nil || sendLimitsEnabled(current)
		}
		if !enabled {
			b.sendCountSkipped.Add(1)
			b.Log.Warnf("Send usage counting skipped: store or identity lookup unavailable; limits are off")
			return true
		}
		writeErrorCode(w, 503, "send_usage_unavailable", "Send budget unavailable; do not send")
		return false
	}
	if reason == "limit_exceeds_batch" {
		writeJSON(w, 429, map[string]any{"success": false, "error": "send_rate_limited", "limit": reason, "retry_after_s": nil, "message": "Batch exceeds a send limit; split the batch before sending"})
		return false
	}
	if reason != "" {
		w.Header().Set("Retry-After", strconv.Itoa(delay))
		writeJSON(w, 429, map[string]any{"success": false, "error": "send_rate_limited", "limit": reason, "retry_after_s": delay, "message": "Send limit reached; stop and report to the operator instead of retrying"})
		return false
	}
	return true
}

func (b *Bridge) allowSendAction(w http.ResponseWriter, r *http.Request, target string) bool {
	return !b.SendIncludeActions || b.allowSend(w, r, target)
}

func (b *Bridge) sendUsageSnapshot(ctx context.Context) (sendUsage, error) {
	// Pure reads stay concurrent with the WAL writer used by send reservations.
	tx, err := b.Store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return sendUsage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	s, err := b.readSendState(ctx, tx, b.sendClock())
	return s.sendUsage, err
}

func (b *Bridge) handleSendUsage(w http.ResponseWriter, r *http.Request) {
	s, err := b.sendUsageSnapshot(r.Context())
	if err != nil {
		writeErrorCode(w, 503, "send_usage_unavailable", "Send usage unavailable")
		return
	}
	writeJSON(w, 200, s)
}

func (b *Bridge) sendMetrics() string {
	skipped := fmt.Sprintf("# TYPE whatsapp_bridge_send_count_skipped_total counter\nwhatsapp_bridge_send_count_skipped_total %d\n", b.sendCountSkipped.Load())
	if b.Store == nil {
		return skipped
	}
	// Scrapes must still expose pool pressure while database connections are held.
	ctx, cancel := context.WithTimeout(b.ctx, 100*time.Millisecond)
	defer cancel()
	usage, err := b.sendUsageSnapshot(ctx)
	if err != nil {
		return skipped
	}
	var out strings.Builder
	out.WriteString(skipped)
	_, _ = fmt.Fprintf(&out, "# TYPE whatsapp_bridge_send_today gauge\nwhatsapp_bridge_send_today %d\n# TYPE whatsapp_bridge_send_new_chats_today gauge\nwhatsapp_bridge_send_new_chats_today %d\n# TYPE whatsapp_bridge_send_rate_limited_total counter\n# TYPE whatsapp_bridge_send_refusals_total counter\n", usage.Today, usage.NewChatsToday)
	rows, err := b.Store.db.QueryContext(ctx, "SELECT reason,total FROM send_refusals")
	if err != nil {
		return out.String()
	}
	defer func() { _ = rows.Close() }()
	totals := map[string]int64{}
	for rows.Next() {
		var name string
		var total int64
		if rows.Scan(&name, &total) == nil {
			totals[name] = total
		}
	}
	for _, reason := range []string{"per_minute", "per_day", "new_chats_per_day", "min_interval", "limit_exceeds_batch"} {
		_, _ = fmt.Fprintf(&out, "whatsapp_bridge_send_rate_limited_total{limit=%q} %d\nwhatsapp_bridge_send_refusals_total{reason=%q} %d\n", reason, totals[reason], reason, totals[reason])
	}
	return out.String()
}

func (b *Bridge) allowParticipantAdds(w http.ResponseWriter, r *http.Request, targets ...string) bool {
	// The handler already applies allowsIdentity to the complete batch. Counting
	// uses parsed PN/LID identities; it must never add DM policy or network probes.
	return b.allowSend(w, r, targets...)
}
