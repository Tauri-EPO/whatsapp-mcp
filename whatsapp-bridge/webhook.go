package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// maxMediaBase64Bytes is the maximum file size that will be base64-encoded and
// included in a webhook payload. Files larger than this limit are skipped to
// avoid excessive memory use and oversized HTTP requests.
const maxMediaBase64Bytes = 10 * 1024 * 1024 // 10 MB

// defaultWebhookURL is used when WEBHOOK_URL is not set. It must never receive
// the bridge token: unlike an operator-configured WEBHOOK_URL, nothing has
// vetted this address, so any other local process that happens to bind this
// port could otherwise capture the token just by being reachable.
const defaultWebhookURL = "http://localhost:8769/whatsapp/webhook"

// webhookSender POSTs inbound-message events to WEBHOOK_URL. One instance
// lives on Bridge; tests build their own with a test server as defaultURL.
type webhookSender struct {
	client *http.Client
	// token is the shared bridge token attached as "X-Bridge-Token" to every
	// outbound POST — the same token the bridge requires on inbound /api/*
	// requests. Empty (no token configured) omits the header so deployments
	// that predate the token rollout keep working. A dedicated header is used
	// rather than Authorization so it never collides with a receiver's own
	// Authorization-based auth (e.g. HTTP Basic auth derived from credentials
	// embedded in WEBHOOK_URL). Receivers accept it via this header or
	// "Authorization: Bearer".
	token string
	// defaultURL is used when WEBHOOK_URL is unset (see defaultWebhookURL).
	defaultURL string
	// enabled mirrors WEBHOOK_ENABLED and url WEBHOOK_URL ("" = unset), both
	// read once when the sender is built instead of on every message.
	enabled bool
	url     string
	// failures counts POSTs that errored or got a non-2xx (nil = not counted).
	failures *atomic.Int64
}

// newWebhookSender builds the production sender. The 30-second timeout
// prevents a slow or unreachable webhook from blocking message handling
// indefinitely. Redirects are never followed: WEBHOOK_URL is a single
// operator-configured endpoint, not a browsable URL, and following a 3xx
// would forward X-Bridge-Token to whatever host the redirect names — Go only
// strips Authorization/Cookie on cross-origin redirects, not custom headers.
//
// enabled is WEBHOOK_ENABLED as main() resolved it (env_bool.go).
func newWebhookSender(token string, enabled bool) *webhookSender {
	return &webhookSender{
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token:      token,
		defaultURL: defaultWebhookURL,
		enabled:    enabled,
		url:        os.Getenv("WEBHOOK_URL"),
	}
}

// Enabled reports whether outbound webhooks are on (WEBHOOK_ENABLED at startup).
func (w *webhookSender) Enabled() bool { return w != nil && w.enabled }

// WebhookPayload represents the data sent to the webhook
type WebhookPayload struct {
	EventType       string   `json:"eventType,omitempty"`
	Sender          string   `json:"sender"`
	Content         string   `json:"content"`
	ChatJID         string   `json:"chatJID"`
	IsFromMe        bool     `json:"isFromMe"`
	QuotedMessageId string   `json:"quotedMessageId,omitempty"`
	QuotedSender    string   `json:"quotedSender,omitempty"`
	QuotedContent   string   `json:"quotedContent,omitempty"`
	QuotedIsFromMe  *bool    `json:"quotedIsFromMe,omitempty"`
	MentionedJIDs   []string `json:"mentionedJids,omitempty"`
	// Media fields - populated when the message contains an image attachment
	MessageID     string `json:"messageId,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	MimeType      string `json:"mimeType,omitempty"`
	MediaFilename string `json:"mediaFilename,omitempty"`
	MediaBase64   string `json:"mediaBase64,omitempty"`
	// Stored is only ever sent as false: the bridge could not write this
	// message, so its ID resolves to nothing (download_media, a quote). Absent
	// means stored (issue #518).
	Stored *bool `json:"stored,omitempty"`
	// Reaction fields - populated when EventType is "reaction".
	ReactionToMessageID string  `json:"reactionToMessageId,omitempty"`
	ReactionEmoji       *string `json:"reactionEmoji,omitempty"`
	ReactionRemoved     *bool   `json:"reactionRemoved,omitempty"`
}

// sendWebhookPayload marshals and POSTs a WebhookPayload to the configured webhook URL.
func (w *webhookSender) sendPayload(payload WebhookPayload) {
	// WEBHOOK_ENABLED=false turns outbound webhooks off entirely. An empty
	// WEBHOOK_URL cannot serve that purpose: os.Getenv cannot tell "unset"
	// from "explicitly empty", and empty deliberately falls back to
	// defaultWebhookURL below. Deployments with no webhook consumer would
	// otherwise POST to that default for every message and log a connection
	// refused error each time.
	if !w.enabled {
		return
	}

	webhookURL := w.url
	explicitlyConfigured := webhookURL != ""
	if !explicitlyConfigured {
		webhookURL = w.defaultURL
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		bridgeLog.Errorf("marshaling webhook payload: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		bridgeLog.Errorf("building webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Authenticate to the hub's fail-closed inbound webhook route with the
	// shared bridge token, via a dedicated header so it can never clobber a
	// receiver's own Authorization-based auth (see the token field doc
	// comment above). Only attach it when BOTH a token is configured AND
	// WEBHOOK_URL was explicitly set by the operator — the bridge token also
	// authorizes /api/* calls like sending messages, and the implicit local
	// default is not a destination anyone vetted, so it must never receive it.
	if w.token != "" && explicitlyConfigured {
		req.Header.Set("X-Bridge-Token", w.token)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.countFailure()
		bridgeLog.Errorf("sending webhook: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 200 {
		bridgeLog.Debugf("✓ Webhook sent for message from %s", payload.Sender)
	} else {
		w.countFailure()
		bridgeLog.Warnf("Webhook failed with status %d", resp.StatusCode)
	}
}

// SendWebhook sends a text-only message to the webhook endpoint. New callers
// should use SendWebhookWithMessageID so receiver-side idempotency can identify
// repeated WhatsApp events; this wrapper remains for compatibility.
func (w *webhookSender) SendWebhook(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string) {
	w.SendWebhookWithMessageID(sender, content, chatJID, isFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, "", true)
}

// storedField renders WebhookPayload.Stored: nothing for a stored message.
func storedField(stored bool) *bool {
	if stored {
		return nil
	}
	return &stored
}

// SendWebhookWithMessageID sends a text-only message and preserves the native
// WhatsApp message ID in the payload for downstream idempotency.
func (w *webhookSender) SendWebhookWithMessageID(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string, messageID string, stored bool) {
	w.sendPayload(WebhookPayload{
		Sender:          sender,
		Content:         content,
		ChatJID:         chatJID,
		IsFromMe:        isFromMe,
		QuotedMessageId: quotedMessageId,
		QuotedSender:    quotedSender,
		QuotedContent:   quotedContent,
		QuotedIsFromMe:  quotedIsFromMe,
		MentionedJIDs:   mentionedJIDs,
		MessageID:       messageID,
		Stored:          storedField(stored),
	})
}

// webhookMedia reads a cached media file for the webhook: the MIME type sniffed
// from its first bytes (never the generated extension, which is always .jpg for
// an image) and, when the file fits maxMediaBase64Bytes, its content. On any
// failure it says why at WARN and returns no bytes, so the webhook still goes
// out with the caption.
//
// The file is named the way downloadMedia named it, chat directory and file
// name, and opened through the store root. The absolute path downloadMedia
// returns was checked when the file was written, which is not a control at the
// read: a component swapped for a symlink in between would have had another
// file encoded into the payload and sent to WEBHOOK_URL (issue #493). Two
// things stand in the way now. Nothing is followed, not even a link that stays
// inside the store (openStoreMedia). And the bytes must be the ones the message
// declared: their SHA-256 is compared with wantSHA256, the message's own
// file_sha256, because a hard link or a rename can put another regular file of
// the store (the token, a database) under the cached name without any link.
// Without a hash to compare with, nothing is sent.
func (b *Bridge) webhookMedia(chatJID, filename string, wantSHA256 []byte) (mimeType string, data []byte) {
	f, size, err := openStoreMedia(b.StoreRoot, chatMediaRel(chatJID), filename)
	if err != nil {
		b.Log.Warnf("Could not open media file for the webhook: %v", err)
		return sniffMIME(nil), nil
	}
	defer func() { _ = f.Close() }()
	if size > maxMediaBase64Bytes {
		b.Log.Warnf("Media file too large for base64 encoding (%d bytes), skipping MediaBase64", size)
		head := make([]byte, 512)
		n, _ := io.ReadFull(f, head)
		return sniffMIME(head[:n]), nil
	}
	// The cap is on the handle being read: exactly the size it reported, so a
	// file that grows afterwards cannot put more than the limit in memory.
	data = make([]byte, size)
	if _, err := io.ReadFull(f, data); err != nil {
		b.Log.Warnf("Could not read media file for base64 encoding: %v", err)
		return sniffMIME(nil), nil
	}
	if sum := sha256.Sum256(data); len(wantSHA256) == 0 || !bytes.Equal(sum[:], wantSHA256) {
		b.Log.Warnf("Cached media is not the file the message declared (SHA-256 differs or is unknown); sending the webhook without it")
		return sniffMIME(nil), nil
	}
	return sniffMIME(data), data
}

// sniffMIME is the content type of a file from its first bytes, and
// application/octet-stream when there are none to look at.
func sniffMIME(head []byte) string {
	if len(head) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head)
}

// openStoreMedia opens store/<chatDir>/<name> for reading without following a
// symlink at either component, and returns its size. os.Root keeps the open
// inside the store but does follow a link that stays inside it, so each
// component is checked with Lstat, opened, and the handle compared with what
// Lstat saw: a name swapped for a link between the two is a different file and
// is refused. The directory is opened first and the file is named inside that
// handle, never by the full path again, so swapping the directory after its
// check changes nothing.
func openStoreMedia(root *os.Root, chatDir, name string) (*os.File, int64, error) {
	if root == nil {
		return nil, 0, errors.New("store directory unavailable")
	}
	if err := checkMediaPathComponents(chatDir, name); err != nil {
		return nil, 0, err
	}
	notADir := errors.New("chat directory is not a real directory inside the store")
	seenDir, err := root.Lstat(chatDir)
	if err != nil || !seenDir.IsDir() {
		return nil, 0, notADir
	}
	dir, err := root.OpenRoot(chatDir)
	if err != nil {
		return nil, 0, notADir
	}
	defer func() { _ = dir.Close() }()
	if pinned, err := dir.Stat("."); err != nil || !os.SameFile(seenDir, pinned) {
		return nil, 0, errors.New("chat directory changed while it was being opened")
	}
	seen, err := dir.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if !seen.Mode().IsRegular() {
		return nil, 0, errors.New("cached media is not a regular file")
	}
	f, err := dir.Open(name)
	if err != nil {
		return nil, 0, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(seen, opened) {
		_ = f.Close()
		return nil, 0, errors.New("cached media changed while it was being opened")
	}
	return f, opened.Size(), nil
}

// SendWebhookWithMedia sends a message to the webhook endpoint including the
// base64-encoded image bytes in media (Bridge.webhookMedia reads them). With no
// bytes the webhook is still sent – just without the MediaBase64 field, so the
// text caption is not lost.
func (w *webhookSender) SendWebhookWithMedia(
	sender, content, chatJID string,
	isFromMe bool,
	quotedMessageId, quotedSender, quotedContent string,
	quotedIsFromMe *bool, mentionedJIDs []string,
	messageID, mediaType, mimeType, mediaFilename string, media []byte, stored bool,
) {
	if !w.enabled {
		return
	}

	var mediaBase64 string
	if len(media) > 0 {
		mediaBase64 = base64.StdEncoding.EncodeToString(media)
	}

	w.sendPayload(WebhookPayload{
		Sender:          sender,
		Content:         content,
		ChatJID:         chatJID,
		IsFromMe:        isFromMe,
		QuotedMessageId: quotedMessageId,
		QuotedSender:    quotedSender,
		QuotedContent:   quotedContent,
		QuotedIsFromMe:  quotedIsFromMe,
		MentionedJIDs:   mentionedJIDs,
		MessageID:       messageID,
		MediaType:       mediaType,
		MimeType:        mimeType,
		MediaFilename:   mediaFilename,
		MediaBase64:     mediaBase64,
		Stored:          storedField(stored),
	})
}

// SendReactionWebhook sends a typed reaction event to the webhook endpoint.
func (w *webhookSender) SendReactionWebhook(sender, chatJID string, isFromMe bool, messageID, reactionToMessageID, emoji string, stored bool) {
	removed := emoji == ""
	w.sendPayload(WebhookPayload{
		EventType:           "reaction",
		Sender:              sender,
		Content:             emoji,
		ChatJID:             chatJID,
		IsFromMe:            isFromMe,
		MessageID:           messageID,
		MediaType:           "reaction",
		ReactionToMessageID: reactionToMessageID,
		Stored:              storedField(stored),
		ReactionEmoji:       &emoji,
		ReactionRemoved:     &removed,
	})
}

// In main.go, handleMessage forwards webhooks for messages with text content.
// It will forward self-sent messages when the env var FORWARD_SELF=true.

func (w *webhookSender) countFailure() {
	if w != nil && w.failures != nil {
		w.failures.Add(1)
	}
}
