package main

import (
	"context"
	"database/sql"
	"errors"

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
func (store *MessageStore) outboundQuotePreview(ctx context.Context, chat, id string) (*waE2E.Message, error) {
	if store == nil || id == "" {
		return nil, ctx.Err()
	}
	var content, kind, filename string
	var raw sql.NullString
	var sha []byte
	err := store.db.QueryRowContext(ctx, `SELECT COALESCE(content, ''), COALESCE(media_type, ''), COALESCE(filename, ''), media_presentation, file_sha256 FROM messages WHERE id = ? AND chat_jid = ?`, id, chat).Scan(&content, &kind, &filename, &raw, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
	return m, nil
}
