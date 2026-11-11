package main

// POST /api/chat/delete {"chat_jid": "...", "delete_media": bool, "confirm": true}
//
// The phone's "Delete chat", sent as a deleteChat app-state patch anchored on
// the newest stored message (the same anchor archive uses). The phone removes
// everything up to that anchor; so does the local cleanup, which runs only
// after the server accepted the patch: rows of every stored alias with a
// timestamp at or before the anchor, their calls and polls, and
// their cached media (removed first, see deleteChatLocal). Anything newer
// than the anchor stays on both sides.
//
// Irreversible. No anchor means no patch: whatsmeow would fall back to "now",
// a range the phone never saw. An uncertain outcome is never retried here.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/appstate"
)

// Local cleanup gets its own budget: the patch is already accepted, so a
// cancelled request must not leave the archive half-deleted. Send (20 s) plus
// cleanup stays under the MCP client's 60 s timeout for this one call.
const chatDeleteCleanupBudget = 15 * time.Second

type chatDeleteCleanup struct {
	Messages     int64
	MediaRemoved int
	MediaFailed  int
}

type chatDeleteDeps struct {
	archiveDeps
	cleanup func(ctx context.Context, chats []string, upTo time.Time) (chatDeleteCleanup, error)
}

func handleDeleteChat(deps chatDeleteDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policy := deps.policy.effective(r.Context())
		ctx, cancel := requestContext(r, archiveDeadline)
		defer cancel()
		var req struct {
			ChatJID     string `json:"chat_jid"`
			DeleteMedia bool   `json:"delete_media"`
			Confirm     bool   `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if strings.TrimSpace(req.ChatJID) == "" {
			writeError(w, http.StatusBadRequest, "chat_jid is required")
			return
		}
		if !req.Confirm {
			writeErrorCode(w, http.StatusBadRequest, "invalid_argument", "Deleting a chat is irreversible: pass confirm=true")
			return
		}
		anchor, ok := deps.prepareChatAppState(ctx, w, policy, req.ChatJID, chatAppStateLabels{
			verb: "deleted", noun: "chat delete", action: "Chat delete",
		})
		if !ok {
			return
		}
		err := deps.send(ctx, appstate.BuildDeleteChat(anchor.target, anchor.ts, anchor.key, req.DeleteMedia))
		payload := map[string]any{"deleted": true, "delete_media": req.DeleteMedia, "anchor_timestamp": dbTime(anchor.ts)}
		if appStateAccepted(err) {
			// Accepted by the server: mirror it locally on a budget of its own.
			cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(r.Context()), chatDeleteCleanupBudget)
			result, cleanErr := deps.cleanup(cleanCtx, anchor.chats, anchor.ts)
			cleanCancel()
			payload["local_messages_removed"] = result.Messages
			payload["local_media_removed"] = result.MediaRemoved
			if result.MediaFailed > 0 {
				payload["local_media_failed"] = result.MediaFailed
			}
			if cleanErr != nil {
				payload["local_cleanup_error"] = "Deleted on WhatsApp; local archive cleanup failed, do not resend: " + cleanErr.Error()
			}
		}
		writeAppStateResult(w, err, payload, "Chat delete")
	}
}

// deleteChatLocal removes what the accepted deleteChat patch removed on the
// phone. Cached media goes first, while the rows still exist: the file names
// derive from them, and the S3 references cascade away with the rows. A media
// row is deleted only when its file is gone (or was never cached), so a file
// that could not be removed keeps a row a later purge can still find; a media
// row stored after the collection is left alone for the same reason.
func (b *Bridge) deleteChatLocal(ctx context.Context, chats []string, upTo time.Time) (chatDeleteCleanup, error) {
	var result chatDeleteCleanup
	var media []mediaRow
	before := upTo.Add(time.Second) // EachMediaRowMatching is exclusive; stored times have seconds.
	for _, chat := range chats {
		if _, err := b.Store.EachMediaRowMatchingContext(ctx, chat, before, purgeCursor{}, "", chatPolicy{}, math.MaxInt, func(row mediaRow) bool {
			media = append(media, row)
			return true
		}); err != nil {
			return result, err
		}
	}
	var gone []mediaRow
	var mediaErr error
	storage := b.mediaStorage()
	const batch = 256
	for start := 0; start < len(media) && mediaErr == nil; start += batch {
		rows := media[start:min(start+batch, len(media))]
		results, err := storage.Delete(ctx, rows, false)
		byKey := make(map[[2]string]PurgeResult, len(results))
		for _, res := range results {
			byKey[[2]string{res.MessageID, res.ChatJID}] = res
		}
		for _, row := range rows {
			res, ok := byKey[[2]string{row.ID, row.ChatJID}]
			switch {
			case ok && res.Purged:
				result.MediaRemoved++
				gone = append(gone, row)
			case ok && res.Reason == purgeReasonNotCached:
				gone = append(gone, row)
			default:
				result.MediaFailed++
			}
		}
		if err != nil {
			mediaErr = err
			result.MediaFailed += len(media) - min(start+batch, len(media))
		}
	}
	removed, err := b.Store.deleteChatRows(ctx, chats, upTo, gone)
	result.Messages = removed
	if err != nil {
		return result, err
	}
	return result, mediaErr
}

// cacheableMediaTypes are the kinds EachMediaRowMatching walks.
const cacheableMediaTypes = `('image','video','audio','document','sticker')`

// deleteChatRows deletes, in one transaction, the chat's non-media rows up to
// the anchor and the listed media rows, then what only they referenced. The
// chat row itself goes only when no message is left under it.
func (store *MessageStore) deleteChatRows(ctx context.Context, chats []string, upTo time.Time, media []mediaRow) (int64, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	bound := dbTime(upTo)
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(chats)), ",") + ")"
	args := func(extra ...any) []any {
		out := make([]any, 0, len(chats)+len(extra))
		for _, chat := range chats {
			out = append(out, chat)
		}
		return append(out, extra...)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE chat_jid IN `+in+` AND timestamp <= ? AND COALESCE(media_type, '') NOT IN `+cacheableMediaTypes, args(bound)...) //nolint:gosec // Only "?" placeholders and a literal list are concatenated; every value is bound.
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	for _, row := range media {
		res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ? AND chat_jid = ?`, row.ID, row.ChatJID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	for _, stmt := range []string{
		`DELETE FROM calls WHERE chat_jid IN ` + in + ` AND timestamp <= ?`,
		`DELETE FROM polls WHERE chat_jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = polls.message_id AND m.chat_jid = polls.chat_jid)`,
		`DELETE FROM poll_votes WHERE chat_jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = poll_votes.poll_message_id AND m.chat_jid = poll_votes.chat_jid)`,
		`DELETE FROM chats WHERE jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = chats.jid)`,
	} {
		extra := []any{}
		if strings.Contains(stmt, "timestamp <= ?") {
			extra = append(extra, bound)
		}
		if _, err := tx.ExecContext(ctx, stmt, args(extra...)...); err != nil {
			return 0, err
		}
	}
	return removed, tx.Commit()
}
