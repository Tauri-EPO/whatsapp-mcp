package main

// Cached media belongs to the bridge. Backends receive a stored message
// identity, never an arbitrary path supplied by a REST caller.
import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"time"
)

type mediaCacheEntry struct {
	Name, Path string
	Bytes      int64
}

type mediaCacheUsage struct {
	Bytes, StatusBytes int64
	Files, StatusFiles int
}

// Write owns publication; fill keeps the existing CDN download/retry flow.
// Delete takes a batch so a content-addressed backend can account for the last
// reference in both dry runs and actual purges.
type mediaStorage interface {
	Backend() string
	Lookup(context.Context, mediaRow) (*mediaCacheEntry, error)
	Open(context.Context, mediaRow, string) (io.ReadCloser, int64, error)
	Write(context.Context, mediaRow, func(string) (int64, error)) (int64, error)
	Delete(context.Context, []mediaRow, bool) ([]PurgeResult, error)
	Usage(context.Context) (mediaCacheUsage, error)
	Sweep(context.Context, time.Duration, *time.Duration, time.Time) (int, int64, int)
}

type localMediaStorage struct {
	root   *os.Root
	finder *cachedMediaFinder
}

func (b *Bridge) mediaStorage() mediaStorage {
	if b.MediaStorage != nil {
		return b.MediaStorage
	}
	return localMediaStorage{root: b.StoreRoot}
}

func (localMediaStorage) Backend() string { return "local" }

func (s localMediaStorage) Lookup(ctx context.Context, row mediaRow) (*mediaCacheEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	found, err := findCachedMedia(s.root, chatMediaRel(row.ChatJID), mediaFileNames(row.MediaType, row.Timestamp, row.ID, row.Filename))
	if found == nil {
		return nil, err
	}
	defer found.Close()
	return &mediaCacheEntry{Name: found.name, Path: path.Join(chatMediaRel(row.ChatJID), found.name), Bytes: found.info.Size()}, nil
}

func (s localMediaStorage) Open(ctx context.Context, row mediaRow, name string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if name == "" {
		entry, err := s.Lookup(ctx, row)
		if err != nil {
			return nil, 0, err
		}
		if entry == nil {
			return nil, 0, os.ErrNotExist
		}
		name = entry.Name
	}
	return openStoreMedia(s.root, chatMediaRel(row.ChatJID), name)
}

func (s localMediaStorage) Write(ctx context.Context, row mediaRow, fill func(string) (int64, error)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.root == nil {
		return 0, errors.New("store directory unavailable")
	}
	chat := chatMediaRel(row.ChatJID)
	name := mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)
	if err := checkMediaPathComponents(chat, name); err != nil {
		return 0, err
	}
	if _, err := requireChatMediaDir(s.root, chat); err != nil {
		return 0, err
	}
	return fill(path.Join(chat, name))
}

func (s localMediaStorage) Delete(ctx context.Context, rows []mediaRow, dry bool) ([]PurgeResult, error) {
	finder := s.finder
	if finder == nil {
		finder = &cachedMediaFinder{root: s.root}
		defer finder.Close()
	}
	results := make([]PurgeResult, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		results = append(results, purgeOneUsing(s.root, row, dry, finder))
	}
	return results, nil
}

func (s localMediaStorage) Usage(ctx context.Context) (mediaCacheUsage, error) {
	var usage mediaCacheUsage
	err := eachCachedMediaContext(ctx, s.root, func(chat string, file *cachedMedia) {
		usage.Bytes += file.info.Size()
		usage.Files++
		if chat == "status@broadcast" {
			usage.StatusBytes += file.info.Size()
			usage.StatusFiles++
		}
	})
	return usage, err
}

func (s localMediaStorage) Sweep(_ context.Context, age time.Duration, statusAge *time.Duration, now time.Time) (int, int64, int) {
	return sweepMediaWithStatus(s.root, age, statusAge, now)
}
