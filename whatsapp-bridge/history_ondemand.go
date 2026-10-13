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
	"regexp"
	"strings"
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
// bridge currently holds for a chat or its known twin. It returns sql.ErrNoRows
// when neither spelling has a stored message to anchor on.
func oldestStoredMessage(ctx context.Context, store *MessageStore, chat, twin string) (storedChat, id string, fromMe bool, ts time.Time, err error) {
	// Keep each lookup on (chat_jid, timestamp), with at most one row per
	// spelling. A combined IN/CASE ordering scans and sorts the whole chat.
	found := false
	for index, spelling := range []string{chat, twin} {
		if spelling == "" || (index == 1 && spelling == chat) {
			continue
		}
		var candidateID string
		var candidateOwn bool
		var rawTS any
		err = store.db.QueryRowContext(ctx,
			`SELECT id, is_from_me, timestamp FROM messages
			 WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1`,
			spelling).Scan(&candidateID, &candidateOwn, &rawTS)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", "", false, time.Time{}, err
		}
		candidateTime := anchorTime(rawTS)
		if !found || candidateTime.Before(ts) {
			storedChat, id, fromMe, ts = spelling, candidateID, candidateOwn, candidateTime
			found = true
		}
	}
	if !found {
		return "", "", false, time.Time{}, sql.ErrNoRows
	}
	return storedChat, id, fromMe, ts, nil
}

// repairHistoryAnchor finds the target only in the requested chat or its known
// phone/LID twin, then uses the first non-pointer message in a later second.
// The target itself must be included in the replay, rather than excluded as the
// SDK's "oldest message". With no successor, use a timestamp-only anchor one
// second later: BuildHistorySyncRequest serializes seconds despite its MS name.
func repairHistoryAnchor(ctx context.Context, store *MessageStore, chat, twin, target string) (storedChat, id string, fromMe bool, ts time.Time, err error) {
	var rawTS any
	err = store.db.QueryRowContext(ctx, `SELECT chat_jid, timestamp FROM messages
		WHERE id = ? AND chat_jid IN (?, ?)
		ORDER BY CASE WHEN chat_jid = ? THEN 0 ELSE 1 END LIMIT 1`,
		target, chat, twin, chat).Scan(&storedChat, &rawTS)
	if err != nil {
		return "", "", false, time.Time{}, err
	}
	targetTime := anchorTime(rawTS)
	if targetTime.IsZero() {
		return storedChat, "", false, time.Time{}, nil
	}
	// Reactions and poll votes are pointer rows. Revoke/edit update originals;
	// other content-free protocol envelopes do not create message rows.
	err = store.db.QueryRowContext(ctx, `SELECT id, is_from_me, timestamp FROM messages
		WHERE chat_jid = ? AND timestamp >
			(SELECT timestamp FROM messages WHERE id = ? AND chat_jid = ?)
		AND COALESCE(media_type, '') NOT IN ('reaction', 'poll_vote')
		ORDER BY timestamp, id LIMIT 1`, storedChat, target, storedChat).Scan(&id, &fromMe, &rawTS)
	if errors.Is(err, sql.ErrNoRows) {
		return storedChat, "", false, targetTime.Add(time.Second), nil
	}
	if err != nil {
		return storedChat, "", false, time.Time{}, err
	}
	return storedChat, id, fromMe, anchorTime(rawTS), nil
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
		policy := policy.effective(r.Context())
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
		typedChat := chatJID
		twin := types.EmptyJID
		if chatJID.Server == types.DefaultUserServer || chatJID.Server == types.HiddenUserServer {
			if alt, lookupErr := lookupAltJID(ctx, client, chatJID); lookupErr == nil && !alt.IsEmpty() &&
				((chatJID.Server == types.DefaultUserServer && alt.Server == types.HiddenUserServer) ||
					(chatJID.Server == types.HiddenUserServer && alt.Server == types.DefaultUserServer)) {
				twin = alt.ToNonAD()
			}
		}
		var id string
		var fromMe bool
		var ts time.Time
		var err error
		var storedChat string
		anchorTwin := chatJID.String()
		if !twin.IsEmpty() {
			anchorTwin = twin.String()
		}
		if req.BeforeMessageID == nil {
			storedChat, id, fromMe, ts, err = oldestStoredMessage(ctx, messageStore, chatJID.String(), anchorTwin)
		} else {
			storedChat, id, fromMe, ts, err = repairHistoryAnchor(ctx, messageStore, chatJID.String(), anchorTwin, *req.BeforeMessageID)
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

		// Both spellings may supply an archive anchor, but only allowed
		// destinations may be named in a peer request. The typed one passed
		// authorizeChat before any lookup; prefer storage only if allowed too.
		if policy.Allows(storedChat) {
			if storedChat == twin.String() {
				twin = typedChat
			}
			chatJID, err = types.ParseJID(storedChat)
			if err != nil {
				writeErr(http.StatusInternalServerError, "Stored anchor has an invalid chat spelling")
				return
			}
		} else {
			twin = types.EmptyJID
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
			failure := historySendError(err, typedChat)
			bridgeLog.Warnf("Failed to send history request for chat=%q: %s", req.ChatJID, failure)
			writeErr(http.StatusInternalServerError, "Failed to send history request: "+failure)
			return
		}
		sentKinds := []string{historyChatKind(chatJID)}
		twinFailed := false
		if !twin.IsEmpty() && policy.Allows(twin.String()) {
			info.Chat = twin
			twinMsg := client.BuildHistorySyncRequest(&info, count)
			var sendErr error
			if twinMsg == nil {
				sendErr = errors.New("failed to build history sync request")
			} else {
				sendErr = send(ctx, twinMsg)
			}
			if sendErr != nil {
				// The stored-spelling request already left the bridge. Keep its
				// success and do not expose the discovered identifier or error.
				twinFailed = true
				bridgeLog.Warnf("Failed optional history twin request for chat=%q: %s", req.ChatJID, historySendError(sendErr, typedChat))
			} else {
				sentKinds = append(sentKinds, historyChatKind(twin))
			}
		}

		bridgeLog.Debugf("← /api/history chat=%q count=%d anchor=%q", req.ChatJID, count, id)
		message := fmt.Sprintf(
			"Requested up to %d messages older than %s for %s. History arrives asynchronously; the phone decides how much is returned.",
			count, ts.Format(time.RFC3339), req.ChatJID)
		if req.BeforeMessageID != nil {
			message = fmt.Sprintf(
				"Requested re-delivery of up to %d messages up to and including the target for %s. History arrives asynchronously; the phone decides how much is returned.",
				count, req.ChatJID)
		}
		message += fmt.Sprintf(" Sent %d history request(s) under %s spelling(s).", len(sentKinds), strings.Join(sentKinds, ", "))
		if twinFailed {
			message += " The optional twin request could not be sent."
		}
		_ = json.NewEncoder(w).Encode(SendMessageResponse{
			Success: true,
			Message: message,
		})
	}))
}

// Keep the transport cause without exposing discovered JIDs, bare numeric
// identifiers or network-error URLs in the response or warning.
func historySendError(err error, typed types.JID) string {
	message := fmt.Sprint(sdkSafeArgs([]any{err})[0])
	message = regexp.MustCompile(`[\w.:-]+@[\w.-]+`).ReplaceAllStringFunc(message, func(value string) string {
		if value == typed.String() {
			return value
		}
		return "[redacted]"
	})
	return regexp.MustCompile(`\b[0-9]{5,}\b`).ReplaceAllStringFunc(message, func(value string) string {
		if value == typed.User {
			return value
		}
		return "[redacted]"
	})
}

func historyChatKind(chat types.JID) string {
	switch chat.Server {
	case types.DefaultUserServer:
		return "phone"
	case types.HiddenUserServer:
		return "LID"
	case types.GroupServer:
		return "group"
	default:
		return "chat"
	}
}
