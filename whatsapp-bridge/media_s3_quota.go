package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"
)

type s3QuotaReservedKey struct{}

func (b *Bridge) acquireS3WriteBudget(ctx context.Context, hash []byte, incoming uint64) (context.Context, func(), error) {
	var exists int
	if err := b.Store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache WHERE sha256=? AND backend='s3'`, hash).Scan(&exists); err != nil {
		return ctx, func() {}, errMediaS3
	}
	if exists > 0 {
		return ctx, func() {}, nil
	}
	bounded, release, err := b.acquireS3MediaQuota(ctx, incoming)
	return context.WithValue(bounded, s3QuotaReservedKey{}, true), release, err
}

func (b *Bridge) acquireS3MediaQuota(ctx context.Context, incoming uint64) (bounded context.Context, release func(), resultErr error) {
	accountingCtx := ctx
	try, _ := ctx.Value(quotaTryKey{}).(bool)
	if try {
		var cancel context.CancelFunc
		accountingCtx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
	}
	defer func() {
		if resultErr != nil && accountingCtx.Err() != nil {
			if try && ctx.Err() == nil {
				resultErr = errMediaQuotaBusy
			} else {
				resultErr = accountingCtx.Err()
			}
		}
	}()
	unlock, err := b.lockMediaQuota(accountingCtx)
	if err != nil {
		return ctx, func() {}, err
	}
	defer unlock()
	quota, types, target, err := b.mediaQuotaSettings(accountingCtx)
	if err != nil {
		return ctx, func() {}, err
	}
	if quota == 0 {
		b.mediaQuotaNeedsLowWater.Store(false)
		return ctx, func() {}, nil
	}
	if target < 1 || target > 99 {
		return ctx, func() {}, errors.New("invalid eviction target")
	}
	s := b.mediaStorage().(*s3MediaStorage)
	usage, err := s.Usage(accountingCtx)
	if err != nil {
		return ctx, func() {}, err
	}
	if usage.Bytes < 0 {
		return ctx, func() {}, errMediaS3
	}
	used := uint64(usage.Bytes)
	for reservation := range b.mediaQuotaReservations {
		if reservation.finished.Load() {
			delete(b.mediaQuotaReservations, reservation)
		} else {
			used += reservation.bytes
		}
	}
	if incoming > quota {
		b.recordQuotaPause()
		return ctx, func() {}, errMediaQuota
	}
	if used >= quota || incoming > quota-min(used, quota) || b.mediaQuotaNeedsLowWater.Load() {
		rows, err := s.cachedRows(accountingCtx)
		if err != nil {
			return ctx, func() {}, err
		}
		low := mediaQuotaLowWater(quota, target)
		var freed uint64
		// Oldest references first. A shared object's bytes are only freed on
		// its final eligible reference; an unlisted type keeps its object alive.
		for _, row := range rows {
			if used <= low && incoming <= quota-used {
				break
			}
			kind := row.MediaType
			if row.ChatJID == "status@broadcast" {
				kind = "status"
			}
			if !slices.Contains(types, kind) {
				continue
			}
			results, err := s.Delete(accountingCtx, []mediaRow{row}, false)
			if err != nil {
				return ctx, func() {}, err
			}
			for _, result := range results {
				if result.Purged {
					if result.Bytes < 0 {
						return ctx, func() {}, errMediaS3
					}
					n := uint64(result.Bytes)
					used -= min(used, n)
					freed += n
					b.metrics.recordMediaEviction(kind, n)
				}
			}
		}
		b.mediaQuotaNeedsLowWater.Store(len(types) > 0 && used > low)
		if len(types) > 0 {
			b.Log.Infof("Media eviction: freed_bytes=%d remaining_bytes=%d target_percent=%d", freed, used, target)
		}
	}
	if used >= quota || incoming > quota-used || b.mediaQuotaNeedsLowWater.Load() {
		b.recordQuotaPause()
		return ctx, func() {}, errMediaQuota
	}
	limit := incoming
	if limit == 0 {
		limit = quota - used
	}
	if current := mediaLimit(ctx); current > 0 {
		limit = min(limit, current)
	}
	r := &mediaQuotaReservation{bytes: limit}
	if b.mediaQuotaReservations == nil {
		b.mediaQuotaReservations = map[*mediaQuotaReservation]bool{}
	}
	b.mediaQuotaReservations[r] = true
	return withMediaLimit(context.WithValue(ctx, quotaBudgetKey{}, true), limit), func() { r.finished.Store(true) }, nil
}

func (b *Bridge) cacheOutboundS3(ctx context.Context, sent sentMessage, media outboundMedia, data []byte) {
	if sent.ChatJID == "status@broadcast" && !b.statusMediaEnabled(ctx) {
		return
	}
	if b.MediaMaxBytes > 0 && uint64(len(data)) > b.MediaMaxBytes {
		b.recordAutoSizeSkip(sent.ID, sent.ChatJID)
		return
	}
	row := mediaRow{ID: sent.ID, ChatJID: sent.ChatJID, MediaType: media.mediaType, Timestamp: sent.Timestamp, Filename: media.filename}
	_, err := b.mediaStorage().Write(ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err != nil {
		b.Log.Warnf("Sent media cache failed: %v", err)
	}
}
