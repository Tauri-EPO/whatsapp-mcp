package main

// POST /api/media/purge — drop cached media bytes, keep the rows.
//
//	{"items": [{"message_id": "...", "chat_jid": "..."}], "dry_run": true}
//	{"chat_jid": "...", "older_than_days": 30, "min_bytes": 1048576, "media_type": "video", "dry_run": false}
//
// Every file removed is the one downloadMedia would produce for a message row
// that exists in messages.db: the path is built from the row's chat_jid,
// media_type, timestamp and id (mediaFileName / chatMediaDir), never from a
// client-supplied path, and is checked to stay under the store directory.
// Rows, hashes and notes are untouched, so download_media can fetch the file
// again later (media-retry covers expired CDN links). dry_run defaults to true:
// a missing field reports what would be removed and removes nothing.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// purgeMaxFiles bounds one call so a mistaken criteria form stays bounded.
const purgeMaxFiles = 500

var purgeMediaTypes = []string{"image", "video", "audio", "document", "sticker"}

type PurgeItem struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type MediaPurgeRequest struct {
	Items         []PurgeItem `json:"items"`
	ChatJID       string      `json:"chat_jid"`
	OlderThanDays int         `json:"older_than_days"`
	MinBytes      int64       `json:"min_bytes"`
	MediaType     string      `json:"media_type"`
	DryRun        *bool       `json:"dry_run"`
}

type PurgeResult struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	Purged    bool   `json:"purged"`
	Bytes     int64  `json:"bytes"`
	File      string `json:"file,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type MediaPurgeResponse struct {
	Success     bool          `json:"success"`
	Message     string        `json:"message"`
	DryRun      bool          `json:"dry_run"`
	Matched     int           `json:"matched"`
	PurgedFiles int           `json:"purged_files"`
	PurgedBytes int64         `json:"purged_bytes"`
	Truncated   bool          `json:"truncated"`
	Items       []PurgeResult `json:"items"`
}

// mediaRow is what the purge needs from a message row to locate its file.
type mediaRow struct {
	ID        string
	ChatJID   string
	MediaType string
	Timestamp time.Time
	Filename  string // sender's document name; picks the on-disk extension
}

// MediaRow returns the row for one (id, chat) pair; sql.ErrNoRows when absent.
func (store *MessageStore) MediaRow(messageID, chatJID string) (mediaRow, error) {
	var row mediaRow
	var mediaType, filename sql.NullString
	err := store.db.QueryRow(
		`SELECT id, chat_jid, media_type, timestamp, filename FROM messages WHERE id = ? AND chat_jid = ?`,
		messageID, chatJID,
	).Scan(&row.ID, &row.ChatJID, &mediaType, &row.Timestamp, &filename)
	row.MediaType, row.Filename = mediaType.String, filename.String
	return row, err
}

// MediaRowsMatching resolves the criteria form: media rows in chat (or all
// chats the policy allows), older than a cutoff, at least minBytes, of one
// type. Ordered oldest first and capped at limit+1 so callers can flag
// truncation.
func (store *MessageStore) MediaRowsMatching(chatJID string, before time.Time, minBytes int64, mediaType string, policy chatPolicy, limit int) ([]mediaRow, error) {
	clauses := []string{"media_type IN ('image','video','audio','document','sticker')"}
	var args []any
	if chatJID != "" {
		clauses = append(clauses, "chat_jid = ?")
		args = append(args, chatJID)
	}
	if !before.IsZero() {
		clauses = append(clauses, "timestamp < ?")
		args = append(args, before)
	}
	if minBytes > 0 {
		clauses = append(clauses, "file_length >= ?")
		args = append(args, minBytes)
	}
	if mediaType != "" {
		clauses = append(clauses, "media_type = ?")
		args = append(args, mediaType)
	}
	rows, err := store.db.Query(
		`SELECT id, chat_jid, media_type, timestamp, COALESCE(filename, '') FROM messages WHERE `+strings.Join(clauses, " AND ")+
			` ORDER BY timestamp ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []mediaRow
	for rows.Next() {
		var row mediaRow
		if err := rows.Scan(&row.ID, &row.ChatJID, &row.MediaType, &row.Timestamp, &row.Filename); err != nil {
			return nil, err
		}
		if !policy.Allows(row.ChatJID) {
			continue
		}
		out = append(out, row)
		if len(out) > limit {
			break
		}
	}
	return out, rows.Err()
}

// purgeOne removes (or, in dry-run, measures) the cached file of one row.
func purgeOne(row mediaRow, dryRun bool) PurgeResult {
	res := PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID}
	if row.MediaType == "" || row.MediaType == "reaction" || row.MediaType == "poll_vote" {
		res.Reason = "not a media message"
		return res
	}
	root, err := filepath.Abs(storeDir())
	if err != nil {
		res.Reason = "store directory unavailable"
		return res
	}
	chatDir := chatMediaDir(row.ChatJID)
	cached := cachedMediaPath(chatDir, row.MediaType, row.Timestamp, row.ID, row.Filename)
	if cached == "" {
		// Still run the containment check on the would-be path so a bad row is
		// reported as such rather than as "not cached".
		cached = filepath.Join(chatDir, mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename))
	}
	path, err := filepath.Abs(cached)
	if err != nil {
		res.Reason = "invalid path"
		return res
	}
	if rel, err := filepath.Rel(root, path); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		// The JID comes from a database row, so this cannot happen in practice;
		// refuse anyway so a corrupted row can never delete outside the store.
		res.Reason = "path outside the store directory"
		return res
	}
	info, err := os.Stat(path)
	if err != nil {
		res.Reason = "not cached"
		return res
	}
	if info.IsDir() {
		res.Reason = "not a file"
		return res
	}
	res.Bytes = info.Size()
	res.File = filepath.Base(path)
	if dryRun {
		res.Purged = true
		return res
	}
	if err := os.Remove(path); err != nil {
		res.Reason = "remove failed: " + err.Error()
		res.Bytes = 0
		return res
	}
	res.Purged = true
	return res
}

func writePurgeResponse(w http.ResponseWriter, status int, resp MediaPurgeResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleMediaPurge serves POST /api/media/purge.
func (b *Bridge) handleMediaPurge() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req MediaPurgeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: "Invalid request format", DryRun: true})
			return
		}
		dryRun := req.DryRun == nil || *req.DryRun
		req.ChatJID = strings.TrimSpace(req.ChatJID)
		req.MediaType = strings.TrimSpace(req.MediaType)
		if req.MediaType != "" && !slices.Contains(purgeMediaTypes, req.MediaType) {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: "media_type must be one of " + strings.Join(purgeMediaTypes, ", "), DryRun: dryRun})
			return
		}
		if len(req.Items) == 0 && req.ChatJID == "" && req.OlderThanDays == 0 && req.MinBytes == 0 && req.MediaType == "" {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: "Provide items, or at least one of chat_jid / older_than_days / min_bytes / media_type", DryRun: dryRun})
			return
		}
		if req.OlderThanDays < 0 || req.MinBytes < 0 {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: "older_than_days and min_bytes must not be negative", DryRun: dryRun})
			return
		}
		if req.ChatJID != "" && rejectByChatPolicy(w, b.Policy, req.ChatJID) {
			return
		}

		var rows []mediaRow
		var results []PurgeResult
		truncated := false
		if len(req.Items) > 0 {
			if len(req.Items) > purgeMaxFiles {
				req.Items = req.Items[:purgeMaxFiles]
				truncated = true
			}
			for _, item := range req.Items {
				item.MessageID, item.ChatJID = strings.TrimSpace(item.MessageID), strings.TrimSpace(item.ChatJID)
				if item.MessageID == "" || item.ChatJID == "" {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "message_id and chat_jid are required"})
					continue
				}
				if !b.Policy.Allows(item.ChatJID) {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "chat not in " + chatPolicyEnv})
					continue
				}
				row, err := b.Store.MediaRow(item.MessageID, item.ChatJID)
				if errors.Is(err, sql.ErrNoRows) {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "message not found"})
					continue
				}
				if err != nil {
					writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Failed to look up message: " + err.Error(), DryRun: dryRun})
					return
				}
				rows = append(rows, row)
			}
		} else {
			var before time.Time
			if req.OlderThanDays > 0 {
				before = time.Now().AddDate(0, 0, -req.OlderThanDays)
			}
			matched, err := b.Store.MediaRowsMatching(req.ChatJID, before, req.MinBytes, req.MediaType, b.Policy, purgeMaxFiles)
			if err != nil {
				writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Failed to query media rows: " + err.Error(), DryRun: dryRun})
				return
			}
			if len(matched) > purgeMaxFiles {
				matched = matched[:purgeMaxFiles]
				truncated = true
			}
			rows = matched
		}

		resp := MediaPurgeResponse{Success: true, DryRun: dryRun, Truncated: truncated}
		for _, row := range rows {
			res := purgeOne(row, dryRun)
			if res.Purged {
				resp.PurgedFiles++
				resp.PurgedBytes += res.Bytes
			}
			results = append(results, res)
		}
		resp.Matched = len(rows)
		resp.Items = results
		if resp.Items == nil {
			resp.Items = []PurgeResult{}
		}
		if dryRun {
			resp.Message = fmt.Sprintf("Dry run: %d cached file(s), %d bytes would be removed; repeat with dry_run=false to purge", resp.PurgedFiles, resp.PurgedBytes)
		} else {
			resp.Message = fmt.Sprintf("Removed %d cached file(s), %d bytes; rows kept, download_media can fetch them again", resp.PurgedFiles, resp.PurgedBytes)
			if resp.PurgedFiles > 0 {
				b.storeStats.invalidate()
			}
			b.Log.Infof("Media purge: %d file(s), %d bytes removed (%d matched)", resp.PurgedFiles, resp.PurgedBytes, resp.Matched)
		}
		writePurgeResponse(w, http.StatusOK, resp)
	}
}
