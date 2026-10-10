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
	return b.purgeStatusOnStartContext(b.ctx)
}

func (b *Bridge) purgeStatusOnStartContext(ctx context.Context) error {
	applied, err := migrationAppliedContext(ctx, b.Store.db, statusPurgeMarker)
	if err != nil || applied {
		return err
	}
	if err := b.statusPurgePolicy(ctx); err != nil {
		return err
	}
	no := false
	result, err := b.purgeOperatorMedia(ctx, operatorMediaPurge{Type: "status", Chat: "status@broadcast", DryRun: &no})
	if err != nil {
		return err
	}
	if result.Failed > 0 {
		return errors.New("status purge encountered refused paths or failed removals")
	}
	_, err = b.Store.db.ExecContext(ctx, "INSERT INTO schema_migrations(name) VALUES (?)", statusPurgeMarker)
	return err
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

func purgeStatusCLI(args []string, out, diagnostics io.Writer) int {
	fail := func(err error) int {
		_, _ = fmt.Fprintf(diagnostics, "purge-status-media: %v\n", err)
		return 1
	}
	flags := flag.NewFlagSet("purge-status-media", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	dry := flags.Bool("dry-run", false, "Report files and bytes without deleting")
	orphans := flags.Bool("include-orphans", false, "Also remove generated cached files with no message row")
	if err := flags.Parse(args); err != nil {
		return fail(err)
	}
	if flags.NArg() != 0 {
		return fail(errors.New("unexpected positional arguments"))
	}
	root, err := openStoreRoot()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		return fail(err)
	}
	defer lock.Release()
	defaults, err := runtimeDefaults(os.Getenv)
	if err != nil {
		return fail(err)
	}
	readOnly, err := parseReadOnly(os.Getenv(readOnlyEnv))
	if err != nil {
		return fail(err)
	}
	tools, err := newToolPolicy(os.Getenv(allowToolsEnv), os.Getenv(denyToolsEnv))
	if err != nil {
		return fail(err)
	}
	archive, err := openArchiveDB(root, "messages.db", false)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = archive.Close() }()
	store := &MessageStore{db: archive.DB}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b := &Bridge{Store: store, StoreRoot: root, Log: newTextWriter("Bridge", "INFO", diagnostics, false), ReadOnly: readOnly, Tools: tools, RuntimeDefaults: defaults, storeStats: newStoreStats(root)}
	if err := b.statusPurgePolicy(ctx); err != nil {
		return fail(err)
	}
	result, err := b.purgeOperatorMedia(ctx, operatorMediaPurge{Type: "status", Chat: "status@broadcast", DryRun: dry, IncludeOrphans: *orphans})
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return fail(err)
	}
	if result.Failed > 0 {
		return fail(fmt.Errorf("%d refused paths or failed removals", result.Failed))
	}
	return 0
}
