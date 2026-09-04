package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"net/http"
	"strings"
	"time"
)

// Outbound chat actions: read receipts, reactions, typing presence.
// Registered in rest.go; all behind the token, Host and chat allow-list checks.

// handleMarkRead serves POST /api/mark-read.
func (b *Bridge) handleMarkRead() http.HandlerFunc {
	client, messageStore := b.Client, b.Store
	return func(w http.ResponseWriter, r *http.Request) {
		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if req.ChatJID == "" || len(req.MessageIDs) == 0 {
			writeError(w, http.StatusBadRequest, "chat_jid and message_ids are required")
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.ChatJID) {
			return
		}

		messageIDs := make([]types.MessageID, len(req.MessageIDs))
		for i, id := range req.MessageIDs {
			if strings.TrimSpace(id) == "" {
				writeError(w, http.StatusBadRequest, "message_ids must not contain empty values")
				return
			}
			messageIDs[i] = types.MessageID(id)
		}

		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			writeError(w, http.StatusBadRequest, "Invalid chat_jid")
			return
		}

		senderJID := types.EmptyJID
		if req.SenderJID != "" {
			if strings.Contains(req.SenderJID, "@") {
				senderJID, err = types.ParseJID(req.SenderJID)
			} else {
				senderJID = types.NewJID(strings.TrimSpace(req.SenderJID), types.DefaultUserServer)
			}
			if err != nil || senderJID.User == "" || senderJID.Server == "" {
				writeError(w, http.StatusBadRequest, "Invalid sender_jid")
				return
			}
		} else if chatJID.Server == types.GroupServer {
			writeError(w, http.StatusBadRequest, "sender_jid is required for group read receipts")
			return
		}

		readAt := time.Now()
		if req.Timestamp != "" {
			readAt, err = time.Parse(time.RFC3339, req.Timestamp)
			if err != nil {
				writeError(w, http.StatusBadRequest, "timestamp must be RFC 3339")
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if !client.IsConnected() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(SendMessageResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

		// Validate against the storage (phone-form) chat JID before any
		// external side effect. LID rewrite happens only for the receipt.
		if err := messageStore.ValidateInboundMarkRead(req.ChatJID, req.SenderJID, req.MessageIDs); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// MCP storage normalizes chats/senders to phone JIDs; MarkRead routes
		// the receipt `to`/`participant` as given, so resolve PN -> LID the
		// same way sendWhatsAppMessage does or migrated contacts silently fail.
		chatJID, err = resolveRecipientJID(client, req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			writeError(w, http.StatusBadRequest, "Invalid chat_jid")
			return
		}
		if req.SenderJID != "" {
			senderJID, err = resolveRecipientJID(client, req.SenderJID)
			if err != nil || senderJID.User == "" || senderJID.Server == "" {
				writeError(w, http.StatusBadRequest, "Invalid sender_jid")
				return
			}
		}

		if err := client.MarkRead(context.Background(), messageIDs, readAt, chatJID, senderJID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
			return
		}

		// Advance the local read marker immediately so list_chats unread
		// clears without waiting for the self-read receipt round-trip.
		localReadAt := readAt
		if ts, ok, tsErr := messageStore.MaxMessageTimestamp(req.ChatJID, req.MessageIDs); tsErr == nil && ok {
			localReadAt = ts
		}
		if err := messageStore.MarkChatRead(req.ChatJID, localReadAt); err != nil {
			// Receipt already sent; log but still report success to the caller.
			bridgeLog.Warnf("failed to persist local read marker for %s: %v", req.ChatJID, err)
		}

		_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: true, Message: "Messages marked as read"})
	}
}

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
		if _, err := client.SendMessage(context.Background(), chatJID, msg); err != nil {
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
		err = client.SendChatPresence(context.Background(), recipientJID, state, types.ChatPresenceMediaText)

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
