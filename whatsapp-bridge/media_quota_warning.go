package main

import (
	"context"
	"time"
)

func (b *Bridge) observeMediaQuota(quota uint64, bytes int64) bool {
	percent := b.MediaQuotaWarnPercent
	if percent == 0 {
		percent = 80
	}
	state := int64(0)
	if quota > 0 && bytes >= 0 {
		used := uint64(bytes)
		threshold := quota/100*uint64(percent) + (quota%100*uint64(percent)+99)/100
		if used >= quota {
			state = 2
		} else if used >= threshold {
			state = 1
		}
	}
	previous := b.mediaQuotaWarningState.Swap(state)
	if state > 0 && state != previous {
		label := "warning"
		if state == 2 {
			label = "full"
		}
		b.Log.Warnf("Media quota %s: bytes=%d quota_bytes=%d", label, bytes, quota)
		if b.ForwardConnection && b.Webhook.Enabled() {
			// Synchronous and bounded; no untracked goroutine survives shutdown.
			ctx, cancel := context.WithTimeout(b.ctx, connectionEventTimeout)
			b.Webhook.sendJSON(ctx, map[string]any{"type": "media_quota", "state": label, "at": time.Now().UTC()})
			cancel()
		}
	}
	return state > 0
}
