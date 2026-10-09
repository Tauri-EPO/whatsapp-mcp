package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Resolve the archive's phone namespace using the send's context, before any
// upload. A missing mapping preserves the LID, matching inbound storage.
func outboundLookupChatJID(ctx context.Context, client *whatsmeow.Client, jid types.JID) (types.JID, error) {
	jid = jid.ToNonAD()
	if jid.Server == types.HiddenUserServer {
		if pn, err := lookupAltJID(ctx, client, jid); err == nil && !pn.IsEmpty() {
			jid = pn.ToNonAD()
		}
	}
	return jid, ctx.Err()
}

func (store *MessageStore) outboundChatSettings(ctx context.Context, chat string) (ChatEphemeralSettings, error) {
	var settings ChatEphemeralSettings
	if store == nil {
		return settings, ctx.Err()
	}
	err := store.db.QueryRowContext(ctx, "SELECT ephemeral_expiration, ephemeral_setting_timestamp FROM chats WHERE jid = ?", chat).Scan(&settings.Expiration, &settings.SettingTimestamp)
	return settings, err
}

// A quote can reference only this chat. Prefer the archived caption and typed
// presentation; a missing row retains the caller's legacy text preview. Do not
// copy download credentials or invent a thumbnail the archive never retained.
func (store *MessageStore) loadOutboundQuote(ctx context.Context, client *whatsmeow.Client, chat string, quote outboundQuote, deriveParticipant bool) (outboundQuote, error) {
	if store == nil || quote.id == "" {
		return quote, ctx.Err()
	}
	var content, kind, filename string
	var sender, server string
	var fromMe bool
	var raw sql.NullString
	var sha []byte
	err := store.db.QueryRowContext(ctx, `SELECT COALESCE(content, ''), COALESCE(media_type, ''), COALESCE(filename, ''), media_presentation, file_sha256, COALESCE(sender, ''), COALESCE(sender_server, ''), is_from_me FROM messages WHERE id = ? AND chat_jid = ?`, quote.id, chat).Scan(&content, &kind, &filename, &raw, &sha, &sender, &server, &fromMe)
	if errors.Is(err, sql.ErrNoRows) {
		return quote, nil
	}
	if err != nil {
		return quote, err
	}
	if deriveParticipant {
		quote.participant, err = storedQuoteParticipant(ctx, client, sender, server, fromMe)
		if err != nil {
			return quote, err
		}
	}
	p := readMediaPresentation(raw.String, kind, sha)
	m := &waE2E.Message{}
	switch kind {
	case "image":
		m.ImageMessage = &waE2E.ImageMessage{Caption: proto.String(content)}
		if p != nil && p.MIME != "" {
			m.ImageMessage.Mimetype = proto.String(p.MIME)
		}
	case "video":
		m.VideoMessage = &waE2E.VideoMessage{Caption: proto.String(content)}
		if p != nil && p.MIME != "" {
			m.VideoMessage.Mimetype = proto.String(p.MIME)
		}
	case "audio":
		m.AudioMessage = &waE2E.AudioMessage{}
		if p != nil {
			m.AudioMessage.Mimetype = proto.String(p.MIME)
			m.AudioMessage.PTT, m.AudioMessage.Seconds, m.AudioMessage.Waveform = p.PTT, p.Seconds, p.Waveform
		}
	case "sticker":
		m.StickerMessage = &waE2E.StickerMessage{}
		if p != nil {
			m.StickerMessage.Mimetype, m.StickerMessage.IsAnimated = proto.String(p.MIME), p.Animated
		}
	case "document":
		name := ""
		if !generatedMediaName(filename) {
			name = outboundFileName(filename)
		}
		m.DocumentMessage = &waE2E.DocumentMessage{Caption: proto.String(content), FileName: proto.String(name)}
		if p != nil {
			m.DocumentMessage.Mimetype, m.DocumentMessage.Title = proto.String(p.MIME), p.Title
			if p.Name != nil {
				m.DocumentMessage.FileName = proto.String(outboundFileName(*p.Name))
			}
		}
	default:
		m.Conversation = proto.String(content)
	}
	quote.preview = m
	return quote, nil
}

// Stored bare senders need their recorded namespace; guessing a phone JID for
// an unresolved LID misattributes the reply. Legacy full JIDs keep their server.
// Own rows use this account's JID. Both use the established wire LID resolver.
func storedQuoteParticipant(ctx context.Context, client *whatsmeow.Client, sender, server string, fromMe bool) (string, error) {
	if fromMe {
		if client == nil || client.Store == nil || client.Store.ID == nil {
			return "", ctx.Err()
		}
		return resolveQuotedParticipantJIDContext(ctx, client, storedSender(client.Store.ID.ToNonAD())), ctx.Err()
	}
	if sender == "" {
		return "", ctx.Err()
	}
	if !strings.Contains(sender, "@") {
		if server == "" {
			return "", ctx.Err()
		}
		sender += "@" + server
	}
	jid, err := normalizedUserJID(sender)
	if err != nil {
		return "", ctx.Err()
	}
	return resolveQuotedParticipantJIDContext(ctx, client, jid.String()), ctx.Err()
}
