package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"time"
)

var errMediaSpoolFull = errors.New("verified media spool capacity exhausted")

// Both downloading and completed files remain charged, including while a slow
// client holds a reader. An idle verified object can serve subsequent ranges.
type s3VerifiedSpools struct {
	mu                              sync.Mutex
	root                            *os.Root
	entries                         map[string]*s3VerifiedSpool
	bytes, maxBytes                 int64
	readers, maxReaders, maxEntries int
	idleTTL                         time.Duration
	closed                          bool
}

type s3VerifiedSpool struct {
	key, rel  string
	size      int64
	ready     bool
	digest    []byte
	transient bool
	readers   map[*s3VerifiedReader]bool
	timer     *time.Timer
	idleAt    time.Time
}

type s3VerifiedReader struct {
	*os.File
	cache  *s3VerifiedSpools
	entry  *s3VerifiedSpool
	hash   []byte
	closed bool // Guarded by cache.mu, including lifecycle shutdown.
}

func newS3VerifiedSpools(root *os.Root) *s3VerifiedSpools {
	return &s3VerifiedSpools{root: root, entries: map[string]*s3VerifiedSpool{}, maxBytes: 512 * 1024 * 1024, maxReaders: 4, maxEntries: 4, idleTTL: 2 * time.Minute}
}

func (c *s3VerifiedSpools) removeLocked(entry *s3VerifiedSpool) error {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if err := c.root.Remove(entry.rel + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := c.root.Remove(entry.rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(c.entries, entry.key)
	c.bytes -= entry.size
	return nil
}

// Called under the storage content lock, so a hash has at most one loader.
func (c *s3VerifiedSpools) acquire(hash []byte, size int64) (*s3VerifiedReader, bool, error) {
	return c.acquireKey(hex.EncodeToString(hash), hash, size, false, false)
}

func (c *s3VerifiedSpools) acquireKey(key string, hash []byte, size int64, flexible, onlyExisting bool) (*s3VerifiedReader, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, os.ErrClosed
	}
	if size < 0 || (!flexible && size > c.maxBytes) || c.readers >= c.maxReaders {
		return nil, false, errMediaSpoolFull
	}
	entry := c.entries[key]
	if (entry == nil || !entry.transient) && onlyExisting {
		return nil, false, nil
	}
	if flexible {
		size = min(size, c.maxBytes)
	}
	if entry == nil && flexible && size <= 26 {
		return nil, false, errMediaSpoolFull
	}
	if entry != nil && (!entry.ready || (!flexible && entry.size != size)) {
		return nil, false, errMediaSpoolFull
	}
	reused := entry != nil
	if entry == nil {
		for len(c.entries) >= c.maxEntries || size > c.maxBytes-c.bytes {
			var oldest *s3VerifiedSpool
			for _, candidate := range c.entries {
				if len(candidate.readers) == 0 && (oldest == nil || candidate.idleAt.Before(oldest.idleAt)) {
					oldest = candidate
				}
			}
			if oldest == nil {
				if flexible && len(c.entries) < c.maxEntries && c.maxBytes-c.bytes > 26 {
					size = c.maxBytes - c.bytes
					break
				}
				return nil, false, errMediaSpoolFull
			}
			if err := c.removeLocked(oldest); err != nil {
				return nil, false, errMediaS3
			}
		}
		entry = &s3VerifiedSpool{key: key, rel: ".media-verified-" + rand.Text(), size: size, transient: flexible, readers: map[*s3VerifiedReader]bool{}}
		f, err := c.root.OpenFile(entry.rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, false, errMediaS3
		}
		c.entries[key] = entry
		c.bytes += size
		reader := &s3VerifiedReader{File: f, cache: c, entry: entry, hash: append([]byte(nil), hash...)}
		entry.readers[reader] = true
		c.readers++
		return reader, false, nil
	}
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.transient = flexible
	f, err := c.root.Open(entry.rel)
	if err != nil {
		entry.ready = false
		_ = c.removeLocked(entry)
		return nil, false, errMediaS3
	}
	reader := &s3VerifiedReader{File: f, cache: c, entry: entry, hash: append([]byte(nil), hash...)}
	entry.readers[reader] = true
	c.readers++
	return reader, reused, nil
}

func (c *s3VerifiedSpools) verified(reader *s3VerifiedReader) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || reader.closed {
		return os.ErrClosed
	}
	reader.entry.ready = true
	reader.entry.digest = append([]byte(nil), reader.hash...)
	return nil
}

func (c *s3VerifiedSpools) discard(hash []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[hex.EncodeToString(hash)]; entry != nil {
		entry.ready = false
		if len(entry.readers) == 0 {
			_ = c.removeLocked(entry)
		}
	}
}

func (reader *s3VerifiedReader) MediaSHA256() []byte { return reader.hash }

func (reader *s3VerifiedReader) Close() error {
	c := reader.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if reader.closed {
		return nil
	}
	reader.closed = true
	var err error
	if reader.File != nil {
		err = reader.File.Close()
	}
	entry := reader.entry
	delete(entry.readers, reader)
	c.readers--
	if len(entry.readers) == 0 {
		if !entry.ready || c.closed {
			return errors.Join(err, c.removeLocked(entry))
		}
		entry.idleAt = time.Now()
		entry.timer = time.AfterFunc(c.idleTTL, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.entries[entry.key] == entry && len(entry.readers) == 0 && time.Since(entry.idleAt) >= c.idleTTL {
				_ = c.removeLocked(entry)
			}
		})
	}
	return err
}

func (c *s3VerifiedSpools) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	var result error
	for _, entry := range c.entries {
		for reader := range entry.readers {
			reader.closed = true
			if reader.File != nil {
				result = errors.Join(result, reader.File.Close())
			}
			delete(entry.readers, reader)
			c.readers--
		}
		result = errors.Join(result, c.removeLocked(entry))
	}
	return result
}
