package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mediaQuotaEnv = "WHATSAPP_MEDIA_QUOTA_BYTES"
const mediaEvictTypesEnv = "WHATSAPP_MEDIA_QUOTA_EVICT_TYPES"
const mediaEvictTargetEnv = "WHATSAPP_MEDIA_QUOTA_EVICT_TARGET_PERCENT"

var errMediaQuota = errors.New("automatic media cache paused at local quota")
var errMediaQuotaBusy = errors.New("automatic cache accounting busy")

type automaticCacheKey struct{}
type quotaBudgetKey struct{}
type quotaTryKey struct{}
type quotaPathKey struct{}
type mediaQuotaReservation struct {
	path  string
	bytes uint64
}

func (b *Bridge) lockMediaQuota(ctx context.Context) (func(), error) {
	b.mediaQuotaMu.Lock()
	if b.mediaQuotaLease == nil {
		b.mediaQuotaLease = make(chan struct{}, 1)
	}
	lease := b.mediaQuotaLease
	b.mediaQuotaMu.Unlock()
	if try, _ := ctx.Value(quotaTryKey{}).(bool); try {
		select {
		case lease <- struct{}{}:
		default:
			return nil, errMediaQuotaBusy
		}
	} else {
		select {
		case lease <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-lease }, nil
}

func (b *Bridge) recordQuotaPause() {
	b.metrics.mediaQuotaRefusals.Add(1)
	now := time.Now().Unix()
	previous := b.mediaQuotaLastPause.Load()
	if now-previous >= 60 && b.mediaQuotaLastPause.CompareAndSwap(previous, now) {
		b.Log.Infof("Automatic media caching paused at local quota")
	}
}

func withAutomaticCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, automaticCacheKey{}, true)
}
func automaticCache(ctx context.Context) bool {
	value, _ := ctx.Value(automaticCacheKey{}).(bool)
	return value
}

func parseQuota(raw string) (uint64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: expected non-negative bytes", mediaQuotaEnv, configValue(raw))
	}
	return n, nil
}
func parseEvictTypes(raw string) ([]string, error) {
	set := map[string]bool{}
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	for _, kind := range strings.Split(raw, ",") {
		kind = strings.TrimSpace(kind)
		if kind != "status" && !slices.Contains(purgeMediaTypes, kind) {
			return nil, fmt.Errorf("invalid %s=%q: expected image, video, audio, document, sticker or status", mediaEvictTypesEnv, configValue(raw))
		}
		set[kind] = true
	}
	return sortedNames(set), nil
}
func parseEvictTarget(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 90, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 99 {
		return 0, fmt.Errorf("invalid %s=%q: expected 1-99 percent", mediaEvictTargetEnv, configValue(raw))
	}
	return n, nil
}

func mediaSettingDefinitions() []settingDefinition {
	return []settingDefinition{
		{statusMediaSetting, mediaAutoDownloadStatusEnv, false, func(raw json.RawMessage) (any, error) {
			var n bool
			if string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
				return nil, errors.New("expected boolean")
			}
			return n, nil
		}, func(raw string) (any, error) { return resolveStatusAutoDownload(raw) }},
		{"media.quota_bytes", mediaQuotaEnv, uint64(0), func(raw json.RawMessage) (any, error) {
			var n uint64
			if json.Unmarshal(raw, &n) != nil {
				return nil, errors.New("expected non-negative bytes")
			}
			return n, nil
		}, func(raw string) (any, error) { return parseQuota(raw) }},
		{"media.quota_evict_types", mediaEvictTypesEnv, []string{}, func(raw json.RawMessage) (any, error) {
			var names []string
			if json.Unmarshal(raw, &names) != nil || names == nil {
				return nil, errors.New("expected type array")
			}
			for _, name := range names {
				if name == "" || strings.Contains(name, ",") {
					return nil, errors.New("invalid type")
				}
			}
			return parseEvictTypes(strings.Join(names, ","))
		}, func(raw string) (any, error) { return parseEvictTypes(raw) }},
		{"media.quota_evict_target_percent", mediaEvictTargetEnv, 90, func(raw json.RawMessage) (any, error) {
			var n int
			if json.Unmarshal(raw, &n) != nil {
				return nil, errors.New("expected percent")
			}
			return parseEvictTarget(strconv.Itoa(n))
		}, func(raw string) (any, error) { return parseEvictTarget(raw) }},
	}
}

func applyRuntimeMediaCeilings(out *runtimeSettingsSnapshot, defaults map[string]runtimeSetting) {
	for _, key := range []string{statusMediaSetting, "media.quota_bytes", "media.quota_evict_types", "media.quota_evict_target_percent"} {
		deploy, ok := defaults[key]
		if !ok || deploy.Source != "env" {
			continue
		}
		setting := out.Settings[key]
		switch key {
		case statusMediaSetting:
			if deploy.Value == false {
				setting.Value = false
			}
		case "media.quota_bytes":
			ceiling := deploy.Value.(uint64)
			value := setting.Value.(uint64)
			if ceiling > 0 && (value == 0 || value > ceiling) {
				setting.Value = ceiling
			}
		case "media.quota_evict_types":
			value := []string{}
			for _, kind := range setting.Value.([]string) {
				if slices.Contains(deploy.Value.([]string), kind) {
					value = append(value, kind)
				}
			}
			setting.Value = value
		case "media.quota_evict_target_percent":
			setting.Value = min(setting.Value.(int), deploy.Value.(int))
		}
		out.Settings[key] = setting
	}
}

func (b *Bridge) mediaQuotaSettings(ctx context.Context) (uint64, []string, int, error) {
	if b.RuntimeDefaults == nil {
		return b.MediaQuotaBytes, b.MediaEvictTypes, 90, nil
	}
	snapshot, err := b.settingsSnapshot(ctx)
	if err != nil {
		return 0, nil, 0, err
	}
	return snapshot.Settings["media.quota_bytes"].Value.(uint64), snapshot.Settings["media.quota_evict_types"].Value.([]string), snapshot.Settings["media.quota_evict_target_percent"].Value.(int), nil
}

// Account and evict under a short lease, then reserve bytes before any network
// work. The detached transfer owns its reservation through retries/publication;
// its actual plaintext cannot exceed the reservation and release reconciles it
// with the canonical filesystem on the next admission. Active paths are excluded
// from that walk, preventing both double counting and eviction before release.
// On-demand downloads keep the existing local behavior pending #649 streaming.
func (b *Bridge) acquireMediaQuota(ctx context.Context, incoming uint64) (context.Context, func(), error) {
	release, err := b.lockMediaQuota(ctx)
	if err != nil {
		return ctx, func() {}, err
	}
	if err := ctx.Err(); err != nil {
		release()
		return ctx, func() {}, err
	}
	quota, types, target, err := b.mediaQuotaSettings(ctx)
	if err != nil {
		release()
		return ctx, func() {}, err
	}
	if quota == 0 {
		b.mediaQuotaNeedsLowWater.Store(false)
		release()
		return ctx, func() {}, nil
	}
	if target < 1 || target > 99 {
		release()
		return ctx, func() {}, errors.New("invalid eviction target")
	}
	if incoming > quota {
		b.recordQuotaPause()
		release()
		return ctx, func() {}, errMediaQuota
	}
	var used uint64
	active := map[string]bool{}
	for reservation := range b.mediaQuotaReservations {
		used += reservation.bytes
		active[reservation.path] = true
	}
	type candidate struct {
		chat, name, kind string
		bytes            uint64
		at               time.Time
	}
	var candidates []candidate
	err = eachCachedMediaContext(ctx, b.StoreRoot, func(chat string, file *cachedMedia) {
		if active[path.Join(chat, file.name)] {
			return
		}
		size := file.info.Size()
		if size < 0 {
			return
		}
		bytes := uint64(size)
		used += bytes
		kind := mediaScope(chat, file.name)
		if slices.Contains(types, kind) {
			candidates = append(candidates, candidate{chat, file.name, kind, bytes, file.info.ModTime()})
		}
	})
	if err != nil {
		release()
		return ctx, func() {}, err
	}
	if used >= quota || incoming > quota-used || b.mediaQuotaNeedsLowWater.Load() {
		low := mediaQuotaLowWater(quota, target)
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].at.Equal(candidates[j].at) {
				return candidates[i].chat+"/"+candidates[i].name < candidates[j].chat+"/"+candidates[j].name
			}
			return candidates[i].at.Before(candidates[j].at)
		})
		var freed uint64
		for _, file := range candidates {
			if ctx.Err() != nil {
				break
			}
			if used <= low && incoming <= quota-used {
				break
			}
			found, err := findCachedMedia(b.StoreRoot, file.chat, []string{file.name})
			if err != nil || found == nil {
				continue
			}
			size := found.info.Size()
			if size < 0 {
				found.Close()
				continue
			}
			bytes := uint64(size)
			if found.Remove() == nil {
				used -= min(used, bytes)
				freed += bytes
				b.metrics.recordMediaEviction(file.kind, bytes)
			}
			found.Close()
		}
		if freed > 0 {
			b.storeStats.invalidate()
		}
		if len(types) > 0 {
			b.Log.Infof("Media eviction: freed_bytes=%d remaining_bytes=%d target_percent=%d", freed, used, target)
		}
		// If eligible files cannot reach the target, keep caching paused until
		// cleanup or a settings change brings usage under the new low-water mark.
		b.mediaQuotaNeedsLowWater.Store(len(types) > 0 && used > low)
	}
	if ctx.Err() != nil {
		release()
		return ctx, func() {}, ctx.Err()
	}
	if used >= quota || incoming > quota-used || b.mediaQuotaNeedsLowWater.Load() {
		b.recordQuotaPause()
		release()
		return ctx, func() {}, errMediaQuota
	}
	limit := quota - used
	if incoming > 0 {
		limit = min(limit, incoming)
	}
	if current := mediaLimit(ctx); current > 0 {
		if limit < current {
			ctx = context.WithValue(ctx, quotaBudgetKey{}, true)
		}
		limit = min(limit, current)
	} else {
		ctx = context.WithValue(ctx, quotaBudgetKey{}, true)
	}
	name, _ := ctx.Value(quotaPathKey{}).(string)
	reservation := &mediaQuotaReservation{path: name, bytes: limit}
	if b.mediaQuotaReservations == nil {
		b.mediaQuotaReservations = map[*mediaQuotaReservation]bool{}
	}
	b.mediaQuotaReservations[reservation] = true
	release()
	var once sync.Once
	return withMediaLimit(ctx, limit), func() {
		once.Do(func() {
			unlock, _ := b.lockMediaQuota(context.Background())
			delete(b.mediaQuotaReservations, reservation)
			unlock()
		})
	}, nil
}

func mediaQuotaLowWater(quota uint64, target int) uint64 {
	if target < 1 || target > 99 {
		return 0
	}
	// Division first avoids an overflow even for the largest unsigned quota.
	return quota/100*uint64(target) + quota%100*uint64(target)/100
}

func (b *Bridge) mediaCachingPaused(quota uint64, bytes int64, types []string, target int) bool {
	if quota == 0 || bytes < 0 {
		return false
	}
	used := uint64(bytes)
	return used >= quota || (b.mediaQuotaNeedsLowWater.Load() && len(types) > 0 && used > mediaQuotaLowWater(quota, target))
}

func (m *metricsRegistry) recordMediaEviction(kind string, bytes uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mediaEvicted == nil {
		m.mediaEvicted = map[string]uint64{}
	}
	m.mediaEvicted[kind] += bytes
}

func (b *Bridge) mediaQuotaMetrics() []string {
	m := b.metrics
	lines := []string{"# HELP whatsapp_bridge_media_quota_refusals_total Automatic cache transfers refused at the local quota.", "# TYPE whatsapp_bridge_media_quota_refusals_total counter", fmt.Sprintf("whatsapp_bridge_media_quota_refusals_total %d", m.mediaQuotaRefusals.Load()), "# HELP whatsapp_bridge_media_evicted_bytes_total Local cached bytes evicted by type.", "# TYPE whatsapp_bridge_media_evicted_bytes_total counter"}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, kind := range []string{"image", "video", "audio", "document", "sticker", "status"} {
		lines = append(lines, fmt.Sprintf("whatsapp_bridge_media_evicted_bytes_total{type=%q} %d", kind, m.mediaEvicted[kind]))
	}
	return lines
}
