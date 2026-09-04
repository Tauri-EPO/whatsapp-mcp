package main

// Outbound messages: /api/send request/response types, recipient and
// mention resolution, media classification and upload, and the Ogg Opus
// analysis used for voice notes.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	// Set on a successful /api/send so the caller can react to, quote or
	// delete what it just sent without searching for it.
	MessageID string `json:"message_id,omitempty"`
	ChatJID   string `json:"chat_jid,omitempty"`
	Timestamp string `json:"timestamp,omitempty"` // RFC 3339
}

// sendFunc is the /api/send backend (sendWhatsAppMessage in production).
type sendFunc func(ctx context.Context, recipient, message, mediaPath, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage)

// sentMessage identifies a message the bridge just sent.
type sentMessage struct {
	ID        string
	ChatJID   string
	Timestamp time.Time
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient       string `json:"recipient"`
	Message         string `json:"message"`
	MediaPath       string `json:"media_path,omitempty"`
	QuotedMessageID string `json:"quoted_message_id,omitempty"`
	QuotedSenderJID string `json:"quoted_sender_jid,omitempty"`
	QuotedContent   string `json:"quoted_content,omitempty"`
	// Mentions lists users to @-mention (phone numbers or JIDs). The message
	// text must contain a matching "@<number>" token for each entry, or the
	// mention won't render on recipients' devices.
	Mentions []string `json:"mentions,omitempty"`
}

// classifyMediaPath maps a file extension to (whatsmeow upload type, MIME
// type, persist-side category). Single source of truth for the upload path
// (which needs the whatsmeow.MediaType + MIME) and the SQLite persist path
// (which stores the short category string).
func classifyMediaPath(mediaPath string) (whatsmeow.MediaType, string, string) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(mediaPath), "."))
	switch ext {
	case "jpg", "jpeg":
		return whatsmeow.MediaImage, "image/jpeg", "image"
	case "png":
		return whatsmeow.MediaImage, "image/png", "image"
	case "gif":
		return whatsmeow.MediaImage, "image/gif", "image"
	case "webp":
		return whatsmeow.MediaImage, "image/webp", "image"
	case "ogg":
		return whatsmeow.MediaAudio, "audio/ogg; codecs=opus", "audio"
	case "mp4":
		return whatsmeow.MediaVideo, "video/mp4", "video"
	case "avi":
		return whatsmeow.MediaVideo, "video/avi", "video"
	case "mov":
		return whatsmeow.MediaVideo, "video/quicktime", "video"
	default:
		if m := mime.TypeByExtension("." + ext); m != "" {
			return whatsmeow.MediaDocument, m, "document"
		}
		return whatsmeow.MediaDocument, "application/octet-stream", "document"
	}
}

func buildDisappearingMode() *waE2E.DisappearingMode {
	return &waE2E.DisappearingMode{
		Initiator: waE2E.DisappearingMode_CHANGED_IN_CHAT.Enum(),
		Trigger:   waE2E.DisappearingMode_CHAT_SETTING.Enum(),
	}
}

func mergeEphemeralContextInfo(existing *waE2E.ContextInfo, settings ChatEphemeralSettings) *waE2E.ContextInfo {
	if existing == nil {
		existing = &waE2E.ContextInfo{}
	}
	existing.Expiration = proto.Uint32(settings.Expiration)
	existing.EphemeralSettingTimestamp = proto.Int64(settings.SettingTimestamp)
	existing.DisappearingMode = buildDisappearingMode()
	return existing
}

func applyChatEphemeralSettings(msg *waE2E.Message, settings ChatEphemeralSettings) {
	if msg == nil || settings.Expiration == 0 || settings.SettingTimestamp == 0 {
		return
	}

	switch {
	case msg.ExtendedTextMessage != nil:
		msg.ExtendedTextMessage.ContextInfo = mergeEphemeralContextInfo(msg.ExtendedTextMessage.GetContextInfo(), settings)
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = mergeEphemeralContextInfo(msg.ImageMessage.GetContextInfo(), settings)
	case msg.AudioMessage != nil:
		msg.AudioMessage.ContextInfo = mergeEphemeralContextInfo(msg.AudioMessage.GetContextInfo(), settings)
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = mergeEphemeralContextInfo(msg.VideoMessage.GetContextInfo(), settings)
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = mergeEphemeralContextInfo(msg.DocumentMessage.GetContextInfo(), settings)
	case msg.Conversation != nil:
		text := msg.GetConversation()
		msg.Conversation = nil
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: mergeEphemeralContextInfo(nil, settings),
		}
	}
}

// resolveRecipientJID parses a phone number or JID string and resolves PN -> LID
// for personal chats before sending.
func resolveRecipientJID(client *whatsmeow.Client, recipient string) (types.JID, error) {
	var recipientJID types.JID
	var err error

	if strings.Contains(recipient, "@") {
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return types.JID{}, fmt.Errorf("error parsing JID: %v", err)
		}
	} else {
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// For personal chats, resolve phone number JID to LID (Linked Identity).
	// WhatsApp is migrating to LID-based addressing; messages sent to the
	// phone JID silently fail for migrated contacts.
	if recipientJID.Server == types.DefaultUserServer {
		ctx := context.Background()
		lid, lidErr := client.Store.LIDs.GetLIDForPN(ctx, recipientJID)
		if lidErr == nil && !lid.IsEmpty() {
			bridgeLog.Debugf("Resolved %s -> %s (LID)", recipientJID, lid)
			recipientJID = lid
		} else {
			// Cache miss or cache error — ask the WhatsApp server.
			if lidErr != nil {
				bridgeLog.Warnf("LID cache lookup failed for %s: %v, falling back to server", recipientJID, lidErr)
			}
			info, infoErr := client.GetUserInfo(ctx, []types.JID{recipientJID})
			if infoErr != nil {
				bridgeLog.Warnf("server LID lookup failed for %s: %v", recipientJID, infoErr)
			} else if userInfo, ok := info[recipientJID]; ok && !userInfo.LID.IsEmpty() {
				bridgeLog.Debugf("Resolved %s -> %s (LID via server)", recipientJID, userInfo.LID)
				recipientJID = userInfo.LID
			}
		}
	}

	return recipientJID, nil
}

// resolveMentionJIDs maps mention entries (phone numbers or JIDs) to the JID
// strings WhatsApp expects in ContextInfo.MentionedJID. Phone-number entries
// contribute both the phone JID and, when known, its LID form so the mention
// renders regardless of the group's addressing mode.
func resolveMentionJIDs(client *whatsmeow.Client, mentions []string) []string {
	var resolved []string
	for _, mention := range mentions {
		var jid types.JID
		if strings.Contains(mention, "@") {
			parsed, err := types.ParseJID(mention)
			if err != nil {
				bridgeLog.Warnf("skipping unparseable mention %q: %v", mention, err)
				continue
			}
			jid = parsed
		} else {
			jid = types.JID{User: mention, Server: types.DefaultUserServer}
		}
		resolved = append(resolved, jid.String())
		if jid.Server == types.DefaultUserServer {
			if lid, err := client.Store.LIDs.GetLIDForPN(context.Background(), jid); err == nil && !lid.IsEmpty() {
				resolved = append(resolved, lid.String())
			}
		}
	}
	return resolved
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string, quotedMsgID string, quotedSenderJID string, quotedContent string, mentions []string) (bool, string, sentMessage) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", sentMessage{}
	}

	mentionedJIDs := resolveMentionJIDs(client, mentions)

	var settingsLookupJID types.JID
	var err error

	if strings.Contains(recipient, "@") {
		settingsLookupJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), sentMessage{}
		}
	} else {
		settingsLookupJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// Capture pre-LID-resolution JID for SQLite storage.
	// handleMessage uses resolveLIDChat to map LID→phone for incoming events;
	// for outbound we keep the pre-resolution form so the chat stays unified
	// under @s.whatsapp.net (matches what list_chats / list_messages expect).
	storageJID := settingsLookupJID

	recipientJID, err := resolveRecipientJID(client, recipient)
	if err != nil {
		return false, err.Error(), sentMessage{}
	}

	var msg *waE2E.Message

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath) //nolint:gosec // mediaPath was canonicalised and confined to WHATSAPP_MEDIA_ROOTS by validateMediaPath
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err), sentMessage{}
		}

		mediaType, mimeType, _ := classifyMediaPath(mediaPath)

		// Upload media to WhatsApp servers
		resp, err := client.Upload(ctx, mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), sentMessage{}
		}
		bridgeLog.Debugf("Media uploaded (%d bytes)", resp.FileLength)

		msg, err = buildMediaMessage(mediaType, mimeType, mediaPath, mediaData, resp, message)
		if err != nil {
			return false, err.Error(), sentMessage{}
		}
	} else {
		quotedParticipant := ""
		if quotedMsgID != "" {
			// Normalise to a JID recipients can match (bare numbers, LID upgrade);
			// otherwise the quoted bubble shows "You" for everyone. See #13.
			quotedParticipant = resolveQuotedParticipantJID(client, quotedSenderJID)
		}
		msg = buildOutboundText(message, quotedMsgID, quotedParticipant, quotedContent, mentionedJIDs)
	}
	attachCaptionMentions(msg, mentionedJIDs)

	// Normalize @lid recipients to phone JID before the lookup. Chats are
	// persisted under @s.whatsapp.net (handleMessage normalizes via
	// resolveLIDChat); without this step, an API caller passing an @lid
	// recipient would silently miss the disappearing-message settings row.
	settings, err := messageStore.GetChatEphemeralSettings(resolveUserJID(client, settingsLookupJID, types.EmptyJID).String())
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Sprintf("Error loading chat settings: %v", err), sentMessage{}
	}
	if err == nil {
		applyChatEphemeralSettings(msg, settings)
	}

	// Send message
	resp, err := client.SendMessage(ctx, recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), sentMessage{}
	}
	sent := sentMessage{ID: resp.ID, ChatJID: storageJID.String(), Timestamp: resp.Timestamp}
	if sent.Timestamp.IsZero() {
		sent.Timestamp = time.Now()
	}

	// whatsmeow does not re-emit events.Message for messages this client
	// itself just sent, so without an explicit StoreMessage call here
	// list_messages / get_last_interaction never see our own outbound
	// traffic until WhatsApp's multi-device sync echoes them back.
	if messageStore != nil && client.Store != nil && client.Store.ID != nil {
		// Normalize @lid recipients to phone JID so outbound rows land in
		// the same chat row as inbound (which handleMessage normalizes via
		// resolveLIDChat). Otherwise sending to an @lid input would
		// fragment the chat under a separate jid.
		persistJID := resolveUserJID(client, storageJID, types.EmptyJID)
		chatJID := persistJID.String()
		sent.ChatJID = chatJID
		senderUser := client.Store.ID.User
		timestamp := sent.Timestamp

		var mediaType, filename string
		if mediaPath != "" {
			filename = filepath.Base(mediaPath)
			_, _, mediaType = classifyMediaPath(mediaPath)
		}

		// Pass empty name so StoreChat preserves any existing resolved
		// contact/group name; we don't have one available here and
		// must not clobber names from inbound handling or history sync.
		if chatErr := messageStore.StoreChat(chatJID, "", timestamp); chatErr != nil {
			bridgeLog.Warnf("failed to store outbound chat metadata: %v", chatErr)
		}
		if storeErr := messageStore.StoreMessage(
			resp.ID, chatJID, senderUser, message, timestamp, true,
			mediaType, filename, "", nil, nil, nil, 0, quotedMsgID,
		); storeErr != nil {
			bridgeLog.Warnf("failed to persist outbound message: %v", storeErr)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient), sent
}

// buildMediaMessage wraps an upload result in the waE2E message for its
// media type. Voice notes (audio/ogg) are analysed for duration and waveform;
// an unparsable Ogg file is an error because WhatsApp would show a broken
// player. Documents carry the base filename only (see media_path.go).
func buildMediaMessage(mediaType whatsmeow.MediaType, mimeType, mediaPath string, mediaData []byte, resp whatsmeow.UploadResponse, caption string) (*waE2E.Message, error) {
	msg := &waE2E.Message{}
	switch mediaType {
	case whatsmeow.MediaImage:
		msg.ImageMessage = &waE2E.ImageMessage{
			Caption:       proto.String(caption),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
	case whatsmeow.MediaAudio:
		var seconds uint32 = 30 // Default fallback
		var waveform []byte
		if strings.Contains(mimeType, "ogg") {
			analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
			if err != nil {
				return nil, fmt.Errorf("failed to analyze Ogg Opus file: %v", err)
			}
			seconds = analyzedSeconds
			waveform = analyzedWaveform
		} else {
			bridgeLog.Warnf("Not an Ogg Opus file: %s", mimeType)
		}
		msg.AudioMessage = &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
			Seconds:       proto.Uint32(seconds),
			PTT:           proto.Bool(true),
			Waveform:      waveform,
		}
	case whatsmeow.MediaVideo:
		msg.VideoMessage = &waE2E.VideoMessage{
			Caption:       proto.String(caption),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
	default: // whatsmeow.MediaDocument
		msg.DocumentMessage = &waE2E.DocumentMessage{
			// outboundFileName, not a manual split on "/": the document
			// filename travels to the recipient, and on Windows the path
			// is already backslash-normalised, so the naive split leaks
			// the whole absolute path. See media_path.go.
			Title:         proto.String(outboundFileName(mediaPath)),
			FileName:      proto.String(outboundFileName(mediaPath)),
			Caption:       proto.String(caption),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
	}
	return msg, nil
}

// buildOutboundText returns a plain Conversation for bare text, or an
// ExtendedTextMessage carrying ContextInfo when the text quotes a message or
// mentions someone. Only text quoting is supported: the quoted preview on
// the recipient's device would need the original media's key/URL.
func buildOutboundText(text, quotedMsgID, quotedParticipant, quotedContent string, mentionedJIDs []string) *waE2E.Message {
	if quotedMsgID == "" && len(mentionedJIDs) == 0 {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	ctx := &waE2E.ContextInfo{MentionedJID: mentionedJIDs}
	if quotedMsgID != "" {
		ctx.StanzaID = proto.String(quotedMsgID)
		ctx.Participant = proto.String(quotedParticipant)
		ctx.QuotedMessage = &waE2E.Message{Conversation: proto.String(quotedContent)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ctx}}
}

// attachCaptionMentions puts mentions in media captions on the media
// message's own ContextInfo (text messages carry them already).
func attachCaptionMentions(msg *waE2E.Message, mentionedJIDs []string) {
	if len(mentionedJIDs) == 0 {
		return
	}
	switch {
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = &waE2E.ContextInfo{MentionedJID: mentionedJIDs}
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = &waE2E.ContextInfo{MentionedJID: mentionedJIDs}
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = &waE2E.ContextInfo{MentionedJID: mentionedJIDs}
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			// OpusHead body (RFC 7845 §5.1) after the 8-byte magic: Version(1),
			// Channels(1), PreSkip(2), SampleRate(4), Gain(2), Mapping(1). The
			// packet is 19 bytes and a real first page exactly 47, so bound on
			// the 8 body bytes we read, not on the old +12/+16 offsets that
			// pointed past the packet and skipped every real header.
			if headPos >= 0 && headPos+8+8 <= len(pageData) {
				body := pageData[headPos+8:]
				preSkip = binary.LittleEndian.Uint16(body[2:4])
				sampleRate = binary.LittleEndian.Uint32(body[4:8])
				foundOpusHead = true
				bridgeLog.Debugf("Found OpusHead: sampleRate=%d, preSkip=%d", sampleRate, preSkip)
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		bridgeLog.Warnf("OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		bridgeLog.Debugf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		bridgeLog.Warnf("No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	bridgeLog.Debugf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Deterministic per duration so the same voice note always renders the same
	// waveform (rand.Seed is deprecated; a local generator gives the same effect).
	rng := rand.New(rand.NewSource(int64(duration))) //nolint:gosec // decorative waveform, not a secret

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}

// handleSend serves POST /api/send.
func (b *Bridge) handleSend(allowedMediaRoots []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.Log.Debugf("→ /api/send from=%q user_agent=%q", r.RemoteAddr, r.UserAgent())

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		// Validate request
		if req.Recipient == "" {
			writeError(w, http.StatusBadRequest, "Recipient is required")
			return
		}
		if rejectByChatPolicy(w, b.Policy, req.Recipient) {
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			writeError(w, http.StatusBadRequest, "Message or media path is required")
			return
		}

		// Validate and canonicalize media_path against the configured roots
		// before reading. This prevents the bridge from being used as a
		// generic file-read primitive (e.g. media_path=/Users/x/.ssh/id_rsa).
		// Only the canonical path ever reaches sendWhatsAppMessage; the raw
		// request value is never used as a file path.
		resolvedMediaPath := ""
		if req.MediaPath != "" {
			canonical, mpErr := validateMediaPath(req.MediaPath, allowedMediaRoots)
			if mpErr != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(SendMessageResponse{
					Success: false,
					Message: fmt.Sprintf("media_path rejected: %v", mpErr),
				})
				return
			}
			resolvedMediaPath = canonical
		}

		// Avoid logging req.Message verbatim — it's user content and may
		// contain secrets the user pasted into a chat.
		b.Log.Debugf("→ /api/send recipient=%q message_len=%d has_media=%v",
			req.Recipient, len(req.Message), resolvedMediaPath != "")

		// Send the message
		ctx, cancel := requestContext(r, sendDeadline)
		defer cancel()
		success, message, sent := b.Send(ctx, req.Recipient, req.Message, resolvedMediaPath, req.QuotedMessageID, req.QuotedSenderJID, req.QuotedContent, req.Mentions)
		b.Log.Debugf("← /api/send success=%v status=%q id=%q", success, message, sent.ID)
		if success {
			b.metrics.messagesSent.Add(1)
		} else {
			b.metrics.sendFailures.Add(1)
		}
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		resp := SendMessageResponse{Success: success, Message: message}
		if success {
			resp.MessageID, resp.ChatJID = sent.ID, sent.ChatJID
			resp.Timestamp = sent.Timestamp.UTC().Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
