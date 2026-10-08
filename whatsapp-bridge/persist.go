package main

// One extraction + persistence path for live messages (events.go) and
// history sync (history_sync.go). Both used to duplicate the view-once
// unwrap, poll rendering, media extraction and the StoreMessage /
// MarkViewOnce / StorePoll sequence, and both mutated the whatsmeow-owned
// message in place; extractMessage works on a local copy instead.

import (
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// messageWriter is satisfied by *MessageStore (single rows) and *messageBatch
// (one transaction for a live row or a bounded history chunk). Both take the sender as
// the full resolved JID; see StoreMessage (store.go).
type messageWriter interface {
	StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
		mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength any,
		quotedMessageId string, options ...messageMediaOptions) error
	MarkViewOnce(messageID, chatJID string) error
	SetMentions(messageID, chatJID, mentions string) error
	StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error
}

// extractedMessage is the storable view of a waE2E.Message.
type extractedMessage struct {
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
	mentions                              []string
}

// extractMessage pulls text, media, poll, quote and mention data out of m.
// ts and id feed the generated media filename, so pass the same values that
// will be stored (downloadMedia rebuilds the name from the stored row).
func extractMessage(m *waE2E.Message, ts time.Time, id string) extractedMessage {
	e := extractedMessage{inner: m}
	if m != nil {
		// Reuse the pinned SDK's unwrap order without its final mutation of
		// the inner MessageContextInfo. Inherit it on a local view instead.
		rawView := *m
		rawView.MessageContextInfo = nil
		event := (&events.Message{RawMessage: &rawView}).UnwrapRaw()
		e.inner, e.viewOnce = event.Message, event.IsViewOnce
		if e.inner != nil && e.inner.MessageContextInfo == nil && m.MessageContextInfo != nil {
			innerView := *e.inner
			innerView.MessageContextInfo = m.MessageContextInfo
			e.inner = &innerView
		}
	}
	if e.inner == nil {
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
	if e.poll = extractPollCreation(e.inner); e.poll != nil {
		e.content = pollContent(e.poll)
		e.mediaType = "poll"
	}
	if e.viewOnce {
		e.content = viewOnceContent(e.content, e.mediaType)
	}
	e.quotedID, e.quotedSender, e.quotedContent = extractQuotedMessageInfo(e.inner)
	e.mentions = extractMentionedJIDs(e.inner)
	return e
}

// empty reports a message with nothing to store (no text, no media).
func (e extractedMessage) empty() bool { return e.content == "" && e.mediaType == "" }

// persistMessage writes the row plus its poll and view-once side tables.
// Every failure reaches the retry owner. Live and history callers use a batch
// so a failed auxiliary write cannot leave a partially committed message.
func persistMessage(w messageWriter, id, chatJID, sender string, ts time.Time, fromMe bool, e extractedMessage, quoted bool, _ waLog.Logger) error {
	quotedID := ""
	if quoted {
		quotedID = e.quotedID
	}
	var length any
	if e.hasLength {
		length = storedMediaLength(e.fileLen)
	}
	if err := w.StoreMessage(id, chatJID, sender, e.content, ts, fromMe,
		e.mediaType, e.filename, e.url, e.mediaKey, e.fileSHA, e.fileEnc, length, quotedID, messageMediaOptions{directPath: e.directPath, presentation: mediaPresentationOf(e.inner)}); err != nil {
		return err
	}
	// Mentions ride in a side update rather than the insert: only a minority of
	// messages carry any, and the write then costs nothing on the rest
	// (mentions.go).
	if mentions := mentionsColumn(e.mentions); mentions != "" {
		if err := w.SetMentions(id, chatJID, mentions); err != nil {
			return fmt.Errorf("mentions: %w", err)
		}
	}
	if e.poll != nil {
		if err := w.StorePoll(id, chatJID, e.poll, ts); err != nil {
			return fmt.Errorf("poll metadata: %w", err)
		}
	}
	if e.viewOnce {
		if err := w.MarkViewOnce(id, chatJID); err != nil {
			return fmt.Errorf("view-once: %w", err)
		}
	}
	return nil
}
