package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type s3QuotaReservedKey struct{}
type s3QuotaReservationKey struct{}
type s3QuotaHashKey struct{}

func (b *Bridge) acquireS3WriteBudget(ctx context.Context, hash []byte, incoming uint64) (context.Context, func(), error) {
	var exists int
	if err := b.Store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+b.mediaStorage().(*s3MediaStorage).chargedObjectsSQL()+`) WHERE sha256=?`, hash).Scan(&exists); err != nil {
		return ctx, func() {}, errMediaS3
	}
	if exists > 0 {
		return ctx, func() {}, nil
	}
	ctx = context.WithValue(ctx, s3QuotaHashKey{}, hex.EncodeToString(hash))
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
	var reservedBytes uint64
	hashes := []string{}
	for reservation := range b.mediaQuotaReservations {
		if reservation.finished.Load() {
			delete(b.mediaQuotaReservations, reservation)
		} else {
			reservedBytes += reservation.bytes
			if reservation.s3Hash != "" {
				hashes = append(hashes, reservation.s3Hash)
			}
		}
	}
	// The reservation continues to own its bytes while PUT/verification is
	// active. Exclude those hashes from durable charges in the same SQL read;
	// once release finishes, the catalog or recovery intent owns the bytes.
	excluded, err := json.Marshal(hashes)
	if err != nil {
		return ctx, func() {}, errMediaS3
	}
	var charged int64
	if err := s.store.db.QueryRowContext(accountingCtx, `SELECT COALESCE(SUM(bytes),0) FROM (`+s.chargedObjectsSQL()+`) WHERE lower(hex(sha256)) NOT IN (SELECT value FROM json_each(?))`, string(excluded)).Scan(&charged); err != nil || charged < 0 {
		return ctx, func() {}, errMediaS3
	}
	used := uint64(charged) + reservedBytes
	if incoming > quota {
		b.recordQuotaPause()
		return ctx, func() {}, errMediaQuota
	}
	if used >= quota || incoming > quota-min(used, quota) || b.mediaQuotaNeedsLowWater.Load() {
		low := mediaQuotaLowWater(quota, target)
		var freed uint64
		predicate, args := s3MediaTypesPredicate(types)
		// Oldest references first. A shared object's bytes are only freed on
		// its final eligible reference; an unlisted type keeps its object alive.
		err := s.walkCachedRows(accountingCtx, predicate, args, func(rows []mediaRow) (bool, error) {
			for _, row := range rows {
				if used <= low && incoming <= quota-used {
					return false, nil
				}
				kind := row.MediaType
				if row.ChatJID == "status@broadcast" {
					kind = "status"
				}
				results, err := s.Delete(accountingCtx, []mediaRow{row}, false)
				if err != nil {
					return false, err
				}
				for _, result := range results {
					if result.Purged {
						if result.Bytes < 0 {
							return false, errMediaS3
						}
						n := uint64(result.Bytes)
						used -= min(used, n)
						freed += n
						b.metrics.recordMediaEviction(kind, n)
					}
				}
			}
			return used > low || incoming > quota-used, nil
		})
		if err != nil {
			return ctx, func() {}, err
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
	r.s3Hash, _ = ctx.Value(s3QuotaHashKey{}).(string)
	if b.mediaQuotaReservations == nil {
		b.mediaQuotaReservations = map[*mediaQuotaReservation]bool{}
	}
	b.mediaQuotaReservations[r] = true
	bounded = context.WithValue(ctx, s3QuotaReservationKey{}, r)
	return withMediaLimit(context.WithValue(bounded, quotaBudgetKey{}, true), limit), func() { r.finished.Store(true) }, nil
}

func (b *Bridge) bindS3QuotaReservation(ctx context.Context, hash []byte, size int64) error {
	if size < 0 {
		return errMediaHash
	}
	r, _ := ctx.Value(s3QuotaReservationKey{}).(*mediaQuotaReservation)
	if r == nil {
		return nil
	}
	// Called before the content lock, preserving quota -> content lock order.
	unlock, err := b.lockMediaQuota(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r.s3Hash = hex.EncodeToString(hash)
	r.bytes = min(r.bytes, uint64(size))
	return nil
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
	name := mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)
	key, err := filepath.Abs(storePath(chatMediaRel(row.ChatJID), name))
	if err == nil {
		_, err = b.mediaTransfers.do(ctx, "sent-cache:"+key, func() (int64, error) {
			transferCtx, cancel := transferContext(b.ctx, ctx)
			defer cancel()
			for {
				attempted := false
				written, err := b.mediaTransfers.do(transferCtx, key, func() (int64, error) {
					attempted = true
					return b.mediaStorage().Write(transferCtx, row, func(rel string) (int64, error) {
						return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
					})
				})
				if err == nil || transferCtx.Err() != nil || attempted {
					return written, err
				}
				// A failed joined download released the key. Retain the bytes
				// and publish them ourselves; never retry our own write error.
			}
		})
	}
	if err != nil {
		b.Log.Warnf("Sent media cache failed: %v", err)
	}
}
