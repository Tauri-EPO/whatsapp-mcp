package main

// Chat filing uses app state, not a message. The archive anchor is read with
// permitted storage twins; the patch target is rewritten PN -> LID, like mark-read.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type appStateSendFunc func(context.Context, appstate.PatchInfo) error

// Finish before the MCP HTTP client's default 30-second read timeout.
const archiveDeadline = 20 * time.Second

type appStateNotSentError struct{ error }

func (e appStateNotSentError) Unwrap() error { return e.error }

type archiveAnchorError string

func (e archiveAnchorError) Error() string { return string(e) }

type archiveDeps struct {
	store     *MessageStore
	policy    chatPolicy
	connected func() bool
	resolve   func(context.Context, string) (types.JID, error)
	twin      func(context.Context, types.JID) (types.JID, error)
	send      appStateSendFunc
}

// archiveAnchor selects one complete row, including a deterministic tie-break
// by insertion order for messages sharing a second. MAX(timestamp) loses the key.
// History batches can insert newest-first, so same-second history order is only
// an approximation; the stored schema has no finer chronological discriminator.
func (store *MessageStore) archiveAnchor(ctx context.Context, chats []string) (*waCommon.MessageKey, time.Time, error) {
	var id, chat string
	var sender, server sql.NullString
	var fromMe bool
	var rawTime any
	args := make([]any, len(chats))
	for i, chat := range chats {
		args[i] = chat
	}
	err := store.db.QueryRowContext(ctx, `SELECT id, sender, sender_server, is_from_me, timestamp, chat_jid
		FROM messages WHERE chat_jid IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chats)), ",")+`)
		AND COALESCE(media_type, '') NOT IN ('reaction', 'poll_vote')
		ORDER BY timestamp DESC, rowid DESC LIMIT 1`, args...).
		Scan(&id, &sender, &server, &fromMe, &rawTime, &chat)
	if err != nil {
		return nil, time.Time{}, err
	}
	ts := anchorTime(rawTime)
	if ts.IsZero() {
		return nil, ts, archiveAnchorError("latest message has an invalid timestamp")
	}
	key := &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)}
	jid, _ := types.ParseJID(chat)
	if jid.Server == types.GroupServer {
		participant := sender.String
		if !strings.Contains(participant, "@") {
			if server.String == "" {
				return nil, ts, archiveAnchorError("latest group message has no known sender namespace")
			}
			participant += "@" + server.String
		}
		parsed, err := parseRecipientJID(participant)
		if err != nil || parsed.User == "" || parsed.Server == "" || parsed.Server == types.GroupServer || parsed.User == jid.User {
			return nil, ts, archiveAnchorError("latest group message has no usable sender")
		}
		key.Participant = proto.String(participant)
	}
	return key, ts, nil
}

func handleArchiveChat(deps archiveDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := requestContext(r, archiveDeadline)
		defer cancel()
		var req struct {
			ChatJID  string `json:"chat_jid"`
			Archived *bool  `json:"archived"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		req.ChatJID = strings.TrimSpace(req.ChatJID)
		if req.Archived == nil {
			writeError(w, http.StatusBadRequest, "Valid chat_jid and archived boolean are required")
			return
		}
		chat, ok := authorizeChat(w, deps.policy, req.ChatJID, false)
		if !ok {
			return
		}
		req.ChatJID = chat.String()
		if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
			writeError(w, http.StatusBadRequest, "Only direct chats and groups can be archived")
			return
		}
		if !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected. Please wait for reconnection.")
			return
		}
		target, err := deps.resolve(ctx, req.ChatJID)
		if ctx.Err() != nil {
			writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", "Archive request expired before send; nothing sent; safe to retry")
			return
		}
		if err != nil || target.User == "" || target.Server == "" {
			writeError(w, http.StatusBadRequest, "Cannot resolve chat_jid")
			return
		}
		chats := []string{req.ChatJID}
		addAlias := func(jid types.JID) {
			if !jid.IsEmpty() && jid.String() != req.ChatJID && deps.policy.Allows(jid.String()) {
				chats = append(chats, jid.String())
			}
		}
		addAlias(target)
		if deps.twin != nil {
			if twin, err := deps.twin(ctx, target); err == nil {
				addAlias(twin)
			}
		}
		key, ts, err := deps.store.archiveAnchor(ctx, chats)
		if err != nil {
			status := http.StatusInternalServerError
			code := "internal"
			var invalid archiveAnchorError
			if errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
				code = "not_found"
			} else if ctx.Err() != nil {
				status = http.StatusRequestTimeout
				code = "bridge_unavailable"
			} else if errors.As(err, &invalid) {
				status = http.StatusUnprocessableEntity
				code = "invalid_argument"
			}
			writeErrorCode(w, status, code, "Cannot anchor chat archive: "+err.Error())
			return
		}
		key.RemoteJID = proto.String(target.String())
		if key.GetParticipant() != "" {
			sender, err := deps.resolve(ctx, key.GetParticipant())
			if ctx.Err() != nil {
				writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", "Archive request expired before send; nothing sent; safe to retry")
				return
			}
			if err != nil || sender.User == "" || sender.Server == "" {
				writeError(w, http.StatusBadRequest, "Cannot resolve latest message sender")
				return
			}
			key.Participant = proto.String(sender.String())
		}
		if ctx.Err() != nil {
			writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", "Archive request expired before send; nothing sent; safe to retry")
			return
		}
		if err := deps.send(ctx, appstate.BuildArchive(target, *req.Archived, ts, key)); err != nil {
			var notSent appStateNotSentError
			if errors.As(err, &notSent) {
				writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", "Archive request expired waiting for writer; nothing sent; safe to retry")
				return
			}
			// At the pinned whatsmeow version this prefix is produced only after
			// the server accepted the patch, when its subsequent fetch failed.
			if strings.HasPrefix(err.Error(), "failed to fetch app state after sending update:") {
				writeJSON(w, http.StatusOK, map[string]any{"success": true, "archived": *req.Archived, "sent": true, "confirmed": false,
					"warning": "Patch accepted; app-state confirmation failed. Do not retry automatically."})
				return
			}
			if errors.Is(err, whatsmeow.ErrAppStateUpdate) || errors.Is(err, whatsmeow.ErrNotConnected) ||
				err.Error() == "no app state keys found, creating app state keys is not yet supported" {
				writeErrorCode(w, http.StatusServiceUnavailable, "bridge_unavailable", "Chat archive was not applied; safe to retry after resolving the bridge error: "+err.Error())
				return
			}
			writeError(w, http.StatusBadGateway, "Chat archive outcome is unknown; do not retry automatically: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "archived": *req.Archived, "sent": true, "confirmed": false})
	}
}

// One app-state writer per bridge: the library reads a collection version
// before sending without holding its sync lock. Waiting also obeys the request.
func (b *Bridge) sendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	select {
	case b.appStateGate <- struct{}{}:
	case <-ctx.Done():
		return appStateNotSentError{ctx.Err()}
	}
	defer func() { <-b.appStateGate }()
	if err := ctx.Err(); err != nil {
		return appStateNotSentError{err}
	}
	return b.SendAppState(ctx, patch)
}
