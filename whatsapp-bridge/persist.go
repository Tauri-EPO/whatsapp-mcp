package main

// One extraction + persistence path for live messages (events.go) and
// history sync (history_sync.go). Both used to duplicate the view-once
// unwrap, poll rendering, media extraction and the StoreMessage /
// MarkViewOnce / StorePoll sequence, and both mutated the whatsmeow-owned
// message in place; extractMessage works on a local copy instead.

import (
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// messageWriter is satisfied by *MessageStore (single rows) and *messageBatch
// (one transaction per history-sync conversation).
type messageWriter interface {
	StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
		mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
		quotedMessageId string) error
	MarkViewOnce(messageID, chatJID string) error
	StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error
}

// extractedMessage is the storable view of a waE2E.Message.
type extractedMessage struct {
	// inner is the message with any view-once envelope removed; downstream
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
	poll      *pollCreation

	quotedID, quotedSender, quotedContent string
	mentions                              []string
}

// extractMessage pulls text, media, poll, quote and mention data out of m.
// ts and id feed the generated media filename, so pass the same values that
// will be stored (downloadMedia rebuilds the name from the stored row).
func extractMessage(m *waE2E.Message, ts time.Time, id string) extractedMessage {
	e := extractedMessage{inner: m}
	if inner, wrapped := unwrapViewOnce(m); wrapped {
		e.inner, e.viewOnce = inner, true
	}
	if e.inner == nil {
		return e
	}
	e.content = extractTextContent(e.inner)
	e.mediaType, e.filename, e.url, e.mediaKey, e.fileSHA, e.fileEnc, e.fileLen = extractMediaInfo(e.inner, ts, id)
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
// Side-table failures are logged, not returned: the message row is what
// readers depend on.
func persistMessage(w messageWriter, id, chatJID, sender string, ts time.Time, fromMe bool, e extractedMessage, quoted bool, logger waLog.Logger) error {
	quotedID := ""
	if quoted {
		quotedID = e.quotedID
	}
	if err := w.StoreMessage(id, chatJID, sender, e.content, ts, fromMe,
		e.mediaType, e.filename, e.url, e.mediaKey, e.fileSHA, e.fileEnc, e.fileLen, quotedID); err != nil {
		return err
	}
	if e.poll != nil {
		if err := w.StorePoll(id, chatJID, e.poll, ts); err != nil {
			logger.Warnf("Failed to store poll %s: %v", id, err)
		}
	}
	if e.viewOnce {
		if err := w.MarkViewOnce(id, chatJID); err != nil {
			logger.Warnf("Failed to flag view-once message %s: %v", id, err)
		}
	}
	return nil
}
