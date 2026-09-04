package main

// View-once messages.
//
// A view-once photo, video or voice note arrives wrapped in one of three
// envelopes (ViewOnceMessage, ViewOnceMessageV2, ViewOnceMessageV2Extension),
// each a FutureProofMessage around the real ImageMessage/VideoMessage/
// AudioMessage. Before this file the bridge only looked at the top-level
// fields, so these messages were dropped entirely.
//
// Policy: keep a copy in the archive and leave the phone able to open the
// message once. Downloading through a linked device does not consume the
// view-once state — WhatsApp marks it viewed only when a device sends the
// "played"/view receipt, which the bridge never does.

import (
	"strings"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// unwrapViewOnce returns the inner message when msg is a view-once envelope,
// or msg itself otherwise. The second value reports whether it was wrapped.
func unwrapViewOnce(msg *waProto.Message) (*waProto.Message, bool) {
	if msg == nil {
		return nil, false
	}
	for _, env := range []*waProto.FutureProofMessage{
		msg.GetViewOnceMessage(), msg.GetViewOnceMessageV2(), msg.GetViewOnceMessageV2Extension(),
	} {
		if env != nil && env.GetMessage() != nil {
			return env.GetMessage(), true
		}
	}
	return msg, false
}

// viewOnceContent renders the text stored for a view-once message: the
// caption when there is one, otherwise a marker naming the media kind, so the
// row is never empty and agents can tell it apart.
func viewOnceContent(caption, mediaType string) string {
	caption = strings.TrimSpace(caption)
	if caption != "" {
		return "🔒 " + caption
	}
	if mediaType == "" {
		mediaType = "media"
	}
	return "🔒 view-once " + mediaType
}

// MarkViewOnce flags a stored message as view-once.
func (store *MessageStore) MarkViewOnce(messageID, chatJID string) error {
	_, err := store.db.Exec(`UPDATE messages SET view_once = 1 WHERE id = ? AND chat_jid = ?`, messageID, chatJID)
	return err
}
