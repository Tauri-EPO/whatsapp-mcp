package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

func (s *s3MediaStorage) Delete(ctx context.Context, rows []mediaRow, dry bool) (results []PurgeResult, resultErr error) {
	type object struct {
		hash     []byte
		bytes    int64
		selected int
		last     int
		remove   bool
		indices  []int
	}
	objects := map[string]*object{}
	results = make([]PurgeResult, len(rows))
	done := make([]bool, len(rows))
	for i, row := range rows {
		results[i] = PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID, Reason: purgeReasonNotCached}
	}
	defer func() {
		if resultErr != nil {
			for i := range results {
				if !done[i] {
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
		objects[h].indices = append(objects[h].indices, i)
		objects[h].last = i
		results[i].Purged = true
		results[i].Reason = ""
	}
	// Serialize each object's ref changes with publication, without blocking
	// unrelated hashes or holding a database transaction during remote I/O.
	for _, obj := range objects {
		unlock, err := s.lockHash(ctx, obj.hash)
		if err != nil {
			return results, err
		}
		err = func() error {
			defer unlock()
			// Selection preceded the hash lock; a concurrent replacement may
			// have moved a message to another object while this call waited.
			obj.selected = 0
			for _, i := range obj.indices {
				var matches int
				row := rows[i]
				if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_refs WHERE id=? AND chat_jid=? AND sha256=?`, row.ID, row.ChatJID, obj.hash).Scan(&matches); err != nil {
					return errMediaS3
				}
				if matches == 0 {
					results[i].Purged = false
					results[i].Reason = purgeReasonNotCached
				} else {
					obj.selected++
					obj.last = i
				}
			}
			if obj.selected == 0 {
				return nil
			}
			var count int
			if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_refs WHERE sha256=?`, obj.hash).Scan(&count); err != nil {
				return errMediaS3
			}
			if count == obj.selected {
				obj.remove = true
				results[obj.last].Bytes = obj.bytes
			}
			if dry {
				return nil
			}
			if obj.remove {
				if _, err := s.store.db.ExecContext(ctx, `INSERT OR IGNORE INTO media_cache_deletions(sha256) VALUES(?)`, obj.hash); err != nil {
					return errMediaS3
				}
				if err := s.removeObject(ctx, obj.hash); err != nil {
					// Even a failed/cancelled request can have deleted the object.
					// Keep the marker until a bounded HEAD settles that ambiguity.
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					_, _ = s.reconcileDeletionLocked(cleanup, obj.hash)
					cancel()
					for _, i := range obj.indices {
						results[i].Purged = false
						results[i].Bytes = 0
						results[i].Reason = "S3 media removal failed"
					}
					return nil
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				defer cancel()
				if _, err := s.store.db.ExecContext(cleanup, `DELETE FROM media_cache WHERE sha256=? AND backend='s3'`, obj.hash); err != nil {
					return errMediaS3
				}
				for _, i := range obj.indices {
					done[i] = true
				}
				return nil
			}
			tx, err := s.store.db.BeginTx(ctx, nil)
			if err != nil {
				return errMediaS3
			}
			defer func() { _ = tx.Rollback() }()
			for _, i := range obj.indices {
				row := rows[i]
				if !results[i].Purged {
					continue
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM media_cache_refs WHERE id=? AND chat_jid=? AND sha256=?`, row.ID, row.ChatJID, obj.hash); err != nil {
					return errMediaS3
				}
			}
			if err := tx.Commit(); err != nil {
				return errMediaS3
			}
			for _, i := range obj.indices {
				done[i] = true
			}
			return nil
		}()
		if err != nil {
			return results, err
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
	recoveredBytes, recoveryFailed := s.reconcilePendingDeletions(ctx)
	status := age
	if statusAge != nil {
		status = *statusAge
	}
	clauses := []string{}
	args := []any{}
	if age > 0 {
		clauses = append(clauses, `(m.chat_jid!='status@broadcast' AND m.timestamp<?)`)
		args = append(args, dbTime(now.Add(-age)))
	}
	if status > 0 {
		clauses = append(clauses, `(m.chat_jid='status@broadcast' AND m.timestamp<?)`)
		args = append(args, dbTime(now.Add(-status)))
	}
	predicate := "0"
	if len(clauses) > 0 {
		predicate = "(" + strings.Join(clauses, " OR ") + ")"
	}
	files, failed := 0, 0
	var freed int64
	err := s.walkCachedRows(ctx, predicate, args, func(selected []mediaRow) (bool, error) {
		results, err := s.Delete(ctx, selected, false)
		for _, r := range results {
			if r.Purged {
				files++
				freed += r.Bytes
			} else if r.Reason != purgeReasonNotCached {
				failed++
			}
		}
		return true, err
	})
	if err != nil {
		failed++
	}
	// Failed deletions or replaced message snapshots may leave an object with
	// no references. Keep its bytes charged until a later bounded sweep removes it.
	orphanBytes, orphanFailed := s.sweepUnreferenced(ctx)
	return files, freed + orphanBytes + recoveredBytes, failed + orphanFailed + recoveryFailed
}

func (s *s3MediaStorage) sweepUnreferenced(ctx context.Context) (int64, int) {
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
		removed, err := s.removeOrphan(ctx, o.hash)
		if err != nil {
			failed++
			continue
		}
		if !removed {
			continue
		}
		freed += o.bytes
	}
	return freed, failed
}

const s3CachedRefsSQL = ` FROM media_cache_refs r JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid JOIN media_cache c ON c.sha256=r.sha256 WHERE c.backend='s3' AND `
const s3RefKindSQL = `CASE WHEN m.chat_jid='status@broadcast' THEN 'status' ELSE m.media_type END`

func s3MediaTypesPredicate(types []string) (string, []any) {
	if len(types) == 0 {
		return "0", nil
	}
	args := make([]any, len(types))
	for i, kind := range types {
		args[i] = kind
	}
	return s3RefKindSQL + ` IN (` + strings.TrimSuffix(strings.Repeat("?,", len(types)), ",") + `)`, args
}

// Each network/delete callback runs after closing a page's SQL cursor. Keep
// both selection and Go memory bounded, including while quota admission waits.
// The cursor uses the original stored timestamp spelling, never a derived time.
func (s *s3MediaStorage) walkCachedRows(ctx context.Context, predicate string, args []any, visit func([]mediaRow) (bool, error)) error {
	stamp, chat, id := "", "", ""
	for {
		params := append([]any(nil), args...)
		params = append(params, stamp, chat, id)
		//nolint:gosec // Only fixed internal predicates/placeholders; archive and request values are bound separately.
		cursor, err := s.store.db.QueryContext(ctx, `SELECT m.id,m.chat_jid,m.media_type,m.timestamp,COALESCE(m.filename,''),CAST(m.timestamp AS TEXT)`+s3CachedRefsSQL+`(`+predicate+`) AND (m.timestamp,m.chat_jid,m.id)>(?,?,?) ORDER BY m.timestamp,m.chat_jid,m.id LIMIT 256`, params...)
		if err != nil {
			return errMediaS3
		}
		rows := make([]mediaRow, 0, 256)
		for cursor.Next() {
			var row mediaRow
			if err := cursor.Scan(&row.ID, &row.ChatJID, &row.MediaType, &row.Timestamp, &row.Filename, &stamp); err != nil {
				_ = cursor.Close()
				return errMediaS3
			}
			chat, id = row.ChatJID, row.ID
			rows = append(rows, row)
		}
		err = cursor.Err()
		_ = cursor.Close()
		if err != nil {
			return errMediaS3
		}
		if len(rows) == 0 {
			return nil
		}
		more, err := visit(rows)
		if err != nil || !more || len(rows) < 256 {
			return err
		}
	}
}

func (s *s3MediaStorage) drySelection(ctx context.Context, predicate string, args []any) (int, int64, error) {
	// Count distinct objects only when all of their refs satisfy the selection.
	// Page boundaries must not hide the last ref from dry-run byte accounting.
	query := `SELECT (SELECT COUNT(*)` + s3CachedRefsSQL + `(` + predicate + `)),COALESCE(SUM(c.bytes),0) FROM media_cache c WHERE c.backend='s3' AND EXISTS(SELECT 1 FROM media_cache_refs r JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid WHERE r.sha256=c.sha256 AND (` + predicate + `)) AND NOT EXISTS(SELECT 1 FROM media_cache_refs r JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid WHERE r.sha256=c.sha256 AND NOT COALESCE((` + predicate + `),0))`
	params := append([]any(nil), args...)
	params = append(params, args...)
	params = append(params, args...)
	var files int
	var freed int64
	if err := s.store.db.QueryRowContext(ctx, query, params...).Scan(&files, &freed); err != nil {
		return 0, 0, errMediaS3
	}
	return files, freed, nil
}

func (b *Bridge) purgeS3OperatorMedia(ctx context.Context, req operatorMediaPurge) (operatorMediaResult, error) {
	s, ok := b.mediaStorage().(*s3MediaStorage)
	if !ok {
		return operatorMediaResult{}, errors.New("S3 storage unavailable")
	}
	result := operatorMediaResult{DryRun: req.DryRun == nil || *req.DryRun}
	clauses := []string{"1"}
	args := []any{}
	if req.Chat != "" {
		clauses = append(clauses, "m.chat_jid=?")
		args = append(args, req.Chat)
	}
	if req.Type != "all" {
		clauses = append(clauses, s3RefKindSQL+"=?")
		args = append(args, req.Type)
	}
	if req.OlderDays > 0 {
		clauses = append(clauses, "m.timestamp<?")
		args = append(args, dbTime(time.Now().AddDate(0, 0, -req.OlderDays)))
	}
	predicate := strings.Join(clauses, " AND ")
	var err error
	if result.DryRun {
		result.Files, result.FreedBytes, err = s.drySelection(ctx, predicate, args)
	} else {
		err = s.walkCachedRows(ctx, predicate, args, func(selected []mediaRow) (bool, error) {
			items, err := s.Delete(ctx, selected, false)
			for _, item := range items {
				if item.Purged {
					result.Files++
					result.FreedBytes += item.Bytes
				} else if item.Reason != purgeReasonNotCached {
					result.Failed++
				}
			}
			return true, err
		})
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
	if req.Chat != "" && (req.Type != "status" || req.Chat != "status@broadcast") {
		return nil
	}
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
				removed, err := s.removeOrphan(ctx, o.hash)
				if err != nil {
					result.Failed++
					continue
				}
				if !removed {
					continue
				}
			}
			result.Files++
			result.FreedBytes += o.bytes
		}
	}
}

func (s *s3MediaStorage) removeObject(ctx context.Context, hash []byte) error {
	key, err := s.key(hash)
	if err != nil {
		return err
	}
	release, err := s.transfer(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := s.client.RemoveObject(ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return errMediaS3
	}
	return nil
}

func (s *s3MediaStorage) removeOrphan(ctx context.Context, hash []byte) (bool, error) {
	unlock, err := s.lockHash(ctx, hash)
	if err != nil {
		return false, err
	}
	defer unlock()
	var count int
	if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_cache_refs WHERE sha256=?`, hash).Scan(&count); err != nil {
		return false, errMediaS3
	}
	if count != 0 {
		return false, nil
	}
	if err := s.removeObject(ctx, hash); err != nil {
		return false, err
	}
	_, err = s.store.db.ExecContext(ctx, `DELETE FROM media_cache WHERE sha256=? AND NOT EXISTS(SELECT 1 FROM media_cache_refs WHERE sha256=?)`, hash, hash)
	return err == nil, err
}
