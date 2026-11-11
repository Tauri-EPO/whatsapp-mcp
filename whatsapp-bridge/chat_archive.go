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
	policy    chatPolicyResolver
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
		policy := deps.policy.effective(r.Context())
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
		if req.Archived == nil {
			writeError(w, http.StatusBadRequest, "Valid chat_jid and archived boolean are required")
			return
		}
		anchor, ok := deps.prepareChatAppState(ctx, w, policy, req.ChatJID, chatAppStateLabels{
			verb: "archived", noun: "chat archive", action: "Archive",
		})
		if !ok {
			return
		}
		writeAppStateResult(w, deps.send(ctx, appstate.BuildArchive(anchor.target, *req.Archived, anchor.ts, anchor.key)), map[string]any{"archived": *req.Archived}, "Chat archive")
	}
}

// chatAppStateLabels keeps each endpoint's wording in the shared refusals.
type chatAppStateLabels struct{ verb, noun, action string }

// chatAppStateAnchor is what a chat-level app-state patch needs: the patch
// target (PN rewritten to LID), every stored alias of the chat and its newest row.
type chatAppStateAnchor struct {
	target types.JID
	chats  []string
	key    *waCommon.MessageKey
	ts     time.Time
}

// prepareChatAppState authorizes, resolves and anchors a direct chat or group
// for an app-state patch. It writes the refusal itself and reports ok=false;
// nothing has been sent to WhatsApp at that point.
func (deps archiveDeps) prepareChatAppState(ctx context.Context, w http.ResponseWriter, policy chatPolicy, raw string, labels chatAppStateLabels) (chatAppStateAnchor, bool) {
	expired := labels.action + " request expired before send; nothing sent; safe to retry"
	chat, ok := authorizeChat(w, policy, strings.TrimSpace(raw), false)
	if !ok {
		return chatAppStateAnchor{}, false
	}
	if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
		writeError(w, http.StatusBadRequest, "Only direct chats and groups can be "+labels.verb)
		return chatAppStateAnchor{}, false
	}
	if !deps.connected() {
		writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected. Please wait for reconnection.")
		return chatAppStateAnchor{}, false
	}
	target, err := deps.resolve(ctx, chat.String())
	if ctx.Err() != nil {
		writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", expired)
		return chatAppStateAnchor{}, false
	}
	if err != nil || target.User == "" || target.Server == "" {
		writeError(w, http.StatusBadRequest, "Cannot resolve chat_jid")
		return chatAppStateAnchor{}, false
	}
	chats := []string{chat.String()}
	addAlias := func(jid types.JID) {
		if !jid.IsEmpty() && jid.String() != chat.String() && policy.Allows(jid.String()) {
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
		writeErrorCode(w, status, code, "Cannot anchor "+labels.noun+": "+err.Error())
		return chatAppStateAnchor{}, false
	}
	key.RemoteJID = proto.String(target.String())
	if key.GetParticipant() != "" {
		sender, err := deps.resolve(ctx, key.GetParticipant())
		if ctx.Err() != nil {
			writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", expired)
			return chatAppStateAnchor{}, false
		}
		if err != nil || sender.User == "" || sender.Server == "" {
			writeError(w, http.StatusBadRequest, "Cannot resolve latest message sender")
			return chatAppStateAnchor{}, false
		}
		key.Participant = proto.String(sender.String())
	}
	if ctx.Err() != nil {
		writeErrorCode(w, http.StatusRequestTimeout, "bridge_unavailable", expired)
		return chatAppStateAnchor{}, false
	}
	return chatAppStateAnchor{target: target, chats: chats, key: key, ts: ts}, true
}

// appStateAccepted reports whether the server took the patch: no error, or the
// pinned library's prefix that proves acceptance before its confirmation fetch failed.
func appStateAccepted(err error) bool {
	return err == nil || strings.HasPrefix(err.Error(), "failed to fetch app state after sending update:")
}

func writeAppStateResult(w http.ResponseWriter, err error, payload map[string]any, action string) {
	if err != nil {
		var notSent appStateNotSentError
		if errors.As(err, &notSent) {
			writeErrorCode(w, 408, "bridge_unavailable", action+" request expired waiting for writer; nothing sent; safe to retry")
			return
		}
		if appStateAccepted(err) {
			payload["warning"] = "Patch accepted; app-state confirmation failed. Do not retry automatically."
		} else if errors.Is(err, whatsmeow.ErrAppStateUpdate) || errors.Is(err, whatsmeow.ErrNotConnected) || err.Error() == "no app state keys found, creating app state keys is not yet supported" {
			writeErrorCode(w, 503, "bridge_unavailable", action+" was not applied; safe to retry after resolving the bridge error: "+err.Error())
			return
		} else {
			writeError(w, 502, action+" outcome is unknown; do not retry automatically: "+err.Error())
			return
		}
	}
	payload["success"], payload["sent"], payload["confirmed"] = true, true, false
	writeJSON(w, 200, payload)
}

// One app-state writer per bridge: the library reads a collection version
// before sending without holding its sync lock. Waiting also obeys the request.
func (b *Bridge) withAppStateWriter(ctx context.Context, operation func(context.Context) error) error {
	select {
	case b.appStateGate <- struct{}{}:
	case <-ctx.Done():
		return appStateNotSentError{ctx.Err()}
	}
	defer func() { <-b.appStateGate }()
	if err := ctx.Err(); err != nil {
		return appStateNotSentError{err}
	}
	return operation(ctx)
}

func (b *Bridge) sendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	return b.withAppStateWriter(ctx, func(ctx context.Context) error { return b.SendAppState(ctx, patch) })
}
