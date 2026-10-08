package main

// POST /api/media/purge — drop cached media bytes, keep the rows.
//
//	{"items": [{"message_id": "...", "chat_jid": "..."}], "dry_run": true}
//	{"chat_jid": "...", "older_than_days": 30, "min_bytes": 1048576, "media_type": "video", "dry_run": false}
//
// Every file removed is the one downloadMedia would produce for a message row
// that exists in messages.db: the path is built from the row's chat_jid,
// media_type, timestamp and id (mediaFileName / chatMediaRel), never from a
// client-supplied path, and every stat and delete goes through the store root
// (os.Root), which the kernel keeps inside the store directory.
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
	"slices"
	"strings"
	"time"
)

// purgeMaxFiles bounds one call so a mistaken criteria form stays bounded.
const purgeMaxFiles = 500

// purgeMaxScan bounds how many message rows the criteria form examines in one
// call. Whether a file is cached is only known by looking at the disk, so the
// scan walks the matching rows oldest first and steps over the ones whose file
// is gone (they cost a stat, not a slot in the purgeMaxFiles budget); this is
// the ceiling on those stats. Past it the answer says scan_truncated.
const purgeMaxScan = 100000

// purgeMaxUnreachable caps how many rows the purge cannot touch (a path the
// store root refuses, no store directory) are listed in items; they are all
// counted in unreachable.
const purgeMaxUnreachable = 50

const (
	purgeReasonNotCached     = "not cached"
	purgeReasonNotResolvable = "cached path does not resolve inside the store directory"
)

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
	// Criteria form only. Remaining is how many matching cached files this call
	// left alone because of the per-call cap (the next identical call takes
	// them); ScanTruncated says the scan hit purgeMaxScan before the end of the
	// matching rows, so Remaining is a lower bound; Unreachable counts matching
	// rows whose cached path the purge cannot touch (or the store directory is
	// unavailable).
	Remaining     int  `json:"remaining"`
	ScanTruncated bool `json:"scan_truncated"`
	Unreachable   int  `json:"unreachable"`
	// Failed counts selected files the real call could not remove (read-only
	// directory, immutable file); they are listed in items with the error.
	Failed int `json:"failed"`
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

// EachMediaRowMatching resolves the criteria form: media rows in chat (or all
// chats the policy allows), older than a cutoff, at least minBytes, of one
// type. It walks them oldest first, handing each to fn until fn returns false
// or maxScan allowed rows have been read; scanCut reports that more rows
// matched past maxScan. Streaming, because the caller probes the disk for every row and
// keeps only the few it will remove; fn must not touch the database.
func (store *MessageStore) EachMediaRowMatching(chatJID string, before time.Time, minBytes int64, mediaType string, policy chatPolicy, maxScan int, fn func(mediaRow) bool) (scanCut bool, err error) {
	clauses := []string{"media_type IN ('image','video','audio','document','sticker')"}
	var args []any
	if chatJID != "" {
		clauses = append(clauses, "chat_jid = ?")
		args = append(args, chatJID)
	}
	if !before.IsZero() {
		// dbTime: the bound value has to use the same spelling as the column
		// for the plain string comparison (and the index) to mean anything.
		clauses = append(clauses, "timestamp < ?")
		args = append(args, dbTime(before))
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
		`SELECT id, chat_jid, media_type, timestamp, COALESCE(filename, '') FROM messages WHERE `+strings.Join(clauses, " AND ")+ //nolint:gosec // every clause is a literal written above; the values travel in args as bound parameters
			` ORDER BY timestamp ASC, id ASC`, args...)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	scanned := 0
	for rows.Next() {
		var row mediaRow
		if err := rows.Scan(&row.ID, &row.ChatJID, &row.MediaType, &row.Timestamp, &row.Filename); err != nil {
			return false, err
		}
		// A denied chat's row is skipped before anything touches the disk, so it
		// does not use the scan budget either: the ceiling is on probes.
		if !policy.Allows(row.ChatJID) {
			continue
		}
		if scanned >= maxScan {
			return true, nil
		}
		scanned++
		if !fn(row) {
			return false, nil
		}
	}
	return false, rows.Err()
}

// purgeOne removes (or, in dry-run, measures) the cached file of one row.
// What counts as that file is decided where the download decides it
// (findCachedMedia, media_cache_path.go): a regular file under a plain name in
// the chat's own directory, nothing followed. The delete names the file inside
// the directory handle the lookup opened, so a chat directory swapped for a
// symlink between the lookup and the Remove cannot send it elsewhere.
func purgeOne(root *os.Root, row mediaRow, dryRun bool) PurgeResult {
	res := PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID}
	if row.MediaType == "" || row.MediaType == "reaction" || row.MediaType == "poll_vote" {
		res.Reason = "not a media message"
		return res
	}
	if root == nil {
		res.Reason = "store directory unavailable"
		return res
	}
	found, refused := findCachedMedia(root, chatMediaRel(row.ChatJID), mediaFileNames(row.MediaType, row.Timestamp, row.ID, row.Filename))
	if found == nil {
		switch {
		case errors.Is(refused, errMediaPath):
			// A chat's media lives in exactly one directory directly under the
			// store, each file under one plain name. The root already refuses
			// an escape; this is the rest of what a corrupted chat_jid or
			// message id could name — another chat's directory, a nested path —
			// which containment alone cannot express. The download refuses the
			// same rows with the same check.
			res.Reason = "path outside the store directory"
		case refused != nil:
			// "not cached" would blame a missing file for a path that was
			// refused: a chat directory or a cached name that is a symlink is
			// not followed, by the download either, and whoever put the link
			// there needs to read that the purge will not go through it rather
			// than that nothing is cached.
			res.Reason = purgeReasonNotResolvable
		default:
			res.Reason = purgeReasonNotCached
		}
		return res
	}
	defer found.Close()
	res.Bytes = found.info.Size()
	res.File = found.name
	if dryRun {
		res.Purged = true
		return res
	}
	if err := found.Remove(); err != nil {
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

// purgeScanLimit is the row ceiling of one criteria purge (PurgeScanLimit, or
// purgeMaxScan when unset).
func (b *Bridge) purgeScanLimit() int {
	if b.PurgeScanLimit > 0 {
		return b.PurgeScanLimit
	}
	return purgeMaxScan
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
		if req.ChatJID != "" {
			if _, ok := authorizeChat(w, b.Policy, req.ChatJID, false); !ok {
				return
			}
		}

		var rows []mediaRow
		var results []PurgeResult
		truncated, scanTruncated := false, false
		remaining, unreachable := 0, 0
		scanLimit := b.purgeScanLimit()
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
			if b.StoreRoot == nil {
				// Every probe would say "unavailable" and the scan would walk the
				// whole table to report it row by row.
				writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Store directory unavailable", DryRun: dryRun})
				return
			}
			// Select the files that are cached, not the first rows that match: a
			// row whose file is already gone (an earlier purge, a retention sweep)
			// would otherwise take a slot of purgeMaxFiles again on every call,
			// and the same call could never get past it. The probe is the dry run
			// of purgeOne, so dry_run and the real call see the same set.
			scanCut, err := b.Store.EachMediaRowMatching(req.ChatJID, before, req.MinBytes, req.MediaType, b.Policy, scanLimit, func(row mediaRow) bool {
				if r.Context().Err() != nil {
					return false // the caller hung up; stop walking the disk for it
				}
				probe := purgeOne(b.StoreRoot, row, true)
				switch {
				case probe.Purged && len(rows) < purgeMaxFiles:
					rows = append(rows, row)
				case probe.Purged:
					remaining++
				case probe.Reason != purgeReasonNotCached:
					unreachable++
					if unreachable <= purgeMaxUnreachable {
						results = append(results, probe)
					}
				}
				return true
			})
			if err != nil {
				writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Failed to query media rows: " + err.Error(), DryRun: dryRun})
				return
			}
			scanTruncated = scanCut
			truncated = remaining > 0 || scanCut
		}

		resp := MediaPurgeResponse{Success: true, DryRun: dryRun, Truncated: truncated}
		for _, row := range rows {
			res := purgeOne(b.StoreRoot, row, dryRun)
			if res.Purged {
				resp.PurgedFiles++
				resp.PurgedBytes += res.Bytes
			} else if len(req.Items) == 0 {
				resp.Failed++ // the probe saw the file, so the removal is what failed
			}
			results = append(results, res)
		}
		// A file that cannot be removed stays cached and would head the next
		// call's selection again. When a real call removed nothing and failed, it
		// is not progress: say so instead of inviting the caller to repeat it.
		if !dryRun && resp.Failed > 0 && resp.PurgedFiles == 0 {
			resp.Truncated = false
		}
		resp.Matched = len(rows)
		resp.Remaining, resp.ScanTruncated, resp.Unreachable = remaining, scanTruncated, unreachable
		resp.Items = results
		if resp.Items == nil {
			resp.Items = []PurgeResult{}
		}
		more := ""
		switch {
		case !dryRun && resp.Failed > 0 && resp.PurgedFiles == 0:
			more = fmt.Sprintf("; %d file(s) could not be removed (see items), repeating will not help", resp.Failed)
		case remaining > 0 && scanTruncated:
			more = fmt.Sprintf("; at least %d more cached file(s) match and the scan stopped after %d rows, narrow the criteria (chat_jid, older_than_days, min_bytes, media_type) to reach the rest", remaining, scanLimit)
		case remaining > 0:
			more = fmt.Sprintf("; %d more cached file(s) match, repeat the same call while truncated is true", remaining)
		case scanTruncated:
			more = fmt.Sprintf("; the scan stopped after %d rows, narrow the criteria (chat_jid, older_than_days, min_bytes, media_type) to reach the rest", scanLimit)
		}
		if dryRun {
			resp.Message = fmt.Sprintf("Dry run: %d cached file(s), %d bytes would be removed%s; repeat with dry_run=false to purge", resp.PurgedFiles, resp.PurgedBytes, more)
		} else {
			resp.Message = fmt.Sprintf("Removed %d cached file(s), %d bytes%s; rows kept, download_media can fetch them again", resp.PurgedFiles, resp.PurgedBytes, more)
			if resp.PurgedFiles > 0 {
				b.storeStats.invalidate()
			}
			b.Log.Infof("Media purge: %d file(s), %d bytes removed (%d matched)", resp.PurgedFiles, resp.PurgedBytes, resp.Matched)
		}
		writePurgeResponse(w, http.StatusOK, resp)
	}
}
