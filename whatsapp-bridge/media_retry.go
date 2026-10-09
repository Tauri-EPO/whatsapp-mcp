package main

// Media retry for expired CDN downloads.
//
// WhatsApp media URLs carry short-lived CDN auth tokens (oh=/oe=). Media that
// arrived via history sync, forwards, or that simply sat unread for a few
// days often 403/404/410s by the time an MCP client asks for it. WhatsApp's
// media-retry protocol asks the *sender's phone* to re-upload the file and
// hands back a fresh direct path. whatsmeow exposes the two halves
// (Client.SendMediaRetryReceipt + events.MediaRetry); this file glues them
// into downloadMedia so a stale download is retried exactly once.
//
// The phone that owns the media must be online for the retry to succeed,
// so every wait is bounded by mediaRetryTimeout and a failure is reported
// back to the caller as a normal download error.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// mediaRetryTimeout bounds how long a /api/download call waits for the
// sender's phone to answer a retry request. The REST server's WriteTimeout is
// 60s, so this must leave room for the follow-up download.
const mediaRetryTimeout = 30 * time.Second

// mediaCDNHost is the host whatsmeow itself prefers when it rebuilds a media
// URL from a direct path. Used to persist refreshed paths in the `url` column,
// which extractDirectPathFromURL later splits on ".net/".
const mediaCDNHost = "https://mmg.whatsapp.net"

// mediaRetryHub routes the phone's MediaRetry responses to the downloadMedia
// call waiting for them, keyed by message ID. One instance lives on Bridge.
type mediaRetryHub struct {
	mu      sync.Mutex
	waiters map[string]chan *events.MediaRetry
}

func newMediaRetryHub() *mediaRetryHub {
	return &mediaRetryHub{waiters: map[string]chan *events.MediaRetry{}}
}

// register creates the channel a MediaRetry event for messageID will be
// delivered to. The returned cancel func must be called once the caller stops
// waiting. A second registration for the same ID replaces the first (the
// earlier waiter simply times out).
func (h *mediaRetryHub) register(messageID string) (<-chan *events.MediaRetry, func()) {
	ch := make(chan *events.MediaRetry, 1)
	h.mu.Lock()
	h.waiters[messageID] = ch
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if cur, ok := h.waiters[messageID]; ok && cur == ch {
			delete(h.waiters, messageID)
		}
		h.mu.Unlock()
	}
}

// dispatch hands a MediaRetry event to the waiter registered for its message
// ID. Returns false when nobody is waiting (e.g. the request timed out
// already, or the retry was triggered by another linked device).
func (h *mediaRetryHub) dispatch(evt *events.MediaRetry) bool {
	if h == nil || evt == nil {
		return false
	}
	h.mu.Lock()
	ch, ok := h.waiters[string(evt.MessageID)]
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- evt:
		return true
	default:
		return false
	}
}

// definitiveRetryResult reports whether a MediaRetryNotification result means
// "never". Only NOT_FOUND does: the phone looked and no longer has the file.
// GENERAL_ERROR is the catch-all (and the zero value of the enum), and
// DECRYPTION_ERROR can be our side of the exchange — a stored media key that is
// wrong, or a receipt this bridge built badly — so a regression here must not
// mark an archive permanently gone. Both stay retryable.
func definitiveRetryResult(res waMmsRetry.MediaRetryNotification_ResultType) bool {
	return res == waMmsRetry.MediaRetryNotification_NOT_FOUND
}

// cdnRefusalStatus returns the HTTP status of a whatsmeow download error that
// is the CDN turning the request down (403, 404, 410), or 0 for anything else:
// network errors, hash mismatches and the rest are left alone. For a message
// old enough the refusal means its link expired and a media retry could help;
// for a recent one it cannot mean that (cdnFreshWindow).
func cdnRefusalStatus(err error) int {
	switch {
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403):
		return http.StatusForbidden
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404):
		return http.StatusNotFound
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return http.StatusGone
	}
	return 0
}

// cdnFreshWindow is how young a message has to be for a CDN refusal not to be
// an expired link. WhatsApp's CDN serves a file for days before its link goes
// stale, so six hours is far inside that lifetime and still covers a file that
// arrives and is read in the same sitting. Inside the window the refusal is
// reported as it is, a failure of the request worth another try; past it the
// bridge cannot tell an expiry from a bad request and keeps asking the sender's
// phone, as before (issue #452).
//
// The age is the message's, not the upload's: a recent message can carry an
// old link (a client that forwards without re-uploading). linkExpired catches
// the one case that can be read off the path itself.
const cdnFreshWindow = 6 * time.Hour

// linkExpiry reads the expiry stamp of a CDN path, its `oe` parameter, a
// hexadecimal Unix time. ok is false when the path has none that parses. That
// `oe` is the CDN's expiry is an observation, not a documented contract, so it
// is only ever used to send a refused download to the media retry sooner,
// which is what every refusal did before the fresh window existed.
func linkExpiry(directPath string) (expiry time.Time, ok bool) {
	_, query, _ := strings.Cut(directPath, "?")
	for _, pair := range strings.Split(query, "&") {
		if value, found := strings.CutPrefix(pair, "oe="); found {
			if secs, err := strconv.ParseInt(value, 16, 64); err == nil && secs > 0 {
				return time.Unix(secs, 0), true
			}
		}
	}
	return time.Time{}, false
}

// linkExpired reports a path whose own stamp says it expired before now.
func linkExpired(directPath string, now time.Time) bool {
	expiry, ok := linkExpiry(directPath)
	return ok && expiry.Before(now)
}

// cdnRefusedError is the CDN refusing the media of a message inside
// cdnFreshWindow. It is not errMediaUnavailable: nothing says the file is gone.
type cdnRefusedError struct {
	status int
	age    time.Duration
}

func (e *cdnRefusedError) Error() string {
	return fmt.Sprintf("the WhatsApp CDN refused the request (HTTP %d) for a message only %s old; "+
		"its link cannot have expired yet, so this is not a lost file: try again later",
		e.status, e.age.Round(time.Second))
}

// Where the direct path of a download came from, for the log.
const (
	pathFromMessage = "the message's direct path"
	pathFromURL     = "the path cut out of the stored url"
)

// mediaRequestShape describes a refused download for the log without any of
// its secrets: where the path came from, its first segment and how deep it is,
// the names (never the values) of its query parameters, whether its expiry
// stamp has passed, the host of the stored url, whether that url names the
// same object, and the sizes of the key and the hashes. The file name, the
// tokens and the hashes themselves stay out, and so does anything the sender
// could have made long or misleading.
func mediaRequestShape(storedURL, directPath, source string, mediaKey, fileSHA256, fileEncSHA256 []byte) string {
	pathPart, query, hasQuery := strings.Cut(directPath, "?")
	segments := strings.Split(strings.Trim(pathPart, "/"), "/")
	first := "…"
	if len(segments) > 1 && isShapeToken(segments[0]) {
		first = segments[0]
	}
	params := "no query"
	if hasQuery {
		var names []string
		for _, pair := range strings.Split(query, "&") {
			name, _, isPair := strings.Cut(pair, "=")
			if !isPair || !isShapeToken(name) {
				name = "?"
			}
			names = append(names, name)
		}
		params = "query " + strings.Join(names, ",")
	}
	stamp := "no expiry stamp"
	if expiry, ok := linkExpiry(directPath); ok {
		stamp = "expiry stamp in the future"
		if expiry.Before(time.Now()) {
			stamp = "expiry stamp in the past"
		}
	}
	host, sameAsURL := "none", "no url stored"
	if storedURL != "" {
		host = "not a plain host name"
		if u, err := neturl.Parse(storedURL); err == nil && isHostShape(u.Host) {
			host = u.Host
		}
		// The official clients append a parameter of their own to the url
		// (mms3), so the object is compared apart from the parameters.
		urlPath := extractDirectPathFromURL(storedURL)
		object := func(p string) string { before, _, _ := strings.Cut(p, "?"); return before }
		switch {
		case urlPath == directPath:
			sameAsURL = "same as the url's path"
		case object(urlPath) == object(directPath):
			sameAsURL = "same object as the url's path, other parameters"
		default:
			sameAsURL = "another object than the url's path"
		}
	}
	return fmt.Sprintf("asked for %s: /%s/ (%d segments, %s, %s), %s; url host %s; key %d, sha256 %d, enc sha256 %d bytes",
		source, first, len(segments), params, stamp, sameAsURL, host, len(mediaKey), len(fileSHA256), len(fileEncSHA256))
}

// isHostShape reports a host that looks like a DNS name and is short enough
// to print: the url is the sender's, and so is whatever it puts there.
func isHostShape(host string) bool {
	if host == "" || len(host) > 64 {
		return false
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

// isShapeToken reports a path segment or parameter name short and plain
// enough to be structure (v, o1, ccb, oh, _nc_sid) rather than a secret.
func isShapeToken(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// mediaRetryFunc is downloadViaMediaRetry bound to a bridge; tests inject a fake.
type mediaRetryFunc func(ctx context.Context, messageID, chatJID string, downloader *MediaDownloader, root *os.Root, relPath string) (int64, error)

// mediaURLFromDirectPath turns a direct path into the URL form stored in
// messages.url. A download asks for messages.direct_path, which a media retry
// refreshes as well (storeRefreshedMedia); the url is what a row without that
// column falls back on (extractDirectPathFromURL), so the two are kept in step.
func mediaURLFromDirectPath(directPath string) string {
	if !strings.HasPrefix(directPath, "/") {
		directPath = "/" + directPath
	}
	return mediaCDNHost + directPath
}

// mediaRetryMessageInfo rebuilds the MessageInfo SendMediaRetryReceipt needs
// from what messages.db stores. `sender` is the bare user part the bridge
// persists (phone or LID digits); a full JID is accepted too.
func mediaRetryMessageInfo(messageID, chatJID, sender string, isFromMe bool) (*types.MessageInfo, error) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, fmt.Errorf("invalid chat JID %q: %w", chatJID, err)
	}
	var senderJID types.JID
	if sender != "" {
		senderJID, err = parseRecipientJID(sender)
		if err != nil {
			return nil, fmt.Errorf("invalid sender JID %q: %w", sender, err)
		}
	} else {
		senderJID = chat
	}
	return &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   senderJID,
			IsFromMe: isFromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID: messageID,
	}, nil
}

// mediaRetryDirectPath decodes the phone's answer and returns the fresh direct
// path, or an error describing why the media can't be recovered.
func mediaRetryDirectPath(evt *events.MediaRetry, mediaKey []byte) (string, error) {
	// An error node has no authenticated ciphertext. In particular the SDK's
	// ErrMediaNotAvailableOnPhone may describe a mismatched legacy receipt;
	// keep it retryable. Only the decrypted NOT_FOUND above proves absence.
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
	if err != nil {
		return "", err
	}
	if res := notif.GetResult(); res != waMmsRetry.MediaRetryNotification_SUCCESS {
		if definitiveRetryResult(res) {
			return "", fmt.Errorf("sender's phone declined media retry: %s: %w", res.String(), errMediaUnavailable)
		}
		return "", fmt.Errorf("sender's phone declined media retry: %s", res.String())
	}
	if notif.GetDirectPath() == "" {
		return "", errors.New("media retry response carried no direct path")
	}
	return notif.GetDirectPath(), nil
}

// storeRefreshedMedia persists what a media retry handed back, so the next
// download skips the retry: the url form of the fresh path, and the path
// itself in place of the message's own one, which has just expired.
func storeRefreshedMedia(messageStore *MessageStore, messageID, chatJID string, refreshed *MediaDownloader) {
	if err := messageStore.StoreMediaInfo(messageID, chatJID, refreshed.URL, refreshed.MediaKey, refreshed.FileSHA256, refreshed.FileEncSHA256, refreshed.FileLength); err != nil {
		bridgeLog.Warnf("Media retry succeeded but failed to persist refreshed URL for %s: %v", messageID, err)
	}
	if err := messageStore.SetDirectPath(messageID, chatJID, refreshed.DirectPath); err != nil {
		bridgeLog.Warnf("Media retry succeeded but failed to persist the fresh direct path for %s: %v", messageID, err)
	}
}

// downloadViaMediaRetry asks the sender's phone to re-upload the media behind
// (messageID, chatJID) and downloads it from the refreshed direct path. On
// success the new URL is persisted so the next download skips the retry.
// relPath is the destination relative to root, the store root.
func downloadViaMediaRetry(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, hub *mediaRetryHub, messageID, chatJID string, downloader *MediaDownloader, root *os.Root, relPath string) (int64, error) {
	info, err := messageStore.mediaRetryInfo(ctx, messageID, chatJID)
	if err != nil {
		return 0, err
	}

	// Register before sending so a fast phone can't answer into the void.
	if hub == nil {
		return 0, errors.New("media retry unavailable: no retry hub configured")
	}
	ch, cancel := hub.register(messageID)
	defer cancel()

	if err := client.SendMediaRetryReceipt(ctx, info, downloader.MediaKey); err != nil {
		return 0, fmt.Errorf("send media retry receipt: %w", err)
	}

	timer := time.NewTimer(mediaRetryTimeout)
	defer timer.Stop()
	select {
	case evt := <-ch:
		directPath, err := mediaRetryDirectPath(evt, downloader.MediaKey)
		if err != nil {
			return 0, err
		}
		refreshed := *downloader
		refreshed.DirectPath = directPath
		refreshed.URL = mediaURLFromDirectPath(directPath)
		written, err := downloadToPath(ctx, root, client, &refreshed, relPath)
		if err != nil {
			return 0, fmt.Errorf("download after media retry: %w", err)
		}
		storeRefreshedMedia(messageStore, messageID, chatJID, &refreshed)
		return written, nil
	case <-timer.C:
		return 0, fmt.Errorf("timed out after %s waiting for the sender's phone to re-upload the media (it must be online)", mediaRetryTimeout)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// The pinned SDK puts Chat in rmr.jid and Sender in rmr.participant for
// groups. New rows retain the delivery identities; legacy rows can only use
// their archived chat and their recorded sender namespace, without guessing
// a phone namespace for LID digits.
func (store *MessageStore) mediaRetryInfo(ctx context.Context, id, chat string) (*types.MessageInfo, error) {
	var sender, server, wireChat, wireSender string
	var own bool
	err := store.db.QueryRowContext(ctx, `SELECT sender, COALESCE(sender_server,''), is_from_me,
		COALESCE(media_retry_chat,''), COALESCE(media_retry_sender,'') FROM messages WHERE id=? AND chat_jid=?`, id, chat).
		Scan(&sender, &server, &own, &wireChat, &wireSender)
	if err != nil {
		return nil, fmt.Errorf("look up message for media retry: %w", err)
	}
	if wireChat != "" {
		chat = wireChat
	}
	if wireSender != "" {
		sender = wireSender
	} else if server != "" {
		sender = types.NewJID(sender, server).String()
	}
	return mediaRetryMessageInfo(id, chat, sender, own)
}
