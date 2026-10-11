package main

// On-demand history sync.
//
// whatsmeow only *receives* whatever history the phone pushes at pair time.
// `--full-history-pair` widens that request, but it only applies to a fresh
// pair, so recovering a gap in a single chat otherwise means deleting
// whatsapp.db, re-scanning the QR and re-syncing everything.
//
// whatsmeow already exposes the primitive for a targeted request
// (Client.BuildHistorySyncRequest); it just isn't reachable from the bridge.
// This file wires it to POST /api/history so older messages can be requested
// for one chat at runtime. The response arrives as an events.HistorySync with
// type ON_DEMAND, which the existing handler already stores, so no new storage
// or parsing path is introduced.
//
// As with pair-time sync, the phone has the final word on how much it returns
// (see AGENTS.md gotcha #4), so Count is a request, not a guarantee.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// HistoryRequest is the JSON body accepted by POST /api/history.
type HistoryRequest struct {
	ChatJID         string  `json:"chat_jid"`
	Count           int     `json:"count,omitempty"`
	BeforeMessageID *string `json:"before_message_id,omitempty"`
}

const (
	// defaultHistoryCount is used when the caller omits count. whatsmeow
	// documents 50 as the recommended number to request at a time.
	defaultHistoryCount = 50
	// maxHistoryCount bounds a single request so a typo can't ask the phone
	// for an unbounded backfill. Callers wanting more should page by calling
	// again, since the anchor moves back as older messages land.
	maxHistoryCount = 500
)

// clampHistoryCount normalises a requested message count into a sane range.
func clampHistoryCount(n int) int {
	switch {
	case n <= 0:
		return defaultHistoryCount
	case n > maxHistoryCount:
		return maxHistoryCount
	default:
		return n
	}
}

// historyAnchorInfo builds the anchor MessageInfo for an on-demand history
// request. The phone returns messages from *before* this message, so callers
// pass the oldest stored message for backfill, or the successor of a repair target.
//
// BuildHistorySyncRequest reads only Chat, ID, IsFromMe and Timestamp — the
// HistorySyncOnDemandRequest wire message carries no field for the anchor's
// sender or a group flag — so only those four are set here.
func historyAnchorInfo(chat types.JID, msgID string, fromMe bool, ts time.Time) types.MessageInfo {
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: fromMe,
		},
		ID:        msgID,
		Timestamp: ts,
	}
}

// oldestStoredMessage returns the anchor fields for the oldest message the
// bridge currently holds for a chat. It returns sql.ErrNoRows when the chat has
// no stored messages to anchor on.
func oldestStoredMessage(store *MessageStore, chatJID string) (id string, fromMe bool, ts time.Time, err error) {
	var rawTS any
	err = store.db.QueryRow(
		`SELECT id, is_from_me, timestamp
		   FROM messages
		  WHERE chat_jid = ?
		  ORDER BY timestamp ASC
		  LIMIT 1`,
		chatJID,
	).Scan(&id, &fromMe, &rawTS)
	if err != nil {
		return "", false, time.Time{}, err
	}
	return id, fromMe, anchorTime(rawTS), nil
}

// repairHistoryAnchor finds the target only in the requested chat or its known
// phone/LID twin, then uses the next row in the store's (timestamp, id) order.
// The target itself must be included in the replay, rather than excluded as the
// SDK's "oldest message". With no successor, use a timestamp-only anchor one
// second later: BuildHistorySyncRequest serializes seconds despite its MS name.
func repairHistoryAnchor(ctx context.Context, store *MessageStore, chat, twin, target string) (id string, fromMe bool, ts time.Time, err error) {
	var storedChat string
	var rawTS any
	err = store.db.QueryRowContext(ctx, `SELECT chat_jid, timestamp FROM messages
		WHERE id = ? AND chat_jid IN (?, ?)
		ORDER BY CASE WHEN chat_jid = ? THEN 0 ELSE 1 END LIMIT 1`,
		target, chat, twin, chat).Scan(&storedChat, &rawTS)
	if err != nil {
		return "", false, time.Time{}, err
	}
	targetTime := anchorTime(rawTS)
	if targetTime.IsZero() {
		return "", false, time.Time{}, nil
	}
	err = store.db.QueryRowContext(ctx, `SELECT id, is_from_me, timestamp FROM messages
		WHERE chat_jid = ? AND (timestamp, id) >
			(SELECT timestamp, id FROM messages WHERE id = ? AND chat_jid = ?)
		ORDER BY timestamp, id LIMIT 1`, storedChat, target, storedChat).Scan(&id, &fromMe, &rawTS)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, targetTime.Add(time.Second), nil
	}
	if err != nil {
		return "", false, time.Time{}, err
	}
	return id, fromMe, anchorTime(rawTS), nil
}

// registerHistoryEndpoint wires POST /api/history onto an existing mux.
func registerHistoryEndpoint(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, connected func() bool, messageStore *MessageStore, policy chatPolicy) {
	registerHistoryEndpointWithSender(mux, auth, client, connected, messageStore, policy,
		func(ctx context.Context, msg *waE2E.Message) error {
			_, err := client.SendPeerMessage(ctx, msg)
			return err
		})
}

// Only the network send is replaced in tests; the pinned SDK builds the request.
func registerHistoryEndpointWithSender(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, client *whatsmeow.Client, connected func() bool, messageStore *MessageStore, policy chatPolicy, send func(context.Context, *waE2E.Message) error) {
	mux.HandleFunc("/api/history", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}

		bridgeLog.Debugf("→ /api/history from=%q user_agent=%q", r.RemoteAddr, r.UserAgent())

		var req HistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if req.ChatJID == "" {
			writeError(w, http.StatusBadRequest, "chat_jid is required")
			return
		}
		chatJID, ok := authorizeChat(w, policy, req.ChatJID, false)
		if !ok {
			return
		}
		count := clampHistoryCount(req.Count)

		w.Header().Set("Content-Type", "application/json")

		writeErr := func(status int, msg string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: msg})
		}

		if !connected() {
			writeErr(http.StatusServiceUnavailable, "Not connected to WhatsApp")
			return
		}
		if client.Store == nil || client.Store.ID == nil {
			writeErr(http.StatusServiceUnavailable, "Client is not paired")
			return
		}

		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		var id string
		var fromMe bool
		var ts time.Time
		var err error
		if req.BeforeMessageID == nil {
			id, fromMe, ts, err = oldestStoredMessage(messageStore, req.ChatJID)
		} else {
			twin, _ := lookupAltJID(ctx, client, chatJID)
			id, fromMe, ts, err = repairHistoryAnchor(ctx, messageStore, chatJID.String(), twin.ToNonAD().String(), *req.BeforeMessageID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			if req.BeforeMessageID != nil {
				writeError(w, http.StatusBadRequest, "before_message_id must identify a stored message in this chat")
				return
			}
			writeErr(http.StatusNotFound,
				"No stored message for this chat to anchor the request; send or receive one message first")
			return
		}
		if err != nil {
			writeErr(http.StatusInternalServerError, fmt.Sprintf("Failed to look up anchor message: %v", err))
			return
		}
		if ts.IsZero() {
			writeErr(http.StatusInternalServerError, "Stored anchor message has an unreadable timestamp")
			return
		}

		info := historyAnchorInfo(chatJID, id, fromMe, ts)

		msg := client.BuildHistorySyncRequest(&info, count)
		if msg == nil {
			writeErr(http.StatusInternalServerError, "Failed to build history sync request")
			return
		}

		// SendPeerMessage is the transport whatsmeow documents for this request
		// type. It resolves our own JID and sets the peer flag internally, so
		// that addressing logic isn't duplicated (and drifted) here.
		if err := send(ctx, msg); err != nil {
			writeErr(http.StatusInternalServerError, fmt.Sprintf("Failed to send history request: %v", err))
			return
		}

		bridgeLog.Debugf("← /api/history chat=%q count=%d anchor=%q", req.ChatJID, count, id)
		_ = json.NewEncoder(w).Encode(SendMessageResponse{
			Success: true,
			Message: fmt.Sprintf(
				"Requested up to %d messages older than %s for %s. History arrives asynchronously; the phone decides how much is returned.",
				count, ts.Format(time.RFC3339), req.ChatJID),
		})
	}))
}
