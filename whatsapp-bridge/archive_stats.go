package main

import (
	"context"
	"time"
)

func (b *Bridge) archiveStats(now time.Time) (messages, session, rows int64, warning bool) {
	s := b.storeStats
	if s == nil {
		return
	}
	store, _, _ := s.snapshot(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbMeasuredAt.IsZero() || now.Sub(s.dbMeasuredAt) > storeUsageTTL {
		s.messagesBytes, s.sessionBytes = 0, 0
		if s.root != nil {
			for _, db := range []struct {
				name  string
				bytes *int64
			}{{"messages.db", &s.messagesBytes}, {"whatsapp.db", &s.sessionBytes}} {
				for _, suffix := range []string{"", "-wal"} {
					if info, err := s.root.Lstat(db.name + suffix); err == nil && info.Mode().IsRegular() {
						*db.bytes += info.Size()
					}
				}
			}
		}
		if b.Store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			// The primary-key covering index avoids reading message text/media pages.
			// Metrics must remain responsive even when every writer-pool connection
			// is held. A short-lived bounded WAL reader never queues on that pool.
			if reader, err := openArchiveDB(s.root, "messages.db", true); err == nil && reader != nil {
				_ = reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&s.rows)
				_ = reader.Close()
			}
			cancel()
		}
		s.dbMeasuredAt = now
	}
	above := b.HistoryLimits.WarnBytes > 0 && store > b.HistoryLimits.WarnBytes
	if above && !s.warning {
		b.Log.Warnf("Store size above configured warning threshold: bytes=%d", store)
		b.queueRuntimeStorageWarning("store", "above_threshold")
		if b.ForwardConnection && b.Webhook.Enabled() {
			// Use the existing tracked lifecycle delivery pool, with bounded timeout.
			b.connectionEventsMu.Lock()
			if !b.connectionEventsClosing {
				b.connectionEvents.Add(1)
				go func() {
					defer b.connectionEvents.Done()
					ctx, cancel := context.WithTimeout(b.ctx, connectionEventTimeout)
					defer cancel()
					b.Webhook.sendJSON(ctx, map[string]string{"type": "store", "state": "above_threshold"})
				}()
			}
			b.connectionEventsMu.Unlock()
		}
	}
	if !above && s.warning {
		b.queueRuntimeStorageWarning("store", "")
	}
	s.warning = above
	return s.messagesBytes, s.sessionBytes, s.rows, above
}

// Observe startup and every cache interval, even without health/metrics polling.
// Sampling is conditional on configured thresholds, and remote reads are bounded.
func (b *Bridge) observeStorageWarnings(now time.Time) {
	if b.ctx.Err() != nil {
		return
	}
	if b.HistoryLimits.WarnBytes > 0 {
		b.archiveStats(now)
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	quota, _, _, err := b.mediaQuotaSettings(ctx)
	if err != nil {
		return
	}
	if quota == 0 {
		b.observeMediaQuota(0, 0)
		return
	}
	usage, err := b.mediaStorage().Usage(ctx)
	if err == nil {
		b.observeMediaQuota(quota, usage.Bytes)
	}
}

func (b *Bridge) startStorageWarnings() {
	// Shutdown seals loop admission with the same lifecycle mutex before waiting.
	b.connectionEventsMu.Lock()
	defer b.connectionEventsMu.Unlock()
	if b.connectionEventsClosing || b.ctx.Err() != nil {
		return
	}
	b.storageWarningLoop.Add(1)
	go func() {
		defer b.storageWarningLoop.Done()
		ticks := b.storageWarningTicks
		if ticks == nil {
			ticker := time.NewTicker(storeUsageTTL)
			defer ticker.Stop()
			ticks = ticker.C
		}
		b.observeStorageWarnings(time.Now())
		for {
			select {
			case now, ok := <-ticks:
				if !ok {
					return
				}
				// Refresh at the exact TTL boundary, including a recently polled cache.
				if b.storeStats != nil {
					b.storeStats.invalidate()
				}
				b.observeStorageWarnings(now)
			case <-b.ctx.Done():
				return
			}
		}
	}()
}
