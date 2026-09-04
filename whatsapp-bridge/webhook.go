package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
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
}

// newWebhookSender builds the production sender. The 30-second timeout
// prevents a slow or unreachable webhook from blocking message handling
// indefinitely. Redirects are never followed: WEBHOOK_URL is a single
// operator-configured endpoint, not a browsable URL, and following a 3xx
// would forward X-Bridge-Token to whatever host the redirect names — Go only
// strips Authorization/Cookie on cross-origin redirects, not custom headers.
func newWebhookSender(token string) *webhookSender {
	return &webhookSender{
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token:      token,
		defaultURL: defaultWebhookURL,
		enabled:    webhooksEnabled(),
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
	// Reaction fields - populated when EventType is "reaction".
	ReactionToMessageID string  `json:"reactionToMessageId,omitempty"`
	ReactionEmoji       *string `json:"reactionEmoji,omitempty"`
	ReactionRemoved     *bool   `json:"reactionRemoved,omitempty"`
}

// webhooksEnabled reports whether webhook processing is enabled. Keep this
// separate from sendWebhookPayload so media callers can avoid their webhook-only
// file work when delivery is disabled.
// webhooksEnabled reads WEBHOOK_ENABLED; call it at startup only (newWebhookSender, main).
func webhooksEnabled() bool {
	return getEnvBool("WEBHOOK_ENABLED", true)
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

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(jsonData)) //nolint:gosec // WEBHOOK_URL is operator configuration, not request input
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

	resp, err := w.client.Do(req) //nolint:gosec // operator-configured destination, redirects disabled
	if err != nil {
		bridgeLog.Errorf("sending webhook: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 200 {
		bridgeLog.Debugf("✓ Webhook sent for message from %s", payload.Sender)
	} else {
		bridgeLog.Warnf("Webhook failed with status %d", resp.StatusCode)
	}
}

// SendWebhook sends a text-only message to the webhook endpoint. New callers
// should use SendWebhookWithMessageID so receiver-side idempotency can identify
// repeated WhatsApp events; this wrapper remains for compatibility.
func (w *webhookSender) SendWebhook(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string) {
	w.SendWebhookWithMessageID(sender, content, chatJID, isFromMe, quotedMessageId, quotedSender, quotedContent, quotedIsFromMe, mentionedJIDs, "")
}

// SendWebhookWithMessageID sends a text-only message and preserves the native
// WhatsApp message ID in the payload for downstream idempotency.
func (w *webhookSender) SendWebhookWithMessageID(sender, content, chatJID string, isFromMe bool, quotedMessageId, quotedSender, quotedContent string, quotedIsFromMe *bool, mentionedJIDs []string, messageID string) {
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
	})
}

// SendWebhookWithMedia sends a message to the webhook endpoint including base64-encoded
// image data read from localPath. If localPath is empty or unreadable the webhook is
// still sent – just without the MediaBase64 field so the text caption is not lost.
func (w *webhookSender) SendWebhookWithMedia(
	sender, content, chatJID string,
	isFromMe bool,
	quotedMessageId, quotedSender, quotedContent string,
	quotedIsFromMe *bool, mentionedJIDs []string,
	messageID, mediaType, mimeType, mediaFilename, localPath string,
) {
	if !w.enabled {
		return
	}

	var mediaBase64 string
	if localPath != "" {
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			bridgeLog.Warnf("Could not stat media file for base64 encoding: %v", statErr)
		} else if info.Size() > maxMediaBase64Bytes {
			bridgeLog.Warnf("Media file too large for base64 encoding (%d bytes), skipping MediaBase64", info.Size())
		} else if data, err := os.ReadFile(localPath); err == nil { //nolint:gosec // localPath comes from downloadMedia inside the store directory
			mediaBase64 = base64.StdEncoding.EncodeToString(data)
		} else {
			bridgeLog.Warnf("Could not read media file for base64 encoding: %v", err)
		}
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
	})
}

// SendReactionWebhook sends a typed reaction event to the webhook endpoint.
func (w *webhookSender) SendReactionWebhook(sender, chatJID string, isFromMe bool, messageID, reactionToMessageID, emoji string) {
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
		ReactionEmoji:       &emoji,
		ReactionRemoved:     &removed,
	})
}

// In main.go, handleMessage forwards webhooks for messages with text content.
// It will forward self-sent messages when the env var FORWARD_SELF=true.
