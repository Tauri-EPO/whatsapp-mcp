package main

// POST /api/delete — revoke a sent message for everyone, or drop it from the
// local archive only.
//
//	{"chat_jid": "...", "message_id": "...", "for_everyone": true|false}
//
// for_everyone=true sends WhatsApp's REVOKE protocol message (the "Delete for
// everyone" action). WhatsApp only honours that for messages this account
// sent, so the bridge refuses anything else up front instead of letting the
// phone silently ignore it. The local row keeps its content and gets
// deleted_at set, exactly as when a revoke arrives from another device.
//
// for_everyone=false removes the row from messages.db only. Nothing is sent
// to WhatsApp; the message stays on every phone. Media already downloaded to
// store/ is left in place.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type DeleteMessageRequest struct {
	ChatJID     string `json:"chat_jid"`
	MessageID   string `json:"message_id"`
	ForEveryone bool   `json:"for_everyone"`
}

type DeleteMessageResponse struct {
	Success     bool   `json:"success"`
	Message     string `json:"message"`
	ForEveryone bool   `json:"for_everyone"`
}

// revokeFunc abstracts whatsmeow's RevokeMessage for testing.
type revokeFunc func(ctx context.Context, chat types.JID, id types.MessageID) error

// DeleteMessageRow removes a message from the local archive. Returns
// sql.ErrNoRows when nothing matched.
func (store *MessageStore) DeleteMessageRow(messageID, chatJID string) error {
	res, err := store.db.Exec(`DELETE FROM messages WHERE id = ? AND chat_jid = ?`, messageID, chatJID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func writeDeleteResponse(w http.ResponseWriter, status int, resp DeleteMessageResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func handleDeleteMessage(store *MessageStore, revoke revokeFunc, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req DeleteMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeDeleteResponse(w, http.StatusBadRequest, DeleteMessageResponse{Message: "Invalid request format"})
			return
		}
		req.ChatJID, req.MessageID = strings.TrimSpace(req.ChatJID), strings.TrimSpace(req.MessageID)
		if req.ChatJID == "" || req.MessageID == "" {
			writeDeleteResponse(w, http.StatusBadRequest, DeleteMessageResponse{Message: "chat_jid and message_id are required"})
			return
		}
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil || chat.User == "" {
			writeDeleteResponse(w, http.StatusBadRequest, DeleteMessageResponse{Message: "Invalid chat_jid: " + req.ChatJID})
			return
		}
		if rejectByChatPolicy(w, policy, chat.String()) {
			return
		}

		if !req.ForEveryone {
			if err := store.DeleteMessageRow(req.MessageID, req.ChatJID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeDeleteResponse(w, http.StatusNotFound, DeleteMessageResponse{Message: "Message not found in local archive"})
					return
				}
				writeDeleteResponse(w, http.StatusInternalServerError, DeleteMessageResponse{Message: "Failed to delete local row: " + err.Error()})
				return
			}
			writeDeleteResponse(w, http.StatusOK, DeleteMessageResponse{Success: true, Message: "Message removed from the local archive only (still visible on WhatsApp)"})
			return
		}

		isFromMe, err := store.GetMessageIsFromMe(req.MessageID, req.ChatJID)
		if err != nil {
			writeDeleteResponse(w, http.StatusInternalServerError, DeleteMessageResponse{Message: "Failed to look up message: " + err.Error(), ForEveryone: true})
			return
		}
		if isFromMe == nil {
			writeDeleteResponse(w, http.StatusNotFound, DeleteMessageResponse{Message: "Message not found in local archive; only messages the bridge has seen can be revoked", ForEveryone: true})
			return
		}
		if !*isFromMe {
			writeDeleteResponse(w, http.StatusForbidden, DeleteMessageResponse{Message: "Only messages sent by this account can be deleted for everyone", ForEveryone: true})
			return
		}
		if err := revoke(r.Context(), chat, req.MessageID); err != nil {
			writeDeleteResponse(w, http.StatusBadGateway, DeleteMessageResponse{Message: "Revoke failed: " + err.Error(), ForEveryone: true})
			return
		}
		if err := store.MarkMessageDeleted(req.MessageID, req.ChatJID, time.Now()); err != nil {
			// The revoke went out; report success but mention the bookkeeping miss.
			writeDeleteResponse(w, http.StatusOK, DeleteMessageResponse{Success: true, Message: "Message revoked for everyone (local deleted_at not updated: " + err.Error() + ")", ForEveryone: true})
			return
		}
		writeDeleteResponse(w, http.StatusOK, DeleteMessageResponse{Success: true, Message: "Message deleted for everyone", ForEveryone: true})
	}
}
