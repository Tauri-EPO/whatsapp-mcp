package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

type mediaMigrationResult struct {
	Files            int            `json:"files"`
	Bytes            int64          `json:"bytes"`
	DedupeSavedBytes int64          `json:"dedupe_saved_bytes"`
	Skipped          int            `json:"skipped"`
	Failed           int            `json:"failed"`
	DryRun           bool           `json:"dry_run"`
	SkipReasons      map[string]int `json:"skip_reasons"`
}

type mediaReadOnlyKey struct{}

func migrateMediaCLI(args []string, out, diagnostic io.Writer) int {
	flags := flag.NewFlagSet("migrate-media", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	to := flags.String("to", "", "s3 or local")
	dry := flags.Bool("dry-run", false, "report without changing media or database contents")
	remove := flags.Bool("delete-source", false, "remove sources only after destination size and hash verification")
	concurrency := flags.Int("concurrency", 1, "bounded migration workers (1-4)")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*to != "s3" && *to != "local") || *concurrency < 1 || *concurrency > 4 {
		_, _ = fmt.Fprintln(diagnostic, "migrate-media requires --to s3|local and --concurrency 1..4")
		return 1
	}
	getenv := func(key string) string {
		if key == "WHATSAPP_MEDIA_BACKEND" {
			return "s3"
		}
		return os.Getenv(key)
	}
	cfg, err := parseMediaBackend(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostic, err)
		return 1
	}
	root, err := openStoreRoot()
	if err != nil {
		_, _ = fmt.Fprintln(diagnostic, "Media store unavailable")
		return 1
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		_, _ = fmt.Fprintln(diagnostic, "Stop the bridge before migrating media")
		return 1
	}
	defer lock.Release()
	info, err := root.Lstat("messages.db")
	if err != nil || !info.Mode().IsRegular() {
		_, _ = fmt.Fprintln(diagnostic, "Message catalog must be an existing regular file")
		return 1
	}
	var store *MessageStore
	if *dry {
		db, openErr := openArchiveDB(root, "messages.db", false)
		if openErr != nil {
			_, _ = fmt.Fprintln(diagnostic, "Message catalog unavailable")
			return 1
		}
		defer func() { _ = db.directory.Close() }()
		store = &MessageStore{db: db.DB}
	} else {
		store, err = NewMessageStore()
		if err != nil {
			_, _ = fmt.Fprintln(diagnostic, "Message catalog unavailable")
			return 1
		}
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	if *dry {
		ctx = context.WithValue(ctx, mediaReadOnlyKey{}, true)
	}
	s3, err := newS3MediaStorage(ctx, cfg, store, root)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostic, "S3 bucket probe failed")
		return 1
	}
	defer func() { _ = s3.Close() }()
	result, err := migrateMedia(ctx, store, root, s3, *to, *dry, *remove, *concurrency)
	_ = json.NewEncoder(out).Encode(result)
	if err != nil || result.Failed > 0 {
		_, _ = fmt.Fprintln(diagnostic, "Media migration incomplete; sources retained for failed files")
		return 1
	}
	return 0
}

func migrateMedia(ctx context.Context, store *MessageStore, root *os.Root, s3 *s3MediaStorage, to string, dry, remove bool, concurrency int) (mediaMigrationResult, error) {
	if dry {
		ctx = context.WithValue(ctx, mediaReadOnlyKey{}, true)
	}
	result := mediaMigrationResult{DryRun: dry, SkipReasons: map[string]int{}}
	local := localMediaStorage{root: root}
	var mu sync.Mutex
	seen := map[string]bool{}
	mapped := map[string]bool{}
	jobs := make(chan mediaRow, concurrency)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for row := range jobs {
				files, size, saved, reason, err := migrateMediaRow(ctx, store, local, s3, row, to, dry, remove, seen, &mu)
				mu.Lock()
				result.Files += files
				result.Bytes += size
				result.DedupeSavedBytes += saved
				if err != nil {
					result.Failed++
				}
				if reason != "" {
					result.Skipped++
					result.SkipReasons[reason]++
				}
				mu.Unlock()
			}
		}()
	}
	_, err := store.EachMediaRowMatchingContext(ctx, "", time.Time{}, purgeCursor{}, "", chatPolicy{}, math.MaxInt, func(row mediaRow) bool {
		if to == "s3" {
			if entry, lookupErr := local.Lookup(ctx, row); lookupErr == nil && entry != nil {
				mapped[entry.Path] = true
			}
		}
		select {
		case jobs <- row:
			return true
		case <-ctx.Done():
			return false
		}
	})
	close(jobs)
	workers.Wait()
	if to == "s3" && err == nil {
		// Report every remaining leaf inside a chat directory, including legacy
		// files with no message row. WalkDir never follows a symlink.
		err = fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry == nil {
				result.Skipped++
				result.SkipReasons["unreadable"]++
				return nil
			}
			parts := strings.Split(rel, "/")
			if len(parts) == 1 {
				if rel != "." && (!isChatDir(entry) || entry.Type()&os.ModeSymlink != 0) {
					if entry.Type()&os.ModeSymlink != 0 && strings.Contains(rel, "@") {
						result.Skipped++
						result.SkipReasons["unsafe_symlink"]++
					}
					if entry.IsDir() {
						return fs.SkipDir
					}
				}
				return nil
			}
			if entry.IsDir() {
				result.Skipped++
				result.SkipReasons["nested_directory"]++
				return fs.SkipDir
			}
			if mapped[rel] {
				return nil
			}
			reason := "unmapped_file"
			if walkErr != nil {
				reason = "unreadable"
			} else if entry.Type()&os.ModeSymlink != 0 {
				reason = "unsafe_symlink"
			}
			result.Skipped++
			result.SkipReasons[reason]++
			return nil
		})
	}
	return result, err
}

func migrateMediaRow(ctx context.Context, store *MessageStore, local localMediaStorage, s3 *s3MediaStorage, row mediaRow, to string, dry, remove bool, seen map[string]bool, mu *sync.Mutex) (int, int64, int64, string, error) {
	source := mediaStorage(local)
	destination := mediaStorage(s3)
	if to == "local" {
		source = s3
		destination = local
	}
	entry, err := source.Lookup(ctx, row)
	if err != nil {
		if to == "local" {
			return 0, 0, 0, "", err
		}
		return 0, 0, 0, "unsafe_or_unreadable", nil
	}
	if entry == nil {
		return 0, 0, 0, "not_cached", nil
	}
	reader, size, err := source.Open(ctx, row, entry.Name)
	if err != nil {
		return 0, 0, 0, "", err
	}
	defer func() { _ = reader.Close() }()
	h := sha256.New()
	var spool *os.File
	if !dry {
		spool, err = os.CreateTemp("", "wamcp-migrate-*")
		if err != nil {
			return 0, 0, 0, "", err
		}
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
		_, err = io.Copy(io.MultiWriter(spool, h), reader)
	} else {
		_, err = io.Copy(h, reader)
	}
	if err != nil {
		return 0, 0, 0, "", err
	}
	hash := h.Sum(nil)
	var expected []byte
	if err = store.db.QueryRowContext(ctx, `SELECT file_sha256 FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID).Scan(&expected); err != nil {
		return 0, 0, 0, "", err
	}
	if len(expected) > 0 && !bytes.Equal(expected, hash) {
		return 0, 0, 0, "", errMediaHash
	}
	if existing, err := destination.Lookup(ctx, row); err != nil {
		return 0, 0, 0, "", err
	} else if existing != nil {
		verified, storedSize, err := destination.Open(ctx, row, existing.Name)
		if err != nil {
			return 0, 0, 0, "", err
		}
		check := sha256.New()
		n, err := io.Copy(check, verified)
		_ = verified.Close()
		if err != nil || storedSize != size || n != size || !bytes.Equal(check.Sum(nil), hash) {
			return 0, 0, 0, "", errMediaHash
		}
		if remove && !dry {
			items, err := source.Delete(ctx, []mediaRow{row}, false)
			if err != nil || len(items) != 1 || !items[0].Purged {
				return 0, 0, 0, "", errors.New("verified source removal failed")
			}
		}
		return 0, 0, 0, "already_migrated", nil
	}
	mu.Lock()
	duplicate := seen[hex.EncodeToString(hash)]
	seen[hex.EncodeToString(hash)] = true
	mu.Unlock()
	saved := int64(0)
	if to == "s3" && duplicate {
		saved = size
	}
	if dry {
		return 1, size, saved, "", nil
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return 0, 0, 0, "", err
	}
	if to == "s3" {
		_, err = s3.put(ctx, row, spool, size)
	} else {
		if err := local.root.MkdirAll(chatMediaRel(row.ChatJID), storeDirMode); err != nil {
			return 0, 0, 0, "", err
		}
		_, err = local.Write(ctx, row, func(rel string) (int64, error) {
			return writeMediaFile(local.root, rel, func(f *os.File) error { _, err := io.Copy(f, spool); return err })
		})
	}
	if err != nil {
		return 0, 0, 0, "", err
	}
	verified, remoteSize, err := destination.Open(ctx, row, "")
	if err != nil {
		return 0, 0, 0, "", err
	}
	h.Reset()
	n, readErr := io.Copy(h, verified)
	_ = verified.Close()
	if readErr != nil || remoteSize != size || n != size || !bytes.Equal(h.Sum(nil), hash) {
		return 0, 0, 0, "", errMediaHash
	}
	if remove {
		items, err := source.Delete(ctx, []mediaRow{row}, false)
		if err != nil || len(items) != 1 || !items[0].Purged {
			return 0, 0, 0, "", errors.New("verified source removal failed")
		}
	}
	return 1, size, saved, "", nil
}
