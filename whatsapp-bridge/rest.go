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
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// MarkReadRequest is the request body for the /api/mark-read endpoint.
// Without message_ids the whole chat is marked read up to UpTo; see
// mark_read.go for the two forms.
type MarkReadRequest struct {
	MessageIDs []string `json:"message_ids"`
	ChatJID    string   `json:"chat_jid"`
	SenderJID  string   `json:"sender_jid,omitempty"`
	Timestamp  string   `json:"timestamp,omitempty"`
	UpTo       string   `json:"up_to,omitempty"`
}

// MarkReadResponse is the /api/mark-read response body. The counts describe
// what left the bridge: Messages acknowledged, distinct Senders they were
// grouped by, receipt Batches sent. Truncated says the chat had more pending
// than one request marks (markReadMaxMessages).
type MarkReadResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Messages  int    `json:"messages"`
	Senders   int    `json:"senders"`
	Batches   int    `json:"batches"`
	Truncated bool   `json:"truncated,omitempty"`
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
	client, messageStore := b.currentClient(), b.Store
	allowedHosts, hostWarning := buildHostAllowList(port, b.RESTBind, b.RESTAllowedHosts)
	if hostWarning != "" {
		b.Log.Warnf("%s", hostWarning)
	}
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return withAuth(token, allowedHosts, h)
	}
	// mutate = auth + the WHATSAPP_READ_ONLY refusal (403, read_only.go) + the
	// per-tool allow/deny lists (tool_policy.go), in that order: read-only wins,
	// then the lists narrow further. Every endpoint with a side effect registers
	// with it; reads keep plain auth, so a new route has to be classified when it
	// is added — and endpointTools must gain a row for it.
	mutate := func(h http.HandlerFunc) http.HandlerFunc {
		return auth(b.ReadOnly.guard(b.runtimeToolGuard(h)))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/media/blob", auth(requireMethod(http.MethodGet, b.handleMediaBlob)))
	mux.HandleFunc("/api/media/cache", auth(requireMethod(http.MethodGet, b.handleMediaCache)))
	mux.HandleFunc("/api/labels", auth(requireMethod(http.MethodGet, handleLabels(messageStore, b.Policy, func(ctx context.Context, chat types.JID) (types.JID, error) {
		if client == nil || client.Store == nil || client.Store.LIDs == nil {
			return types.EmptyJID, nil
		}
		if chat.Server == types.HiddenUserServer {
			return client.Store.LIDs.GetPNForLID(ctx, chat)
		}
		if chat.Server == types.DefaultUserServer {
			return client.Store.LIDs.GetLIDForPN(ctx, chat)
		}
		return types.EmptyJID, nil
	}))))
	mux.HandleFunc("/api/chat/label", mutate(requireMethod(http.MethodPost, handleLabelChat(labelDeps{store: messageStore, policy: b.Policy, connected: func() bool { return b.Connected() }, resolve: func(ctx context.Context, raw string) (types.JID, error) {
		return resolveRecipientJIDContext(ctx, client, raw)
	}, send: b.sendAppState}))))

	// On-demand history sync endpoint (see history_ondemand.go). Mutating: it
	// asks the phone to push history and writes the rows into messages.db.
	registerHistoryEndpoint(mux, mutate, client, func() bool { return b.Connected() }, messageStore, b.Policy)

	b.registerStatusEndpoints(mux, auth)

	// Own identity (see me.go). Authenticated and separate from /api/health:
	// the owner's number is personal data, health bodies end up in logs.
	mux.HandleFunc("/api/me", auth(requireMethod(http.MethodGet, handleMe(clientIdentity(client)))))

	// Group participants (see group_members.go). Needs a live connection.
	mux.HandleFunc("/api/group/members", auth(handleGroupMembers(
		func(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
			if !b.Connected() {
				return nil, errors.New("WhatsApp client is not connected")
			}
			return client.GetGroupInfo(ctx, jid)
		},
		storeContactName(client),
		storeAltJID(client),
		b.Policy,
		b.recordGroupRoster,
	)))

	// Edit an own message / forward a message (edit_forward.go).
	mux.HandleFunc("/api/edit", mutate(handleEditMessage(messageStore,
		func(ctx context.Context, chat types.JID, id types.MessageID, text string) (int64, error) {
			if !b.Connected() {
				return 0, errors.New("WhatsApp client is not connected")
			}
			message := client.BuildEdit(chat, id, &waE2E.Message{Conversation: proto.String(text)})
			stamp := message.GetEditedMessage().GetMessage().GetProtocolMessage().GetTimestampMS()
			var err error
			if b.sendMessage != nil {
				_, err = b.sendMessage(ctx, chat, message)
			} else {
				_, err = client.SendMessage(ctx, chat, message)
			}
			return stamp, err
		},
		b.Policy,
		b.storeLive,
		b.allowSendAction,
	)))
	mux.HandleFunc("/api/forward", mutate(handleForwardMessage(forwardDeps{
		lookup: messageStore.messageContentLookup,
		resolveRecipient: func(ctx context.Context, w http.ResponseWriter, to string) (string, bool) {
			return b.registeredRecipient(ctx, w, to, nil)
		},
		download:  b.DownloadMedia,
		send:      b.Send,
		allowSend: b.allowSend,
	}, b.Policy)))

	// Group management: participants, subject/description, invite link, leave (group_manage.go).
	group := liveGroupOps(client, func() bool { return b.Connected() })
	group.allowSend = b.allowParticipantAdds
	registerGroupManagement(mux, mutate, group, b.Policy, b.recordGroupRoster)

	// Delete a message: revoke for everyone (own messages) or drop the local
	// row only. See delete_message.go.
	mux.HandleFunc("/api/delete", mutate(handleDeleteMessage(messageStore,
		func(ctx context.Context, chat types.JID, id types.MessageID) error {
			if !b.Connected() {
				return errors.New("WhatsApp client is not connected")
			}
			// Revoke = a protocol message keyed to the original (own) message.
			_, err := client.SendMessage(ctx, chat, client.BuildRevoke(chat, types.EmptyJID, id))
			return err
		},
		b.Policy,
		b.storeLive,
		b.allowSendAction,
	)))

	// Poll results (see polls.go).
	mux.HandleFunc("/api/poll", auth(handlePollResults(messageStore, b.Policy)))

	// Handler for sending messages
	mux.HandleFunc("/api/send", mutate(requireMethod(http.MethodPost, b.handleSend(allowedMediaRoots))))

	// Handler for explicitly sending read receipts for selected messages.
	mux.HandleFunc("/api/mark-read", mutate(requireMethod(http.MethodPost, b.handleMarkRead())))
	mux.HandleFunc("/api/chat/archive", mutate(requireMethod(http.MethodPost, handleArchiveChat(archiveDeps{
		store: messageStore, policy: b.Policy, connected: func() bool { return b.Connected() },
		resolve: func(ctx context.Context, raw string) (types.JID, error) {
			return resolveRecipientJIDContext(ctx, client, raw)
		},
		twin: func(ctx context.Context, jid types.JID) (types.JID, error) {
			if jid.Server == types.HiddenUserServer {
				return lookupAltJID(ctx, client, jid)
			}
			return types.EmptyJID, nil
		},
		send: b.sendAppState,
	}))))

	// Handler for sending (or removing) emoji reactions
	mux.HandleFunc("/api/react", mutate(requireMethod(http.MethodPost, b.handleReact())))

	// Handler for downloading media
	mux.HandleFunc("/api/download", auth(requireMethod(http.MethodPost, b.handleDownload())))

	// Drop cached media bytes on request, rows untouched (media_purge.go).
	mux.HandleFunc("/api/media/purge", mutate(requireMethod(http.MethodPost, b.handleMediaPurge())))

	// Handler for sending typing indicator
	mux.HandleFunc("/api/typing", mutate(requireMethod(http.MethodPost, b.handleTyping())))

	return mux
}

// Status handlers read runtime state without retaining one SDK client. Keep
// their method/auth rules shared with the normal mux and independent of a slow
// SDK request that holds the handoff gate. Liveness stays 200 while unpaired;
// readiness still requires both pairing and an active connection.
func (b *Bridge) registerStatusEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/api/health", auth(requireMethod(http.MethodGet, b.handleHealth())))
	mux.HandleFunc("/api/ready", auth(requireMethod(http.MethodGet, b.handleReady())))
	mux.HandleFunc("/api/version", handleVersion(buildInfo().withFTS(b.Store != nil && b.Store.fts)))
	if b.MetricsEnabled {
		mux.HandleFunc("/metrics", requireMethod(http.MethodGet, b.handleMetrics()))
	}
}

// healthStatus is the body of /api/health and /api/ready.
func (b *Bridge) healthStatus() map[string]interface{} {
	base := b.ctx
	if base == nil {
		base = context.Background()
	}
	// Optional runtime metadata shares one short budget. Liveness must still
	// answer when a maintenance request owns every database connection.
	ctx, cancel := context.WithTimeout(base, 100*time.Millisecond)
	defer cancel()
	startedAt, stats := b.startedAt, b.storeStats
	connected := b.Connected()
	paired := b.isPaired()
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
	problem, pairingState := b.connectionSnapshot()
	b.connectionMu.Lock()
	body["connection_problem_persistence_failed"] = b.problemPersistenceFailed
	b.connectionMu.Unlock()
	if problem != nil {
		body["connection_problem"] = problem
		if problem.Kind == "banned" || problem.Kind == "locked" || problem.Kind == "client_outdated" {
			body["status"] = "blocked"
		}
		if problem.Kind == "temporarily_banned" && problem.ExpiresAt != nil && b.connectionNow().Before(*problem.ExpiresAt) {
			body["status"] = "temporarily_banned"
		}
	}
	if pairingState != "" {
		body["pairing_state"], body["status"] = pairingState, pairingState
	}
	if b.operatorPairing != nil {
		body["operator_pairing_state"] = b.operatorPairing.snapshot().State
	}
	if stats != nil {
		storeBytes, mediaBytes, mediaFiles, statusBytes, statusFiles := stats.snapshotScoped(time.Now())
		if b.mediaStorage().Backend() == "s3" {
			if usage, err := b.mediaStorage().Usage(ctx); err == nil {
				mediaBytes, mediaFiles, statusBytes, statusFiles = usage.Bytes, usage.Files, usage.StatusBytes, usage.StatusFiles
			}
		}
		body["media_backend"] = b.mediaStorage().Backend()
		body["store_bytes"] = storeBytes
		body["media_bytes"] = mediaBytes
		body["media_files"] = mediaFiles
		body["media_status_bytes"], body["media_status_files"] = statusBytes, statusFiles
		share := float64(0)
		if mediaBytes > 0 {
			share = float64(statusBytes) / float64(mediaBytes)
		}
		body["media_status_share"] = share
		if quota, types, target, err := b.mediaQuotaSettings(ctx); err == nil {
			body["media_quota_bytes"], body["media_caching_paused"] = quota, b.mediaCachingPaused(quota, mediaBytes, types, target)
			body["media_quota_warning"] = b.observeMediaQuota(quota, mediaBytes)
		}
		_, _, _, warning := b.archiveStats(time.Now())
		body["store_warning"] = warning
	}
	body["history_sync"] = b.historyProgress.snapshot()
	if b.Store != nil {
		if usage, err := b.sendUsageSnapshot(ctx); err == nil {
			body["send_usage"] = usage
		}
	}
	return body
}

func writeJSON(w http.ResponseWriter, code int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (b *Bridge) startRESTServer(port int, token string) error {

	handler := b.runtimeRESTHandler(port, token)

	// Loopback by default so the bridge is not reachable from the LAN;
	// WHATSAPP_BRIDGE_BIND widens that on purpose (rest_bind.go).
	serverAddr := listenAddr(b.RESTBind, port)
	b.Log.Infof("Starting REST API server on %s...", serverAddr)
	listener, deviceBound, err := listenREST(b.RESTBind, port, b.RESTSplit)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", serverAddr, err)
	}
	if b.RESTSplit && !deviceBound {
		b.Log.Warnf("Split REST device binding unavailable; a peer with NET_ADMIN and the bridge token may reach REST through another bridge interface")
	}
	if deviceBound {
		b.Log.Infof("Split REST socket bound to agent interface without added capabilities")
	}

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
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			b.Log.Errorf("REST API server error: %v", err)
		}
	}()
	return nil
}
