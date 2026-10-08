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
	"errors"
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

type outboundPersistence func(types.JID, sentMessage, string, outboundMedia, string) (string, error)

const outboundArchiveWarning = "message was sent, but its archive row could not be written; do not resend it"

func outboundSendStatus(recipient string, persistErr error) string {
	message := fmt.Sprintf("Message sent to %s", recipient)
	if persistErr != nil {
		message += "; warning: " + outboundArchiveWarning
	}
	return message
}

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
	case msg.Conversation != nil:
		text := msg.GetConversation()
		msg.Conversation = nil
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: mergeEphemeralContextInfo(nil, settings),
		}
	default:
		// Image, video, document, audio: added to the context a send already
		// put there (a quote, mentions), never in place of it.
		if slot := mediaContextInfo(msg); slot != nil {
			*slot = mergeEphemeralContextInfo(*slot, settings)
		}
	}
}

// parseRecipientJID reads a recipient the way the REST API takes one: a bare
// phone number means a personal chat, anything with an "@" is a full JID.
func parseRecipientJID(recipient string) (types.JID, error) {
	if !strings.Contains(recipient, "@") {
		return types.JID{User: recipient, Server: types.DefaultUserServer}, nil
	}
	return types.ParseJID(recipient)
}

// recipientSeparators is the explicit contract shared with phone.py; Unicode
// category tables differ between the Go and Python runtimes. Never extend it
// without adding each character to the shared spelling fixture.
const recipientSeparators = " \t-().\u00a0\u202f\u2007\u2009\u200b\u200e\u200f\u202a\u202c\u2066\u2067\u2068\u2069\ufeff\u2010\u2011\u2012\u2013\u2014"

// normalizePhoneRecipient runs once at a send/forward boundary, before policy.
// Full JIDs and invalid spellings stay as given, without gaining an alias.
// Existing short digit-only recipients keep their behavior.
func normalizePhoneRecipient(raw string) (string, error) {
	if strings.Contains(raw, "@") {
		return raw, nil
	}
	compact := strings.Map(func(r rune) rune {
		if strings.ContainsRune(recipientSeparators, r) {
			return -1
		}
		return r
	}, raw)
	compact = strings.TrimPrefix(compact, "+")
	if len(compact) >= 7 && isPhoneDigits(compact) {
		if len(compact) > 15 {
			return "", errors.New("phone recipient exceeds 15 digits; use a full JID for a group")
		}
		return compact, nil
	}
	return raw, nil
}

// isOnWhatsAppFunc asks WhatsApp whether phone numbers ("+" and digits) have
// an account, and under which JID (Client.IsOnWhatsApp in production, a fake
// in tests).
type isOnWhatsAppFunc func(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)

// lidForPNFunc reads the local phone -> LID map (Store.LIDs.GetLIDForPN).
type lidForPNFunc func(ctx context.Context, pn types.JID) (types.JID, error)

// The two ways the registered-number question can end without a number. Any
// other error of canonicalRecipientJID is a recipient that cannot be read.
var (
	// errNotOnWhatsApp: WhatsApp answered for the number, and it has no account.
	errNotOnWhatsApp = errors.New("number is not on WhatsApp")
	// errRecipientLookup: the question failed or came back empty, so nothing
	// is known about the number. Never read it as "not registered".
	errRecipientLookup = errors.New("could not check the number with WhatsApp")
)

// recipientLookupTimeout bounds the registered-number question, so a slow
// answer cannot eat the deadline of the send that follows it.
const recipientLookupTimeout = 10 * time.Second

const notConnectedMessage = "Not connected to WhatsApp"

// canonicalRecipientJID returns the JID a send to recipient is addressed,
// stored and allow-listed under: for a phone number, the one WhatsApp has
// registered. That is not always the number as dialled (a Brazilian mobile
// typed with its ninth digit may be registered without it), and a send to the
// dialled spelling dies in whatsmeow with "no LID found" (issue #444).
//
// A number the LID map knows is one WhatsApp itself named, so it comes back
// as it is and costs no network call. Only a miss asks isOnWhatsApp, the
// question the phone app asks when a number is typed into it. Groups, @lid
// and every other server are returned untouched.
func canonicalRecipientJID(ctx context.Context, lidForPN lidForPNFunc, isOnWhatsApp isOnWhatsAppFunc, recipient string) (types.JID, error) {
	jid, err := parseRecipientJID(recipient)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("error parsing JID: %v", err)
	}
	if jid.Server != types.DefaultUserServer {
		return jid, nil
	}
	if lid, lidErr := lidForPN(ctx, jid); lidErr == nil && !lid.IsEmpty() {
		return jid, nil
	}
	if !isPhoneDigits(jid.User) {
		return types.EmptyJID, fmt.Errorf("%q is not a phone number: use the country code and ASCII digits, or a digits-only @s.whatsapp.net JID", jid.User)
	}
	// The answers are read before the error: whatsmeow returns both when the
	// query worked and only its own write of the LID mapping failed.
	answers, err := isOnWhatsApp(ctx, []string{"+" + jid.User})
	for _, answer := range answers {
		if !answer.IsIn {
			continue
		}
		if registered := registeredPhoneJID(answer); !registered.IsEmpty() {
			return registered, nil
		}
		// On WhatsApp, but the answer names a LID and no phone JID: the
		// number as typed is all there is to go by.
		return jid, nil
	}
	switch {
	case err != nil:
		return types.EmptyJID, fmt.Errorf("%w: %v", errRecipientLookup, err)
	case len(answers) == 0:
		// A throttled or degraded query looks like this too.
		return types.EmptyJID, fmt.Errorf("%w: no answer for it", errRecipientLookup)
	}
	return types.EmptyJID, errNotOnWhatsApp
}

// registeredPhoneJID picks the phone JID out of an IsOnWhatsApp answer. With
// LID addressing the answer's JID is the LID and PhoneNumber the number;
// without it the JID is the number itself.
func registeredPhoneJID(answer types.IsOnWhatsAppResponse) types.JID {
	for _, jid := range []types.JID{answer.PhoneNumber, answer.JID} {
		if jid.Server == types.DefaultUserServer && jid.User != "" {
			return jid.ToNonAD()
		}
	}
	return types.EmptyJID
}

func isPhoneDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// resolveRecipientJID parses a phone number or JID string and resolves PN -> LID
// for personal chats before sending.
//
// It rewrites the address and never the number: /api/mark-read shares it, and
// a receipt goes to a chat the archive already holds, under the JID WhatsApp
// gave it. Finding the registered number is canonicalRecipientJID, which only
// /api/send runs, between its two allow-list checks.
func resolveRecipientJID(client *whatsmeow.Client, recipient string) (types.JID, error) {
	return resolveRecipientJIDContext(context.Background(), client, recipient)
}

func resolveRecipientJIDContext(ctx context.Context, client *whatsmeow.Client, recipient string) (types.JID, error) {
	if err := ctx.Err(); err != nil {
		return types.EmptyJID, err
	}
	recipientJID, err := parseRecipientJID(recipient)
	if err != nil {
		return types.JID{}, fmt.Errorf("error parsing JID: %v", err)
	}

	// For personal chats, resolve phone number JID to LID (Linked Identity).
	// WhatsApp is migrating to LID-based addressing; messages sent to the
	// phone JID silently fail for migrated contacts.
	if recipientJID.Server == types.DefaultUserServer {
		lid, lidErr := client.Store.LIDs.GetLIDForPN(ctx, recipientJID)
		if err := ctx.Err(); err != nil {
			return types.EmptyJID, err
		}
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
func sendWhatsAppMessage(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, persist outboundPersistence, recipient string, message string, mediaPath string, quotedMsgID string, quotedSenderJID string, quotedContent string, mentions []string) (bool, string, sentMessage) {
	if !client.IsConnected() {
		return false, notConnectedMessage, sentMessage{}
	}

	mentionedJIDs := resolveMentionJIDs(client, mentions)

	settingsLookupJID, err := parseRecipientJID(recipient)
	if err != nil {
		return false, fmt.Sprintf("Error parsing JID: %v", err), sentMessage{}
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

	quote := outboundQuote{id: quotedMsgID, content: quotedContent}
	if quotedMsgID != "" {
		// Normalise to a JID recipients can match (bare numbers, LID upgrade);
		// otherwise the quoted bubble shows "You" for everyone. See #13.
		quote.participant = resolveQuotedParticipantJID(client, quotedSenderJID)
	}

	var msg *waE2E.Message
	// What the upload returned, kept for the stored row: nothing else ever
	// carries the key of a file this bridge sent (issue #449).
	var upload whatsmeow.UploadResponse

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath) //nolint:gosec // mediaPath was canonicalised and confined to WHATSAPP_MEDIA_ROOTS by validateMediaPath
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err), sentMessage{}
		}

		mediaType, mimeType, _ := classifyMediaPath(mediaPath)

		// Upload media to WhatsApp servers
		upload, err = client.Upload(ctx, mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), sentMessage{}
		}
		// The length only: the response carries the media key.
		bridgeLog.Debugf("Media uploaded (%d bytes)", upload.FileLength)

		// The message with its quote and mentions, and the caption that
		// really travels with it: that text, not the one asked for, is what
		// the row records below (issue #476).
		msg, message, err = buildOutboundMedia(mediaType, mimeType, mediaPath, mediaData, upload, message, quote, mentionedJIDs)
		if err != nil {
			return false, err.Error(), sentMessage{}
		}
	} else {
		msg = buildOutboundText(message, quote.id, quote.participant, quote.content, mentionedJIDs)
	}

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
		sent.ChatJID, err = persist(storageJID, sent, message, outboundMediaColumns(mediaPath, upload), quotedMsgID)
	}

	return true, outboundSendStatus(recipient, err), sent
}

// persistOutbound stores the row of a message this bridge just sent, and its
// chat, and returns the chat JID they were stored under. client.Store.ID must
// be set (a paired client).
func (b *Bridge) persistOutbound(storageJID types.JID, sent sentMessage, content string, media outboundMedia, quotedMsgID string) (string, error) {
	client, messageStore := b.Client, b.Store
	// Normalize @lid recipients to phone JID so outbound rows land in
	// the same chat row as inbound (which handleMessage normalizes via
	// resolveLIDChat). Otherwise sending to an @lid input would
	// fragment the chat under a separate jid.
	chatJID := resolveUserJID(client, storageJID, types.EmptyJID).String()
	// Our own JID is always a phone JID, so an outbound row records the
	// phone namespace (#375); ToNonAD drops the device suffix.
	senderJID := storedSender(client.Store.ID.ToNonAD())

	// Pass empty name so StoreChat preserves any existing resolved
	// contact/group name; we don't have one available here and
	// must not clobber names from inbound handling or history sync.
	// One budget for both upserts, after the remote send; shutdown cancels
	// retry waits through the same Bridge policy as event/history writes.
	err := b.retryOutbound(
		func() error { return messageStore.StoreChat(chatJID, "", sent.Timestamp) },
		func() error {
			return media.store(messageStore, sent.ID, chatJID, senderJID, content, sent.Timestamp, quotedMsgID)
		},
	)
	if err != nil {
		b.noteStoreFailure("outbound message", sent.ID, chatJID, err)
	}
	return chatJID, err
}

// retryOutbound owns one budget for both repeatable local writes. Separate
// callbacks let tests acquire a real writer lock between the two statements.
func (b *Bridge) retryOutbound(chatWrite, messageWrite func() error) error {
	return b.retryBusy(func() error {
		if err := chatWrite(); err != nil {
			return err
		}
		return messageWrite()
	})
}

// outboundMedia is the media half of an outbound row: the columns
// StoreMessage takes for a file this bridge uploaded. The zero value is a
// text-only send.
type outboundMedia struct {
	mediaType, filename, url            string
	directPath                          string
	mediaKey, fileSHA256, fileEncSHA256 []byte
	fileLength                          uint64
}

// outboundMediaColumns maps an upload to the columns of its row. whatsmeow
// never echoes our own sends back as events.Message, so what is not stored
// here is gone, and the file could never be downloaded again (issue #449).
// Fields are matched by name: StoreMessage takes the plaintext hash before
// the encrypted one, the waE2E literals in buildMediaMessage set them the
// other way round.
//
// The row keeps the upload's direct path, which is what a download asks for,
// and its URL as inbound rows do; an upload that answered with a direct path
// only gets the URL form a media retry uses, so the url column is never empty
// for a file that can be fetched. The filename is the one the recipient was
// shown (outboundFileName), not whatever the host calls the path.
func outboundMediaColumns(mediaPath string, upload whatsmeow.UploadResponse) outboundMedia {
	if mediaPath == "" {
		return outboundMedia{}
	}
	_, _, mediaType := classifyMediaPath(mediaPath)
	url := upload.URL
	if url == "" && upload.DirectPath != "" {
		url = mediaURLFromDirectPath(upload.DirectPath)
	}
	return outboundMedia{
		mediaType:     mediaType,
		filename:      outboundFileName(mediaPath),
		url:           url,
		directPath:    upload.DirectPath,
		mediaKey:      upload.MediaKey,
		fileSHA256:    upload.FileSHA256,
		fileEncSHA256: upload.FileEncSHA256,
		fileLength:    upload.FileLength,
	}
}

// store persists the outbound row with these media columns.
func (m outboundMedia) store(messageStore *MessageStore, id, chatJID, senderJID, content string, timestamp time.Time, quotedMsgID string) error {
	return messageStore.StoreMessage(
		id, chatJID, senderJID, content, timestamp, true,
		m.mediaType, m.filename, m.url, m.mediaKey, m.fileSHA256, m.fileEncSHA256, m.fileLength, quotedMsgID, m.directPath,
	)
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
	ctx := outboundContextInfo(outboundQuote{id: quotedMsgID, participant: quotedParticipant, content: quotedContent}, mentionedJIDs)
	if ctx == nil {
		return &waE2E.Message{Conversation: proto.String(text)}
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ctx}}
}

// outboundQuote is the message a send replies to: its ID, its author as a JID
// recipients can match, and the text shown in the reply bubble. The zero value
// is a send that quotes nothing.
type outboundQuote struct {
	id, participant, content string
}

// outboundContextInfo builds the ContextInfo of a send, the quote and the
// mentions together, or nil when it has neither. Text and media messages both
// get theirs from here, so a reply is a reply whatever it carries.
func outboundContextInfo(quote outboundQuote, mentionedJIDs []string) *waE2E.ContextInfo {
	if quote.id == "" && len(mentionedJIDs) == 0 {
		return nil
	}
	ctx := &waE2E.ContextInfo{MentionedJID: mentionedJIDs}
	if quote.id != "" {
		ctx.StanzaID = proto.String(quote.id)
		ctx.Participant = proto.String(quote.participant)
		ctx.QuotedMessage = &waE2E.Message{Conversation: proto.String(quote.content)}
	}
	return ctx
}

// buildOutboundMedia builds the media message of a send complete, with its
// quote and its caption mentions, and returns next to it the caption that
// travels with the file. A voice note carries no caption, so it mentions
// nobody and its text is "": the caller stores what was sent, not what was
// asked for.
func buildOutboundMedia(mediaType whatsmeow.MediaType, mimeType, mediaPath string, mediaData []byte, upload whatsmeow.UploadResponse, caption string, quote outboundQuote, mentionedJIDs []string) (*waE2E.Message, string, error) {
	msg, err := buildMediaMessage(mediaType, mimeType, mediaPath, mediaData, upload, caption)
	if err != nil {
		return nil, "", err
	}
	if msg.AudioMessage != nil {
		caption, mentionedJIDs = "", nil
	}
	if slot := mediaContextInfo(msg); slot != nil {
		*slot = outboundContextInfo(quote, mentionedJIDs)
	}
	return msg, caption, nil
}

// mediaContextInfo returns the ContextInfo slot of the media message msg
// holds (image, video, document or audio), or nil for anything else.
func mediaContextInfo(msg *waE2E.Message) **waE2E.ContextInfo {
	switch {
	case msg == nil:
		return nil
	case msg.ImageMessage != nil:
		return &msg.ImageMessage.ContextInfo
	case msg.VideoMessage != nil:
		return &msg.VideoMessage.ContextInfo
	case msg.DocumentMessage != nil:
		return &msg.DocumentMessage.ContextInfo
	case msg.AudioMessage != nil:
		return &msg.AudioMessage.ContextInfo
	}
	return nil
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

// registeredRecipient turns the recipient of a send into the JID WhatsApp has
// it registered under (canonicalRecipientJID), and answers the request itself
// when there is nobody to send to: ok is false once it wrote the response.
//
// Security: the handler has already normalized once at its request boundary
// and checked the allow-list on that number before WhatsApp is asked anything,
// so a number outside the list is
// never looked up. The registered number is checked here as well: a number
// that is not on the list must not become reachable through another spelling
// of it that is. And when the question cannot be answered, a bridge with an
// allow-list refuses the send, because it cannot tell which number the
// message would go to; only a bridge with no list to protect falls back to
// the number as typed, which is what every send did before this lookup.
func (b *Bridge) registeredRecipient(ctx context.Context, w http.ResponseWriter, recipient string) (string, bool) {
	if !b.Connected() {
		// Nothing can be asked, so nothing is sent: letting the send find
		// out for itself would let a reconnect in between skip the checks.
		b.metrics.sendFailures.Add(1)
		writeError(w, http.StatusInternalServerError, notConnectedMessage)
		return "", false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, recipientLookupTimeout)
	defer cancel()
	registered, err := canonicalRecipientJID(lookupCtx, b.Client.Store.LIDs.GetLIDForPN, b.IsOnWhatsApp, recipient)
	switch {
	case errors.Is(err, errNotOnWhatsApp):
		b.metrics.sendFailures.Add(1)
		writeError(w, http.StatusNotFound, recipient+" is not on WhatsApp: no account is registered under that number (country code first; supported formatting is accepted)")
		return "", false
	case errors.Is(err, errRecipientLookup) && b.Policy.restricted:
		b.metrics.sendFailures.Add(1)
		writeError(w, http.StatusBadGateway, err.Error()+"; nothing was sent")
		return "", false
	case errors.Is(err, errRecipientLookup):
		b.Log.Warnf("%v; sending to %s as typed", err, recipient)
		return recipient, true
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	if _, ok := authorizeChat(w, b.Policy, registered.String(), true); !ok {
		return "", false
	}
	if typed := normalizeChatEntry(recipient); typed != registered.String() {
		b.Log.Debugf("→ /api/send recipient %s is registered on WhatsApp as %s", typed, registered)
	}
	return registered.String(), true
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
		var err error
		req.Recipient, err = normalizePhoneRecipient(req.Recipient)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Recipient == "" {
			writeError(w, http.StatusBadRequest, "Recipient is required")
			return
		}
		if _, ok := authorizeChat(w, b.Policy, req.Recipient, true); !ok {
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

		ctx, cancel := requestContext(r, sendDeadline)
		defer cancel()
		recipient, ok := b.registeredRecipient(ctx, w, req.Recipient)
		if !ok {
			return
		}

		// Send the message
		success, message, sent := b.Send(ctx, recipient, req.Message, resolvedMediaPath, req.QuotedMessageID, req.QuotedSenderJID, req.QuotedContent, req.Mentions)
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
