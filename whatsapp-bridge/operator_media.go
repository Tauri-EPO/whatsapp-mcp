package main

// Local operator media maintenance. S3 and shared-object accounting await #649.
import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type operatorMediaPurge struct {
	Type           string `json:"type"`
	Chat           string `json:"chat_jid"`
	OlderDays      int    `json:"older_than_days"`
	DryRun         *bool  `json:"dry_run"`
	IncludeOrphans bool   `json:"include_orphans"`
}

type operatorMediaResult struct {
	DryRun      bool  `json:"dry_run"`
	Files       int   `json:"files"`
	FreedBytes  int64 `json:"freed_bytes"`
	OrphanFiles int   `json:"orphan_files"`
	OrphanBytes int64 `json:"orphan_bytes"`
	Failed      int   `json:"failed"`
}

func mediaScope(chat, name string) string {
	if chat == "status@broadcast" {
		return "status"
	}
	kind, _, _ := strings.Cut(name, "_")
	return kind
}

// Per-chat IDs are already within the private operator's full-archive authority
// (export); this listener adds counts, never content, names or media paths.
func (b *Bridge) handleOperatorMediaUsage(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, 400, "limit must be 1-100")
			return
		}
	}
	byType := map[string]int64{"image": 0, "video": 0, "audio": 0, "document": 0, "sticker": 0, "status": 0}
	byChat := map[string]int64{}
	var total int64
	err := eachCachedMediaContext(r.Context(), b.StoreRoot, func(chat string, file *cachedMedia) {
		if r.Context().Err() != nil {
			return
		}
		bytes := file.info.Size()
		byType[mediaScope(chat, file.name)] += bytes
		byChat[chat] += bytes
		total += bytes
	})
	if err != nil || r.Context().Err() != nil {
		writeError(w, 503, "Media usage unavailable")
		return
	}
	type chatUsage struct {
		Chat  string `json:"chat_jid"`
		Bytes int64  `json:"bytes"`
	}
	chats := make([]chatUsage, 0, len(byChat))
	for chat, bytes := range byChat {
		chats = append(chats, chatUsage{chat, bytes})
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].Bytes == chats[j].Bytes {
			return chats[i].Chat < chats[j].Chat
		}
		return chats[i].Bytes > chats[j].Bytes
	})
	quota, types, target, err := b.mediaQuotaSettings(r.Context())
	if err != nil {
		writeError(w, 503, "Runtime settings unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"backend": "local", "bytes": total, "quota_bytes": quota, "caching_paused": b.mediaCachingPaused(quota, total, types, target), "by_type": byType, "by_chat": chats[:min(limit, len(chats))]})
}

func (b *Bridge) handleOperatorMediaPurge(w http.ResponseWriter, r *http.Request) {
	var req operatorMediaPurge
	if !operatorDecode(w, r, &req) {
		return
	}
	if req.Type != "all" && req.Type != "status" && !slices.Contains(purgeMediaTypes, req.Type) {
		writeError(w, 400, "type is required: image, video, audio, document, sticker, status or all")
		return
	}
	if req.OlderDays < 0 || req.OlderDays > 106751 || (req.IncludeOrphans && req.Type != "status") {
		writeError(w, 400, "Invalid age or orphan filter")
		return
	}
	if req.Chat != "" {
		chat, err := canonicalChatJID(req.Chat, false)
		if err != nil || checkMediaPathComponents(chatMediaRel(chat.String()), "probe") != nil {
			writeError(w, 400, "Invalid chat_jid")
			return
		}
		req.Chat = chat.String()
	}
	if req.Type == "status" {
		if req.Chat != "" && req.Chat != "status@broadcast" {
			writeError(w, 400, "status requires the status chat")
			return
		}
		req.Chat = "status@broadcast"
	}
	if b.StoreRoot == nil {
		writeError(w, 503, "Media store unavailable")
		return
	}
	b.streamMediaPurge(w, r, req, nil)
}

func (b *Bridge) streamMediaPurge(w http.ResponseWriter, r *http.Request, req operatorMediaPurge, final func(operatorMediaResult, error) any) {
	if b.StoreRoot == nil {
		writeError(w, 503, "Media store unavailable")
		return
	}
	// Long scans outlive the listener's 15-second WriteTimeout. Stream a single
	// JSON object with progress and the existing final result fields, refreshing
	// only this response's deadline at each bounded heartbeat. No background job
	// survives its HTTP request and a disconnected client cancels the scan.
	ctx, cancel := context.WithTimeout(r.Context(), b.archiveTimeout())
	defer cancel()
	type completion struct {
		result operatorMediaResult
		err    error
	}
	updates := make(chan operatorMediaResult, 1)
	done := make(chan completion, 1)
	go func() {
		result, err := b.purgeOperatorMediaProgress(ctx, req, func(result operatorMediaResult) {
			select {
			case updates <- result:
			default:
			}
		})
		done <- completion{result, err}
	}()
	controller := http.NewResponseController(w)
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()) })
	defer stop()
	w.Header().Set("Content-Type", "application/json")
	write := func(value []byte) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := w.Write(value); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write([]byte(`{"progress":[{"files":0,"freed_bytes":0}`)) {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	latest := operatorMediaResult{DryRun: req.DryRun == nil || *req.DryRun}
	for {
		select {
		case latest = <-updates:
		case <-ticker.C:
			data, _ := json.Marshal(latest)
			if !write(append([]byte(","), data...)) {
				return
			}
		case completed := <-done:
			var payload any = completed.result
			if final != nil {
				payload = final(completed.result, completed.err)
			}
			data, _ := json.Marshal(payload)
			ending := append([]byte("],"), data[1:len(data)-1]...)
			if completed.err != nil {
				ending = append(ending, []byte(`,"ok":false,"error":"Media purge incomplete"`)...)
			}
			_ = write(append(ending, '}'))
			return
		case <-ctx.Done():
			return
		}
	}
}

// Rows are drained in pages before callbacks. There is no 500-file or uncached
// row-tail cap here: one operator/status action walks all matching rows.
func (b *Bridge) purgeOperatorMedia(ctx context.Context, req operatorMediaPurge) (operatorMediaResult, error) {
	return b.purgeOperatorMediaProgress(ctx, req, nil)
}

func (b *Bridge) purgeOperatorMediaProgress(ctx context.Context, req operatorMediaPurge, progress func(operatorMediaResult)) (operatorMediaResult, error) {
	result := operatorMediaResult{DryRun: req.DryRun == nil || *req.DryRun}
	if b.StoreRoot == nil {
		return result, errors.New("store unavailable")
	}
	var before time.Time
	if req.OlderDays > 0 {
		before = time.Now().AddDate(0, 0, -req.OlderDays)
	}
	kind := req.Type
	if kind == "all" || kind == "status" {
		kind = ""
	}
	// Track generated status files only to distinguish row-less files. Other
	// directories and user-created names can never enter the orphan deletion.
	orphans := map[string]bool{}
	if req.Type == "status" {
		if err := eachCachedMediaContext(ctx, b.StoreRoot, func(chat string, file *cachedMedia) {
			if chat == "status@broadcast" {
				orphans[file.name] = true
			}
		}); err != nil {
			return result, err
		}
	}
	finder := &cachedMediaFinder{root: b.StoreRoot}
	defer finder.Close()
	examined := 0
	_, err := b.Store.EachMediaRowMatchingContext(ctx, req.Chat, time.Time{}, purgeCursor{}, kind, chatPolicy{}, math.MaxInt, func(row mediaRow) bool {
		examined++
		if progress != nil && examined%256 == 0 {
			progress(result)
		}
		if ctx.Err() != nil {
			return false
		}
		if req.Type != "all" && req.Type != "status" && row.ChatJID == "status@broadcast" {
			return true
		}
		for _, name := range mediaFileNames(row.MediaType, row.Timestamp, row.ID, row.Filename) {
			if checkMediaPathComponents(chatMediaRel(row.ChatJID), name) == nil {
				delete(orphans, name)
			}
		}
		if !before.IsZero() && !row.Timestamp.Before(before) {
			return true
		}
		if req.Type == "status" {
			seen := map[string]bool{}
			for _, name := range mediaFileNames(row.MediaType, row.Timestamp, row.ID, row.Filename) {
				if seen[name] {
					continue
				}
				seen[name] = true
				found, err := finder.find(chatMediaRel(row.ChatJID), []string{name})
				if err != nil {
					result.Failed++
					continue
				}
				if found == nil {
					continue
				}
				if !result.DryRun && found.Remove() != nil {
					result.Failed++
					found.Close()
					continue
				}
				result.Files++
				result.FreedBytes += found.info.Size()
				found.Close()
			}
			return true
		}
		res := purgeOneUsing(b.StoreRoot, row, result.DryRun, finder)
		if res.Purged {
			result.Files++
			result.FreedBytes += res.Bytes
		} else if res.Reason != purgeReasonNotCached {
			result.Failed++
		}
		return true
	})
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if req.Type == "status" {
		err = eachCachedMediaContext(ctx, b.StoreRoot, func(chat string, file *cachedMedia) {
			if ctx.Err() != nil || chat != "status@broadcast" || !orphans[file.name] || (!before.IsZero() && !file.info.ModTime().Before(before)) {
				return
			}
			result.OrphanFiles++
			result.OrphanBytes += file.info.Size()
			if !req.IncludeOrphans {
				return
			}
			if !result.DryRun && file.Remove() != nil {
				result.Failed++
				return
			}
			result.Files++
			result.FreedBytes += file.info.Size()
		})
	}
	if !result.DryRun {
		b.storeStats.invalidate()
		b.Log.Infof("Operator media purge: type=%s age_days=%d chat_filter=%t include_orphans=%t files=%d freed_bytes=%d failed=%d", req.Type, req.OlderDays, req.Chat != "", req.IncludeOrphans, result.Files, result.FreedBytes, result.Failed)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}
