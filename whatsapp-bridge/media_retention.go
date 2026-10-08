package main

// Media storage controls (issue #60).
//
// Every inbound image/audio/video is downloaded into store/<chat>/ and kept
// forever, which grows without bound on an always-on server. Four knobs:
//
//   WHATSAPP_MEDIA_AUTODOWNLOAD   default true. false = only an explicit
//                                 /api/download (MCP download_media) fetches
//                                 files; media-retry makes late fetches work.
//   WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS
//                                 default false. The status feed
//                                 (status@broadcast) is stored like any chat,
//                                 but its media is cached on arrival only when
//                                 this is true (issue #447).
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
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	mediaAutoDownloadEnv       = "WHATSAPP_MEDIA_AUTODOWNLOAD"
	mediaAutoDownloadStatusEnv = "WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS"
	mediaRetentionEnv          = "WHATSAPP_MEDIA_RETENTION_DAYS"
	mediaMaxBytesEnv           = "WHATSAPP_MEDIA_MAX_BYTES"
	defaultMediaMaxBytes       = 256 * 1024 * 1024
	mediaSweepInterval         = 24 * time.Hour
	storeUsageTTL              = 5 * time.Minute
)

// resolveStatusAutoDownload parses WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS: off
// when unset, and an unreadable value is an error so main() fails fast instead
// of quietly filling the disk, or quietly not caching what was asked for.
func resolveStatusAutoDownload(value string) (bool, error) {
	return parseBoolEnv(mediaAutoDownloadStatusEnv, value, false)
}

// isStatusChat reports whether chat is the status feed (status@broadcast),
// where every contact's status posts arrive.
func isStatusChat(chat types.JID) bool {
	return chat.User == types.StatusBroadcastJID.User && chat.Server == types.StatusBroadcastJID.Server
}

// skipsStatusMedia reports whether media arriving in chat is left on the CDN
// because the chat is the status feed and the operator did not ask for it:
// status media needs both switches. The row keeps its CDN fields, so
// /api/download still fetches the file on demand.
func (b *Bridge) skipsStatusMedia(chat types.JID) bool {
	cachedOnArrival := b.MediaAutoDownload && b.MediaAutoDownloadStatus
	return isStatusChat(chat) && !cachedOnArrival
}

// resolveMediaRetention parses WHATSAPP_MEDIA_RETENTION_DAYS. Zero means
// disabled; negative or non-numeric values are an error so main() fails fast.
func resolveMediaRetention(value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, nil
	}
	days, err := strconv.Atoi(v)
	if err != nil || days < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of days", mediaRetentionEnv, configValue(value))
	}
	if int64(days) > int64((1<<63-1)/(24*time.Hour)) {
		return 0, fmt.Errorf("invalid %s=%q: duration overflows (at most %d days)", mediaRetentionEnv, configValue(value), (1<<63-1)/(24*time.Hour))
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// resolveMediaMaxBytes parses WHATSAPP_MEDIA_MAX_BYTES (default 256 MiB; 0 = no limit).
func resolveMediaMaxBytes(value string) (uint64, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return defaultMediaMaxBytes, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative integer of bytes", mediaMaxBytesEnv, configValue(value))
	}
	return n, nil
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

// sweepMedia deletes regular files under the store root's chat directories
// whose modification time is older than now-maxAge. Returns files removed and
// bytes freed. Errors on individual files are counted, not fatal; a nil root
// (the store could not be opened) counts as one failure.
//
// The shared cache walker pins each real chat directory and visits only plain
// regular generated cache names directly inside it. Deletes use that same
// directory handle; user files, .part downloads, nested directories and
// symlinks are neither traversed nor removed.
func sweepMedia(root *os.Root, maxAge time.Duration, now time.Time) (removed int, freed int64, failed int) {
	cutoff := now.Add(-maxAge)
	if err := eachCachedMedia(root, func(_ string, file *cachedMedia) {
		if !file.info.ModTime().Before(cutoff) {
			return
		}
		if err := file.Remove(); err != nil {
			failed++
			return
		}
		removed++
		freed += file.info.Size()
	}); err != nil {
		failed++
	}
	return removed, freed, failed
}

// storeUsage measures the store directory through its os.Root. storeBytes
// covers all regular store files (databases and nested files included);
// mediaBytes and mediaFiles use the same cache rule as download and purge.
// A nil root measures nothing.
func storeUsage(root *os.Root) (storeBytes, mediaBytes int64, mediaFiles int) {
	if root == nil {
		return 0, 0, 0
	}
	_ = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		storeBytes += info.Size()
		return nil
	})
	_ = eachCachedMedia(root, func(_ string, file *cachedMedia) {
		mediaBytes += file.info.Size()
		mediaFiles++
	})
	return storeBytes, mediaBytes, mediaFiles
}

// storeStats caches storeUsage so /api/health stays cheap under Docker's
// periodic health checks.
type storeStats struct {
	mu         sync.Mutex
	root       *os.Root
	measuredAt time.Time
	store      int64
	media      int64
	files      int
}

func newStoreStats(root *os.Root) *storeStats { return &storeStats{root: root} }

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
// b.ctx is cancelled (Shutdown). b.MediaRetention <= 0 disables it.
func (b *Bridge) runMediaRetention() {
	maxAge := b.MediaRetention
	if maxAge <= 0 {
		return
	}
	sweep := func() {
		removed, freed, failed := sweepMedia(b.StoreRoot, maxAge, time.Now())
		b.storeStats.invalidate()
		if removed > 0 || failed > 0 {
			b.Log.Infof("Media retention: removed %d files (%d bytes) older than %s, %d failures", removed, freed, maxAge, failed)
		} else {
			b.Log.Debugf("Media retention: nothing older than %s", maxAge)
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
