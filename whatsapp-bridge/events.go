package main

// Live event handling: the whatsmeow event dispatcher, inbound message
// processing (handleMessage), calls, protocol messages and the reconnect
// loop. History sync lives in history_sync.go.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func updateChatEphemeralSettingsFromProtocolMessage(messageStore *MessageStore, chatJID string, msg *waE2E.Message, eventTimestamp int64, logger waLog.Logger) {
	if msg == nil || msg.GetProtocolMessage() == nil {
		return
	}

	protoMsg := msg.GetProtocolMessage()
	if protoMsg.GetType() != waE2E.ProtocolMessage_EPHEMERAL_SETTING {
		return
	}

	expiration := protoMsg.GetEphemeralExpiration()
	settingTimestamp := protoMsg.GetEphemeralSettingTimestamp()
	// Fall back to the carrier event's timestamp rather than time.Now() so a
	// late-arriving older event doesn't get stamped "newer than" subsequent
	// updates and then block them via the monotonic WHERE clause in
	// UpdateChatEphemeralSettings.
	if settingTimestamp == 0 {
		settingTimestamp = eventTimestamp
	}

	if err := messageStore.UpdateChatEphemeralSettings(chatJID, expiration, settingTimestamp); err != nil {
		logger.Warnf("Failed to update ephemeral settings for %s: %v", chatJID, err)
	}
}

// handleMessageRevoke records a "delete for everyone" event by stamping
// deleted_at on the target message row. The original content is kept on
// purpose so the local archive can still surface what was retracted.
//
// chatJID is the already-LID-normalised chat from the carrier event;
// using it (rather than Key.RemoteJID, which may carry the raw @lid
// form) keeps the UPDATE aligned with how StoreMessage wrote the row.
func handleMessageRevoke(messageStore *MessageStore, msg *waE2E.Message, chatJID string, eventTimestamp int64, logger waLog.Logger) {
	if msg == nil || msg.GetProtocolMessage() == nil {
		return
	}
	protoMsg := msg.GetProtocolMessage()
	if protoMsg.GetType() != waE2E.ProtocolMessage_REVOKE {
		return
	}
	key := protoMsg.GetKey()
	if key == nil {
		return
	}
	targetID := key.GetID()
	if targetID == "" {
		return
	}
	deletedAt := time.Unix(eventTimestamp, 0)
	if err := messageStore.MarkMessageDeleted(targetID, chatJID, deletedAt); err != nil {
		logger.Warnf("Failed to mark message %s in %s as deleted: %v", targetID, chatJID, err)
	}
}

// Handle regular incoming messages with media support
// originalTimestamps remembers the true send-time of messages that first
// arrived undecryptable (e.g. after an offline gap, when our session lacked
// the sender key). WhatsApp re-sends such messages after a retry receipt, but
// the re-sent copy carries a fresh `t` (the resend time) rather than the
// original send time. The first (undecryptable) delivery *does* carry the
// original `t`, so we cache it and reuse it when the decrypted retry finally
// lands — otherwise those messages get stored with reconnect-time and corrupt
// recency ordering.
type originalTimestamps struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newOriginalTimestamps() *originalTimestamps {
	return &originalTimestamps{m: make(map[string]time.Time)}
}

// remember records the earliest timestamp seen for a message ID. A resend's
// `t` is always >= the original, so the earliest is the true one.
func (o *originalTimestamps) remember(id string, ts time.Time) {
	if o == nil || id == "" || ts.IsZero() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if existing, ok := o.m[id]; !ok || ts.Before(existing) {
		o.m[id] = ts
	}
	// Soft cap so a burst of never-retried undecryptable messages can't grow this
	// map unbounded. Entries are normally consumed on successful decrypt.
	if len(o.m) > 5000 {
		for k := range o.m {
			delete(o.m, k)
			if len(o.m) <= 4000 {
				break
			}
		}
	}
}

// take returns and removes the cached original timestamp for id.
func (o *originalTimestamps) take(id string) (time.Time, bool) {
	if o == nil {
		return time.Time{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ts, ok := o.m[id]
	if ok {
		delete(o.m, id)
	}
	return ts, ok
}

func (b *Bridge) handleMessage(msg *events.Message) {
	client, messageStore, logger := b.Client, b.Store, b.Log
	// View-once envelopes hide the real media one level down; unwrap so every
	// extractor below sees it. See view_once.go for the policy.
	viewOnce := false
	if inner, wrapped := unwrapViewOnce(msg.Message); wrapped {
		msg.Message = inner
		viewOnce = true
	}
	// Resolve LID-based chats to phone-based JIDs so that incoming
	// and outgoing messages land in the same chat entry.
	resolvedChat := resolveLIDChat(client, msg.Info.Chat, msg.Info.SenderAlt, msg.Info.RecipientAlt, msg.Info.IsFromMe)
	chatJID := resolvedChat.String()
	// Resolve the *sender* with a sender-specific alt so that outgoing-from-self
	// messages don't get tagged with the recipient's phone number, and incoming
	// messages from LID-only peers get rewritten to their phone user-part when
	// the LID store has a mapping.
	resolvedSender := resolveUserJID(client, msg.Info.Sender, senderAltForMessage(client, msg.Info))
	sender := resolvedSender.User

	// Get appropriate chat name (pass resolved JID so contact lookup works)
	name := GetChatName(client, messageStore, resolvedChat, chatJID, nil, sender, true, logger)

	// If contact resolution fails (common for LIDs), PushName is often the best available display name.
	// Only apply for direct messages (not groups) and only when the stored name is the numeric JID user.
	if !msg.Info.IsFromMe && msg.Info.Chat.Server != "g.us" && strings.TrimSpace(msg.Info.PushName) != "" {
		pushName := strings.TrimSpace(msg.Info.PushName)
		if name == "" || name == msg.Info.Chat.User {
			logger.Infof("Updating chat name from PushName for %s: %s -> %s", chatJID, name, pushName)
			name = pushName
		}
	}

	// Recover the true send-time if this message first arrived undecryptable and is
	// now landing via a retry-resend (whose stanza `t` is the resend time, not the
	// original). See originalTimestamps.
	msgTimestamp := msg.Info.Timestamp
	if orig, ok := b.origTimes.take(msg.Info.ID); ok && orig.Before(msgTimestamp) {
		logger.Infof("Using original pre-retry timestamp for %s: %s (resend `t` was %s)",
			msg.Info.ID, orig.Format(time.RFC3339), msgTimestamp.Format(time.RFC3339))
		msgTimestamp = orig
	}

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msgTimestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	updateChatEphemeralSettingsFromProtocolMessage(messageStore, chatJID, msg.Message, msg.Info.Timestamp.Unix(), logger)
	handleMessageRevoke(messageStore, msg.Message, chatJID, msg.Info.Timestamp.Unix(), logger)

	// Backfill ephemeral state from any regular message's ContextInfo.
	// EPHEMERAL_SETTING ProtocolMessages and GroupInfo events only fire on
	// changes, so chats whose disappearing timer was set before the bridge
	// started (or before this code shipped) would otherwise stay invisible
	// to outgoing-message logic.
	if backfill := extractChatEphemeralFromMessage(msg.Message); backfill.SettingTimestamp != 0 {
		if err := messageStore.UpdateChatEphemeralSettings(chatJID, backfill.Expiration, backfill.SettingTimestamp); err != nil {
			logger.Warnf("Failed to backfill ephemeral settings for %s: %v", chatJID, err)
		}
	}

	// Poll votes arrive as PollUpdateMessage stanzas: decrypt, map to option
	// names, keep a structured copy for /api/poll and a message row with the
	// poll's ID in `filename` (same convention as reactions). See polls.go.
	if handled, pollID, voteContent := handlePollVote(context.Background(), b.PollVoteDecrypt, messageStore, msg, chatJID, sender, msgTimestamp, logger); handled {
		if voteContent != "" {
			messageStore.storePollVoteMessage(msg.Info.ID, chatJID, sender, voteContent, msgTimestamp, msg.Info.IsFromMe, pollID, logger)
		}
		return
	}

	// Reactions arrive as their own message stanza rather than message content.
	// Persist them in the messages table as media_type="reaction", with the
	// emoji in `content` and the reacted-to message ID in `filename`, then
	// return — a reaction is not a normal content message. An empty emoji is a
	// valid event meaning "reaction removed"; we store it (so consumers see the
	// removal) rather than dropping it.
	if reaction := msg.Message.GetReactionMessage(); reaction != nil {
		reactedToID := ""
		if key := reaction.GetKey(); key != nil {
			reactedToID = key.GetID()
		}
		if reactedToID != "" {
			emoji := reaction.GetText()
			if err := messageStore.StoreMessage(
				msg.Info.ID, chatJID, sender, emoji,
				msgTimestamp, msg.Info.IsFromMe,
				"reaction", reactedToID, "", nil, nil, nil, 0, "",
			); err != nil {
				logger.Warnf("Failed to store reaction: %v", err)
			} else if err := messageStore.SetTargetMessageID(msg.Info.ID, chatJID, reactedToID); err != nil {
				logger.Warnf("Failed to set reaction target: %v", err)
			}
			if b.ForwardSelf || !msg.Info.IsFromMe {
				b.Webhook.SendReactionWebhook(sender, chatJID, msg.Info.IsFromMe, msg.Info.ID, reactedToID, emoji)
			}
		}
		return
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info - pass message timestamp + ID for unique filenames.
	// Must be the same (retry-corrected) timestamp we store below: downloadMedia
	// rebuilds the on-disk filename from the stored timestamp.
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message, msgTimestamp, msg.Info.ID)

	// Native polls: the creation carries no text, so render question + options
	// as content and remember the option list for vote decoding (polls.go).
	poll := extractPollCreation(msg.Message)
	if poll != nil {
		content = pollContent(poll)
		mediaType = "poll"
	}
	if viewOnce {
		content = viewOnceContent(content, mediaType)
	}

	// Extract quoted message info
	quotedMessageId, quotedSender, quotedContent := extractQuotedMessageInfo(msg.Message)
	mentionedJIDs := extractMentionedJIDs(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database first so that downloadMedia (which queries the DB
	// by message ID) can find the row when we call it synchronously below.
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msgTimestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
		quotedMessageId,
	)
	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	}
	if poll != nil {
		if err := messageStore.StorePoll(msg.Info.ID, chatJID, poll, msgTimestamp); err != nil {
			logger.Warnf("Failed to store poll: %v", err)
		}
	}
	if viewOnce {
		if err := messageStore.MarkViewOnce(msg.Info.ID, chatJID); err != nil {
			logger.Warnf("Failed to flag view-once message: %v", err)
		}
	}

	var quotedIsFromMe *bool
	if quotedMessageId != "" {
		var lookupErr error
		quotedIsFromMe, lookupErr = messageStore.GetMessageIsFromMe(quotedMessageId, chatJID)
		if lookupErr != nil {
			logger.Warnf("Failed to resolve quoted message origin: %v", lookupErr)
		}
	}

	// Avoid webhook-only image work when no webhook will receive the message. Media
	// still downloads asynchronously in that case so it remains available to MCP
	// tools, but message handling never blocks on a disabled outbound webhook.
	shouldForward := webhooksEnabled() && (b.ForwardSelf || !msg.Info.IsFromMe)

	// For image messages that will be forwarded, download media synchronously so we
	// can include the base64 payload in the webhook. Other media types (and images
	// when webhook forwarding is disabled) download asynchronously for caching.
	var imageDownloadPath string
	var imageMimeType string
	if mediaType == "image" && url != "" && len(mediaKey) > 0 && shouldForward {
		logger.Infof("Downloading image media for message %s (synchronous)", msg.Info.ID)
		success, _, _, dlPath, dlErr := b.DownloadMedia(msg.Info.ID, chatJID)
		if success && dlErr == nil {
			imageDownloadPath = dlPath
			// Detect MIME type by sniffing the actual file bytes rather than
			// trusting the generated filename extension (always .jpg).
			if f, openErr := os.Open(dlPath); openErr == nil { //nolint:gosec // dlPath is built by downloadMedia under the store directory
				buf := make([]byte, 512)
				if n, readErr := f.Read(buf); readErr == nil || n > 0 {
					imageMimeType = http.DetectContentType(buf[:n])
				}
				_ = f.Close()
			}
			if imageMimeType == "" {
				imageMimeType = "application/octet-stream"
			}
			logger.Infof("✅ Image downloaded: %s (%s)", dlPath, imageMimeType)
		} else {
			logger.Warnf("❌ Image download failed: %v", dlErr)
			// Fall back to async download so media is cached for future MCP tool calls
			if b.MediaAutoDownload {
				go func() {
					_, _, _, _, _ = b.DownloadMedia(msg.Info.ID, chatJID)
				}()
			}
		}
	} else if mediaType != "" && url != "" && len(mediaKey) > 0 && b.MediaAutoDownload {
		// Media that is not included in a webhook payload: async download for caching.
		logger.Infof("Auto-downloading %s media for message %s", mediaType, msg.Info.ID)
		go func() {
			success, _, _, downloadPath, err := b.DownloadMedia(msg.Info.ID, chatJID)
			if success && err == nil {
				logger.Infof("✅ Auto-downloaded media: %s", downloadPath)
			} else {
				logger.Warnf("❌ Auto-download failed: %v", err)
			}
		}()
	}

	// Send webhook for incoming messages.
	// Forward self-messages when FORWARD_SELF=true.
	// Always forward image messages (even without a text caption) so the AI vision
	// pipeline can analyse the image content.
	hasText := content != ""
	hasImage := mediaType == "image"

	if shouldForward && (hasText || hasImage) {
		if hasImage {
			b.Webhook.SendWebhookWithMedia(
				sender, content, chatJID, msg.Info.IsFromMe,
				quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs,
				msg.Info.ID, mediaType, imageMimeType, filename, imageDownloadPath,
			)
		} else {
			b.Webhook.SendWebhookWithMessageID(sender, content, chatJID, msg.Info.IsFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, msg.Info.ID)
		}
	}

	if err == nil {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			bridgeLog.Debugf("[%s] %s %s: [%s: %s] %s", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			bridgeLog.Debugf("[%s] %s %s: %s", timestamp, direction, sender, content)
		}
	}
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func lookupLocalContactName(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, logger waLog.Logger) string {
	if client == nil || client.Store == nil || client.Store.ID == nil || messageStore == nil || messageStore.waDB == nil {
		return ""
	}

	var localName string
	err := messageStore.waDB.QueryRow(
		`SELECT COALESCE(
			NULLIF(full_name, ''),
			NULLIF(push_name, ''),
			NULLIF(first_name, ''),
			NULLIF(business_name, ''),
			''
		) FROM whatsmeow_contacts WHERE our_jid = ? AND their_jid = ?`,
		client.Store.ID.String(),
		chatJID,
	).Scan(&localName)
	if err == nil {
		if localName != "" {
			logger.Infof("Using local contact name for %s: %s", chatJID, localName)
		}
		return localName
	}
	if err != sql.ErrNoRows && !strings.Contains(err.Error(), "no such table: whatsmeow_contacts") {
		logger.Warnf("Failed to query local contact name for %s: %v", chatJID, err)
	}
	return ""
}

// callChatJID resolves the chat JID that a call belongs to. For group calls
// this is the group JID; for 1:1 calls it's the call creator's JID — which
// stays stable across the entire lifecycle (Offer → Accept → Terminate).
//
// meta.From is NOT reliable as the chat key: for Accept events that fire
// when the user picks up on their phone, meta.From is the *accepting*
// device's JID (our own), not the other party's. Using From caused Accept
// UPDATEs to miss the row stored at Offer time, so the state machine fell
// through to "missed" when the user answered elsewhere.
//
// meta.CallCreator is populated from the stanza's call-creator attribute,
// which WhatsApp keeps consistent for every event in the call.
func callChatJID(meta types.BasicCallMeta) string {
	if !meta.GroupJID.IsEmpty() {
		return meta.GroupJID.String()
	}
	if !meta.CallCreator.IsEmpty() {
		return meta.CallCreator.ToNonAD().String()
	}
	return meta.From.ToNonAD().String()
}

// handleCallOffer stores a new call row. The isFromMe path is defensive —
// in practice WhatsApp's primary device handles outbound calls without
// notifying linked devices, so events observed here are always inbound and
// isFromMe stays false. We keep the branch anyway in case behavior changes.
func handleCallOffer(client *whatsmeow.Client, messageStore *MessageStore, meta types.BasicCallMeta, callType string, isGroup bool, logger waLog.Logger) {
	chatJID := callChatJID(meta)

	fromJID := ""
	switch {
	case !meta.CallCreator.IsEmpty():
		fromJID = meta.CallCreator.ToNonAD().String()
	case !meta.From.IsEmpty():
		fromJID = meta.From.ToNonAD().String()
	}

	isFromMe := client.Store.ID != nil && fromJID == client.Store.ID.ToNonAD().String()

	if err := messageStore.StoreCallOffer(meta.CallID, chatJID, fromJID, meta.Timestamp, isFromMe, callType, isGroup); err != nil {
		logger.Warnf("Failed to store call offer: %v", err)
		return
	}

	kind := "Call"
	if isGroup {
		kind = "Group call"
	}
	direction := "incoming"
	if isFromMe {
		direction = "outgoing"
	}
	logger.Infof("%s %s: id=%s type=%s from=%s chat=%s",
		kind, direction, meta.CallID, callType, fromJID, chatJID)
}

// Exit codes for conditions the bridge cannot recover from in-place.
const (
	exitCodeLoggedOut      = 3
	exitCodeClientOutdated = 4
)

// handleEvent dispatches whatsmeow events. reconnectChan is signalled on
// connection loss so reconnectLoop can dial again.
func (b *Bridge) handleEvent(evt interface{}, reconnectChan chan<- bool) {
	switch v := evt.(type) {
	case *events.Message:
		// Process regular messages
		b.handleMessage(v)

	case *events.UndecryptableMessage:
		// The first (failed) delivery carries the original send-time. WhatsApp
		// re-sends after our retry receipt, but that copy's `t` is the resend
		// time — so stash the original now and reuse it in handleMessage.
		b.origTimes.remember(v.Info.ID, v.Info.Timestamp)

	case *events.HistorySync:
		// Process history sync events
		b.handleHistorySync(v)

	case *events.MediaRetry:
		// The sender's phone answered a media-retry request issued by
		// downloadMedia (see media_retry.go); route it to the waiting call.
		if !b.mediaRetry.dispatch(v) {
			b.Log.Debugf("Unclaimed media retry response for %s", v.MessageID)
		}

	case *events.Receipt:
		// Persist read state so consumers can distinguish genuine unread
		// from "latest message is inbound". Only our own reads count.
		if isSelfReadReceipt(v) {
			chatJID := resolveLIDChat(b.Client, v.Chat, v.SenderAlt, v.RecipientAlt, v.IsFromMe).String()
			// Prefer the acknowledged messages' timestamps over the
			// receipt event time so out-of-order delivery cannot advance
			// the marker past an unread message.
			readAt := v.Timestamp
			ids := make([]string, len(v.MessageIDs))
			for i, id := range v.MessageIDs {
				ids[i] = string(id)
			}
			if ts, ok, err := b.Store.MaxMessageTimestamp(chatJID, ids); err != nil {
				b.Log.Warnf("Failed to look up read receipt message times for %s: %v", chatJID, err)
			} else if ok {
				readAt = ts
			}
			if err := b.Store.MarkChatRead(chatJID, readAt); err != nil {
				b.Log.Warnf("Failed to mark chat %s read: %v", chatJID, err)
			}
		}

	case *events.GroupInfo:
		if v.Name != nil && strings.TrimSpace(v.Name.Name) != "" {
			if err := b.Store.RenameChat(v.JID.String(), v.Name.Name); err != nil {
				b.Log.Warnf("Failed to store group rename for %s: %v", v.JID, err)
			} else {
				b.Log.Infof("Group %s renamed to %q", v.JID, v.Name.Name)
			}
		}
		if v.Ephemeral != nil {
			expiration := uint32(0)
			if v.Ephemeral.IsEphemeral {
				expiration = v.Ephemeral.DisappearingTimer
			}
			if err := b.Store.UpdateChatEphemeralSettings(v.JID.String(), expiration, v.Timestamp.Unix()); err != nil {
				b.Log.Warnf("Failed to store group ephemeral settings for %s: %v", v.JID, err)
			}
		}

	case *events.CallOffer:
		// 1:1 incoming call. call_type defaults to "voice"; CallOffer
		// doesn't expose Media directly (it's buried in the binary Data
		// node). Group calls come through CallOfferNotice instead, which
		// DOES expose Media cleanly.
		handleCallOffer(b.Client, b.Store, v.BasicCallMeta, "voice", false, b.Log)

	case *events.CallOfferNotice:
		// Group calls. v.Media is "audio" or "video"; normalize to our
		// "voice"/"video" convention.
		callType := "voice"
		if v.Media == "video" {
			callType = "video"
		}
		isGroup := v.Type == "group" || !v.GroupJID.IsEmpty()
		handleCallOffer(b.Client, b.Store, v.BasicCallMeta, callType, isGroup, b.Log)

	case *events.CallAccept:
		if err := b.Store.MarkCallAnswered(v.CallID, callChatJID(v.BasicCallMeta)); err != nil {
			b.Log.Warnf("Failed to mark call answered: %v", err)
		} else {
			b.Log.Infof("Call answered: id=%s", v.CallID)
		}

	case *events.CallReject:
		if err := b.Store.MarkCallRejected(v.CallID, callChatJID(v.BasicCallMeta)); err != nil {
			b.Log.Warnf("Failed to mark call rejected: %v", err)
		} else {
			b.Log.Infof("Call rejected: id=%s", v.CallID)
		}

	case *events.CallTerminate:
		if err := b.Store.MarkCallTerminated(v.CallID, callChatJID(v.BasicCallMeta), v.Reason, v.Timestamp); err != nil {
			b.Log.Warnf("Failed to mark call terminated: %v", err)
		} else {
			b.Log.Infof("Call terminated: id=%s reason=%q", v.CallID, v.Reason)
		}

	case *events.Connected:
		b.Log.Infof("✓ Successfully connected to WhatsApp servers")

	case *events.LoggedOut:
		// whatsmeow has already wiped the device row; the process cannot re-enter
		// the pairing flow from here. Exit and let the supervisor restart us: the
		// next start finds no session and prints a fresh QR code.
		b.Exit(fmt.Sprintf("device logged out by the phone (reason: %v); exiting so the next start pairs again", v.Reason), exitCodeLoggedOut)

	case *events.Disconnected:
		b.Log.Warnf("⚠️  Disconnected from WhatsApp servers, will attempt reconnection...")
		// Signal reconnection needed
		select {
		case reconnectChan <- true:
		default:
			// Channel already has a reconnect signal
		}

	case *events.ConnectFailure:
		b.Log.Errorf("❌ Connection failure: %v", v.Reason)
		// Signal reconnection needed
		select {
		case reconnectChan <- true:
		default:
		}

	case *events.StreamError:
		b.Log.Errorf("❌ Stream error: %v", v.Code)
		// Signal reconnection needed
		select {
		case reconnectChan <- true:
		default:
		}

	case *events.StreamReplaced:
		// Another WhatsApp Web session took our slot. whatsmeow treats this
		// as a "permanent" disconnect and suppresses the Disconnected event,
		// so we must handle it explicitly. Wait briefly to avoid ping-ponging
		// with the other b.Client, then reconnect.
		b.Log.Warnf("⚠️  Stream replaced by another session — will reconnect after 30s")
		go func() {
			select {
			case <-time.After(streamReplacedDelay):
			case <-b.ctx.Done():
				return
			}
			select {
			case reconnectChan <- true:
			default:
			}
		}()

	case *events.ClientOutdated:
		b.Exit("WhatsApp rejected this client version as outdated; rebuild with a newer whatsmeow (AGENTS.md §2 bump routine)", exitCodeClientOutdated)
	}
}

// streamReplacedDelay is how long to wait before reconnecting after another
// session took our slot (avoids ping-ponging with it). Tests shorten it.
var streamReplacedDelay = 30 * time.Second

// reconnectLoop redials with exponential backoff whenever handleEvent
// reports a lost connection, until Shutdown cancels b.ctx. The backoff wait
// is interruptible so shutdown never waits out a five-minute sleep.
func (b *Bridge) reconnectLoop(reconnectChan chan bool) {
	reconnectBackoff := time.Second * 5
	maxBackoff := time.Minute * 5

	for {
		select {
		case <-reconnectChan:
			b.Log.Infof("🔄 Attempting to reconnect...")

			// Wait before reconnecting, unless we are shutting down
			select {
			case <-time.After(reconnectBackoff):
			case <-b.ctx.Done():
				return
			}

			// Try to reconnect
			if !b.Client.IsConnected() {
				err := b.Connect()
				if err != nil {
					b.Log.Errorf("❌ Reconnection failed: %v", err)
					// Increase backoff for next attempt
					reconnectBackoff = reconnectBackoff * 2
					if reconnectBackoff > maxBackoff {
						reconnectBackoff = maxBackoff
					}
					// Signal another reconnection attempt
					select {
					case reconnectChan <- true:
					default:
					}
				} else {
					b.Log.Infof("✓ Reconnected successfully")
					// Reset backoff on successful connection
					reconnectBackoff = time.Second * 5
				}
			} else {
				b.Log.Infof("Already connected, skipping reconnection")
				reconnectBackoff = time.Second * 5
			}

		case <-b.ctx.Done():
			return
		}
	}
}
