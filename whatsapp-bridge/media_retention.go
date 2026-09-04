package main

// Media storage controls (issue #60).
//
// Every inbound image/audio/video is downloaded into store/<chat>/ and kept
// forever, which grows without bound on an always-on server. Three knobs:
//
//   WHATSAPP_MEDIA_AUTODOWNLOAD   default true. false = only an explicit
//                                 /api/download (MCP download_media) fetches
//                                 files; media-retry makes late fetches work.
//   WHATSAPP_MEDIA_RETENTION_DAYS default unset. N>0 = a daily sweep deletes
//                                 media files older than N days. Message rows
//                                 keep their media metadata, so download_media
//                                 re-fetches on demand.
//   store size                    /api/health reports store_bytes and
//                                 media_bytes, cached for storeUsageTTL.
//
// Media lives only in chat directories (names contain "@"); the sweep never
// touches the SQLite files, the token or the lock at the store root.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mediaAutoDownloadEnv = "WHATSAPP_MEDIA_AUTODOWNLOAD"
	mediaRetentionEnv    = "WHATSAPP_MEDIA_RETENTION_DAYS"
	mediaSweepInterval   = 24 * time.Hour
	storeUsageTTL        = 5 * time.Minute
)

// resolveMediaRetention parses WHATSAPP_MEDIA_RETENTION_DAYS. Zero means
// disabled; negative or non-numeric values are an error so main() fails fast.
func resolveMediaRetention(value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, nil
	}
	days, err := strconv.Atoi(v)
	if err != nil || days < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of days", mediaRetentionEnv, value)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// retentionSummary renders the retention setting for the startup log.
func retentionSummary(maxAge time.Duration) string {
	if maxAge <= 0 {
		return "off"
	}
	return fmt.Sprintf("%d days", int(maxAge.Hours()/24))
}

// isChatDir reports whether a store entry is a per-chat media directory.
func isChatDir(entry os.DirEntry) bool {
	return entry.IsDir() && strings.Contains(entry.Name(), "@")
}

// sweepMedia deletes regular files under root's chat directories whose
// modification time is older than now-maxAge. Returns files removed and
// bytes freed. Errors on individual files are counted, not fatal.
func sweepMedia(root string, maxAge time.Duration, now time.Time) (removed int, freed int64, failed int) {
	cutoff := now.Add(-maxAge)
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, 1
	}
	for _, entry := range entries {
		if !isChatDir(entry) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			info, statErr := d.Info()
			if statErr != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
				return nil
			}
			if rmErr := os.Remove(path); rmErr != nil {
				failed++
				return nil
			}
			removed++
			freed += info.Size()
			return nil
		})
	}
	return removed, freed, failed
}

// storeUsage measures the store directory. storeBytes covers everything
// (databases included); mediaBytes only the chat directories.
func storeUsage(root string) (storeBytes, mediaBytes int64, mediaFiles int) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		storeBytes += info.Size()
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			if top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]; strings.Contains(top, "@") {
				mediaBytes += info.Size()
				mediaFiles++
			}
		}
		return nil
	})
	return storeBytes, mediaBytes, mediaFiles
}

// storeStats caches storeUsage so /api/health stays cheap under Docker's
// periodic health checks.
type storeStats struct {
	mu         sync.Mutex
	root       string
	measuredAt time.Time
	store      int64
	media      int64
	files      int
}

func newStoreStats(root string) *storeStats { return &storeStats{root: root} }

// snapshot returns the cached usage, refreshing it when older than storeUsageTTL.
func (s *storeStats) snapshot(now time.Time) (storeBytes, mediaBytes int64, mediaFiles int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.measuredAt.IsZero() || now.Sub(s.measuredAt) > storeUsageTTL {
		s.store, s.media, s.files = storeUsage(s.root)
		s.measuredAt = now
	}
	return s.store, s.media, s.files
}

// invalidate forces the next snapshot to re-measure (called after a sweep).
func (s *storeStats) invalidate() {
	s.mu.Lock()
	s.measuredAt = time.Time{}
	s.mu.Unlock()
}

// runMediaRetention sweeps once now and then every mediaSweepInterval until
// b.ctx is cancelled (Shutdown). maxAge <= 0 disables it.
func (b *Bridge) runMediaRetention(maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	sweep := func() {
		removed, freed, failed := sweepMedia(storeDir(), maxAge, time.Now())
		b.storeStats.invalidate()
		if removed > 0 || failed > 0 {
			bridgeLog.Infof("Media retention: removed %d files (%d bytes) older than %s, %d failures", removed, freed, maxAge, failed)
		} else {
			bridgeLog.Debugf("Media retention: nothing older than %s", maxAge)
		}
	}
	sweep()
	ticker := time.NewTicker(mediaSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-b.ctx.Done():
			return
		}
	}
}
