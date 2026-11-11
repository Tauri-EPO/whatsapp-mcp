package main

// One extraction + persistence path for live messages (events.go) and
// history sync (history_sync.go). Both used to duplicate the view-once
// unwrap, poll rendering, media extraction and the StoreMessage /
// MarkViewOnce / StorePoll sequence, and both mutated the whatsmeow-owned
// message in place; extractMessage works on a local copy instead.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// messageWriter is satisfied by *MessageStore (single rows) and *messageBatch
// (one transaction for a live row or a bounded history chunk). Both take the sender as
// the full resolved JID; see StoreMessage (store.go).
type messageWriter interface {
	StoreMessage(message storedMessage) error
	MarkViewOnce(messageID, chatJID string) error
	StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error
	UpdateLiveLocation(id, chat, sender string, fromMe bool, p *messageLocation) (bool, error)
	ApplyMessageEdit(chat, sender string, fromMe bool, edit *waE2E.ProtocolMessage, fallback time.Time) error
}

// extractedMessage is the storable view of a waE2E.Message.
type extractedMessage struct {
	editAuthorAlias string // verified author alternative prepared before the writer
	editChatAlias   string // verified chat alternative prepared before the writer
	editPreparation *pendingEditPreparation
	// inner is the message with SDK envelopes removed; downstream
	// extractors (quotes, ephemeral settings, webhook media) should use it.
	inner    *waE2E.Message
	viewOnce bool

	content   string
	mediaType string
	filename  string
	url       string
	mediaKey  []byte
	fileSHA   []byte
	fileEnc   []byte
	fileLen   uint64
	hasLength bool // protocol field presence; explicit zero is a length
	poll      *pollCreation

	// directPath is the media's own direct path, "" when the message has none.
	directPath string

	quotedID, quotedSender, quotedContent string
	forwardingScore                       *uint32
	mentions                              []string
	location                              *messageLocation
	edit                                  *waE2E.ProtocolMessage
	retryChat, retrySender                string
}

// extractMessage pulls text, media, poll, quote and mention data out of m.
// ts and id feed the generated media filename, so pass the same values that
// will be stored (downloadMedia rebuilds the name from the stored row).
func extractMessage(m *waE2E.Message, ts time.Time, id string) extractedMessage {
	e := extractedMessage{inner: m}
	if m != nil {
		// The SDK only mutates the inner payload when inheriting outer
		// MessageContextInfo. Clone that case with the protobuf API; ordinary
		// unwraps keep their original inner pointer and avoid copying bytes.
		rawView := m
		if m.MessageContextInfo != nil {
			rawView = proto.Clone(m).(*waE2E.Message)
		}
		event := (&events.Message{RawMessage: rawView}).UnwrapRaw()
		e.inner, e.viewOnce = event.Message, event.IsViewOnce
	}
	if e.inner == nil {
		return e
	}
	if p := e.inner.GetProtocolMessage(); p != nil && p.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
		e.edit = p
		return e
	}
	e.content = extractTextContent(e.inner)
	e.mediaType, e.filename, e.url, e.mediaKey, e.fileSHA, e.fileEnc, e.fileLen = extractMediaInfo(e.inner, ts, id)
	// Keep an absent document name absent: a replay must not replace an
	// original name with extractMediaInfo's generated cache fallback.
	if _, part := mediaPartOf(e.inner); part != nil {
		if doc, ok := part.(*waE2E.DocumentMessage); ok {
			e.filename = cleanDisplayName(doc.GetFileName())
		}
	}
	e.directPath = extractMediaDirectPath(e.inner)
	e.hasLength = mediaLengthDeclared(e.inner)
	e.location = locationOf(e.inner)
	if e.location != nil {
		e.mediaType = "location"
	}
	if e.poll = extractPollCreation(e.inner); e.poll != nil {
		e.content = pollContent(e.poll)
		e.mediaType = "poll"
	}
	if e.viewOnce {
		e.content = viewOnceContent(e.content, e.mediaType)
	}
	e.quotedID, e.quotedSender, e.quotedContent = extractQuotedMessageInfo(e.inner)
	e.mentions = extractMentionedJIDs(e.inner)
	e.forwardingScore = extractForwardingScore(e.inner)
	return e
}

// Live deliveries and own-phone history share edit extraction, outside archive
// writers. Decryption never changes the SDK event or the target's author/chat.
func (b *Bridge) extractIncomingMessage(ctx context.Context, msg *events.Message, ts time.Time) extractedMessage {
	original := msg.Message
	// The SDK unwraps ordinary edits to their replacement; RawMessage retains
	// the protocol key and ordering timestamp. Direct edits need no special flag.
	if msg.RawMessage != nil && extractMessage(msg.RawMessage, ts, msg.Info.ID).edit != nil {
		original = msg.RawMessage
	}
	ex := extractMessage(original, ts, msg.Info.ID)
	encrypted := ex.inner.GetSecretEncryptedMessage()
	if encrypted == nil {
		return ex
	}
	// Keep the outer payload available for receipt-side metadata/history shares,
	// but never archive an unsupported or undecryptable encrypted delivery row.
	// Retain the SDK flag so the live view-once fallback cannot add a placeholder.
	ignored := extractedMessage{inner: ex.inner, viewOnce: ex.viewOnce || msg.IsViewOnce}
	if encrypted.GetSecretEncType() != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
		b.Log.Debugf("Unhandled secret-encrypted message type: %s", encrypted.GetSecretEncType())
		return ignored
	}
	if encrypted.GetTargetMessageKey().GetID() == "" || b.SecretEditDecrypt == nil {
		return ignored
	}
	decryptEvent := msg
	if msg.Message != ex.inner {
		view := *msg
		view.Message = ex.inner
		decryptEvent = &view
	}
	decrypted, err := b.SecretEditDecrypt(ctx, decryptEvent)
	if err != nil {
		// Missing originals/secrets are routine during sync. Other SDK failures
		// warrant a warning, but error text may contain remote data: omit it.
		log := b.Log.Warnf
		if errors.Is(err, whatsmeow.ErrOriginalMessageSecretNotFound) {
			log = b.Log.Debugf
		}
		log("Failed to decrypt message edit %s in %s", msg.Info.ID, msg.Info.Chat)
		return ignored
	}
	replacement, stamp := decrypted, int64(0)
	if edit := extractMessage(decrypted, ts, msg.Info.ID).edit; edit != nil {
		replacement = edit.GetEditedMessage()
		if edit.GetTimestampMS() > 0 {
			stamp = edit.GetTimestampMS()
		}
	}
	if replacement == nil {
		return ignored
	}
	// Zero uses the caller's retry-corrected delivery time. The encrypted key
	// and outer sender/chat remain authoritative, including for deferred edits.
	ex.edit = &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: encrypted.GetTargetMessageKey(),
		EditedMessage: replacement, TimestampMS: proto.Int64(stamp),
	}
	return ex
}

// empty reports a message with nothing to store (no text, no media).
func (e extractedMessage) empty() bool { return e.edit == nil && e.content == "" && e.mediaType == "" }

// persistMessage writes the row plus its poll and view-once side tables.
// Every failure reaches the retry owner. Live and history callers use a batch
// so a failed auxiliary write cannot leave a partially committed message.
func persistMessage(w messageWriter, id, chatJID, sender string, ts time.Time, fromMe bool, e extractedMessage, quoted bool, logger waLog.Logger) error {
	_, err := persistMessageResult(w, id, chatJID, sender, ts, fromMe, e, quoted, logger)
	return err
}

// consumed means a position update or refused collision: live callers must
// suppress activity, webhooks and automatic media work for this event.
func persistMessageResult(w messageWriter, id, chatJID, sender string, ts time.Time, fromMe bool, e extractedMessage, quoted bool, logger waLog.Logger) (consumed bool, err error) {
	if store, ok := w.(*MessageStore); ok {
		err = store.Batch(func(batch *messageBatch) error {
			consumed, err = persistMessageResult(batch, id, chatJID, sender, ts, fromMe, e, quoted, logger)
			return err
		})
		return consumed, err
	}
	if e.edit != nil {
		return true, w.ApplyMessageEdit(chatJID, sender, fromMe, e.edit, ts)
	}
	if matched, err := w.UpdateLiveLocation(id, chatJID, sender, fromMe, e.location); err != nil || matched {
		if matched && err == nil {
			logger.Debugf("Consumed location update or author collision for %s in %s", id, chatJID)
		}
		return matched, err
	}
	if err := w.StoreMessage(storedMessageSnapshot(id, chatJID, sender, ts, fromMe, e, quoted)); err != nil {
		return false, err
	}
	if e.poll != nil {
		if err := w.StorePoll(id, chatJID, e.poll, ts); err != nil {
			return false, fmt.Errorf("poll metadata: %w", err)
		}
	}
	if e.viewOnce {
		if err := w.MarkViewOnce(id, chatJID); err != nil {
			return false, fmt.Errorf("view-once: %w", err)
		}
	}
	return false, nil
}

// One snapshot spelling for canonical writes and existing-alias media repair.
func storedMessageSnapshot(id, chatJID, sender string, ts time.Time, fromMe bool, e extractedMessage, quoted bool) storedMessage {
	quotedID := ""
	if quoted {
		quotedID = e.quotedID
	}
	var length any
	if e.hasLength {
		length = storedMediaLength(e.fileLen)
	}
	return storedMessage{
		EditAuthorAlias: e.editAuthorAlias,
		EditChatAlias:   e.editChatAlias,
		EditPreparation: e.editPreparation,
		ID:              id,
		ChatJID:         chatJID,
		Sender:          sender,
		Content:         e.content,
		Timestamp:       ts,
		IsFromMe:        fromMe,
		MediaType:       e.mediaType,
		Filename:        e.filename,
		URL:             e.url,
		MediaKey:        e.mediaKey,
		FileSHA256:      e.fileSHA,
		FileEncSHA256:   e.fileEnc,
		FileLength:      length,
		QuotedMessageID: quotedID,
		ForwardingScore: e.forwardingScore,
		Mentions:        mentionsColumn(e.mentions),
		Media:           messageMediaOptions{directPath: e.directPath, presentation: mediaPresentationOf(e.inner), location: e.location, retryChat: e.retryChat, retrySender: e.retrySender},
	}
}
