package main

// REST API: route table, handlers and the HTTP server. Auth and Host
// validation live in auth.go / rest_bind.go, outbound path checks in
// media_path.go, feature endpoints next to their features.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// MarkReadRequest is the request body for the /api/mark-read endpoint.
type MarkReadRequest struct {
	MessageIDs []string `json:"message_ids"`
	ChatJID    string   `json:"chat_jid"`
	SenderJID  string   `json:"sender_jid,omitempty"`
	Timestamp  string   `json:"timestamp,omitempty"`
}

// ReactRequest is the request body for the /api/react endpoint.
type ReactRequest struct {
	Recipient string  `json:"recipient"`  // chat JID
	MessageID string  `json:"message_id"` // ID of the message being reacted to
	FromMe    bool    `json:"from_me"`    // whether the reacted-to message was sent by us
	SenderJID string  `json:"sender_jid"` // full JID of the reacted-to message's sender
	Emoji     *string `json:"emoji"`      // reaction emoji; empty string removes the reaction
}

// Start a REST API server to expose the WhatsApp client functionality.
//
// Auth: every handler is wrapped in withAuth, which enforces both a
// bearer-token check and a Host-header allow-list (loopback only). See
// auth.go for the rationale.
//
// Outbound media: req.MediaPath in /api/send is validated against
// allowedMediaRoots before sendWhatsAppMessage ever sees it. See
// media_path.go.
func (b *Bridge) newRESTMux(port int, token string, allowedMediaRoots []string) *http.ServeMux {
	client, messageStore := b.Client, b.Store
	allowedHosts, hostWarning := buildHostAllowList(port, b.RESTBind, b.RESTAllowedHosts)
	if hostWarning != "" {
		bridgeLog.Warnf("%s", hostWarning)
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return withAuth(token, allowedHosts, h)
	}
	mux := http.NewServeMux()

	// On-demand history sync endpoint (see history_ondemand.go)
	registerHistoryEndpoint(mux, auth, client, messageStore)

	// Health check endpoint
	// Liveness: the process serves requests. Always 200 once the listener is up,
	// with the connection state in the body — so a container awaiting its QR
	// scan is "healthy" (alive) rather than "unhealthy" (broken). Readiness
	// (/api/ready) is what to poll before sending.
	mux.HandleFunc("/api/health", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthStatus(client, b.startedAt, b.storeStats))
	}))

	// Build identity; unauthenticated on purpose (see version.go).
	mux.HandleFunc("/api/version", handleVersion(buildInfo(messageStore != nil && messageStore.fts)))

	// Readiness: 200 only while paired AND connected. whatsmeow reports
	// IsConnected() as soon as the websocket is up, which includes the QR
	// pairing phase, so "connected" alone is not "usable".
	mux.HandleFunc("/api/ready", auth(func(w http.ResponseWriter, r *http.Request) {
		status := healthStatus(client, b.startedAt, b.storeStats)
		code := http.StatusOK
		if status["status"] != "ok" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, status)
	}))

	// Group participants (see group_members.go). Needs a live connection.
	mux.HandleFunc("/api/group/members", auth(handleGroupMembers(
		func(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
			if client == nil || !client.IsConnected() {
				return nil, errors.New("WhatsApp client is not connected")
			}
			return client.GetGroupInfo(ctx, jid)
		},
		storeContactName(client),
		b.Policy,
	)))

	// Edit an own message / forward a message (edit_forward.go).
	mux.HandleFunc("/api/edit", auth(handleEditMessage(messageStore,
		func(ctx context.Context, chat types.JID, id types.MessageID, text string) error {
			if client == nil || !client.IsConnected() {
				return errors.New("WhatsApp client is not connected")
			}
			_, err := client.SendMessage(ctx, chat, client.BuildEdit(chat, id, &waE2E.Message{Conversation: proto.String(text)}))
			return err
		},
		b.Policy,
	)))
	mux.HandleFunc("/api/forward", auth(handleForwardMessage(forwardDeps{
		lookup:   messageStore.messageContentLookup,
		download: b.DownloadMedia,
		send:     b.Send,
	}, b.Policy)))

	// Group management: participants, subject/description, invite link, leave (group_manage.go).
	registerGroupManagement(mux, auth, liveGroupOps(client), b.Policy)

	// Delete a message: revoke for everyone (own messages) or drop the local
	// row only. See delete_message.go.
	mux.HandleFunc("/api/delete", auth(handleDeleteMessage(messageStore,
		func(ctx context.Context, chat types.JID, id types.MessageID) error {
			if client == nil || !client.IsConnected() {
				return errors.New("WhatsApp client is not connected")
			}
			// Revoke = a protocol message keyed to the original (own) message.
			_, err := client.SendMessage(ctx, chat, client.BuildRevoke(chat, types.EmptyJID, id))
			return err
		},
		b.Policy,
	)))

	// Poll results (see polls.go).
	mux.HandleFunc("/api/poll", auth(handlePollResults(messageStore, b.Policy)))

	// Handler for sending messages
	mux.HandleFunc("/api/send", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		bridgeLog.Debugf("→ /api/send from=%q user_agent=%q", r.RemoteAddr, r.UserAgent())

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.Recipient) {
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		// Validate and canonicalize media_path against the configured roots
		// before reading. This prevents the bridge from being used as a
		// generic file-read primitive (e.g. media_path=/Users/x/.ssh/id_rsa).
		// Only the canonical path ever reaches sendWhatsAppMessage; the raw
		// request value is never used as a file path.
		resolvedMediaPath := ""
		if req.MediaPath != "" {
			canonical, mpErr := validateMediaPath(req.MediaPath, allowedMediaRoots)
			if mpErr != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(SendMessageResponse{
					Success: false,
					Message: fmt.Sprintf("media_path rejected: %v", mpErr),
				})
				return
			}
			resolvedMediaPath = canonical
		}

		// Avoid logging req.Message verbatim — it's user content and may
		// contain secrets the user pasted into a chat.
		bridgeLog.Debugf("→ /api/send recipient=%q message_len=%d has_media=%v",
			req.Recipient, len(req.Message), resolvedMediaPath != "")

		// Send the message
		success, message, sent := b.Send(req.Recipient, req.Message, resolvedMediaPath, req.QuotedMessageID, req.QuotedSenderJID, req.QuotedContent, req.Mentions)
		bridgeLog.Debugf("← /api/send success=%v status=%q id=%q", success, message, sent.ID)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		resp := SendMessageResponse{Success: success, Message: message}
		if success {
			resp.MessageID, resp.ChatJID = sent.ID, sent.ChatJID
			resp.Timestamp = sent.Timestamp.UTC().Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))

	// Handler for explicitly sending read receipts for selected messages.
	mux.HandleFunc("/api/mark-read", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.ChatJID) {
			return
		}

		messageIDs := make([]types.MessageID, len(req.MessageIDs))
		for i, id := range req.MessageIDs {
			if strings.TrimSpace(id) == "" {
				http.Error(w, "message_ids must not contain empty values", http.StatusBadRequest)
				return
			}
			messageIDs[i] = types.MessageID(id)
		}

		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
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
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
				return
			}
		} else if chatJID.Server == types.GroupServer {
			http.Error(w, "sender_jid is required for group read receipts", http.StatusBadRequest)
			return
		}

		readAt := time.Now()
		if req.Timestamp != "" {
			readAt, err = time.Parse(time.RFC3339, req.Timestamp)
			if err != nil {
				http.Error(w, "timestamp must be RFC 3339", http.StatusBadRequest)
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
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// MCP storage normalizes chats/senders to phone JIDs; MarkRead routes
		// the receipt `to`/`participant` as given, so resolve PN -> LID the
		// same way sendWhatsAppMessage does or migrated contacts silently fail.
		chatJID, err = resolveRecipientJID(client, req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
			return
		}
		if req.SenderJID != "" {
			senderJID, err = resolveRecipientJID(client, req.SenderJID)
			if err != nil || senderJID.User == "" || senderJID.Server == "" {
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
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
	}))

	// Handler for sending (or removing) emoji reactions
	mux.HandleFunc("/api/react", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req ReactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Recipient == "" || req.MessageID == "" || req.Emoji == nil {
			http.Error(w, "recipient, message_id, and emoji are required", http.StatusBadRequest)
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.Recipient) {
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid recipient JID: %v", err), http.StatusBadRequest)
			return
		}
		var senderJID types.JID
		switch {
		case req.FromMe:
			if client.Store.ID == nil {
				http.Error(w, "Not logged in", http.StatusServiceUnavailable)
				return
			}
			senderJID = *client.Store.ID
		case req.SenderJID != "":
			if senderJID, err = types.ParseJID(req.SenderJID); err != nil {
				http.Error(w, fmt.Sprintf("Invalid sender_jid: %v", err), http.StatusBadRequest)
				return
			}
			if senderJID.User == "" || senderJID.Server == "" {
				http.Error(w, "Invalid sender_jid", http.StatusBadRequest)
				return
			}
		default:
			if chatJID.Server == types.GroupServer {
				http.Error(w, "sender_jid is required for group reactions when from_me is false", http.StatusBadRequest)
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
	}))

	// Handler for downloading media
	mux.HandleFunc("/api/download", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Check if connected
		if !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Log download request for debugging
		bridgeLog.Debugf("📥 Download request: message_id=%s chat_jid=%s", req.MessageID, req.ChatJID)

		// Download the media
		success, mediaType, filename, path, err := b.DownloadMedia(req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}))

	// Handler for sending typing indicator
	mux.HandleFunc("/api/typing", auth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req struct {
			Recipient string `json:"recipient"`
			IsTyping  bool   `json:"is_typing"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
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
	}))

	return mux
}

// healthStatus is the body of /api/health and /api/ready.
func healthStatus(client *whatsmeow.Client, startedAt time.Time, stats *storeStats) map[string]interface{} {
	connected := client != nil && client.IsConnected()
	paired := client != nil && client.Store != nil && client.Store.ID != nil
	status := "ok"
	switch {
	case !paired:
		status = "awaiting_pairing"
	case !connected:
		status = "disconnected"
	}
	body := map[string]interface{}{
		"status":         status,
		"connected":      connected,
		"paired":         paired,
		"uptime_seconds": int(time.Since(startedAt).Seconds()),
		"timestamp":      time.Now().Unix(),
	}
	if stats != nil {
		storeBytes, mediaBytes, mediaFiles := stats.snapshot(time.Now())
		body["store_bytes"] = storeBytes
		body["media_bytes"] = mediaBytes
		body["media_files"] = mediaFiles
	}
	return body
}

func writeJSON(w http.ResponseWriter, code int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (b *Bridge) startRESTServer(port int, token string, allowedMediaRoots []string) {

	handler := b.newRESTMux(port, token, allowedMediaRoots)

	// Loopback by default so the bridge is not reachable from the LAN;
	// WHATSAPP_BRIDGE_BIND widens that on purpose (rest_bind.go).
	serverAddr := listenAddr(b.RESTBind, port)
	bridgeLog.Infof("Starting REST API server on %s...", serverAddr)

	// Create server with timeouts for stability
	server := &http.Server{
		Addr:         serverAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second, // Longer for media downloads
		IdleTimeout:  120 * time.Second,
		Handler:      handler,
	}

	b.httpServer = server

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			bridgeLog.Errorf("REST API server error: %v", err)
		}
	}()
}
