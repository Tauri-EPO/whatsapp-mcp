package main

// REST API: route table, handlers and the HTTP server. Auth and Host
// validation live in auth.go / rest_bind.go, outbound path checks in
// media_path.go, feature endpoints next to their features.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
func (b *Bridge) newRESTMux(port int, token string) *http.ServeMux {
	allowedMediaRoots := b.MediaRoots
	client, messageStore := b.Client, b.Store
	allowedHosts, hostWarning := buildHostAllowList(port, b.RESTBind, b.RESTAllowedHosts)
	if hostWarning != "" {
		b.Log.Warnf("%s", hostWarning)
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return withAuth(token, allowedHosts, h)
	}
	mux := http.NewServeMux()

	// On-demand history sync endpoint (see history_ondemand.go)
	registerHistoryEndpoint(mux, auth, client, func() bool { return b.Connected() }, messageStore)

	// Health check endpoint
	// Liveness: the process serves requests. Always 200 once the listener is up,
	// with the connection state in the body — so a container awaiting its QR
	// scan is "healthy" (alive) rather than "unhealthy" (broken). Readiness
	// (/api/ready) is what to poll before sending.
	mux.HandleFunc("/api/health", auth(requireMethod(http.MethodGet, b.handleHealth())))

	// Build identity; unauthenticated on purpose (see version.go).
	mux.HandleFunc("/api/version", handleVersion(buildInfo(messageStore != nil && messageStore.fts)))
	if getEnvBool(metricsEnv, true) {
		// Prometheus text; unauthenticated like /api/version (counts only, see metrics.go).
		mux.HandleFunc("/metrics", requireMethod(http.MethodGet, b.handleMetrics()))
	}

	// Readiness: 200 only while paired AND connected. whatsmeow reports
	// IsConnected() as soon as the websocket is up, which includes the QR
	// pairing phase, so "connected" alone is not "usable".
	mux.HandleFunc("/api/ready", auth(requireMethod(http.MethodGet, b.handleReady())))

	// Group participants (see group_members.go). Needs a live connection.
	mux.HandleFunc("/api/group/members", auth(handleGroupMembers(
		func(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
			if !b.Connected() {
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
			if !b.Connected() {
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
	registerGroupManagement(mux, auth, liveGroupOps(client, func() bool { return b.Connected() }), b.Policy)

	// Delete a message: revoke for everyone (own messages) or drop the local
	// row only. See delete_message.go.
	mux.HandleFunc("/api/delete", auth(handleDeleteMessage(messageStore,
		func(ctx context.Context, chat types.JID, id types.MessageID) error {
			if !b.Connected() {
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
	mux.HandleFunc("/api/send", auth(requireMethod(http.MethodPost, b.handleSend(allowedMediaRoots))))

	// Handler for explicitly sending read receipts for selected messages.
	mux.HandleFunc("/api/mark-read", auth(requireMethod(http.MethodPost, b.handleMarkRead())))

	// Handler for sending (or removing) emoji reactions
	mux.HandleFunc("/api/react", auth(requireMethod(http.MethodPost, b.handleReact())))

	// Handler for downloading media
	mux.HandleFunc("/api/download", auth(requireMethod(http.MethodPost, b.handleDownload())))

	// Drop cached media bytes on request, rows untouched (media_purge.go).
	mux.HandleFunc("/api/media/purge", auth(requireMethod(http.MethodPost, b.handleMediaPurge())))

	// Handler for sending typing indicator
	mux.HandleFunc("/api/typing", auth(requireMethod(http.MethodPost, b.handleTyping())))

	return mux
}

// healthStatus is the body of /api/health and /api/ready.
func (b *Bridge) healthStatus() map[string]interface{} {
	client, startedAt, stats := b.Client, b.startedAt, b.storeStats
	connected := b.Connected()
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

func (b *Bridge) startRESTServer(port int, token string) {

	handler := b.newRESTMux(port, token)

	// Loopback by default so the bridge is not reachable from the LAN;
	// WHATSAPP_BRIDGE_BIND widens that on purpose (rest_bind.go).
	serverAddr := listenAddr(b.RESTBind, port)
	b.Log.Infof("Starting REST API server on %s...", serverAddr)

	// Create server with timeouts for stability
	server := &http.Server{
		Addr:         serverAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second, // Longer for media downloads
		IdleTimeout:  120 * time.Second,
		Handler:      requestLog(handler, b.metrics),
	}

	b.httpServer = server

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			b.Log.Errorf("REST API server error: %v", err)
		}
	}()
}
