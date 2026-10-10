package main

import (
	"context"
	"database/sql"

	"github.com/minio/minio-go/v7"
)

func missingS3Object(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NoSuchObject" || code == "NotFound"
}

// The caller holds this hash's lock. A durable marker precedes remote DELETE,
// so cancellation or a failed SQL commit can be reconciled after a restart.
// Never scan a bucket or hold a database transaction during the HEAD request.
func (s *s3MediaStorage) reconcileDeletionLocked(ctx context.Context, hash []byte) (bool, error) {
	if !s.deletionJournal {
		return false, nil
	}
	var pending int
	if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_deletions WHERE sha256=?`, hash).Scan(&pending); err != nil {
		return false, errMediaS3
	}
	if pending == 0 {
		return false, nil
	}
	key, err := s.key(hash)
	if err != nil {
		return false, err
	}
	release, err := s.transfer(ctx)
	if err != nil {
		return false, err
	}
	_, statErr := s.client.StatObject(ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	release()
	missing := missingS3Object(statErr)
	if statErr != nil && !missing {
		return false, errMediaS3
	}
	if readonly, _ := ctx.Value(mediaReadOnlyKey{}).(bool); readonly {
		return missing, nil
	}
	if missing {
		// One atomic SQL statement cascades both refs and the journal row. If it
		// fails, the marker remains and a later reader/sweep retries recovery.
		_, err = s.store.db.ExecContext(ctx, `DELETE FROM media_cache WHERE sha256=? AND backend='s3'`, hash)
	} else {
		_, err = s.store.db.ExecContext(ctx, `DELETE FROM media_cache_deletions WHERE sha256=?`, hash)
	}
	if err != nil {
		return false, errMediaS3
	}
	return missing, nil
}

func (s *s3MediaStorage) reconciledReference(ctx context.Context, row mediaRow) ([]byte, int64, error) {
	hash, size, err := s.reference(ctx, row)
	if err != nil || !s.deletionJournal {
		return hash, size, err
	}
	var pending int
	if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_deletions WHERE sha256=?`, hash).Scan(&pending); err != nil {
		return nil, 0, errMediaS3
	}
	if pending == 0 {
		return hash, size, nil
	}
	unlock, err := s.lockHash(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	missing, err := s.reconcileDeletionLocked(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	if missing {
		return nil, 0, sql.ErrNoRows
	}
	return s.reference(ctx, row)
}

func (s *s3MediaStorage) reconcilePendingDeletions(ctx context.Context) (int64, int) {
	if !s.deletionJournal {
		return 0, 0
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT d.sha256,c.bytes FROM media_cache_deletions d JOIN media_cache c ON c.sha256=d.sha256 LIMIT 256`)
	if err != nil {
		return 0, 1
	}
	type pending struct {
		hash  []byte
		bytes int64
	}
	var objects []pending
	for rows.Next() {
		var item pending
		if err = rows.Scan(&item.hash, &item.bytes); err != nil {
			break
		}
		objects = append(objects, item)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return 0, 1
	}
	var freed int64
	failed := 0
	for _, item := range objects {
		unlock, err := s.lockHash(ctx, item.hash)
		if err != nil {
			failed++
			continue
		}
		missing, err := s.reconcileDeletionLocked(ctx, item.hash)
		unlock()
		if err != nil {
			failed++
			continue
		}
		if missing {
			freed += item.bytes
		}
	}
	return freed, failed
}
