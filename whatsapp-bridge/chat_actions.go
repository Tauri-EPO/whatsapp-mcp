package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// Outbound chat actions: reactions and typing presence. Read receipts live in
// mark_read.go. Registered in rest.go; all behind the token, Host and chat
// allow-list checks.

// handleReact serves POST /api/react.
func (b *Bridge) handleReact() http.HandlerFunc {
	client := b.Client
	return func(w http.ResponseWriter, r *http.Request) {
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipient == "" || req.MessageID == "" || req.Emoji == nil {
			writeError(w, http.StatusBadRequest, "recipient, message_id, and emoji are required")
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.Recipient) {
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid recipient JID: %v", err))
			return
		}
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
		if _, err := client.SendMessage(ctx, chatJID, msg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}

// handleTyping serves POST /api/typing.
func (b *Bridge) handleTyping() http.HandlerFunc {
	client := b.Client
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

		// Validate request
		if req.Recipient == "" {
			writeError(w, http.StatusBadRequest, "Recipient is required")
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.Recipient) {
			return
		}

		// Create JID for recipient
		var recipientJID types.JID
		var err error

		// Check if recipient is a JID
		if strings.Contains(req.Recipient, "@") {
			recipientJID, err = types.ParseJID(req.Recipient)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"message": fmt.Sprintf("Error parsing JID: %v", err),
				})
				return
			}
		} else {
			// Create JID from phone number
			recipientJID = types.JID{
				User:   req.Recipient,
				Server: "s.whatsapp.net",
			}
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
		err = client.SendChatPresence(ctx, recipientJID, state, types.ChatPresenceMediaText)

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
