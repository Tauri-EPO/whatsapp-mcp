package main

// Live event handling: the whatsmeow event dispatcher, inbound message
// processing (handleMessage), calls, protocol messages and the reconnect
// loop. History sync lives in history_sync.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
func (b *Bridge) handleMessageRevoke(msg *waE2E.Message, chatJID string, eventTimestamp int64) {
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
	b.storeLive("retracted message", targetID, chatJID, func() error { return b.Store.MarkMessageDeleted(targetID, chatJID, deletedAt) })
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
	lookupCtx := b.ctx
	if lookupCtx == nil {
		lookupCtx = context.Background()
	}
	// The SDK may acknowledge a delivered message during shutdown. Preserve
	// its final archive write; only SDK reads and retry waits are cancellable.
	writeCtx := context.WithoutCancel(lookupCtx)
	client, messageStore, logger := b.currentClient(), b.Store, b.Log
	// View-once envelopes hide the real media one level down. Work on a local
	// copy of the event with the envelope removed so every extractor below
	// sees the real message and other handlers keep the original untouched.
	original := msg.Message
	// ParseWebMessage unwraps an edit into its new text and target ID. Its
	// RawMessage retains the protocol key/timestamp needed for ordered updates.
	// A direct or ephemeral protocol edit may have IsEdit=false in the SDK.
	if msg.RawMessage != nil && extractMessage(msg.RawMessage, msg.Info.Timestamp, msg.Info.ID).edit != nil {
		original = msg.RawMessage
	}
	if inner, wrapped := unwrapViewOnce(msg.Message); wrapped {
		unwrapped := *msg
		unwrapped.Message = inner
		msg = &unwrapped
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
	// What the row records: the same user part plus the namespace it lives in,
	// so an unresolved LID is never read back as a phone number (#375).
	storedSenderJID := storedSender(resolvedSender)

	// Get appropriate chat name (pass resolved JID so contact lookup works)
	name := GetChatName(client, messageStore, resolvedChat, chatJID, nil, sender, true, logger)

	// If contact resolution fails (common for LIDs), PushName is often the best available display name.
	// Only apply for direct messages (not groups) and only when the stored name is the numeric JID user,
	// in the namespace the chat arrived in or the one it was resolved to.
	if !msg.Info.IsFromMe && msg.Info.Chat.Server != "g.us" && strings.TrimSpace(msg.Info.PushName) != "" {
		pushName := strings.TrimSpace(msg.Info.PushName)
		if name == "" || name == msg.Info.Chat.User || name == resolvedChat.User {
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

	// storeRow writes one row of this message together with what it needs and
	// implies: the chat row it references (and that row's name) before, the
	// chat's last message time after. The first two share one retry, so a new
	// chat that meets a busy database is not lost to the foreign key on the
	// second attempt; the time moves only once the row is in, so a message that
	// is dropped below, or that fails to store, is not activity (issues #519,
	// #531).
	rowConsumed := false
	storeRow := func(kind string, write func() error) bool {
		if !b.storeLive(kind, msg.Info.ID, chatJID, func() error {
			if err := messageStore.EnsureChat(chatJID, name); err != nil {
				return err
			}
			return write()
		}) {
			return false
		}
		if rowConsumed {
			return true
		}
		if err := b.retryBusy(func() error { return messageStore.StoreChat(chatJID, "", msgTimestamp) }); err != nil {
			logger.Warnf("Failed to update the last message time of %s: %v", chatJID, err)
		}
		return true
	}

	// A group sender we have no roster row for is a member we know about
	// (group_events.go); no-op for DMs and for members already recorded.
	b.noteGroupSender(chatJID, resolvedSender, msgTimestamp)

	updateChatEphemeralSettingsFromProtocolMessage(messageStore, chatJID, msg.Message, msg.Info.Timestamp.Unix(), logger)
	b.handleMessageRevoke(msg.Message, chatJID, msg.Info.Timestamp.Unix())

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
	if handled, pollID, voteContent := b.handlePollVote(context.Background(), msg, chatJID, sender, msgTimestamp); handled {
		if voteContent != "" {
			storeRow("poll vote", func() error {
				var err error
				rowConsumed, err = messageStore.storePollVoteMessageResult(msg.Info.ID, chatJID, storedSenderJID, voteContent, msgTimestamp, msg.Info.IsFromMe, pollID, logger)
				return err
			})
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
			stored := storeRow("reaction", func() error {
				return messageStore.Batch(func(batch *messageBatch) error {
					ex := extractedMessage{content: emoji, mediaType: "reaction", filename: reactedToID, hasLength: true}
					var err error
					rowConsumed, err = persistMessageResult(batch, msg.Info.ID, chatJID, storedSenderJID, msgTimestamp, msg.Info.IsFromMe, ex, false, logger)
					if err != nil || rowConsumed {
						return err
					}
					return batch.write(func() error { return setTargetMessageIDWith(batch.tx, msg.Info.ID, chatJID, reactedToID) })
				})
			})
			if rowConsumed {
				return
			}
			if b.forwardsToWebhook(resolvedChat, msg.Info.IsFromMe, msg.Info.Chat) {
				b.Webhook.SendReactionWebhook(sender, chatJID, msg.Info.IsFromMe, msg.Info.ID, reactedToID, emoji, stored)
			} else {
				b.logWebhookWithheld(resolvedChat, msg.Info.ID)
			}
		}
		return
	}

	// Extract text content
	// Text, media, poll, quote and mentions in one pass (persist.go). The
	// timestamp must be the retry-corrected one stored below: downloadMedia
	// rebuilds the on-disk filename from the stored row.
	ex := extractMessage(original, msgTimestamp, msg.Info.ID)
	ex.retryChat, ex.retrySender = msg.Info.Chat.String(), msg.Info.Sender.ToNonAD().String()
	// whatsmeow unwraps live envelopes before dispatch and keeps this flag.
	if msg.IsViewOnce && !ex.viewOnce {
		ex.viewOnce = true
		ex.content = viewOnceContent(ex.content, ex.mediaType)
	}
	content, mediaType, filename, fileLength := ex.content, ex.mediaType, ex.filename, ex.fileLen
	quotedMessageId, quotedSender, quotedContent := ex.quotedID, ex.quotedSender, ex.quotedContent
	mentionedJIDs := ex.mentions

	// Group history shared when a member is added has neither text nor media,
	// so recognise it before the gate and import bundles through history sync.
	// Every
	// device of the group may see the message, the one that did the add
	// included, hence from_me. Counts and timestamps only: never the
	// receivers, the path or the keys.
	b.handleHistoryShare(ex.inner, chatJID, msg.Info.ID, msg.Info.IsFromMe)

	// Skip if there's no content and no media
	if ex.empty() {
		return
	}
	// Store message in database first so that downloadMedia (which queries the DB
	// by message ID) can find the row when we call it synchronously below.
	// A busy database is tried again a bounded number of times; a write that is
	// given up is one ERROR naming the message (ID and chat only, never the
	// content) and a count on /metrics (store_failures.go).
	writePreparedMessage := func() error {
		locationSender := storedSenderJID
		if ex.edit != nil || ex.location != nil && ex.location.Live {
			var err error
			targetID := msg.Info.ID
			if ex.edit != nil {
				targetID = ex.edit.GetKey().GetID()
			}
			locationSender, err = b.liveLocationSender(lookupCtx, messageStore.db, targetID, chatJID, storedSenderJID, msg.Info.IsFromMe)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				locationSender = storedSenderJID
			}
		}
		if ex.edit == nil {
			var err error
			ex.editAuthorAlias, ex.editChatAlias, ex.editPreparation, err = b.preparePendingEdit(lookupCtx, msg.Info.ID, chatJID, locationSender, msg.Info.IsFromMe)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				ex.editAuthorAlias, ex.editChatAlias = "", ""
			}
			// A delivery's verified phone alternative may be available before
			// the SDK map catches up. Retain its original LID author for matching.
			if ex.editAuthorAlias == "" && msg.Info.Sender.Server == types.HiddenUserServer &&
				resolvedSender.Server == types.DefaultUserServer && locationSender == storedSender(resolvedSender) {
				ex.editAuthorAlias = msg.Info.Sender.ToNonAD().String()
			}
			// Incoming PN deliveries can carry the verified reverse LID hint.
			// An outgoing SenderAlt may identify the peer, never our own author.
			if ex.editAuthorAlias == "" && !msg.Info.IsFromMe && locationSender == storedSender(resolvedSender) {
				author, authorOK := pendingEditUserJID(msg.Info.Sender.String())
				alt, altOK := pendingEditUserJID(msg.Info.SenderAlt.String())
				if authorOK && altOK && author.Server == types.DefaultUserServer && alt.Server == types.HiddenUserServer {
					ex.editAuthorAlias = alt.String()
					if chat, ok := pendingEditUserJID(chatJID); ok && chat == author && ex.editChatAlias == "" {
						ex.editChatAlias = alt.String()
					}
				}
			}
			if ex.editChatAlias == "" && msg.Info.Chat.Server == types.HiddenUserServer && resolvedChat.Server == types.DefaultUserServer {
				ex.editChatAlias = msg.Info.Chat.ToNonAD().String()
			}
		}
		return messageStore.BatchContext(writeCtx, func(batch *messageBatch) error {
			var err error
			rowConsumed, err = persistMessageResult(batch, msg.Info.ID, chatJID, locationSender, msgTimestamp, msg.Info.IsFromMe, ex, true, logger)
			return err
		})
	}
	writeMessage := func() error { return retryPendingEditPreparation(writePreparedMessage) }
	var stored bool
	if ex.edit != nil {
		// An edit updates an existing target; it must not create an empty chat
		// when that target was never archived.
		stored = b.storeLive("message edit", msg.Info.ID, chatJID, writeMessage)
	} else {
		stored = storeRow("message", writeMessage)
	}
	// If the live-location ownership check cannot commit, withhold effects too:
	// an unverified known key must not escape as an unstored webhook message.
	if ex.edit != nil || rowConsumed || (!stored && ex.location != nil && ex.location.Live) {
		return
	}
	if stored {
		b.metrics.messagesStored.Add(1)
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
	forwardChat := b.forwardsToWebhook(resolvedChat, msg.Info.IsFromMe, msg.Info.Chat)
	shouldForward := forwardChat && !bareContentEnvelope(ex.inner, content)

	if !forwardChat {
		b.logWebhookWithheld(resolvedChat, msg.Info.ID)
	}

	// A message that was not stored has no row for downloadMedia to find: a
	// download could only fail a second time and bury the store error under
	// "failed to find message" (issue #454). The webhook below still goes out,
	// or the text would be lost downstream too, and says "stored": false so the
	// receiver does not look the message up (issue #518).
	downloadable := stored && mediaComplete(chatJID, ex.url, ex.directPath, ex.mediaKey, ex.fileSHA, ex.fileEnc)

	// Is this file written to the store as it arrives? One answer for both ways
	// of doing it below: WHATSAPP_MEDIA_AUTODOWNLOAD off means no file until
	// somebody asks (issue #484), status updates need
	// WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS on top (issue #447), and with
	// WHATSAPP_MEDIA_MAX_BYTES set a file above it is left for download_media,
	// as is one whose message declares no length: there is nothing to hold
	// against the cap (issue #474).
	wanted := mediaType != "" && downloadable && b.MediaAutoDownload
	skipStatusMedia := b.skipsStatusMedia(resolvedChat)
	noLength := b.MediaMaxBytes > 0 && !ex.hasLength
	tooLarge := b.MediaMaxBytes > 0 && fileLength > b.MediaMaxBytes
	cacheOnArrival := wanted && !skipStatusMedia && !noLength && !tooLarge

	// An image that will be forwarded is downloaded synchronously, so the
	// webhook can carry its bytes; everything else is cached in the background.
	// When the file is not cached on arrival the webhook carries the image
	// message without the payload, as it does when a download fails.
	var imageData []byte
	var imageMimeType string
	switch {
	case cacheOnArrival && mediaType == "image" && shouldForward:
		logger.Infof("Downloading image media for message %s (synchronous)", msg.Info.ID)
		success, _, dlName, dlPath, dlErr := b.DownloadMedia(withAutomaticCache(withMediaLimit(context.Background(), b.MediaMaxBytes)), msg.Info.ID, chatJID)
		if success && dlErr == nil {
			// One read through the store root gives the sniffed MIME type and
			// the bytes for the payload, which must hash to what the message
			// declared (webhook.go).
			imageMimeType, imageData = b.webhookMedia(chatJID, dlName, ex.fileSHA)
			logger.Infof("✅ Image downloaded: %s (%s, %d bytes for the webhook)", dlPath, imageMimeType, len(imageData))
		} else {
			if errors.Is(dlErr, errAutoMediaLimit) {
				b.recordAutoSizeSkip(msg.Info.ID, chatJID)
			} else {
				logger.Warnf("❌ Image download failed: %v", dlErr)
			}
			// Fall back to a background download so media is cached for future MCP tool calls
			if permanentMediaCode(dlErr) == "" && !errors.Is(dlErr, errAutoMediaLimit) {
				b.queueAutoDownload(msg.Info.ID, chatJID, mediaType)
			}
		}
	case cacheOnArrival:
		// Media that is not included in a webhook payload: cached in the
		// background by the bounded pool (media_budget.go), so a burst cannot
		// start one transfer per message and shutdown can stop them all.
		logger.Infof("Auto-downloading %s media for message %s", mediaType, msg.Info.ID)
		b.queueAutoDownload(msg.Info.ID, chatJID, mediaType)
	case wanted && skipStatusMedia:
		logger.Debugf("Not caching %s media of status update %s: %s is off (download_media still works)", mediaType, msg.Info.ID, mediaAutoDownloadStatusEnv)
	case wanted && noLength:
		logger.Infof("Skipping auto-download of %s media for message %s: no length declared to check against WHATSAPP_MEDIA_MAX_BYTES=%d (download_media still works)", mediaType, msg.Info.ID, b.MediaMaxBytes)
	case wanted && tooLarge:
		b.recordAutoSizeSkip(msg.Info.ID, chatJID)
		logger.Infof("Skipping auto-download of %s media for message %s: %d bytes exceeds WHATSAPP_MEDIA_MAX_BYTES=%d (download_media still works)", mediaType, msg.Info.ID, fileLength, b.MediaMaxBytes)
	}

	// Send the webhook (forwardsToWebhook decided whether, above). An image is
	// forwarded even without a caption, so the receiver knows it arrived: with
	// its bytes when they were downloaded for the payload, without them when
	// the file is not cached on arrival or the download failed.
	hasText := content != ""
	hasImage := mediaType == "image"

	if shouldForward && (hasText || hasImage) {
		if hasImage {
			b.Webhook.SendWebhookWithMedia(
				sender, content, chatJID, msg.Info.IsFromMe,
				quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs,
				msg.Info.ID, mediaType, imageMimeType, filename, imageData, stored,
			)
		} else {
			b.Webhook.SendWebhookWithMessageID(sender, content, chatJID, msg.Info.IsFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, msg.Info.ID, stored)
		}
	}

	if stored {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			b.Log.Debugf("[%s] %s %s: [%s: %s] %s", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			b.Log.Debugf("[%s] %s %s: %s", timestamp, direction, sender, content)
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
func (b *Bridge) handleCallOffer(meta types.BasicCallMeta, callType string, isGroup bool) {
	client, logger := b.currentClient(), b.Log
	chatJID := callChatJID(meta)

	fromJID := ""
	switch {
	case !meta.CallCreator.IsEmpty():
		fromJID = meta.CallCreator.ToNonAD().String()
	case !meta.From.IsEmpty():
		fromJID = meta.From.ToNonAD().String()
	}

	isFromMe := client.Store.ID != nil && fromJID == client.Store.ID.ToNonAD().String()

	if !b.storeLive("call offer", meta.CallID, chatJID, func() error {
		return b.Store.StoreCallOffer(meta.CallID, chatJID, fromJID, meta.Timestamp, isFromMe, callType, isGroup)
	}) {
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
	case *events.LabelEdit:
		b.recordLabel(v)
	case *events.LabelAssociationChat:
		b.recordChatLabel(v)
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

	case *events.Contact:
		name := v.Action.GetFullName()
		if strings.TrimSpace(name) == "" {
			name = v.Action.GetFirstName()
		}
		b.refreshContactChatName(v.JID, types.EmptyJID, name)

	case *events.PushName:
		b.refreshContactChatName(v.JID, v.JIDAlt, v.NewPushName)

	case *events.BusinessName:
		b.refreshContactChatName(v.JID, types.EmptyJID, v.NewBusinessName)

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
			chatJID := resolveLIDChat(b.currentClient(), v.Chat, v.SenderAlt, v.RecipientAlt, v.IsFromMe).String()
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
			if b.storeLive("group rename", "", v.JID.String(), func() error { return b.Store.RenameChat(v.JID.String(), v.Name.Name) }) {
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
		// Join/Leave/Promote/Demote keep group_members current (group_events.go).
		b.applyGroupParticipantChanges(v)

	case *events.CallOffer:
		// 1:1 incoming call. call_type defaults to "voice"; CallOffer
		// doesn't expose Media directly (it's buried in the binary Data
		// node). Group calls come through CallOfferNotice instead, which
		// DOES expose Media cleanly.
		b.handleCallOffer(v.BasicCallMeta, "voice", false)

	case *events.CallOfferNotice:
		// Group calls. v.Media is "audio" or "video"; normalize to our
		// "voice"/"video" convention.
		callType := "voice"
		if v.Media == "video" {
			callType = "video"
		}
		isGroup := v.Type == "group" || !v.GroupJID.IsEmpty()
		b.handleCallOffer(v.BasicCallMeta, callType, isGroup)

	case *events.CallAccept:
		if b.storeLive("answered call", v.CallID, callChatJID(v.BasicCallMeta), func() error { return b.Store.MarkCallAnswered(v.CallID, callChatJID(v.BasicCallMeta)) }) {
			b.Log.Infof("Call answered: id=%s", v.CallID)
		}

	case *events.CallReject:
		if b.storeLive("rejected call", v.CallID, callChatJID(v.BasicCallMeta), func() error { return b.Store.MarkCallRejected(v.CallID, callChatJID(v.BasicCallMeta)) }) {
			b.Log.Infof("Call rejected: id=%s", v.CallID)
		}

	case *events.CallTerminate:
		if b.storeLive("terminated call", v.CallID, callChatJID(v.BasicCallMeta), func() error {
			return b.Store.MarkCallTerminated(v.CallID, callChatJID(v.BasicCallMeta), v.Reason, v.Timestamp)
		}) {
			b.Log.Infof("Call terminated: id=%s reason=%q", v.CallID, v.Reason)
		}

	case *events.Connected:
		b.startLabelSync()
		b.clearConnectionProblem()
		b.recipientNumbers.clear()
		b.Log.Infof("✓ Successfully connected to WhatsApp servers")
		b.notifyConnection("connected", "authenticated", true, false)
	case *events.ManualLoginReconnect:
		// The library's 515 login handshake also needs to use our dial gate.
		b.scheduleReconnect(reconnectChan)
	case *events.KeepAliveTimeout:
		if b.connectionNow().Sub(v.LastSuccess) > whatsmeow.KeepAliveMaxFailTime {
			b.Log.Warnf("WhatsApp keepalive stalled; scheduling a gated reconnect")
			b.notifyConnection("disconnected", "keepalive_stalled", false, false)
			b.scheduleReconnect(reconnectChan)
		}

	case *events.LoggedOut:
		if b.operatorLogout.Load() {
			return
		}
		b.recipientNumbers.clear()
		code := int(v.Reason)
		if !v.OnConnect && code == 0 {
			code = 401
		}
		p := b.recordConnectionProblem(code, 0, 0)
		b.notifyConnection("logged_out", p.Kind, true, true)
		if code != 401 {
			return
		}
		// whatsmeow has already wiped the device row; the process cannot re-enter
		// the pairing flow from here. Exit and let the supervisor restart us: the
		// next start finds no session and prints a fresh QR code.
		b.Exit(fmt.Sprintf("device logged out by the phone (reason: %v); exiting so the next start pairs again", v.Reason), exitCodeLoggedOut)

	case *events.Disconnected:
		if b.operatorLogout.Load() {
			return
		}
		b.recipientNumbers.clear()
		b.notifyConnection("disconnected", "transport_lost", false, false)
		b.Log.Warnf("⚠️  Disconnected from WhatsApp servers, will attempt reconnection...")
		// Signal reconnection needed
		select {
		case reconnectChan <- true:
		default:
			// Channel already has a reconnect signal
		}

	case *events.ConnectFailure:
		p := b.recordConnectionProblem(int(v.Reason), 0, 0)
		b.notifyConnection("disconnected", p.Kind, true, false)
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
		// with the other b.currentClient(), then reconnect.
		//
		// The delay is read here, not inside the goroutine: the field is
		// configuration, and reading it on the event path keeps the timer
		// goroutine off a value another goroutine could still be writing.
		delay := b.StreamReplacedDelay
		b.notifyConnection("disconnected", "session_replaced", false, false)
		b.Log.Warnf("⚠️  Stream replaced by another session — will reconnect after %s", delay)
		go func() {
			select {
			case <-time.After(delay):
			case <-b.ctx.Done():
				return
			}
			select {
			case reconnectChan <- true:
			default:
			}
		}()

	case *events.ClientOutdated:
		b.recordConnectionProblem(405, 0, 0)
		b.notifyConnection("logged_out", "client_outdated", true, true)
		b.Exit("WhatsApp rejected this client version as outdated; rebuild with a newer whatsmeow (AGENTS.md §2 bump routine)", exitCodeClientOutdated)
	case *events.TemporaryBan:
		b.recordConnectionProblem(402, int(v.Code), v.Expire)
		b.notifyConnection("disconnected", "temporarily_banned", true, false)
		select {
		case reconnectChan <- true:
		default:
		}
	case *events.PairPasskeyError:
		b.setPairingState("passkey_failed")
		b.Log.Warnf("WhatsApp passkey check failed; see docs/DOCKER.md Pairing")
	}
}

func (b *Bridge) scheduleReconnect(reconnectChan chan<- bool) {
	// Disconnect may wait for whatsmeow's node-handler queue. Do it in the
	// reconnect consumer after the callback returns, rather than in that queue.
	b.forceReconnect.Store(true)
	select {
	case reconnectChan <- true:
	default:
	}
}

// defaultStreamReplacedDelay is how long to wait before reconnecting after
// another session took our slot (avoids ping-ponging with it). It is the
// default of Bridge.StreamReplacedDelay, which is what the event path reads:
// a package-level variable tests reassign is state two goroutines end up
// sharing without a lock (gotcha 11, issue #351).
const defaultStreamReplacedDelay = 30 * time.Second

// The defaults of Bridge.ReconnectInitialBackoff / ReconnectMaxBackoff: the
// first wait before redialling, doubled per failure up to the maximum and reset
// on success. Constants for the same reason as the delay above — a test that
// shortens the wait sets it on its own Bridge, not on shared state (gotcha 11).
const (
	defaultReconnectInitialBackoff = 5 * time.Second
	defaultReconnectMaxBackoff     = 5 * time.Minute
)

// reconnectLoop redials with exponential backoff whenever handleEvent
// reports a lost connection, until Shutdown cancels b.ctx. The backoff wait
// is interruptible so shutdown never waits out a five-minute sleep.
func (b *Bridge) reconnectLoop(reconnectChan chan bool) {
	// A zero bound is a Bridge built without newBridge, not a request to redial
	// as fast as WhatsApp can refuse: the doubling would clamp back to the zero
	// maximum and the loop would spin. Both are floored to the production value.
	reconnectBackoff := b.ReconnectInitialBackoff
	if reconnectBackoff <= 0 {
		reconnectBackoff = defaultReconnectInitialBackoff
	}
	maxBackoff := b.ReconnectMaxBackoff
	if maxBackoff < reconnectBackoff {
		maxBackoff = defaultReconnectMaxBackoff
	}

	for {
		select {
		case <-reconnectChan:

			// Wait before reconnecting, unless we are shutting down
			select {
			case <-time.After(reconnectBackoff):
			case <-b.ctx.Done():
				return
			}

			// Try to reconnect
			if err := b.waitConnectionAllowed(); err != nil {
				return
			}
			b.clientGate.RLock()
			if b.forceReconnect.Swap(false) {
				if b.Disconnect != nil {
					b.Disconnect()
				} else if b.currentClient() != nil {
					b.currentClient().Disconnect()
				}
			}
			if !b.Connected() {
				b.Log.Infof("🔄 Attempting to reconnect...")
				b.metrics.reconnects.Add(1)
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
					reconnectBackoff = b.ReconnectInitialBackoff
				}
			} else {
				b.Log.Infof("Already connected, skipping reconnection")
				reconnectBackoff = b.ReconnectInitialBackoff
			}
			b.clientGate.RUnlock()

		case <-b.ctx.Done():
			return
		}
	}
}
