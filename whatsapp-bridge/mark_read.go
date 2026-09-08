package main

// POST /api/mark-read — send WhatsApp read receipts. Two forms:
//
//	{"chat_jid", "message_ids": [...], "sender_jid"}  one receipt for a listed
//	                                                  set from one sender
//	{"chat_jid", "up_to": "<RFC3339>"}                every inbound message the
//	                                                  read marker has not
//	                                                  covered yet, grouped by
//	                                                  sender, in batches
//
// Both send real receipts, visible on the other person's phone. The whole-chat
// form exists because the natural operation after triaging a conversation
// ("this chat is handled up to now") otherwise makes the caller enumerate
// every ID and group them by sender, which breaks down at the thousand unread
// messages some chats hold.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// markReadFunc abstracts whatsmeow's MarkRead for testing.
type markReadFunc func(ctx context.Context, ids []types.MessageID, readAt time.Time, chat, sender types.JID) error

// markReadDeps are the runtime dependencies of the endpoint. resolve is the
// PN -> LID rewrite applied to the JIDs a receipt is addressed with
// (resolveRecipientJID in production).
type markReadDeps struct {
	store     *MessageStore
	policy    chatPolicy
	connected func() bool
	resolve   func(jid string) (types.JID, error)
	markRead  markReadFunc
	log       waLog.Logger
}

// markReadBatch is how many message IDs travel in one receipt when a whole
// chat is marked read. WhatsApp takes a list per receipt, so a 1,000-message
// backlog costs ten stanzas instead of a thousand; the batch stays small
// enough that a failure loses little, because the read marker only advances
// over what was acknowledged.
const markReadBatch = 100

// markReadMaxMessages caps one whole-chat request. Every message in it turns
// into traffic on someone else's phone, and the rows are held in memory; past
// this point the response says `truncated` and the caller repeats the call,
// which resumes from where the read marker now is. The whole run also shares
// one actionDeadline, and repeating is safe: the marker only ever covers what
// was acknowledged.
const markReadMaxMessages = 2000

// handleMarkRead serves POST /api/mark-read with the live WhatsApp client.
// The dependencies are read off the Bridge per request, not captured when the
// mux is built: Connected and Policy are fields tests (and the reconnect loop)
// replace after startup.
func (b *Bridge) handleMarkRead() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client := b.Client
		markReadHandler(markReadDeps{
			store:     b.Store,
			policy:    b.Policy,
			connected: b.Connected,
			resolve:   func(jid string) (types.JID, error) { return resolveRecipientJID(client, jid) },
			markRead: func(ctx context.Context, ids []types.MessageID, readAt time.Time, chat, sender types.JID) error {
				return client.MarkRead(ctx, ids, readAt, chat, sender)
			},
			log: b.Log,
		})(w, r)
	}
}

func markReadHandler(deps markReadDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		req.ChatJID = strings.TrimSpace(req.ChatJID)
		if req.ChatJID == "" {
			writeError(w, http.StatusBadRequest, "chat_jid is required")
			return
		}
		if rejectByChatPolicy(w, deps.policy, req.ChatJID) {
			return
		}

		// The storage (phone-form) JID: validation, the archive query and the
		// read marker all use it. The LID rewrite happens later, and only for
		// the receipt itself.
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil || chatJID.User == "" || chatJID.Server == "" {
			writeError(w, http.StatusBadRequest, "Invalid chat_jid")
			return
		}

		now := time.Now()
		readAt := now
		if req.Timestamp != "" {
			if readAt, err = time.Parse(time.RFC3339, req.Timestamp); err != nil {
				writeError(w, http.StatusBadRequest, "timestamp must be RFC 3339")
				return
			}
		}

		if len(req.MessageIDs) == 0 {
			markWholeChatRead(w, r, deps, req, chatJID, readAt, now)
			return
		}
		markListedRead(w, r, deps, req, chatJID, readAt)
	}
}

// markListedRead sends a single receipt for the IDs the caller listed; they
// must all come from one chat and one sender.
func markListedRead(w http.ResponseWriter, r *http.Request, deps markReadDeps, req MarkReadRequest, chatJID types.JID, readAt time.Time) {
	if strings.TrimSpace(req.UpTo) != "" {
		writeError(w, http.StatusBadRequest, "up_to is only used without message_ids")
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

	// Parsed for its shape only: the receipt is addressed with the resolved
	// form below, but a malformed sender has to fail before any side effect.
	if req.SenderJID != "" {
		if _, err := parseSenderJID(req.SenderJID); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid sender_jid")
			return
		}
	} else if chatJID.Server == types.GroupServer {
		writeError(w, http.StatusBadRequest, "sender_jid is required for group read receipts")
		return
	}

	if !deps.connected() {
		writeMarkRead(w, http.StatusServiceUnavailable, MarkReadResponse{
			Message: "WhatsApp client is not connected. Please wait for reconnection.",
		})
		return
	}

	// Validate against the storage (phone-form) chat JID before any external
	// side effect: every ID must be an inbound message of this chat and sender.
	if err := deps.store.ValidateInboundMarkRead(req.ChatJID, req.SenderJID, req.MessageIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// MCP storage normalizes chats/senders to phone JIDs; MarkRead routes the
	// receipt `to`/`participant` as given, so resolve PN -> LID the same way
	// sendWhatsAppMessage does or migrated contacts silently fail.
	receiptChat, err := deps.resolve(req.ChatJID)
	if err != nil || receiptChat.User == "" || receiptChat.Server == "" {
		writeError(w, http.StatusBadRequest, "Invalid chat_jid")
		return
	}
	receiptSender := types.EmptyJID
	if req.SenderJID != "" {
		receiptSender, err = deps.resolve(req.SenderJID)
		if err != nil || receiptSender.User == "" || receiptSender.Server == "" {
			writeError(w, http.StatusBadRequest, "Invalid sender_jid")
			return
		}
	}

	ctx, cancel := requestContext(r, actionDeadline)
	defer cancel()
	if err := deps.markRead(ctx, messageIDs, readAt, receiptChat, receiptSender); err != nil {
		writeMarkRead(w, http.StatusInternalServerError, MarkReadResponse{Message: err.Error()})
		return
	}

	// Advance the local read marker immediately so list_chats unread clears
	// without waiting for the self-read receipt round-trip.
	localReadAt := readAt
	if ts, ok, tsErr := deps.store.MaxMessageTimestamp(req.ChatJID, req.MessageIDs); tsErr == nil && ok {
		localReadAt = ts
	}
	persistReadMarker(deps, req.ChatJID, localReadAt)

	writeMarkRead(w, http.StatusOK, MarkReadResponse{
		Success:  true,
		Message:  "Messages marked as read",
		Messages: len(messageIDs),
		Senders:  1,
		Batches:  1,
	})
}

// markWholeChatRead acknowledges every inbound message the local read marker
// has not covered yet, up to req.UpTo (default: now), grouped by sender.
func markWholeChatRead(w http.ResponseWriter, r *http.Request, deps markReadDeps, req MarkReadRequest, chatJID types.JID, readAt, now time.Time) {
	if strings.TrimSpace(req.SenderJID) != "" {
		writeError(w, http.StatusBadRequest, "sender_jid is only used together with message_ids; without them the senders come from the archive")
		return
	}
	upTo := now
	if trimmed := strings.TrimSpace(req.UpTo); trimmed != "" {
		parsed, err := time.Parse(time.RFC3339, trimmed)
		if err != nil {
			writeError(w, http.StatusBadRequest, "up_to must be RFC 3339")
			return
		}
		upTo = parsed
	}
	if !deps.connected() {
		writeMarkRead(w, http.StatusServiceUnavailable, MarkReadResponse{
			Message: "WhatsApp client is not connected. Please wait for reconnection.",
		})
		return
	}

	// One row past the cap is read so a full page can be told apart from a
	// page that happens to end exactly on it.
	pending, err := deps.store.UnreadInboundMessages(req.ChatJID, upTo, markReadMaxMessages+1)
	if err != nil {
		writeMarkRead(w, http.StatusInternalServerError, MarkReadResponse{Message: "Failed to read the chat: " + err.Error()})
		return
	}
	// overflow is the first row left for the next call; its timestamp bounds how
	// far the read marker may advance (see coveredPrefix).
	var overflow *unreadInboundMessage
	if len(pending) > markReadMaxMessages {
		extra := pending[markReadMaxMessages]
		overflow = &extra
		pending = pending[:markReadMaxMessages]
	}
	truncated := overflow != nil
	if len(pending) == 0 {
		writeMarkRead(w, http.StatusOK, MarkReadResponse{Success: true, Message: "No unread inbound messages in that range"})
		return
	}

	receiptChat, err := deps.resolve(req.ChatJID)
	if err != nil || receiptChat.User == "" || receiptChat.Server == "" {
		writeError(w, http.StatusBadRequest, "Invalid chat_jid")
		return
	}

	ctx, cancel := requestContext(r, actionDeadline)
	defer cancel()

	groups, unaddressable := groupBySender(pending, chatJID)
	// Rows nothing can address count as covered without a receipt: they are
	// archive rows whose participant WhatsApp never told us (history sync), and
	// leaving them uncovered would pin the read marker in front of them forever,
	// so the chat could never be marked read at all.
	acked := make(map[string]bool, len(pending))
	for _, id := range unaddressable {
		acked[id] = true
	}
	batches := 0
	var failure error
	fatal := false // a send failure is about the connection: stop the whole run

	for _, group := range groups {
		receiptSender := types.EmptyJID
		if group.sender != "" {
			// A receipt in a group names the participant. The stored sender is
			// the bare user part, phone-form whenever the LID map knew it.
			receiptSender, err = deps.resolve(group.sender)
			if err != nil || receiptSender.User == "" || receiptSender.Server == "" {
				// One unaddressable participant must not cost the other senders
				// their receipts; the marker stops in front of this one anyway.
				failure = fmt.Errorf("could not resolve sender %q", group.sender)
				continue
			}
		}
		for start := 0; start < len(group.ids); start += markReadBatch {
			end := min(start+markReadBatch, len(group.ids))
			chunk := group.ids[start:end]
			if err := deps.markRead(ctx, chunk, readAt, receiptChat, receiptSender); err != nil {
				failure, fatal = err, true
				break
			}
			batches++
			for _, id := range chunk {
				acked[string(id)] = true
			}
		}
		if fatal {
			break
		}
	}

	covered := coveredPrefix(pending, acked, overflow)
	if covered > 0 {
		persistReadMarker(deps, req.ChatJID, pending[covered-1].Timestamp)
	}

	if failure != nil {
		writeMarkRead(w, http.StatusInternalServerError, MarkReadResponse{
			Message: fmt.Sprintf("Acknowledged %d of %d message(s) before failing, read marker covers %d: %v",
				len(acked)-len(unaddressable), len(pending)-len(unaddressable), covered, failure),
			Messages:  covered,
			Senders:   len(groups),
			Batches:   batches,
			Truncated: truncated,
		})
		return
	}
	message := fmt.Sprintf("Marked %d message(s) from %d sender(s) as read", covered, len(groups))
	if truncated {
		message += fmt.Sprintf("; more than %d were pending, call again to continue", markReadMaxMessages)
	}
	writeMarkRead(w, http.StatusOK, MarkReadResponse{
		Success:   true,
		Message:   message,
		Messages:  covered,
		Senders:   len(groups),
		Batches:   batches,
		Truncated: truncated,
	})
}

// coveredPrefix is how many of pending the read marker may cover: the leading
// run of acknowledged rows, cut back so the marker never lands inside a second
// that also holds a row it must not cover.
//
// The next call selects with `timestamp > marker` and WhatsApp timestamps have
// one-second resolution, so a marker set mid-tie would skip the tied rows for
// good. The row it must not cover is the first unacknowledged one, or — when
// everything acknowledged — the first row left over the cap (overflow, nil when
// there is none).
//
// The one case where the cut-back is refused is a page that is entirely one
// second: advancing is then the only way a caller ever makes progress.
func coveredPrefix(pending []unreadInboundMessage, acked map[string]bool, overflow *unreadInboundMessage) int {
	covered := 0
	boundary, bounded := time.Time{}, false
	for _, msg := range pending {
		if !acked[msg.ID] {
			boundary, bounded = msg.Timestamp, true
			break
		}
		covered++
	}
	if !bounded && overflow != nil {
		boundary, bounded = overflow.Timestamp, true
	}
	if !bounded {
		return covered
	}
	trimmed := covered
	for trimmed > 0 && !pending[trimmed-1].Timestamp.Before(boundary) {
		trimmed--
	}
	if trimmed == 0 && covered == len(pending) {
		return covered
	}
	return trimmed
}

// senderBatch is every pending message of one sender, oldest first. An empty
// sender means "no participant on the receipt", which is what a one-to-one
// chat wants.
type senderBatch struct {
	sender string
	ids    []types.MessageID
}

// groupBySender splits the pending rows per sender, keeping first-seen (oldest)
// order so the conversation is acknowledged front to back.
//
// A one-to-one chat collapses into a single group: its receipt carries no
// participant, so the stored sender — which may be a LID user the map could not
// rewrite — is not needed. Anything else (a group, and broadcast or newsletter
// JIDs, which the archive stores like any other chat) needs a participant per
// receipt.
//
// The second return is the IDs no participant can be derived for: rows the
// phone replayed without one, which history sync attributes to the chat itself
// (history_sync.go). They get no receipt; the caller counts them as covered so
// they cannot pin the read marker in front of the rest of the chat.
func groupBySender(pending []unreadInboundMessage, chatJID types.JID) ([]senderBatch, []string) {
	if chatJID.Server == types.DefaultUserServer || chatJID.Server == types.HiddenUserServer {
		ids := make([]types.MessageID, 0, len(pending))
		for _, msg := range pending {
			ids = append(ids, types.MessageID(msg.ID))
		}
		return []senderBatch{{ids: ids}}, nil
	}
	index := make(map[string]int, 4)
	groups := make([]senderBatch, 0, 4)
	var unaddressable []string
	for _, msg := range pending {
		if msg.Sender == "" || msg.Sender == chatJID.User {
			unaddressable = append(unaddressable, msg.ID)
			continue
		}
		at, ok := index[msg.Sender]
		if !ok {
			at = len(groups)
			index[msg.Sender] = at
			groups = append(groups, senderBatch{sender: msg.Sender})
		}
		groups[at].ids = append(groups[at].ids, types.MessageID(msg.ID))
	}
	return groups, unaddressable
}

// parseSenderJID accepts either a full JID or a bare phone number.
func parseSenderJID(sender string) (types.JID, error) {
	sender = strings.TrimSpace(sender)
	if !strings.Contains(sender, "@") {
		jid := types.NewJID(sender, types.DefaultUserServer)
		if jid.User == "" {
			return types.EmptyJID, fmt.Errorf("invalid sender_jid %q", sender)
		}
		return jid, nil
	}
	jid, err := types.ParseJID(sender)
	if err != nil || jid.User == "" || jid.Server == "" {
		return types.EmptyJID, fmt.Errorf("invalid sender_jid %q", sender)
	}
	return jid, nil
}

// persistReadMarker advances chats.last_read_time. The receipts have already
// gone out by then, so a failure is logged and the caller still hears what was
// marked.
func persistReadMarker(deps markReadDeps, chatJID string, readAt time.Time) {
	if err := deps.store.MarkChatRead(chatJID, readAt); err != nil {
		deps.log.Warnf("failed to persist local read marker for %s: %v", chatJID, err)
	}
}

func writeMarkRead(w http.ResponseWriter, status int, resp MarkReadResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
