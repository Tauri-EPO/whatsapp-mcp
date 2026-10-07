package main

// Message content extraction: text, quotes, mentions, media metadata and
// ephemeral settings pulled out of a waE2E.Message. Shared by the live event
// path (events.go) and history sync (history_sync.go).

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Extract text content from a message
func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// Captions on media messages — surface them as searchable content
	// alongside the media itself. Audio messages don't carry captions.
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	// Shared contact cards (vCards) carry no URL/MediaKey — the vCard text is
	// embedded directly in the message rather than fetched from the CDN — so
	// without this branch extractMediaInfo also returns "" for them and the
	// message is silently dropped at the "no content and no media" gate in
	// handleMessage: shared contacts were vanishing entirely.
	if contact := msg.GetContactMessage(); contact != nil {
		if body := formatContactContent(contact.GetDisplayName(), contact.GetVcard()); body != "" {
			return "📇 " + body
		}
	}
	if contacts := msg.GetContactsArrayMessage(); contacts != nil {
		if list := contacts.GetContacts(); len(list) > 0 {
			names := make([]string, 0, len(list))
			for _, c := range list {
				if body := formatContactContent(c.GetDisplayName(), c.GetVcard()); body != "" {
					names = append(names, body)
				}
			}
			if len(names) > 0 {
				return fmt.Sprintf("📇 %d contacts shared: %s", len(names), strings.Join(names, "; "))
			}
		}
	}

	// Shared locations carry only coordinates, a label and a JPEG thumbnail
	// (no CDN payload), so like contact cards they were dropped at the "no
	// content and no media" gate. Text only: the thumbnail is not stored.
	if loc := msg.GetLocationMessage(); loc != nil {
		if body := formatLocationContent(loc); body != "" {
			return body
		}
	}
	if live := msg.GetLiveLocationMessage(); live != nil {
		if body := formatLiveLocationContent(live); body != "" {
			return body
		}
	}

	// WhatsApp Business templates arrive hydrated — body lives in
	// HydratedTemplate.HydratedContentText. Without this branch every
	// template-sent message (e.g. WABA Connect Hrms_* notifications)
	// returns "" and the row is silently skipped at the storage gate.
	if tpl := msg.GetTemplateMessage(); tpl != nil {
		if h := tpl.GetHydratedTemplate(); h != nil {
			if t := h.GetHydratedContentText(); t != "" {
				return t
			}
		}
	}
	if btn := msg.GetButtonsMessage(); btn != nil {
		if t := btn.GetContentText(); t != "" {
			return t
		}
		if t := btn.GetText(); t != "" {
			return t
		}
	}
	if ia := msg.GetInteractiveMessage(); ia != nil {
		if body := ia.GetBody(); body != nil {
			if t := body.GetText(); t != "" {
				return t
			}
		}
	}
	if lst := msg.GetListMessage(); lst != nil {
		if t := lst.GetDescription(); t != "" {
			return t
		}
	}
	if br := msg.GetButtonsResponseMessage(); br != nil {
		if t := br.GetSelectedDisplayText(); t != "" {
			return t
		}
	}
	if tbr := msg.GetTemplateButtonReplyMessage(); tbr != nil {
		if t := tbr.GetSelectedDisplayText(); t != "" {
			return t
		}
	}

	return ""
}

// formatContactContent renders a shared contact card as searchable text:
// the display name plus every phone number in the vCard body. All numbers are
// kept (not just the first) because the vCard is the only copy we ever get —
// there is no CDN payload to re-download later. Returns "" when there is
// nothing usable (no display name and no TEL line).
func formatContactContent(displayName, vcard string) string {
	phones := extractVCardPhones(vcard)
	if displayName == "" && len(phones) == 0 {
		return ""
	}
	if len(phones) > 0 {
		return fmt.Sprintf("%s (%s)", displayName, strings.Join(phones, ", "))
	}
	return displayName
}

// formatCoordinates renders a latitude/longitude pair as "(-23.550520,
// -46.633308)": strconv with a fixed six decimals (~0.1 m), so the text does
// not depend on the machine's locale and the same point always reads the same.
// Returns "" when the message carried neither coordinate.
func formatCoordinates(lat, lng *float64) string {
	if lat == nil && lng == nil {
		return ""
	}
	var la, lo float64
	if lat != nil {
		la = *lat
	}
	if lng != nil {
		lo = *lng
	}
	return fmt.Sprintf("(%s, %s)",
		strconv.FormatFloat(la, 'f', 6, 64), strconv.FormatFloat(lo, 'f', 6, 64))
}

// formatLocationContent renders a shared location as searchable text:
// "📍 <name> — <address> (<lat>, <lng>)", then the place URL and the comment
// when present, all joined with " — ". With neither name nor address the
// coordinates stand alone. Returns "" when the message carries nothing at all.
func formatLocationContent(loc *waE2E.LocationMessage) string {
	var parts []string
	var label []string
	for _, s := range []string{loc.GetName(), loc.GetAddress()} {
		if s = strings.TrimSpace(s); s != "" {
			label = append(label, s)
		}
	}
	head := strings.Join(label, " — ")
	if coords := formatCoordinates(loc.DegreesLatitude, loc.DegreesLongitude); coords != "" {
		head = strings.TrimSpace(head + " " + coords)
	}
	if head != "" {
		parts = append(parts, head)
	}
	for _, s := range []string{loc.GetURL(), loc.GetComment()} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "📍 " + strings.Join(parts, " — ")
}

// formatLiveLocationContent renders the first position of a live share:
// "📍 Live location (<lat>, <lng>)" plus the caption. Later position updates
// are not tracked, so the row records where the share started.
func formatLiveLocationContent(live *waE2E.LiveLocationMessage) string {
	coords := formatCoordinates(live.DegreesLatitude, live.DegreesLongitude)
	caption := strings.TrimSpace(live.GetCaption())
	if coords == "" && caption == "" {
		return ""
	}
	out := "📍 Live location"
	if coords != "" {
		out += " " + coords
	}
	if caption != "" {
		out += " — " + caption
	}
	return out
}

// extractVCardPhones returns the values of all TEL lines in a vCard blob,
// e.g. "TEL;type=CELL;waid=6281234567890:+62 812-3456-7890" -> "+62 812-3456-7890".
// iPhone-exported vCards wrap properties in groups ("item1.TEL;...:+62 ..."),
// so the property name is compared after stripping any group prefix.
func extractVCardPhones(vcard string) []string {
	var phones []string
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		prop := line
		if i := strings.IndexAny(prop, ";:"); i != -1 {
			prop = prop[:i]
		}
		if dot := strings.LastIndex(prop, "."); dot != -1 {
			prop = prop[dot+1:]
		}
		if !strings.EqualFold(prop, "TEL") {
			continue
		}
		if idx := strings.LastIndex(line, ":"); idx != -1 {
			if phone := strings.TrimSpace(line[idx+1:]); phone != "" {
				phones = append(phones, phone)
			}
		}
	}
	return phones
}

// extractChatEphemeralFromMessage reads the chat's ephemeral state off an
// inbound message's ContextInfo. Every regular message in an ephemeral chat
// stamps Expiration / EphemeralSettingTimestamp on the sub-message's
// ContextInfo, which lets the bridge backfill chats whose disappearing state
// was set before the bridge ever saw an EPHEMERAL_SETTING toggle or a
// fresh history sync. Returns the zero ChatEphemeralSettings when no
// ContextInfo is present (e.g. plain Conversation, ProtocolMessage).
func extractChatEphemeralFromMessage(msg *waE2E.Message) ChatEphemeralSettings {
	if msg == nil {
		return ChatEphemeralSettings{}
	}
	var ctx *waE2E.ContextInfo
	switch {
	case msg.ExtendedTextMessage != nil:
		ctx = msg.ExtendedTextMessage.GetContextInfo()
	case msg.ImageMessage != nil:
		ctx = msg.ImageMessage.GetContextInfo()
	case msg.AudioMessage != nil:
		ctx = msg.AudioMessage.GetContextInfo()
	case msg.VideoMessage != nil:
		ctx = msg.VideoMessage.GetContextInfo()
	case msg.DocumentMessage != nil:
		ctx = msg.DocumentMessage.GetContextInfo()
	case msg.StickerMessage != nil:
		ctx = msg.StickerMessage.GetContextInfo()
	}
	if ctx == nil {
		return ChatEphemeralSettings{}
	}
	return ChatEphemeralSettings{
		Expiration:       ctx.GetExpiration(),
		SettingTimestamp: ctx.GetEphemeralSettingTimestamp(),
	}
}

// Extract quoted message info from ContextInfo
func extractQuotedMessageInfo(msg *waE2E.Message) (quotedMessageId string, quotedSender string, quotedContent string) {
	if msg == nil {
		return "", "", ""
	}

	var contextInfo *waE2E.ContextInfo

	// Check all message types that can have ContextInfo
	if extText := msg.GetExtendedTextMessage(); extText != nil {
		contextInfo = extText.GetContextInfo()
	} else if img := msg.GetImageMessage(); img != nil {
		contextInfo = img.GetContextInfo()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		contextInfo = vid.GetContextInfo()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		contextInfo = doc.GetContextInfo()
	} else if aud := msg.GetAudioMessage(); aud != nil {
		contextInfo = aud.GetContextInfo()
	}

	if contextInfo == nil {
		return "", "", ""
	}

	// Extract quoted message ID (StanzaID)
	if contextInfo.StanzaID != nil {
		quotedMessageId = *contextInfo.StanzaID
	}

	// Extract quoted sender (Participant)
	if contextInfo.Participant != nil {
		quotedSender = *contextInfo.Participant
	}

	// Extract quoted message content
	if quotedMsg := contextInfo.QuotedMessage; quotedMsg != nil {
		quotedContent = extractTextContent(quotedMsg)
	}

	return quotedMessageId, quotedSender, quotedContent
}

// extractMentionedJIDs returns native WhatsApp mention targets from ContextInfo.
func extractMentionedJIDs(msg *waE2E.Message) []string {
	if msg == nil {
		return nil
	}

	var contextInfo *waE2E.ContextInfo
	if extText := msg.GetExtendedTextMessage(); extText != nil {
		contextInfo = extText.GetContextInfo()
	} else if img := msg.GetImageMessage(); img != nil {
		contextInfo = img.GetContextInfo()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		contextInfo = vid.GetContextInfo()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		contextInfo = doc.GetContextInfo()
	} else if aud := msg.GetAudioMessage(); aud != nil {
		contextInfo = aud.GetContextInfo()
	}

	if contextInfo == nil || len(contextInfo.MentionedJID) == 0 {
		return nil
	}

	return append([]string(nil), contextInfo.MentionedJID...)
}

// Extract media info from a message. Filenames embed the message ID so that
// two messages arriving in the same second do not collide on a single file.
func extractMediaInfo(msg *waE2E.Message, msgTimestamp time.Time, msgID string) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Use message timestamp for filename, fallback to current time if zero
	ts := msgTimestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	tsStr := ts.Format("20060102_150405")
	suffix := tsStr
	if msgID != "" {
		suffix = tsStr + "_" + msgID
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + suffix + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + suffix + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + suffix + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + suffix
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	// Sticker message: WebP image, no caption, same URL+MediaKey+SHA shape as other media.
	// On the wire stickers surface as type="media" with an <enc mediatype="sticker"> payload, e.g.:
	//   <message id="..." type="media">
	//     <enc mediatype="sticker" type="msg" v="2"><!-- 660 bytes --></enc>
	//   </message>
	if stk := msg.GetStickerMessage(); stk != nil {
		return "sticker", "sticker_" + suffix + ".webp",
			stk.GetURL(), stk.GetMediaKey(), stk.GetFileSHA256(), stk.GetFileEncSHA256(), stk.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}
