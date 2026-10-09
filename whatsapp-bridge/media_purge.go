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
	"encoding/base64"
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

const (
	purgeMaxItems     = 1000
	purgeMaxBodyBytes = 1024 * 1024
)

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
	purgeReasonNotResolvable = "cached path refused or inaccessible: expected a regular file in a real chat directory"
)

var purgeMediaTypes = []string{"image", "video", "audio", "document", "sticker"}

type PurgeItem struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type MediaPurgeRequest struct {
	Scope          string      `json:"scope"`
	IncludeOrphans bool        `json:"include_orphans"`
	Items          []PurgeItem `json:"items"`
	ChatJID        string      `json:"chat_jid"`
	OlderThanDays  int         `json:"older_than_days"`
	MinBytes       int64       `json:"min_bytes"`
	MediaType      string      `json:"media_type"`
	DryRun         *bool       `json:"dry_run"`
	Cursor         string      `json:"cursor"`
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
	// Remaining counts unexamined explicit items, or -1 for an unknown criteria
	// tail (0 at the end). ScanTruncated means the row ceiling was reached.
	// Unreachable counts criteria rows whose cached path was refused.
	Remaining     int  `json:"remaining"`
	ScanTruncated bool `json:"scan_truncated"`
	Unreachable   int  `json:"unreachable"`
	// Failed counts selected files the real call could not remove (read-only
	// directory, immutable file); they are listed in items with the error.
	Failed int `json:"failed"`
	// A criteria continuation seeks past the last examined row. Remaining is
	// -1 when the unexamined tail is unknown; it is never counted by disk scans.
	NextCursor string `json:"next_cursor,omitempty"`
	Examined   int    `json:"examined"`
}

type purgeCursor struct {
	Timestamp string `json:"t"`
	ID        string `json:"i"`
	Chat      string `json:"c"`
}

func decodePurgeCursor(value string) (purgeCursor, error) {
	var cursor purgeCursor
	if value == "" {
		return cursor, nil
	}
	if len(value) > 4096 {
		return cursor, errors.New("invalid purge cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, errors.New("invalid purge cursor")
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ID == "" || cursor.Chat == "" {
		return cursor, errors.New("invalid purge cursor")
	}
	if _, err := time.Parse("2006-01-02 15:04:05-07:00", cursor.Timestamp); err != nil {
		return cursor, errors.New("invalid purge cursor")
	}
	return cursor, nil
}

func encodePurgeCursor(row mediaRow) string {
	data, _ := json.Marshal(purgeCursor{Timestamp: dbTime(row.Timestamp), ID: row.ID, Chat: row.ChatJID})
	return base64.RawURLEncoding.EncodeToString(data)
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
// chats the policy allows), older than a cutoff, of one
// type. It walks them oldest first, handing each to fn until fn returns false
// or maxScan allowed rows have been read; scanCut reports that more rows
// may remain past maxScan. min_bytes is checked against cached bytes by the
// caller, never against the sender-declared SQL length. Rows are drained in bounded pages and the cursor is closed before callbacks,
// so disk work never holds a database connection and fn may query the database.
func (store *MessageStore) EachMediaRowMatching(chatJID string, before time.Time, after purgeCursor, mediaType string, policy chatPolicy, maxScan int, fn func(mediaRow) bool) (scanCut bool, err error) {
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
	if mediaType != "" {
		clauses = append(clauses, "media_type = ?")
		args = append(args, mediaType)
	}
	const pageSize = 256
	scanned := 0
	for {
		pageClauses := append([]string(nil), clauses...)
		pageArgs := append([]any(nil), args...)
		if after.ID != "" {
			pageClauses = append(pageClauses, "(timestamp, id, chat_jid) > (?, ?, ?)")
			pageArgs = append(pageArgs, after.Timestamp, after.ID, after.Chat)
		}
		rows, err := store.db.Query(
			`SELECT id, chat_jid, media_type, timestamp, COALESCE(filename, ''), CAST(timestamp AS TEXT) FROM messages WHERE `+strings.Join(pageClauses, " AND ")+ //nolint:gosec // Literal clauses and bound values only; the limit is fixed.
				` ORDER BY timestamp ASC, id ASC, chat_jid ASC LIMIT 256`, pageArgs...)
		if err != nil {
			return false, err
		}
		page := make([]mediaRow, 0, pageSize)
		for rows.Next() {
			var row mediaRow
			var rawTimestamp string
			if err := rows.Scan(&row.ID, &row.ChatJID, &row.MediaType, &row.Timestamp, &row.Filename, &rawTimestamp); err != nil {
				_ = rows.Close()
				return false, err
			}
			page = append(page, row)
			// SQL order uses the original spelling, even for a legacy timestamp.
			after = purgeCursor{Timestamp: rawTimestamp, ID: row.ID, Chat: row.ChatJID}
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil {
			return false, rowErr
		}
		if closeErr != nil {
			return false, closeErr
		}
		for _, row := range page {
			if !policy.Allows(row.ChatJID) {
				continue
			}
			if scanned >= maxScan {
				return true, nil
			}
			scanned++
			if !fn(row) {
				return true, nil
			}
		}
		if len(page) < pageSize {
			return false, nil
		}
	}
}

// purgeOne removes (or, in dry-run, measures) the cached file of one row.
// What counts as that file is decided where the download decides it
// (findCachedMedia, media_cache_path.go): a regular file under a plain name in
// the chat's own directory, nothing followed. The delete names the file inside
// the directory handle the lookup opened, so a chat directory swapped for a
// symlink between the lookup and the Remove cannot send it elsewhere.
func purgeOne(root *os.Root, row mediaRow, dryRun bool) PurgeResult {
	return purgeOneUsing(root, row, dryRun, nil)
}

func purgeOneUsing(root *os.Root, row mediaRow, dryRun bool, finder *cachedMediaFinder) PurgeResult {
	res := PurgeResult{MessageID: row.ID, ChatJID: row.ChatJID}
	if row.MediaType == "" || row.MediaType == "reaction" || row.MediaType == "poll_vote" {
		res.Reason = "not a media message"
		return res
	}
	if root == nil {
		res.Reason = "store directory unavailable"
		return res
	}
	lookup := findCachedMedia
	if finder != nil {
		lookup = func(_ *os.Root, chat string, names []string) (*cachedMedia, error) { return finder.find(chat, names) }
	}
	found, refused := lookup(root, chatMediaRel(row.ChatJID), mediaFileNames(row.MediaType, row.Timestamp, row.ID, row.Filename))
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, purgeMaxBodyBytes)).Decode(&req); err != nil {
			message := "Invalid request format"
			var oversized *http.MaxBytesError
			if errors.As(err, &oversized) {
				message = fmt.Sprintf("Request body must not exceed %d bytes", purgeMaxBodyBytes)
			}
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: message, DryRun: true})
			return
		}
		dryRun := req.DryRun == nil || *req.DryRun
		if req.Scope != "" || req.IncludeOrphans {
			if req.Scope != "status" || len(req.Items) > 0 || req.Cursor != "" || req.ChatJID != "" || req.MediaType != "" || req.OlderThanDays != 0 || req.MinBytes != 0 {
				writeError(w, 400, "scope=status is a standalone purge shortcut")
				return
			}
			result, err := b.purgeOperatorMedia(r.Context(), operatorMediaPurge{Type: "status", Chat: "status@broadcast", DryRun: req.DryRun, IncludeOrphans: req.IncludeOrphans})
			if err != nil {
				writeError(w, 503, "Status purge incomplete")
				return
			}
			writeJSON(w, 200, map[string]any{"success": true, "dry_run": result.DryRun, "purged_files": result.Files, "purged_bytes": result.FreedBytes, "orphan_files": result.OrphanFiles, "orphan_bytes": result.OrphanBytes, "failed": result.Failed, "matched": result.Files, "truncated": false})
			return
		}
		if len(req.Items) > purgeMaxItems {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: fmt.Sprintf("items must contain at most %d entries; use criteria for bulk purges", purgeMaxItems), DryRun: dryRun})
			return
		}
		after, err := decodePurgeCursor(req.Cursor)
		if err != nil || (req.Cursor != "" && len(req.Items) > 0) {
			writePurgeResponse(w, http.StatusBadRequest, MediaPurgeResponse{Message: "cursor must be a criteria next_cursor", DryRun: dryRun})
			return
		}
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
			chat, ok := authorizeChat(w, b.Policy, req.ChatJID, false)
			if !ok {
				return
			}
			req.ChatJID = chat.String()
		}

		var rows []mediaRow
		finder := &cachedMediaFinder{root: b.StoreRoot}
		defer finder.Close()
		var results []PurgeResult
		truncated, scanTruncated := false, false
		remaining, unreachable := 0, 0
		examined := 0
		nextCursor := ""
		scanLimit := b.purgeScanLimit()
		if len(req.Items) > 0 {
			seen := make(map[PurgeItem]bool)
			for index, item := range req.Items {
				if len(rows) >= purgeMaxFiles || examined >= scanLimit {
					remaining = len(req.Items) - index
					truncated = true
					scanTruncated = examined >= scanLimit
					break
				}
				examined++
				item.MessageID, item.ChatJID = strings.TrimSpace(item.MessageID), strings.TrimSpace(item.ChatJID)
				if item.MessageID == "" || item.ChatJID == "" {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "message_id and chat_jid are required"})
					continue
				}
				chat, err := canonicalChatJID(item.ChatJID, false)
				if err != nil {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: err.Error()})
					continue
				}
				item.ChatJID = chat.String()
				if !b.Policy.Allows(item.ChatJID) {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "chat not in " + chatPolicyEnv})
					continue
				}
				if seen[item] {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "duplicate"})
					continue
				}
				seen[item] = true
				row, err := b.Store.MediaRow(item.MessageID, item.ChatJID)
				if errors.Is(err, sql.ErrNoRows) {
					results = append(results, PurgeResult{MessageID: item.MessageID, ChatJID: item.ChatJID, Reason: "message not found"})
					continue
				}
				if err != nil {
					writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Failed to look up message: " + err.Error(), DryRun: dryRun})
					return
				}
				probe := purgeOneUsing(b.StoreRoot, row, true, finder)
				if probe.Purged {
					rows = append(rows, row)
				} else {
					results = append(results, probe)
				}
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
			var last mediaRow
			scanCut, err := b.Store.EachMediaRowMatching(req.ChatJID, before, after, req.MediaType, b.Policy, scanLimit, func(row mediaRow) bool {
				if r.Context().Err() != nil {
					return false // the caller hung up; stop walking the disk for it
				}
				last = row
				examined++
				probe := purgeOneUsing(b.StoreRoot, row, true, finder)
				switch {
				case probe.Purged && probe.Bytes >= req.MinBytes:
					rows = append(rows, row)
				case probe.Purged:
					// A sender-declared size never substitutes for cached bytes.
				case probe.Reason != purgeReasonNotCached:
					unreachable++
					if unreachable <= purgeMaxUnreachable {
						results = append(results, probe)
					}
				}
				return len(rows) < purgeMaxFiles
			})
			if err != nil {
				writePurgeResponse(w, http.StatusInternalServerError, MediaPurgeResponse{Message: "Failed to query media rows: " + err.Error(), DryRun: dryRun})
				return
			}
			scanTruncated = scanCut && examined >= scanLimit && len(rows) < purgeMaxFiles
			truncated = scanCut
			if scanCut {
				remaining = -1 // unknown: no quadratic scan of the tail
				nextCursor = encodePurgeCursor(last)
			}
		}

		resp := MediaPurgeResponse{Success: true, DryRun: dryRun, Truncated: truncated}
		for _, row := range rows {
			res := purgeOneUsing(b.StoreRoot, row, dryRun, finder)
			if res.Purged {
				resp.PurgedFiles++
				resp.PurgedBytes += res.Bytes
			} else if !dryRun {
				resp.Failed++ // the probe saw the file, so the removal is what failed
			}
			results = append(results, res)
		}
		// A file that cannot be removed stays cached and would head the next
		// call's selection again. When a real call removed nothing and failed, it
		// is not progress: say so instead of inviting the caller to repeat it.
		if !dryRun && resp.Failed > 0 && resp.PurgedFiles == 0 {
			resp.Truncated = false
			nextCursor = ""
		}
		resp.Matched = len(rows)
		resp.Remaining, resp.ScanTruncated, resp.Unreachable = remaining, scanTruncated, unreachable
		resp.NextCursor, resp.Examined = nextCursor, examined
		resp.Items = results
		if resp.Items == nil {
			resp.Items = []PurgeResult{}
		}
		more := ""
		switch {
		case !dryRun && resp.Failed > 0 && resp.PurgedFiles == 0:
			more = fmt.Sprintf("; %d file(s) could not be removed (see items), repeating will not help", resp.Failed)
		case remaining > 0:
			more = fmt.Sprintf("; %d named item(s) unexamined, submit those remaining items", remaining)
		case resp.NextCursor != "":
			more = fmt.Sprintf("; examined %d rows, continue the same criteria with cursor=next_cursor (remaining cached count unknown)", examined)
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
