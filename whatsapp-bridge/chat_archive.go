package main

// Chat filing uses app state, not a message. The archive anchor is read with
// the storage JID; only the patch target is rewritten PN -> LID, like mark-read.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type appStateSendFunc func(context.Context, appstate.PatchInfo) error

type archiveDeps struct {
	store     *MessageStore
	policy    chatPolicy
	connected func() bool
	resolve   func(string) (types.JID, error)
	send      appStateSendFunc
}

// archiveAnchor selects one complete row, including a deterministic tie-break
// for messages sharing a second. A MAX(timestamp) alone would lose the key.
func (store *MessageStore) archiveAnchor(chat string) (*waCommon.MessageKey, time.Time, error) {
	var id string
	var sender, server sql.NullString
	var fromMe bool
	var rawTime any
	err := store.db.QueryRow(`SELECT id, sender, sender_server, is_from_me, timestamp
		FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC, id DESC LIMIT 1`, chat).
		Scan(&id, &sender, &server, &fromMe, &rawTime)
	if err != nil {
		return nil, time.Time{}, err
	}
	ts := anchorTime(rawTime)
	if ts.IsZero() {
		return nil, ts, fmt.Errorf("latest message has an invalid timestamp")
	}
	key := &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)}
	jid, _ := types.ParseJID(chat)
	if jid.Server == types.GroupServer {
		participant := sender.String
		if !strings.Contains(participant, "@") {
			if server.String == "" {
				return nil, ts, fmt.Errorf("latest group message has no known sender namespace")
			}
			participant += "@" + server.String
		}
		parsed, err := types.ParseJID(participant)
		if err != nil || parsed.User == "" || parsed.Server == "" || parsed.Server == types.GroupServer || parsed.User == jid.User {
			return nil, ts, fmt.Errorf("latest group message has no usable sender")
		}
		key.Participant = proto.String(participant)
	}
	return key, ts, nil
}

func handleArchiveChat(deps archiveDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatJID  string `json:"chat_jid"`
			Archived *bool  `json:"archived"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		req.ChatJID = strings.TrimSpace(req.ChatJID)
		chat, err := types.ParseJID(req.ChatJID)
		if err != nil || chat.User == "" || chat.Server == "" || req.Archived == nil {
			writeError(w, http.StatusBadRequest, "Valid chat_jid and archived boolean are required")
			return
		}
		if rejectByChatPolicy(w, deps.policy, req.ChatJID) {
			return
		}
		if !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected. Please wait for reconnection.")
			return
		}
		key, ts, err := deps.store.archiveAnchor(req.ChatJID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
			}
			writeError(w, status, "Cannot anchor chat archive: "+err.Error())
			return
		}
		target, err := deps.resolve(req.ChatJID)
		if err != nil || target.User == "" || target.Server == "" {
			writeError(w, http.StatusBadRequest, "Invalid chat_jid")
			return
		}
		key.RemoteJID = proto.String(target.String())
		if key.GetParticipant() != "" {
			sender, err := deps.resolve(key.GetParticipant())
			if err != nil || sender.User == "" || sender.Server == "" {
				writeError(w, http.StatusBadRequest, "Cannot resolve latest message sender")
				return
			}
			key.Participant = proto.String(sender.String())
		}
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		if err := deps.send(ctx, appstate.BuildArchive(target, *req.Archived, ts, key)); err != nil {
			writeError(w, http.StatusBadGateway, "Chat archive failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "archived": *req.Archived})
	}
}
