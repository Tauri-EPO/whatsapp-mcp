package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/minio/minio-go/v7"
)

func (s *s3MediaStorage) Delete(ctx context.Context, rows []mediaRow, dry bool) (results []PurgeResult, resultErr error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	type object struct {
		hash     []byte
		bytes    int64
		selected int
		last     int
		remove   bool
	}
	objects := map[string]*object{}
	results = make([]PurgeResult, len(rows))
	committed := false
	defer func() {
		if resultErr != nil && !committed {
			for i := range results {
				if results[i].Purged {
					results[i].Purged = false
					results[i].Bytes = 0
					results[i].Reason = "S3 media removal incomplete"
				}
			}
		}
	}()
	seen := map[string]bool{}
	for i, row := range rows {
		results[i] = PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID, Reason: purgeReasonNotCached}
		identity := row.ChatJID + "\x00" + row.ID
		if seen[identity] {
			continue
		}
		seen[identity] = true
		var hash []byte
		var size int64
		err := s.store.db.QueryRowContext(ctx, `SELECT c.sha256,c.bytes FROM media_cache c JOIN media_cache_refs r ON r.sha256=c.sha256 WHERE r.id=? AND r.chat_jid=? AND c.backend='s3'`, row.ID, row.ChatJID).Scan(&hash, &size)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return results, errMediaS3
		}
		h := hex.EncodeToString(hash)
		if objects[h] == nil {
			objects[h] = &object{hash: hash, bytes: size}
		}
		objects[h].selected++
		objects[h].last = i
		results[i].Purged = true
		results[i].Reason = ""
	}
	// Determine freed bytes using the entire selected set, including references
	// across SQL pages; a shared object contributes only on its last reference.
	for _, obj := range objects {
		var count int
		if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_refs WHERE sha256=?`, obj.hash).Scan(&count); err != nil {
			return results, errMediaS3
		}
		if count == obj.selected {
			obj.remove = true
			if dry {
				results[obj.last].Bytes = obj.bytes
			}
		}
	}
	if dry {
		return results, nil
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return results, errMediaS3
	}
	defer func() { _ = tx.Rollback() }()
	for i, row := range rows {
		if !results[i].Purged {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_cache_refs WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
			return results, errMediaS3
		}
	}
	// Detach durably before remote deletion. Cancellation/crash can leave an
	// unreferenced object, but can never resurrect a reference to deleted bytes.
	if err := tx.Commit(); err != nil {
		return results, errMediaS3
	}
	committed = true
	for _, obj := range objects {
		if !obj.remove {
			continue
		}
		var count int
		if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_refs WHERE sha256=?`, obj.hash).Scan(&count); err != nil {
			return results, errMediaS3
		}
		if count != 0 {
			continue
		}
		key, err := s.key(obj.hash)
		if err != nil {
			return results, err
		}
		if err := s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
			results[obj.last].Purged = false
			results[obj.last].Bytes = 0
			results[obj.last].Reason = "S3 media removal failed"
			continue
		}
		results[obj.last].Bytes = obj.bytes
		if _, err := s.store.db.ExecContext(ctx, `DELETE FROM media_cache WHERE sha256=? AND NOT EXISTS(SELECT 1 FROM media_cache_refs WHERE sha256=?)`, obj.hash, obj.hash); err != nil {
			return results, errMediaS3
		}
	}
	return results, nil
}

func (s *s3MediaStorage) Usage(ctx context.Context) (mediaCacheUsage, error) {
	var usage mediaCacheUsage
	err := s.store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0),COUNT(*),COALESCE(SUM(CASE WHEN media_type='status' THEN bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN media_type='status' THEN 1 ELSE 0 END),0) FROM media_cache WHERE backend='s3'`).Scan(&usage.Bytes, &usage.Files, &usage.StatusBytes, &usage.StatusFiles)
	if err != nil {
		return usage, errMediaS3
	}
	return usage, nil
}

func (s *s3MediaStorage) Sweep(ctx context.Context, age time.Duration, statusAge *time.Duration, now time.Time) (int, int64, int) {
	rows, err := s.cachedRows(ctx)
	if err != nil {
		return 0, 0, 1
	}
	selected := []mediaRow{}
	for _, row := range rows {
		maxAge := age
		if row.ChatJID == "status@broadcast" && statusAge != nil {
			maxAge = *statusAge
		}
		if maxAge > 0 && row.Timestamp.Before(now.Add(-maxAge)) {
			selected = append(selected, row)
		}
	}
	results, err := s.Delete(ctx, selected, false)
	files, failed := 0, 0
	var freed int64
	for _, r := range results {
		if r.Purged {
			files++
			freed += r.Bytes
		} else if r.Reason != purgeReasonNotCached {
			failed++
		}
	}
	if err != nil {
		failed++
	}
	// Failed deletions or replaced message snapshots may leave an object with
	// no references. Keep its bytes charged until a later bounded sweep removes it.
	orphanBytes, orphanFailed := s.sweepUnreferenced(ctx)
	return files, freed + orphanBytes, failed + orphanFailed
}

func (s *s3MediaStorage) sweepUnreferenced(ctx context.Context) (int64, int) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return 0, 1
	}
	defer unlock()
	rows, err := s.store.db.QueryContext(ctx, `SELECT sha256,bytes FROM media_cache c WHERE backend='s3' AND NOT EXISTS(SELECT 1 FROM media_cache_refs r WHERE r.sha256=c.sha256) LIMIT 256`)
	if err != nil {
		return 0, 1
	}
	type orphan struct {
		hash  []byte
		bytes int64
	}
	var objects []orphan
	for rows.Next() {
		var o orphan
		if rows.Scan(&o.hash, &o.bytes) != nil {
			_ = rows.Close()
			return 0, 1
		}
		objects = append(objects, o)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, 1
	}
	var freed int64
	failed := 0
	for _, o := range objects {
		key, err := s.key(o.hash)
		if err == nil {
			err = s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{})
		}
		if err != nil {
			failed++
			continue
		}
		freed += o.bytes
		if _, err = s.store.db.ExecContext(ctx, `DELETE FROM media_cache WHERE sha256=? AND NOT EXISTS(SELECT 1 FROM media_cache_refs WHERE sha256=?)`, o.hash, o.hash); err != nil {
			failed++
		}
	}
	return freed, failed
}

func (s *s3MediaStorage) cachedRows(ctx context.Context) ([]mediaRow, error) {
	cursor, err := s.store.db.QueryContext(ctx, `SELECT m.id,m.chat_jid,m.media_type,m.timestamp,COALESCE(m.filename,'') FROM media_cache_refs r JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid JOIN media_cache c ON c.sha256=r.sha256 WHERE c.backend='s3' ORDER BY m.timestamp,m.chat_jid,m.id`)
	if err != nil {
		return nil, errMediaS3
	}
	defer func() { _ = cursor.Close() }()
	var rows []mediaRow
	for cursor.Next() {
		var row mediaRow
		if err := cursor.Scan(&row.ID, &row.ChatJID, &row.MediaType, &row.Timestamp, &row.Filename); err != nil {
			return nil, errMediaS3
		}
		rows = append(rows, row)
	}
	if cursor.Err() != nil {
		return nil, errMediaS3
	}
	return rows, nil
}

func (b *Bridge) purgeS3OperatorMedia(ctx context.Context, req operatorMediaPurge) (operatorMediaResult, error) {
	s, ok := b.mediaStorage().(*s3MediaStorage)
	if !ok {
		return operatorMediaResult{}, errors.New("S3 storage unavailable")
	}
	result := operatorMediaResult{DryRun: req.DryRun == nil || *req.DryRun}
	rows, err := s.cachedRows(ctx)
	if err != nil {
		return result, err
	}
	selected := []mediaRow{}
	for _, row := range rows {
		if req.Chat != "" && row.ChatJID != req.Chat {
			continue
		}
		kind := row.MediaType
		if row.ChatJID == "status@broadcast" {
			kind = "status"
		}
		if req.Type != "all" && req.Type != kind {
			continue
		}
		if req.OlderDays > 0 && !row.Timestamp.Before(time.Now().AddDate(0, 0, -req.OlderDays)) {
			continue
		}
		selected = append(selected, row)
	}
	items, err := s.Delete(ctx, selected, result.DryRun)
	for _, item := range items {
		if item.Purged {
			result.Files++
			result.FreedBytes += item.Bytes
		} else if item.Reason != purgeReasonNotCached {
			result.Failed++
		}
	}
	if err == nil {
		err = s.operatorOrphans(ctx, req, &result)
	}
	if !result.DryRun {
		b.storeStats.invalidate()
		b.Log.Infof("Operator media purge: type=%s age_days=%d chat_filter=%t files=%d freed_bytes=%d failed=%d", req.Type, req.OlderDays, req.Chat != "", result.Files, result.FreedBytes, result.Failed)
	}
	return result, err
}

func (s *s3MediaStorage) operatorOrphans(ctx context.Context, req operatorMediaPurge, result *operatorMediaResult) error {
	// Detached objects no longer have a chat identity. A chat-scoped request
	// cannot infer that identity, so it leaves them to an unscoped type cleanup.
	if req.Chat != "" {
		return nil
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	after := []byte{}
	cutoff := ""
	if req.OlderDays > 0 {
		cutoff = dbTime(time.Now().AddDate(0, 0, -req.OlderDays))
	}
	for {
		rows, err := s.store.db.QueryContext(ctx, `SELECT sha256,bytes FROM media_cache c WHERE backend='s3' AND sha256>? AND (?='all' OR media_type=?) AND (?='' OR stored_at<?) AND NOT EXISTS(SELECT 1 FROM media_cache_refs r WHERE r.sha256=c.sha256) ORDER BY sha256 LIMIT 256`, after, req.Type, req.Type, cutoff, cutoff)
		if err != nil {
			return errMediaS3
		}
		type orphan struct {
			hash  []byte
			bytes int64
		}
		var objects []orphan
		for rows.Next() {
			var o orphan
			if rows.Scan(&o.hash, &o.bytes) != nil {
				_ = rows.Close()
				return errMediaS3
			}
			objects = append(objects, o)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return errMediaS3
		}
		if len(objects) == 0 {
			return nil
		}
		for _, o := range objects {
			after = o.hash
			result.OrphanFiles++
			result.OrphanBytes += o.bytes
			if !req.IncludeOrphans {
				continue
			}
			if !result.DryRun {
				key, err := s.key(o.hash)
				if err == nil {
					err = s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{})
				}
				if err != nil {
					result.Failed++
					continue
				}
				if _, err = s.store.db.ExecContext(ctx, `DELETE FROM media_cache WHERE sha256=? AND NOT EXISTS(SELECT 1 FROM media_cache_refs WHERE sha256=?)`, o.hash, o.hash); err != nil {
					return errMediaS3
				}
			}
			result.Files++
			result.FreedBytes += o.bytes
		}
	}
}
