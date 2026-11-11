package main

// POST /api/delete-for-me {"chat_jid": "...", "message_ids": ["..."], "confirm": true}
//
// WhatsApp's "Delete for me": the messages disappear from this account's phone
// and linked devices, any sender, any age; the other side keeps its copies.
// It is not /api/delete (revoke for everyone, or a local-only row drop).
//
// whatsmeow has the index and the action but no Build helper (upstream
// tulir/whatsmeow#702 closed unmerged), so the patch is built here: one
// regular_high mutation per message, index
// [deleteMessageForMe, chat, id, fromMe "1"|"0", participant], version 3 (as
// Baileys), DeleteMessageForMeAction{DeleteMedia, MessageTimestamp seconds}.
// This shape was verified on a live paired bridge by upstream VGP #293.
//
// Direct chats only: in a group the participant slot must name the sender of
// another member's message, a spelling nobody has verified yet. Every ID is
// anchored on its stored row (fromMe and timestamp come from there); one
// unknown ID refuses the whole batch, before anything is sent. The local rows
// go only after the server accepted the patch, cached media first.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// deleteForMeMaxIDs bounds one patch, as VGP #293 did per bridge batch.
const deleteForMeMaxIDs = 200

type deleteForMeRef struct {
	row    mediaRow // ID, ChatJID (the stored alias), MediaType, Timestamp, Filename
	fromMe bool
}

type deleteForMeDeps struct {
	archiveDeps
	cleanup func(ctx context.Context, refs []deleteForMeRef) (chatDeleteCleanup, error)
}

// buildDeleteMessageForMePatch builds the patch whatsmeow lacks a helper for.
func buildDeleteMessageForMePatch(chat types.JID, refs []deleteForMeRef) appstate.PatchInfo {
	mutations := make([]appstate.MutationInfo, 0, len(refs))
	for _, ref := range refs {
		fromMe := "0"
		if ref.fromMe {
			fromMe = "1"
		}
		mutations = append(mutations, appstate.MutationInfo{
			Index:   []string{appstate.IndexDeleteMessageForMe, chat.String(), ref.row.ID, fromMe, "0"},
			Version: 3,
			Value: &waSyncAction.SyncActionValue{
				DeleteMessageForMeAction: &waSyncAction.DeleteMessageForMeAction{
					DeleteMedia:      proto.Bool(true),
					MessageTimestamp: proto.Int64(ref.row.Timestamp.Unix()),
				},
			},
		})
	}
	return appstate.PatchInfo{Type: appstate.WAPatchRegularHigh, Mutations: mutations}
}

// deleteForMeRefs anchors every requested ID on its stored rows under the
// chat's aliases in one query: the first row (insertion order) builds the
// mutation, and every row, twin copies included, is what the local cleanup
// removes. Missing IDs are returned so the caller can refuse the batch; a
// reaction or vote pointer row is not a message the phone can delete.
func (store *MessageStore) deleteForMeRefs(ctx context.Context, chats []string, ids []string) (patch, local []deleteForMeRef, missing []string, err error) {
	query := `SELECT id, chat_jid, is_from_me, timestamp, COALESCE(media_type, ''), COALESCE(filename, '') FROM messages WHERE chat_jid IN ` + placeholders(len(chats)) + ` AND id IN ` + placeholders(len(ids)) + ` ORDER BY rowid` //nolint:gosec // Only "?" placeholders are concatenated; every value is bound.
	rows, err := store.db.QueryContext(ctx, query, stringArgs(append(append([]string(nil), chats...), ids...))...)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := make(map[string][]deleteForMeRef, len(ids))
	for rows.Next() {
		var ref deleteForMeRef
		var rawTime any
		if err := rows.Scan(&ref.row.ID, &ref.row.ChatJID, &ref.fromMe, &rawTime, &ref.row.MediaType, &ref.row.Filename); err != nil {
			_ = rows.Close()
			return nil, nil, nil, err
		}
		ref.row.Timestamp = anchorTime(rawTime)
		byID[ref.row.ID] = append(byID[ref.row.ID], ref)
	}
	closeErr := rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	if closeErr != nil {
		return nil, nil, nil, closeErr
	}
	for _, id := range ids {
		found := byID[id]
		if len(found) == 0 {
			missing = append(missing, id)
			continue
		}
		switch first := found[0]; {
		case first.row.MediaType == "reaction" || first.row.MediaType == "poll_vote":
			return nil, nil, nil, archiveAnchorError("message " + id + " is a reaction or poll vote, not a message")
		case first.row.Timestamp.IsZero():
			return nil, nil, nil, archiveAnchorError("message " + id + " has an invalid timestamp")
		}
		patch = append(patch, found[0])
		local = append(local, found...)
	}
	return patch, local, missing, nil
}

func handleDeleteForMe(deps deleteForMeDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policy := deps.policy.effective(r.Context())
		ctx, cancel := requestContext(r, archiveDeadline)
		defer cancel()
		var req struct {
			ChatJID    string   `json:"chat_jid"`
			MessageIDs []string `json:"message_ids"`
			Confirm    bool     `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		ids := make([]string, 0, len(req.MessageIDs))
		seen := make(map[string]bool, len(req.MessageIDs))
		for _, id := range req.MessageIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				writeErrorCode(w, http.StatusBadRequest, "invalid_argument", "message_ids must be non-empty and unique")
				return
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 || len(ids) > deleteForMeMaxIDs {
			writeErrorCode(w, http.StatusBadRequest, "invalid_argument", "message_ids takes 1 to 200 IDs")
			return
		}
		if !req.Confirm {
			writeErrorCode(w, http.StatusBadRequest, "invalid_argument", "Delete for me is irreversible: pass confirm=true")
			return
		}
		chat, ok := authorizeChat(w, policy, req.ChatJID, false)
		if !ok {
			return
		}
		if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer {
			writeErrorCode(w, http.StatusBadRequest, "invalid_argument", "Delete for me supports direct chats only for now")
			return
		}
		if !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected. Please wait for reconnection.")
			return
		}
		expired := "Delete for me request expired before send; nothing sent; safe to retry"
		target, chats, ok := deps.resolveChatAliases(ctx, w, policy, chat, expired)
		if !ok {
			return
		}
		refs, local, missing, err := deps.store.deleteForMeRefs(ctx, chats, ids)
		var invalid archiveAnchorError
		switch {
		case ctx.Err() != nil:
			writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", expired)
			return
		case errors.As(err, &invalid):
			writeErrorCode(w, http.StatusUnprocessableEntity, "invalid_argument", err.Error()+"; nothing sent")
			return
		case err != nil:
			writeErrorCode(w, http.StatusInternalServerError, "internal", "Cannot read the messages: "+err.Error())
			return
		case len(missing) > 0:
			writeErrorCode(w, http.StatusNotFound, "not_found", "Not in the local archive for this chat, nothing sent: "+strings.Join(missing, ", "))
			return
		}
		err = deps.send(ctx, buildDeleteMessageForMePatch(target, refs))
		payload := map[string]any{"deleted_for_me": len(refs), "chat_jid": chat.String()}
		if appStateAccepted(err) {
			cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(r.Context()), chatDeleteCleanupBudget)
			result, cleanErr := deps.cleanup(cleanCtx, local)
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
		writeAppStateResult(w, err, payload, "Delete for me")
	}
}

// deleteForMeLocal mirrors an accepted patch: cached media first (while the
// rows exist), then the rows whose files are gone and every non-media row.
func (b *Bridge) deleteForMeLocal(ctx context.Context, refs []deleteForMeRef) (chatDeleteCleanup, error) {
	var result chatDeleteCleanup
	var media, plain []mediaRow
	for _, ref := range refs {
		if cacheableMediaType[ref.row.MediaType] {
			media = append(media, ref.row)
		} else {
			plain = append(plain, ref.row)
		}
	}
	gone, removedMedia, failed, mediaErr := b.removeCachedMedia(ctx, media)
	result.MediaRemoved, result.MediaFailed = removedMedia, failed
	removed, err := b.Store.deleteMessageRows(ctx, append(plain, gone...))
	result.Messages = removed
	if err != nil {
		return result, err
	}
	return result, mediaErr
}

// deleteMessageRows removes the listed rows in one transaction, then the
// reactions and votes aimed at them, orphaned polls and an emptied chat row.
func (store *MessageStore) deleteMessageRows(ctx context.Context, rows []mediaRow) (int64, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	removed, err := deleteMessageKeys(ctx, tx, rows)
	if err != nil {
		return 0, err
	}
	byChat := map[string][]string{}
	for _, row := range rows {
		byChat[row.ChatJID] = append(byChat[row.ChatJID], row.ID)
	}
	for chat, ids := range byChat {
		if err := deleteChatOrphans(ctx, tx, []string{chat}, ids); err != nil {
			return 0, err
		}
	}
	return removed, tx.Commit()
}
