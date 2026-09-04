package main

// Bridge bundles the runtime dependencies that the event loop and the REST
// API need, so they are passed explicitly instead of living in package-level
// variables. main() builds exactly one; tests build small ones with fakes.
//
// handleMessage, handleHistorySync and the REST mux are methods on Bridge and
// read policy, poll decrypter, media downloader and forward-self from it.
// Tests build one with testBridge() and override fields; nothing here is
// package state. Remaining package-level state (webhook token, original-
// timestamp registry) is tracked separately in issue #47.

import (
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// mediaDownloader is downloadMedia's signature; tests substitute a fake.
type mediaDownloader func(messageID, chatJID string) (bool, string, string, string, error)

type Bridge struct {
	Client *whatsmeow.Client
	Store  *MessageStore
	Log    waLog.Logger

	// Policy restricts which chats outbound endpoints may act on (WHATSAPP_ALLOWED_CHATS).
	Policy chatPolicy
	// PollVoteDecrypt decodes PollUpdateMessage payloads; nil = votes are skipped.
	PollVoteDecrypt pollVoteDecrypter
	// DownloadMedia fetches media for a stored message (defaults to downloadMedia).
	DownloadMedia mediaDownloader
	// ForwardSelf forwards self-sent messages to the webhook (FORWARD_SELF).
	ForwardSelf bool
	// MediaAutoDownload caches inbound media as it arrives (WHATSAPP_MEDIA_AUTODOWNLOAD).
	MediaAutoDownload bool
	// Webhook delivers inbound events to WEBHOOK_URL (nil = tests that never expect one).
	Webhook *webhookSender
	// Exit terminates the process for conditions the bridge cannot recover from in-place
	// (device logged out, client outdated); main() wires it to a clean os.Exit so the
	// supervisor restarts into the pairing path. Tests inject a recorder.
	Exit func(reason string, code int)
	// RESTBind is the REST listen address (WHATSAPP_BRIDGE_BIND, default 127.0.0.1).
	RESTBind string
	// RESTAllowedHosts is the raw WHATSAPP_BRIDGE_ALLOWED_HOSTS value (see rest_bind.go).
	RESTAllowedHosts string

	// origTimes caches send-times of undecryptable first deliveries (see originalTimestamps).
	origTimes *originalTimestamps
	// mediaRetry routes MediaRetry events to waiting downloads (see mediaRetryHub).
	mediaRetry *mediaRetryHub
	// startedAt feeds uptime_seconds in /api/health.
	startedAt time.Time
	// historyVotes tracks background decoding of history-sync poll votes (polls.go).
	historyVotes sync.WaitGroup
	// storeStats caches store/media sizes for /api/health (see media_retention.go).
	storeStats *storeStats
}

// newBridge wires the production dependencies from a live client and store.
// bridgeToken is the REST bearer token, also attached to outbound webhooks.
func newBridge(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger, bridgeToken string) *Bridge {
	b := &Bridge{
		Client:            client,
		Store:             store,
		Log:               logger,
		Policy:            loadChatPolicy(),
		PollVoteDecrypt:   whatsmeowPollVoteDecrypter(client),
		ForwardSelf:       getEnvBool("FORWARD_SELF", true),
		MediaAutoDownload: getEnvBool(mediaAutoDownloadEnv, true),
		Webhook:           newWebhookSender(bridgeToken),
		RESTBind:          defaultBridgeBind,
		origTimes:         newOriginalTimestamps(),
		mediaRetry:        newMediaRetryHub(),
		startedAt:         time.Now(),
		storeStats:        newStoreStats(storeDir()),
	}
	b.DownloadMedia = b.downloadMedia
	b.Exit = func(reason string, code int) {
		logger.Errorf("%s", reason)
		os.Exit(code)
	}
	return b
}
