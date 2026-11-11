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
	"database/sql"
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
	gone, mediaRemoved, mediaFailed, mediaErr := b.removeCachedMedia(ctx, media)
	result.MediaRemoved, result.MediaFailed = mediaRemoved, mediaFailed
	removed, err := b.Store.deleteChatRows(ctx, chats, upTo, gone)
	result.Messages = removed
	if err != nil {
		return result, err
	}
	return result, mediaErr
}

// removeCachedMedia deletes the cached files of rows that still exist (local or
// S3) and returns the rows that may now be deleted: file removed or never
// cached. A row whose file could not be removed is left out of gone, so it
// keeps pointing at that file for a later purge.
func (b *Bridge) removeCachedMedia(ctx context.Context, media []mediaRow) (gone []mediaRow, removed, failed int, err error) {
	storage := b.mediaStorage()
	const batch = 256
	for start := 0; start < len(media) && err == nil; start += batch {
		rows := media[start:min(start+batch, len(media))]
		results, deleteErr := storage.Delete(ctx, rows, false)
		byKey := make(map[[2]string]PurgeResult, len(results))
		for _, res := range results {
			byKey[[2]string{res.MessageID, res.ChatJID}] = res
		}
		for _, row := range rows {
			res, ok := byKey[[2]string{row.ID, row.ChatJID}]
			switch {
			case ok && res.Purged:
				removed++
				gone = append(gone, row)
			case ok && res.Reason == purgeReasonNotCached:
				gone = append(gone, row)
			default:
				failed++
			}
		}
		if deleteErr != nil {
			err = deleteErr
			failed += len(media) - min(start+batch, len(media))
		}
	}
	return gone, removed, failed, err
}

// cacheableMediaTypeNames are the kinds EachMediaRowMatching walks; both the
// SQL list and the lookup below derive from this one slice.
var cacheableMediaTypeNames = []string{"image", "video", "audio", "document", "sticker"}

var (
	cacheableMediaTypes = "('" + strings.Join(cacheableMediaTypeNames, "','") + "')"
	cacheableMediaType  = func() map[string]bool {
		out := make(map[string]bool, len(cacheableMediaTypeNames))
		for _, name := range cacheableMediaTypeNames {
			out[name] = true
		}
		return out
	}()
)

func placeholders(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

func stringArgs(values []string, extra ...any) []any {
	out := make([]any, 0, len(values)+len(extra))
	for _, value := range values {
		out = append(out, value)
	}
	return append(out, extra...)
}

// deleteChatRows deletes, in one transaction, the chat's non-media rows up to
// the anchor and the listed media rows, then what only they referenced.
func (store *MessageStore) deleteChatRows(ctx context.Context, chats []string, upTo time.Time, media []mediaRow) (int64, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	bound := dbTime(upTo)
	in := placeholders(len(chats))
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE chat_jid IN `+in+` AND timestamp <= ? AND COALESCE(media_type, '') NOT IN `+cacheableMediaTypes, stringArgs(chats, bound)...) //nolint:gosec // Only "?" placeholders and a literal list are concatenated; every value is bound.
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()
	n, err := deleteMessageKeys(ctx, tx, media)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calls WHERE chat_jid IN `+in+` AND timestamp <= ?`, stringArgs(chats, bound)...); err != nil { //nolint:gosec // Placeholders only.
		return 0, err
	}
	if err := deleteChatOrphans(ctx, tx, chats, nil); err != nil {
		return 0, err
	}
	return removed + n, tx.Commit()
}

// deleteMessageKeys removes the listed (id, chat) rows inside tx.
func deleteMessageKeys(ctx context.Context, tx *sql.Tx, rows []mediaRow) (int64, error) {
	var removed int64
	for _, row := range rows {
		res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ? AND chat_jid = ?`, row.ID, row.ChatJID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return removed, nil
}

// deleteChatOrphans removes, for the given chats, what only deleted messages
// referenced: reaction and vote pointer rows aimed at deletedIDs (filename is
// the pre-migration spelling of the target), polls and votes whose poll
// message is gone, and the chat row once no message is left under it.
func deleteChatOrphans(ctx context.Context, tx *sql.Tx, chats, deletedIDs []string) error {
	in := placeholders(len(chats))
	if len(deletedIDs) > 0 {
		query := `DELETE FROM messages WHERE chat_jid IN ` + in + ` AND media_type IN ('reaction','poll_vote') AND COALESCE(target_message_id, filename) IN ` + placeholders(len(deletedIDs)) //nolint:gosec // Only "?" placeholders and literals are concatenated; every value is bound.
		if _, err := tx.ExecContext(ctx, query, stringArgs(append(append([]string(nil), chats...), deletedIDs...))...); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`DELETE FROM polls WHERE chat_jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = polls.message_id AND m.chat_jid = polls.chat_jid)`,
		`DELETE FROM poll_votes WHERE chat_jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = poll_votes.poll_message_id AND m.chat_jid = poll_votes.chat_jid)`,
		`DELETE FROM chats WHERE jid IN ` + in + ` AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = chats.jid)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, stringArgs(chats)...); err != nil {
			return err
		}
	}
	return nil
}
