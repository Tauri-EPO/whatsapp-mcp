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

// Pending retry requests keyed by message ID. The event loop routes the
// phone's MediaRetry response to whichever downloadMedia call is waiting.
var (
	mediaRetryWaiters = map[string]chan *events.MediaRetry{}
	mediaRetryMu      sync.Mutex
)

// registerMediaRetryWaiter creates the channel a MediaRetry event for
// messageID will be delivered to. The returned cancel func must be called
// once the caller stops waiting. A second registration for the same ID
// replaces the first (the earlier waiter simply times out).
func registerMediaRetryWaiter(messageID string) (<-chan *events.MediaRetry, func()) {
	ch := make(chan *events.MediaRetry, 1)
	mediaRetryMu.Lock()
	mediaRetryWaiters[messageID] = ch
	mediaRetryMu.Unlock()
	return ch, func() {
		mediaRetryMu.Lock()
		if cur, ok := mediaRetryWaiters[messageID]; ok && cur == ch {
			delete(mediaRetryWaiters, messageID)
		}
		mediaRetryMu.Unlock()
	}
}

// dispatchMediaRetry hands a MediaRetry event to the waiter registered for its
// message ID. Returns false when nobody is waiting (e.g. the request timed
// out already, or the retry was triggered by another linked device).
func dispatchMediaRetry(evt *events.MediaRetry) bool {
	if evt == nil {
		return false
	}
	mediaRetryMu.Lock()
	ch, ok := mediaRetryWaiters[string(evt.MessageID)]
	mediaRetryMu.Unlock()
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

// isExpiredMediaError reports whether a whatsmeow download error means the CDN
// no longer serves the stored URL, i.e. a media retry could help. Network
// errors, hash mismatches and everything else are left alone.
func isExpiredMediaError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// mediaURLFromDirectPath turns a fresh direct path from a retry response into
// the URL form stored in messages.url, so later downloads (and
// extractDirectPathFromURL) keep working without special cases.
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
	switch {
	case strings.Contains(sender, "@"):
		senderJID, err = types.ParseJID(sender)
		if err != nil {
			return nil, fmt.Errorf("invalid sender JID %q: %w", sender, err)
		}
	case sender != "":
		senderJID = types.NewJID(sender, types.DefaultUserServer)
	default:
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
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
	if err != nil {
		return "", err
	}
	if res := notif.GetResult(); res != waMmsRetry.MediaRetryNotification_SUCCESS {
		return "", fmt.Errorf("sender's phone declined media retry: %s", res.String())
	}
	if notif.GetDirectPath() == "" {
		return "", errors.New("media retry response carried no direct path")
	}
	return notif.GetDirectPath(), nil
}

// downloadViaMediaRetry asks the sender's phone to re-upload the media behind
// (messageID, chatJID) and downloads it from the refreshed direct path. On
// success the new URL is persisted so the next download skips the retry.
func downloadViaMediaRetry(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string, downloader *MediaDownloader) ([]byte, error) {
	var sender string
	var isFromMe bool
	if err := messageStore.db.QueryRow(
		"SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&sender, &isFromMe); err != nil {
		return nil, fmt.Errorf("look up message for media retry: %w", err)
	}
	info, err := mediaRetryMessageInfo(messageID, chatJID, sender, isFromMe)
	if err != nil {
		return nil, err
	}

	// Register before sending so a fast phone can't answer into the void.
	ch, cancel := registerMediaRetryWaiter(messageID)
	defer cancel()

	if err := client.SendMediaRetryReceipt(ctx, info, downloader.MediaKey); err != nil {
		return nil, fmt.Errorf("send media retry receipt: %w", err)
	}

	timer := time.NewTimer(mediaRetryTimeout)
	defer timer.Stop()
	select {
	case evt := <-ch:
		directPath, err := mediaRetryDirectPath(evt, downloader.MediaKey)
		if err != nil {
			return nil, err
		}
		refreshed := *downloader
		refreshed.DirectPath = directPath
		refreshed.URL = mediaURLFromDirectPath(directPath)
		data, err := client.Download(ctx, &refreshed)
		if err != nil {
			return nil, fmt.Errorf("download after media retry: %w", err)
		}
		if err := messageStore.StoreMediaInfo(messageID, chatJID, refreshed.URL, refreshed.MediaKey, refreshed.FileSHA256, refreshed.FileEncSHA256, refreshed.FileLength); err != nil {
			fmt.Printf("⚠️  Media retry succeeded but failed to persist refreshed URL for %s: %v\n", messageID, err)
		}
		return data, nil
	case <-timer.C:
		return nil, fmt.Errorf("timed out after %s waiting for the sender's phone to re-upload the media (it must be online)", mediaRetryTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
