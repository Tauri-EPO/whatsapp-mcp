package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"net/http"

	"go.mau.fi/whatsmeow/types"
)

type chatPresenceSender func(context.Context, types.JID, types.ChatPresence, types.ChatPresenceMedia) error

// Outbound chat actions: reactions and typing presence. Read receipts live in
// mark_read.go. Registered in rest.go; all behind the token, Host and chat
// allow-list checks.

// handleReact serves POST /api/react.
func (b *Bridge) handleReact() http.HandlerFunc {
	client := b.currentClient()
	return func(w http.ResponseWriter, r *http.Request) {
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipient == "" || req.MessageID == "" || req.Emoji == nil {
			writeError(w, http.StatusBadRequest, "recipient, message_id, and emoji are required")
			return
		}
		chatJID, ok := authorizeChat(w, b.Policy, req.Recipient, false)
		if !ok {
			return
		}
		var err error
		var senderJID types.JID
		switch {
		case req.FromMe:
			if client.Store.ID == nil {
				writeError(w, http.StatusServiceUnavailable, "Not logged in")
				return
			}
			senderJID = *client.Store.ID
		case req.SenderJID != "":
			if senderJID, err = types.ParseJID(req.SenderJID); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid sender_jid: %v", err))
				return
			}
			if senderJID.User == "" || senderJID.Server == "" {
				writeError(w, http.StatusBadRequest, "Invalid sender_jid")
				return
			}
		default:
			if chatJID.Server == types.GroupServer {
				writeError(w, http.StatusBadRequest, "sender_jid is required for group reactions when from_me is false")
				return
			}
			senderJID = chatJID
		}
		msg := client.BuildReaction(chatJID, senderJID, req.MessageID, *req.Emoji)
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		if !b.allowSendAction(w, r.WithContext(ctx), chatJID.String()) {
			return
		}
		send := b.sendMessage
		if send == nil {
			send = func(ctx context.Context, jid types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
				return client.SendMessage(ctx, jid, msg)
			}
		}
		if _, err := send(ctx, chatJID, msg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}

// handleTyping serves POST /api/typing.
func (b *Bridge) handleTyping() http.HandlerFunc {
	client := b.currentClient()
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse the request body
		var req struct {
			Recipient string `json:"recipient"`
			IsTyping  bool   `json:"is_typing"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		normalized, err := normalizePhoneRecipient(req.Recipient)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Recipient = normalized

		// Validate request
		if req.Recipient == "" {
			writeError(w, http.StatusBadRequest, "Recipient is required")
			return
		}
		recipientJID, ok := authorizeChat(w, b.Policy, req.Recipient, true)
		if !ok {
			return
		}

		// Determine the chat presence state
		var state types.ChatPresence
		if req.IsTyping {
			state = types.ChatPresenceComposing
		} else {
			state = types.ChatPresencePaused
		}

		// Send the chat presence update
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		sendPresence := b.chatPresence
		if sendPresence == nil {
			sendPresence = client.SendChatPresence
		}
		err = sendPresence(ctx, recipientJID, state, types.ChatPresenceMediaText)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Send response
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("Failed to send typing indicator: %v", err),
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": fmt.Sprintf("Typing indicator set to %v", req.IsTyping),
			})
		}
	}
}
