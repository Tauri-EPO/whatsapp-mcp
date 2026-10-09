package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const statusRetentionEnv = "WHATSAPP_MEDIA_STATUS_RETENTION_DAYS"
const purgeStatusOnStartEnv = "WHATSAPP_MEDIA_PURGE_STATUS_ON_START"
const statusMediaSetting = "media.autodownload_status"
const statusPurgeMarker = "purge-status-media-v1"

func resolveStatusRetention(raw string) (*time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	value, err := resolveMediaRetention(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s=%q: expected 0-106751 days", statusRetentionEnv, configValue(raw))
	}
	return &value, nil
}

func (b *Bridge) statusMediaEnabled(ctx context.Context) bool {
	if b.RuntimeDefaults == nil {
		return b.MediaAutoDownloadStatus
	}
	snapshot, err := b.settingsSnapshot(ctx)
	return err == nil && snapshot.Settings[statusMediaSetting].Value == true
}

// Startup owns the instance lock. Record completion only after a successful
// filesystem purge; a crash/failed removal safely retries the remaining files.
// Metadata belongs to messages.db, never the MCP-owned notes.db.
func (b *Bridge) purgeStatusOnStart() error {
	applied, err := migrationApplied(b.Store.db, statusPurgeMarker)
	if err != nil || applied {
		return err
	}
	if err := b.statusPurgePolicy(b.ctx); err != nil {
		return err
	}
	no := false
	result, err := b.purgeOperatorMedia(b.ctx, operatorMediaPurge{Type: "status", Chat: "status@broadcast", DryRun: &no})
	if err != nil {
		return err
	}
	if result.Failed > 0 {
		return errors.New("status purge encountered refused paths or failed removals")
	}
	return recordMigration(b.Store.db, statusPurgeMarker)
}

func (b *Bridge) statusPurgePolicy(ctx context.Context) error {
	if b.ReadOnly.enabled {
		return errors.New("status purge disabled by read-only")
	}
	policy := b.Tools
	if b.RuntimeDefaults != nil {
		snapshot, err := b.settingsSnapshot(ctx)
		if err != nil {
			return err
		}
		policy = snapshot.policy
	}
	if message := policy.refusal("/api/media/purge"); message != "" {
		return errors.New(message)
	}
	return nil
}

func purgeStatusCLI(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("purge-status-media", flag.ContinueOnError)
	flags.SetOutput(out)
	dry := flags.Bool("dry-run", false, "Report files and bytes without deleting")
	orphans := flags.Bool("include-orphans", false, "Also remove generated cached files with no message row")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 1
	}
	root, err := openStoreRoot()
	if err != nil {
		return 1
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		return 1
	}
	defer lock.Release()
	defaults, err := runtimeDefaults(os.Getenv)
	if err != nil {
		return 1
	}
	readOnly, err := parseReadOnly(os.Getenv(readOnlyEnv))
	if err != nil {
		return 1
	}
	tools, err := newToolPolicy(os.Getenv(allowToolsEnv), os.Getenv(denyToolsEnv))
	if err != nil {
		return 1
	}
	archive, err := openArchiveDB(root, "messages.db", false)
	if err != nil {
		return 1
	}
	defer func() { _ = archive.Close() }()
	store := &MessageStore{db: archive.DB}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b := &Bridge{Store: store, StoreRoot: root, Log: bridgeLog, ReadOnly: readOnly, Tools: tools, RuntimeDefaults: defaults, storeStats: newStoreStats(root)}
	if err := b.statusPurgePolicy(ctx); err != nil {
		return 1
	}
	result, err := b.purgeOperatorMedia(ctx, operatorMediaPurge{Type: "status", Chat: "status@broadcast", DryRun: dry, IncludeOrphans: *orphans})
	if err != nil || json.NewEncoder(out).Encode(result) != nil || result.Failed > 0 {
		return 1
	}
	return 0
}
