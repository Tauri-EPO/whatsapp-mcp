package main

// POST /api/edit    {"chat_jid", "message_id", "text"}          — edit an own message
// POST /api/forward {"chat_jid", "message_id", "to_chat_jid"}   — re-send a message elsewhere
//
// Edit sends WhatsApp's EditedMessage protocol message (BuildEdit); WhatsApp
// only accepts it for messages this account sent within its edit window
// (about 15 minutes), so the bridge refuses other rows up front and updates
// the local content once the edit went out.
//
// Forward re-sends the stored content (text, or the cached media file with
// its caption) to another chat through the normal send path. The media is
// fetched first if it is not cached. It arrives as a fresh message, without
// WhatsApp's "Forwarded" label.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type editRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

type forwardRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	ToChatJID string `json:"to_chat_jid"`
}

type editForwardResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
	ChatJID   string `json:"chat_jid,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// editFunc sends the edit; production wraps client.BuildEdit + SendMessage.
type editFunc func(ctx context.Context, chat types.JID, id types.MessageID, text string) error

// UpdateMessageContent rewrites the stored text after an edit.
func (store *MessageStore) UpdateMessageContent(messageID, chatJID, content string) error {
	_, err := store.db.Exec(`UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?`, content, messageID, chatJID)
	return err
}

func writeEditForward(w http.ResponseWriter, status int, resp editForwardResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func parseChatAndMessage(w http.ResponseWriter, policy chatPolicy, rawChat, rawID string) (types.JID, string, bool) {
	rawChat, rawID = strings.TrimSpace(rawChat), strings.TrimSpace(rawID)
	if rawChat == "" || rawID == "" {
		writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "chat_jid and message_id are required"})
		return types.EmptyJID, "", false
	}
	chat, err := types.ParseJID(rawChat)
	if err != nil || chat.User == "" {
		writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "Invalid chat_jid: " + rawChat})
		return types.EmptyJID, "", false
	}
	if rejectByChatPolicy(w, policy, chat.String()) {
		return types.EmptyJID, "", false
	}
	return chat, rawID, true
}

func handleEditMessage(store *MessageStore, edit editFunc, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeEditForward(w, http.StatusMethodNotAllowed, editForwardResponse{Message: "method not allowed"})
			return
		}
		var req editRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "Invalid request format"})
			return
		}
		chat, id, ok := parseChatAndMessage(w, policy, req.ChatJID, req.MessageID)
		if !ok {
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" {
			writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "text must not be empty"})
			return
		}
		isFromMe, err := store.GetMessageIsFromMe(id, chat.String())
		if err != nil {
			writeEditForward(w, http.StatusInternalServerError, editForwardResponse{Message: "Failed to look up message: " + err.Error()})
			return
		}
		if isFromMe == nil {
			writeEditForward(w, http.StatusNotFound, editForwardResponse{Message: "Message not found in local archive"})
			return
		}
		if !*isFromMe {
			writeEditForward(w, http.StatusForbidden, editForwardResponse{Message: "Only messages sent by this account can be edited"})
			return
		}
		if err := edit(r.Context(), chat, id, text); err != nil {
			writeEditForward(w, http.StatusBadGateway, editForwardResponse{Message: "Edit failed: " + err.Error()})
			return
		}
		if err := store.UpdateMessageContent(id, chat.String(), text); err != nil {
			writeEditForward(w, http.StatusOK, editForwardResponse{Success: true, Message: "Message edited (local content not updated: " + err.Error() + ")", MessageID: id, ChatJID: chat.String()})
			return
		}
		writeEditForward(w, http.StatusOK, editForwardResponse{Success: true, Message: "Message edited", MessageID: id, ChatJID: chat.String()})
	}
}

// forwardDeps: the store row lookup, the media fetch and the send are injected.
type forwardDeps struct {
	lookup   func(id, chatJID string) (content, mediaType string, found bool, err error)
	download mediaDownloader
	send     sendFunc
}

func handleForwardMessage(deps forwardDeps, policy chatPolicy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeEditForward(w, http.StatusMethodNotAllowed, editForwardResponse{Message: "method not allowed"})
			return
		}
		var req forwardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "Invalid request format"})
			return
		}
		chat, id, ok := parseChatAndMessage(w, policy, req.ChatJID, req.MessageID)
		if !ok {
			return
		}
		to := strings.TrimSpace(req.ToChatJID)
		if to == "" {
			writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "to_chat_jid is required"})
			return
		}
		if rejectByChatPolicy(w, policy, to) {
			return
		}
		content, mediaType, found, err := deps.lookup(id, chat.String())
		if err != nil {
			writeEditForward(w, http.StatusInternalServerError, editForwardResponse{Message: "Failed to look up message: " + err.Error()})
			return
		}
		if !found {
			writeEditForward(w, http.StatusNotFound, editForwardResponse{Message: "Message not found in local archive"})
			return
		}
		mediaPath := ""
		switch mediaType {
		case "", "poll", "reaction", "poll_vote":
			if mediaType != "" {
				writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "cannot forward a " + mediaType})
				return
			}
			if strings.TrimSpace(content) == "" {
				writeEditForward(w, http.StatusBadRequest, editForwardResponse{Message: "message has no text to forward"})
				return
			}
		default:
			ctx, cancel := requestContext(r, downloadDeadline)
			defer cancel()
			okDl, _, _, path, dlErr := deps.download(ctx, id, chat.String())
			if dlErr != nil || !okDl {
				msg := "media is not available to forward"
				if dlErr != nil {
					msg += ": " + dlErr.Error()
				}
				writeEditForward(w, http.StatusBadGateway, editForwardResponse{Message: msg})
				return
			}
			mediaPath = path
		}
		sendCtx, cancelSend := requestContext(r, sendDeadline)
		defer cancelSend()
		success, msg, sent := deps.send(sendCtx, to, content, mediaPath, "", "", "", nil)
		if !success {
			writeEditForward(w, http.StatusBadGateway, editForwardResponse{Message: "Forward failed: " + msg})
			return
		}
		resp := editForwardResponse{Success: true, Message: "Message forwarded", MessageID: sent.ID, ChatJID: sent.ChatJID}
		if !sent.Timestamp.IsZero() {
			resp.Timestamp = sent.Timestamp.UTC().Format(time.RFC3339)
		}
		writeEditForward(w, http.StatusOK, resp)
	}
}

// messageContentLookup reads content and media_type for forwarding.
func (store *MessageStore) messageContentLookup(id, chatJID string) (string, string, bool, error) {
	var content, mediaType string
	err := store.db.QueryRow(`SELECT content, COALESCE(media_type, '') FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID).Scan(&content, &mediaType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return content, mediaType, true, nil
}
