package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var errMediaS3 = errors.New("S3 media storage unavailable")
var errMediaHash = errors.New("media plaintext SHA256 mismatch")
var errMediaSnapshot = errors.New("media snapshot changed during transfer")

// Startup holds the instance lock before this cleanup. All plaintext spools
// live in that instance's root, so another bridge's active files are untouched.
func cleanMediaSpools(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".media-stage-") || strings.HasPrefix(e.Name(), ".media-stream-") {
			if err := root.Remove(e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *s3MediaStorage) spool() (*os.File, error) {
	return s.root.OpenFile(".media-stage-"+rand.Text(), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}

type s3MediaStorage struct {
	client          *minio.Client
	store           *MessageStore
	root            *os.Root
	cfg             mediaBackendConfig
	mu              sync.Mutex
	locks           map[string]*s3ObjectLock
	transfers       chan struct{}
	bridge          *Bridge
	deletionJournal bool
}

func newS3MediaStorage(ctx context.Context, cfg mediaBackendConfig, store *MessageStore, root *os.Root) (*s3MediaStorage, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errMediaS3
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	transport, err := minio.DefaultTransport(u.Scheme == "https")
	if err != nil {
		return nil, errMediaS3
	}
	// Signed requests and media never reach an inherited HTTP proxy.
	transport.Proxy = nil
	client, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: u.Scheme == "https", Region: cfg.Region, BucketLookup: lookup, MaxRetries: 2, Transport: transport})
	if err != nil {
		return nil, errMediaS3
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ok, err := client.BucketExists(probe, cfg.Bucket)
	if err != nil || !ok {
		return nil, errMediaS3
	}
	if readonly, _ := ctx.Value(mediaReadOnlyKey{}).(bool); !readonly {
		if err := cleanMediaSpools(root); err != nil {
			return nil, errMediaS3
		}
	}
	// A dry-run can open an older archive without upgrading its schema.
	var journal int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='media_cache_deletions'`).Scan(&journal); err != nil {
		return nil, errMediaS3
	}
	return &s3MediaStorage{client: client, cfg: cfg, store: store, root: root, locks: make(map[string]*s3ObjectLock), transfers: make(chan struct{}, 4), deletionJournal: journal != 0}, nil
}

func (*s3MediaStorage) Backend() string { return "s3" }

type s3ObjectLock struct {
	gate  chan struct{}
	users int
}

// Only operations on the same content hash serialize. The mutex never spans
// network I/O; idle entries disappear instead of growing with archive size.
func (s *s3MediaStorage) lockHash(ctx context.Context, hash []byte) (func(), error) {
	key := hex.EncodeToString(hash)
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &s3ObjectLock{gate: make(chan struct{}, 1)}
		s.locks[key] = l
	}
	l.users++
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
	select {
	case l.gate <- struct{}{}:
		return func() { <-l.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func (s *s3MediaStorage) transfer(ctx context.Context) (func(), error) {
	select {
	case s.transfers <- struct{}{}:
		return func() { <-s.transfers }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *s3MediaStorage) key(hash []byte) (string, error) {
	if len(hash) != sha256.Size {
		return "", errMediaHash
	}
	h := hex.EncodeToString(hash)
	return s.cfg.Prefix + "blobs/sha256/" + h[:2] + "/" + h, nil
}

func (s *s3MediaStorage) reference(ctx context.Context, row mediaRow) ([]byte, int64, error) {
	var hash []byte
	var size int64
	err := s.store.db.QueryRowContext(ctx, `SELECT c.sha256, c.bytes FROM media_cache c JOIN media_cache_refs r ON r.sha256=c.sha256 JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid WHERE r.id=? AND r.chat_jid=? AND c.backend='s3' AND c.sha256=m.file_sha256`, row.ID, row.ChatJID).Scan(&hash, &size)
	return hash, size, err
}

func (s *s3MediaStorage) Lookup(ctx context.Context, row mediaRow) (*mediaCacheEntry, error) {
	if err := checkMediaPathComponents(chatMediaRel(row.ChatJID), mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)); err != nil {
		return nil, err
	}
	_, size, err := s.reconciledReference(ctx, row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errMediaS3
	}
	return &mediaCacheEntry{Name: mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename), Path: mediaBlobURI(row.ChatJID, row.ID), Bytes: size}, nil
}

func mediaBlobURI(chat, id string) string {
	return "whatsapp://media/" + url.PathEscape(chat) + "/" + url.PathEscape(id)
}

func (s *s3MediaStorage) Open(ctx context.Context, row mediaRow, _ string) (io.ReadCloser, int64, error) {
	hash, size, err := s.reconciledReference(ctx, row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	unlock, err := s.lockHash(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	if _, err := s.reconcileDeletionLocked(ctx, hash); err != nil {
		return nil, 0, err
	}
	current, _, err := s.reference(ctx, row)
	if err != nil || !bytes.Equal(current, hash) {
		return nil, 0, errMediaSnapshot
	}
	release, err := s.transfer(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	key, err := s.key(hash)
	if err != nil {
		return nil, 0, err
	}
	object, err := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, errMediaS3
	}
	defer func() { _ = object.Close() }()
	// Verify before releasing any bytes to a caller. A private disk spool keeps
	// memory bounded, and is removed when the reader closes, including failures.
	f, err := s.spool()
	if err != nil {
		return nil, 0, errMediaS3
	}
	reader := &temporaryMediaReader{File: f, root: s.root, rel: filepath.Base(f.Name())}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(object, size+1))
	if err != nil || n != size || !bytes.Equal(h.Sum(nil), hash) {
		_ = reader.Close()
		return nil, 0, errMediaHash
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = reader.Close()
		return nil, 0, errMediaS3
	}
	if readonly, _ := ctx.Value(mediaReadOnlyKey{}).(bool); !readonly {
		if _, err := s.store.db.ExecContext(ctx, `UPDATE media_cache SET last_access_at=? WHERE sha256=?`, dbTime(time.Now()), hash); err != nil {
			_ = reader.Close()
			return nil, 0, errMediaS3
		}
	}
	reader.hash = hash
	return reader, size, nil
}

func (s *s3MediaStorage) OpenRange(ctx context.Context, row mediaRow, offset, length int64) (*mediaByteRange, error) {
	return openMediaRange(ctx, s, row, offset, length)
}

type temporaryMediaReader struct {
	*os.File
	root *os.Root
	rel  string
	hash []byte // Catalog hash, verified against the complete object before Open returns.
}

func (f *temporaryMediaReader) MediaSHA256() []byte { return f.hash }

func (f *temporaryMediaReader) Close() error {
	err := f.File.Close()
	var remove error
	if f.root != nil {
		remove = f.root.Remove(f.rel)
	} else {
		remove = os.Remove(f.Name())
	}
	if err != nil {
		return err
	}
	return remove
}

func (s *s3MediaStorage) Write(ctx context.Context, row mediaRow, fill func(string) (int64, error)) (int64, error) {
	name := mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)
	if err := checkMediaPathComponents(chatMediaRel(row.ChatJID), name); err != nil {
		return 0, err
	}
	// Download into a private, instance-owned name, never the local cache path.
	// An incomplete backend migration must leave its existing source intact.
	f, err := s.spool()
	if err != nil {
		return 0, err
	}
	rel := filepath.Base(f.Name())
	_ = f.Close()
	defer func() { _ = s.root.Remove(rel); _ = s.root.Remove(rel + ".part") }()
	n, err := fill(rel)
	if err != nil {
		return 0, err
	}
	stage := transientMediaStorage{localMediaStorage: localMediaStorage{root: s.root}, rel: rel}
	reader, actual, err := stage.Open(ctx, row, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = reader.Close() }()
	if actual != n {
		return 0, errMediaHash
	}
	return s.put(ctx, row, reader, n)
}

func (s *s3MediaStorage) put(ctx context.Context, row mediaRow, reader io.Reader, size int64) (stored int64, putErr error) {
	if size < 0 {
		return 0, errMediaHash
	}
	// Copy a confined source to a private spool, hashing before any upload.
	f, err := s.spool()
	if err != nil {
		return 0, errMediaS3
	}
	tmp := &temporaryMediaReader{File: f, root: s.root, rel: filepath.Base(f.Name())}
	defer func() { _ = tmp.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), reader)
	if err != nil || n != size {
		return 0, errMediaHash
	}
	hash := h.Sum(nil)
	var expected []byte
	if err := s.store.db.QueryRowContext(ctx, `SELECT file_sha256 FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID).Scan(&expected); err != nil {
		return 0, errMediaS3
	}
	if len(expected) > 0 && !bytes.Equal(expected, hash) {
		if s.bridge != nil {
			s.bridge.Log.Warnf("Media plaintext SHA256 mismatch; cache publication refused")
		}
		return 0, errMediaHash
	}
	reserved, _ := ctx.Value(s3QuotaReservedKey{}).(bool)
	var unlock func()
	for {
		if reserved && mediaLimit(ctx) > 0 && uint64(size) > mediaLimit(ctx) {
			return 0, errMediaQuota
		}
		unlock, err = s.lockHash(ctx, hash)
		if err != nil {
			return 0, err
		}
		if s.bridge == nil || reserved {
			break
		}
		var exists int
		if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache WHERE sha256=? AND backend='s3'`, hash).Scan(&exists); err != nil {
			unlock()
			return 0, errMediaS3
		}
		if exists > 0 {
			break
		}
		// Dedupe is authoritative only under the content lock. Release it
		// before admission, which may evict objects and take content locks.
		unlock()
		bounded, release, err := s.bridge.acquireMediaQuota(ctx, uint64(size))
		if err != nil {
			return 0, err
		}
		defer release()
		ctx, reserved = bounded, true
	}
	defer unlock()
	release, err := s.transfer(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	key, err := s.key(hash)
	if err != nil {
		return 0, err
	}
	uploaded := false
	defer func() {
		if putErr != nil && uploaded {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if err := s.client.RemoveObject(cleanup, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil && s.bridge != nil {
				s.bridge.Log.Warnf("Unreferenced S3 upload cleanup failed")
			}
		}
	}()
	stat, statErr := s.client.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if statErr != nil {
		code := minio.ToErrorResponse(statErr).Code
		if code != "NoSuchKey" && code != "NoSuchObject" && code != "NotFound" {
			return 0, errMediaS3
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, errMediaS3
		}
		uploaded = true
		if _, err := s.client.PutObject(ctx, s.cfg.Bucket, key, f, size, minio.PutObjectOptions{ContentType: "application/octet-stream", DisableMultipart: true}); err != nil {
			return 0, errMediaS3
		}
	} else if stat.Size != size {
		return 0, errMediaHash
	}
	// ETags are not plaintext hashes. Verify the complete remote object even
	// when resuming an upload or adopting an already existing deduped object.
	object, err := s.client.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return 0, errMediaS3
	}
	h.Reset()
	n, err = io.Copy(h, io.LimitReader(object, size+1))
	_ = object.Close()
	if err != nil || n != size || !bytes.Equal(h.Sum(nil), hash) {
		return 0, errMediaHash
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, errMediaS3
	}
	defer func() { _ = tx.Rollback() }()
	// Revalidate the source snapshot inside the writer transaction. A later
	// history replay must never inherit this transfer's bytes or missing hash.
	var current []byte
	if err := tx.QueryRowContext(ctx, `SELECT file_sha256 FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID).Scan(&current); err != nil {
		return 0, errMediaS3
	}
	if !bytes.Equal(current, expected) {
		return 0, errMediaSnapshot
	}
	kind := row.MediaType
	if row.ChatJID == "status@broadcast" {
		kind = "status"
	}
	now := dbTime(time.Now())
	if _, err = tx.ExecContext(ctx, `INSERT INTO media_cache(sha256,bytes,media_type,backend,stored_at,last_access_at) VALUES(?,?,?,'s3',?,?) ON CONFLICT(sha256) DO UPDATE SET backend='s3',bytes=excluded.bytes,last_access_at=excluded.last_access_at`, hash, size, kind, now, now); err != nil {
		return 0, errMediaS3
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO media_cache_refs(id,chat_jid,sha256) VALUES(?,?,?) ON CONFLICT(id,chat_jid) DO UPDATE SET sha256=excluded.sha256`, row.ID, row.ChatJID, hash); err != nil {
		return 0, errMediaS3
	}
	if s.deletionJournal {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_cache_deletions WHERE sha256=?`, hash); err != nil {
			return 0, errMediaS3
		}
	}
	if len(expected) == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET file_sha256=? WHERE id=? AND chat_jid=?`, hash, row.ID, row.ChatJID); err != nil {
			return 0, errMediaS3
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, errMediaS3
	}
	return size, nil
}
