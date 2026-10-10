package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

type transientMediaKey struct{}

// The HTTP fallback owns a unique, private leaf outside all chat directories.
// It cannot be mistaken for cached media, shared by another response or leaked
// by a detached transfer after the request has already removed its spool.
type transientMediaStorage struct {
	localMediaStorage
	rel string
}

func (s transientMediaStorage) Lookup(context.Context, mediaRow) (*mediaCacheEntry, error) {
	return nil, nil
}
func (s transientMediaStorage) Write(_ context.Context, _ mediaRow, fill func(string) (int64, error)) (int64, error) {
	return fill(s.rel)
}
func (s transientMediaStorage) Open(ctx context.Context, _ mediaRow, _ string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	seen, err := s.root.Lstat(s.rel)
	if err != nil || !seen.Mode().IsRegular() {
		return nil, 0, os.ErrNotExist
	}
	f, err := s.root.Open(s.rel)
	if err != nil {
		return nil, 0, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(seen, opened) {
		_ = f.Close()
		return nil, 0, os.ErrNotExist
	}
	return f, opened.Size(), nil
}

func (b *Bridge) handleMediaBlob(w http.ResponseWriter, r *http.Request) {
	chat, ok := authorizeChat(w, b.Policy, r.URL.Query().Get("chat_jid"), false)
	if !ok {
		return
	}
	id := r.URL.Query().Get("message_id")
	row, err := b.Store.MediaRow(id, chat.String())
	if err != nil {
		writeError(w, 404, "Media message not found")
		return
	}
	if checkMediaPathComponents(chatMediaRel(row.ChatJID), mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)) != nil {
		writeError(w, 403, "Media identity refused")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), downloadDeadline)
	defer cancel()
	storage := b.mediaStorage()
	entry, err := storage.Lookup(ctx, row)
	if err != nil {
		writeError(w, 503, "Media cache unavailable")
		return
	}
	if entry == nil {
		if r.URL.Query().Get("cache_only") == "true" {
			writeError(w, 404, "Media is not cached")
			return
		}
		if !b.Connected() {
			writeError(w, 503, "WhatsApp client is not connected")
			return
		}
		_, _, _, _, err = b.DownloadMedia(ctx, row.ID, row.ChatJID)
		if errors.Is(err, errMediaQuota) {
			// Keep the fallback owned by this HTTP request. Its private temporary
			// local file disappears after streaming, and never enters the catalog.
			rel := ".media-stream-" + rand.Text()
			ctx = context.WithValue(ctx, transientMediaKey{}, rel)
			defer func() { _ = b.StoreRoot.Remove(rel); _ = b.StoreRoot.Remove(rel + ".part") }()
			_, _, _, _, err = b.downloadMediaAttempt(ctx, row.ID, row.ChatJID)
			storage = transientMediaStorage{localMediaStorage: localMediaStorage{root: b.StoreRoot}, rel: rel}
			w.Header().Set("X-Media-Cached", "false")
			w.Header().Set("X-Media-Reason", "quota")
		}
		if err != nil {
			writeError(w, 502, "Media download failed")
			return
		}
	}
	f, size, err := storage.Open(ctx, row, "")
	if err != nil {
		writeError(w, 502, "Media read failed")
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("X-Media-Cached") == "" {
		w.Header().Set("X-Media-Cached", "true")
	}
	// Bound writes as well as the upstream transfer.
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(downloadDeadline))
	_, _ = io.Copy(w, io.LimitReader(f, size))
}

func (b *Bridge) handleMediaCache(w http.ResponseWriter, r *http.Request) {
	chat, ok := authorizeChat(w, b.Policy, r.URL.Query().Get("chat_jid"), false)
	if !ok {
		return
	}
	if b.mediaStorage().Backend() != "s3" {
		writeJSON(w, 200, map[string]any{"backend": "local", "items": []any{}})
		return
	}
	cursor := r.URL.Query().Get("cursor")
	ids, specific := r.URL.Query()["message_id"]
	if specific && (len(ids) == 0 || len(ids) > 256) {
		writeError(w, 400, "Media catalog accepts at most 256 message IDs")
		return
	}
	query := `SELECT r.id,c.bytes,hex(c.sha256) FROM media_cache_refs r JOIN media_cache c ON c.sha256=r.sha256 JOIN messages m ON m.id=r.id AND m.chat_jid=r.chat_jid WHERE r.chat_jid=? AND c.backend='s3' AND c.sha256=m.file_sha256`
	args := []any{chat.String()}
	if storage, ok := b.mediaStorage().(*s3MediaStorage); ok && storage.deletionJournal {
		query += ` AND NOT EXISTS(SELECT 1 FROM media_cache_deletions d WHERE d.sha256=c.sha256)`
	}
	if specific {
		for _, id := range ids {
			if id == "" {
				writeError(w, 400, "Media catalog message IDs must not be empty")
				return
			}
		}
		batch, err := json.Marshal(ids)
		if err != nil {
			writeError(w, 400, "Invalid media catalog message IDs")
			return
		}
		query += ` AND r.id IN (SELECT value FROM json_each(?))`
		args = append(args, string(batch))
	} else {
		query += ` AND r.id>?`
		args = append(args, cursor)
	}
	rows, err := b.Store.db.QueryContext(r.Context(), query+` ORDER BY r.id LIMIT 257`, args...)
	if err != nil {
		writeError(w, 503, "Media catalog unavailable")
		return
	}
	defer func() { _ = rows.Close() }()
	items := []map[string]any{}
	for rows.Next() {
		var id, sha string
		var size int64
		if rows.Scan(&id, &size, &sha) != nil {
			writeError(w, 503, "Media catalog unavailable")
			return
		}
		items = append(items, map[string]any{"message_id": id, "bytes": size, "sha256": strings.ToLower(sha)})
	}
	if rows.Err() != nil {
		writeError(w, 503, "Media catalog unavailable")
		return
	}
	next := ""
	if len(items) > 256 {
		items = items[:256]
		next = items[len(items)-1]["message_id"].(string)
	}
	writeJSON(w, 200, map[string]any{"backend": "s3", "items": items, "next_cursor": next})
}

func (b *Bridge) handleS3MediaUsage(w http.ResponseWriter, r *http.Request, limit int) {
	ctx := r.Context()
	usage, err := b.mediaStorage().Usage(ctx)
	if err != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	byType := map[string]int64{"image": 0, "video": 0, "audio": 0, "document": 0, "sticker": 0, "status": 0}
	rows, err := b.Store.db.QueryContext(ctx, `SELECT media_type,SUM(bytes) FROM media_cache WHERE backend='s3' GROUP BY media_type`)
	if err != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	for rows.Next() {
		var kind string
		var size int64
		if rows.Scan(&kind, &size) != nil {
			_ = rows.Close()
			writeError(w, 503, "Media usage unavailable")
			return
		}
		byType[kind] = size
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	rows, err = b.Store.db.QueryContext(ctx, `SELECT r.chat_jid,SUM(c.bytes) FROM media_cache_refs r JOIN media_cache c ON c.sha256=r.sha256 WHERE c.backend='s3' GROUP BY r.chat_jid`)
	if err != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	type chatUsage struct {
		Chat  string `json:"chat_jid"`
		Bytes int64  `json:"bytes"`
	}
	chats := []chatUsage{}
	var referenced int64
	for rows.Next() {
		var chat chatUsage
		if rows.Scan(&chat.Chat, &chat.Bytes) != nil {
			_ = rows.Close()
			writeError(w, 503, "Media usage unavailable")
			return
		}
		chats = append(chats, chat)
		referenced += chat.Bytes
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].Bytes == chats[j].Bytes {
			return chats[i].Chat < chats[j].Chat
		}
		return chats[i].Bytes > chats[j].Bytes
	})
	quota, types, target, err := b.mediaQuotaSettings(ctx)
	if err != nil {
		writeError(w, 503, "Runtime settings unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"backend": "s3", "bytes": usage.Bytes, "quota_bytes": quota, "caching_paused": b.mediaCachingPaused(quota, usage.Bytes, types, target), "dedupe_saved_bytes": max(int64(0), referenced-usage.Bytes), "by_type": byType, "by_chat": chats[:min(limit, len(chats))]})
}

func parseMediaURI(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "whatsapp" || u.Host != "media" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", errors.New("invalid cached media URI")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || checkMediaPathComponents(chatMediaRel(parts[0]), parts[1]) != nil {
		return "", "", errors.New("invalid cached media URI")
	}
	return parts[0], parts[1], nil
}

// Only bridge-generated identifiers enter storage. No object key, bucket or
// credential is accepted from callers or disclosed through these endpoints.
type cachedSendSourceKey struct{}

func (b *Bridge) materializeCachedMedia(ctx context.Context, uri string) (string, *forwardSource, func(), error) {
	chat, id, err := parseMediaURI(uri)
	if err != nil {
		return "", nil, func() {}, err
	}
	row, err := b.Store.MediaRow(id, chat)
	if err != nil {
		return "", nil, func() {}, err
	}
	source, found, err := b.Store.messageContentLookup(id, chat)
	if err != nil {
		return "", nil, func() {}, err
	}
	if !found || source.mediaType != row.MediaType || source.filename != row.Filename {
		return "", nil, func() {}, errMediaSnapshot
	}
	reader, _, err := b.mediaStorage().Open(ctx, row, "")
	if err != nil {
		return "", nil, func() {}, err
	}
	defer func() { _ = reader.Close() }()
	f, err := os.CreateTemp("", "wamcp-send-*"+path.Ext(mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)))
	if err != nil {
		return "", nil, func() {}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	_, err = io.Copy(f, reader)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", nil, func() {}, err
	}
	return f.Name(), &source, cleanup, nil
}
