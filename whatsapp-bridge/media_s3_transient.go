package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

type verifiedTransientStorage struct {
	localMediaStorage
	reader *s3VerifiedReader
}

func (s verifiedTransientStorage) Open(ctx context.Context, _ mediaRow, _ string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return s.reader, s.reader.entry.size, nil
}
func (s verifiedTransientStorage) OpenRange(ctx context.Context, row mediaRow, offset, length int64) (*mediaByteRange, error) {
	return openMediaRange(ctx, s, row, offset, length)
}

func (s *s3MediaStorage) transientIdentity(ctx context.Context, row mediaRow) (string, []byte, []byte, int64, error) {
	var plain, enc, key []byte
	var source, direct, kind, name string
	var length sql.NullInt64
	var at time.Time
	err := s.store.db.QueryRowContext(ctx, `SELECT file_sha256,file_enc_sha256,media_key,COALESCE(url,''),COALESCE(direct_path,''),file_length,COALESCE(media_type,''),COALESCE(filename,''),timestamp FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID).Scan(&plain, &enc, &key, &source, &direct, &length, &kind, &name, &at)
	if err != nil {
		return "", nil, nil, 0, errMediaS3
	}
	encoded, err := json.Marshal([]any{row.ID, row.ChatJID, plain, enc, key, source, direct, length, kind, name, dbTime(at)})
	if err != nil {
		return "", nil, nil, 0, errMediaS3
	}
	snapshot := sha256.Sum256(encoded)
	identity := "transient:" + hex.EncodeToString(snapshot[:])
	if len(plain) == sha256.Size {
		identity = hex.EncodeToString(plain)
	}
	return identity, plain, snapshot[:], length.Int64, nil
}

// A cold quota fallback uses the same bounded pool as remote GETs. It never
// publishes a persistent cache reference, and unknown hashes stay snapshot-scoped.
func (s *s3MediaStorage) transient(ctx context.Context, row mediaRow, limit int64, load bool) (mediaStorage, error) {
	key, expected, snapshot, declared, err := s.transientIdentity(ctx, row)
	if err != nil {
		return nil, err
	}
	lockKey := expected
	if len(lockKey) != sha256.Size {
		lockKey = snapshot
	}
	unlock, err := s.lockHash(ctx, lockKey)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if limit <= 0 {
		limit = 512*1024*1024 - 26
	}
	if declared > limit {
		return nil, errAutoMediaLimit
	}
	budget := min(limit, max(int64(128*1024*1024-26), declared))
	reader, reused, err := s.spools.acquireKey(key, expected, budget+26, true, !load)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	keep := false
	defer func() {
		if !keep {
			_ = reader.Close()
		}
	}()
	if reused {
		reader.hash = append([]byte(nil), reader.entry.digest...)
		if reader.entry.size > limit {
			return nil, errAutoMediaLimit
		}
		keep = true
		return verifiedTransientStorage{reader: reader}, nil
	}
	// Leave the reserved entry charged while the SDK writes its confined .part.
	s.spools.mu.Lock()
	if s.spools.closed {
		s.spools.mu.Unlock()
		return nil, errMediaS3
	}
	_ = reader.File.Close()
	reader.File = nil
	reserved := reader.entry.size - 26
	rel := reader.entry.rel
	s.spools.mu.Unlock()
	if reserved <= 0 {
		return nil, errMediaSpoolFull
	}
	_ = s.root.Remove(rel)
	defer func() { _ = s.root.Remove(rel + ".part") }()
	ctx = withMediaLimit(context.WithValue(ctx, transientMediaKey{}, rel), uint64(reserved))
	if _, _, _, _, err := s.bridge.downloadMediaAttempt(ctx, row.ID, row.ChatJID); err != nil {
		return nil, err
	}
	f, err := s.root.Open(rel)
	if err != nil {
		return nil, errMediaS3
	}
	s.spools.mu.Lock()
	if s.spools.closed {
		s.spools.mu.Unlock()
		_ = f.Close()
		return nil, errMediaS3
	}
	reader.File = f
	s.spools.mu.Unlock()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, reserved+1))
	if err != nil || n > reserved {
		return nil, errAutoMediaLimit
	}
	digest := h.Sum(nil)
	if len(expected) > 0 && !bytes.Equal(expected, digest) {
		s.bridge.Log.Warnf("Media plaintext SHA256 mismatch; transient read refused")
		return nil, errMediaHash
	}
	_, _, current, _, err := s.transientIdentity(ctx, row)
	if err != nil || !bytes.Equal(current, snapshot) {
		return nil, errMediaSnapshot
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, errMediaS3
	}
	s.spools.mu.Lock()
	if s.spools.closed {
		s.spools.mu.Unlock()
		return nil, errMediaS3
	}
	s.spools.bytes -= reader.entry.size - n
	reader.entry.size = n
	reader.entry.ready = true
	reader.entry.digest = append([]byte(nil), digest...)
	reader.hash = digest
	s.spools.mu.Unlock()
	keep = true
	return verifiedTransientStorage{reader: reader}, nil
}
