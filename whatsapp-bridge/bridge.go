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
	"context"
	"database/sql"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// mediaDownloader is downloadMedia's signature; tests substitute a fake.
type mediaDownloader func(ctx context.Context, messageID, chatJID string) (bool, string, string, string, error)

type Bridge struct {
	forceReconnect             atomic.Bool
	connectionEventsMu         sync.Mutex
	connectionEvents           sync.WaitGroup
	connectionEventsClosing    bool
	connectionDeliveryDone     <-chan struct{}
	connectionDisconnectCancel context.CancelFunc
	lastConnectionEvent        string
	ForwardConnection          bool
	ConnectionDebounce         time.Duration
	connectionEventWait        func(context.Context, time.Duration) bool
	connectionMu               sync.Mutex
	connectionProblem          *ConnectionProblem
	connectionChanged          chan struct{}
	connectionBlocked          func() // Test observation of a parked dial gate.
	pairingState               string
	problemPersistenceFailed   bool
	problemNow                 func() time.Time
	problemWait                func(time.Duration) error
	Client                     *whatsmeow.Client
	runtimeClient              atomic.Pointer[whatsmeow.Client]
	clientGate                 sync.RWMutex
	runtimePaired              atomic.Bool
	operatorPairing            *operatorPairing
	operatorServer             *http.Server
	sessionDB                  *sql.DB // Startup-owned session pool, observed without acquiring a connection.
	Store                      *MessageStore
	HistoryLimits              historyLimits
	historyProgress            historyProgress
	SnapshotDir                string
	Archive                    archiveConfig
	snapshotSpace              func(*os.File) (uint64, error)
	snapshotPrune              func(context.Context, *os.Root, *os.File, string, int) error
	exportBusy                 atomic.Bool
	snapshotBusy               atomic.Bool
	archiveSessionMu           sync.Mutex
	archiveSessionReaders      [2]*archiveSessionRead
	Log                        waLog.Logger

	// StoreRoot is the store directory opened as an os.Root (store_dir.go).
	// The retention sweep, the store-size measurement, /api/media/purge, the
	// inbound media download and the webhook's read of an image do every stat,
	// read, write and delete through it, so the kernel — not a filepath
	// comparison — keeps them inside the store. nil in tests that never touch
	// the store; those paths then report "unavailable" instead of guessing (a
	// webhook then goes out without its image).
	StoreRoot *os.Root

	// Policy restricts which chats outbound endpoints may act on (WHATSAPP_ALLOWED_CHATS).
	Policy chatPolicy
	// ReadOnly refuses every endpoint with a side effect (WHATSAPP_READ_ONLY,
	// read_only.go). Zero value = disabled; main() parses it and refuses to
	// start on a value it cannot read.
	ReadOnly readOnlyPolicy
	// Tools refuses the mutating endpoints whose MCP tools are not allowed
	// (WHATSAPP_ALLOW_TOOLS / WHATSAPP_DENY_TOOLS, tool_policy.go). Zero value =
	// unrestricted; main() parses it and refuses to start on an unknown name.
	Tools                 toolPolicy
	RuntimeDefaults       map[string]runtimeSetting
	sendMu                sync.Mutex
	sendNow               func() time.Time
	SendIncludeActions    bool
	MCPEnvHash            string
	settingsMu            sync.Mutex
	settingsWarnMu        sync.Mutex
	settingsWarned        map[string]int64
	operatorLogout        atomic.Bool
	operatorSessionWiped  atomic.Bool
	operatorRetiredClient atomic.Pointer[whatsmeow.Client]
	logoutClient          func(context.Context) error
	wipeSession           func(context.Context) error
	logoutDrainTimeout    time.Duration // Zero selects the production one-second bound.
	// PollVoteDecrypt decodes PollUpdateMessage payloads; nil = votes are skipped.
	PollVoteDecrypt pollVoteDecrypter
	// DownloadMedia fetches media for a stored message (defaults to downloadMedia).
	DownloadMedia mediaDownloader
	// ForwardSelf forwards self-sent messages to the webhook (FORWARD_SELF).
	ForwardSelf bool
	// ForwardStatus forwards status updates (status@broadcast) to the webhook
	// (WEBHOOK_FORWARD_STATUS). Zero value = the feed stays off the webhook,
	// which is for conversations (webhook.go: forwardsToWebhook).
	ForwardStatus bool
	// Channel posts and broadcast lists each require their own webhook opt-in.
	ForwardChannels   bool
	ForwardBroadcasts bool
	// MetricsEnabled serves GET /metrics (WHATSAPP_METRICS, metrics.go).
	MetricsEnabled bool
	// MediaAutoDownload caches inbound media and successfully sent files (WHATSAPP_MEDIA_AUTODOWNLOAD).
	MediaAutoDownload bool
	// MediaAutoDownloadStatus extends it to the status feed
	// (WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS, media_retention.go). Zero value =
	// status media is stored as a row and fetched only on demand; main() parses
	// it and refuses to start on a value it cannot read.
	MediaAutoDownloadStatus bool
	// MediaMaxBytes: files larger than this are not automatically cached
	// (WHATSAPP_MEDIA_MAX_BYTES); /api/download still fetches them on demand.
	MediaMaxBytes uint64
	// Webhook delivers inbound events to WEBHOOK_URL (nil = tests that never expect one).
	Webhook *webhookSender
	// Connect dials WhatsApp (defaults to Client.Connect); the reconnect loop uses it.
	Connect    func() error
	Disconnect func()
	// Connected reports whether the WhatsApp socket is up (defaults to Client.IsConnected);
	// handlers that need WhatsApp check it, tests override it.
	Connected func() bool
	// Send performs /api/send (defaults to sendWhatsAppMessage); tests inject a fake.
	Send sendFunc
	// Raw upload and send overrides feed the shared messageSendNetwork seam.
	uploadMedia outboundUploadFunc
	sendMessage outboundSendFunc
	// chatPresence records typing targets in tests; nil uses Client.SendChatPresence.
	chatPresence chatPresenceSender
	// LabelResync is the regular snapshot fetch; startLabelSync owns its gate.
	LabelResync        func(context.Context) error
	LabelResyncTimeout time.Duration
	labelSyncOnce      sync.Once
	labelSyncWait      sync.WaitGroup
	// SendAppState is the raw client call; sendAppState serializes writers.
	SendAppState appStateSendFunc
	appStateGate chan struct{}
	// IsOnWhatsApp asks WhatsApp which number a recipient is registered under
	// (defaults to Client.IsOnWhatsApp); /api/send and /api/forward ask it for a number the LID
	// map does not know, tests inject a fake.
	IsOnWhatsApp isOnWhatsAppFunc
	// recipientNumbers remembers positive typed -> registered answers, never permissions.
	recipientNumbers recipientNumberCache
	// Exit terminates the process for conditions the bridge cannot recover from in-place
	// (device logged out, client outdated); main() wires it to a clean os.Exit so the
	// supervisor restarts into the pairing path. Tests inject a recorder.
	Exit func(reason string, code int)
	// RESTBind is the REST listen address (WHATSAPP_BRIDGE_BIND, default 127.0.0.1).
	RESTBind string
	// RESTAllowedHosts is the raw WHATSAPP_BRIDGE_ALLOWED_HOSTS value (see rest_bind.go).
	RESTAllowedHosts string
	// MediaRoots are the directories /api/send may read outbound files from (WHATSAPP_MEDIA_ROOTS).
	MediaRoots []string
	// MediaRetention is the age after which cached media is swept (0 = keep forever).
	MediaRetention time.Duration
	// GroupRosterSync is how stale a cached group roster may get before the
	// background pass refreshes it (WHATSAPP_GROUP_ROSTER_SYNC_HOURS,
	// group_events.go). 0 disables the pass, and group_members then only grows
	// from /api/group/members, group events and group messages.
	GroupRosterSync time.Duration
	// SessionKeepalive is how often the device is briefly marked available so
	// WhatsApp counts it as in use (WHATSAPP_SESSION_KEEPALIVE_HOURS,
	// session_keepalive.go). 0 disables it, and WhatsApp then logs the device
	// out about a month after pairing.
	SessionKeepalive time.Duration
	// The waits of the keepalive loop (session_keepalive.go); a test shortens
	// them on its own Bridge.
	SessionKeepaliveSettle time.Duration
	SessionKeepalivePoll   time.Duration
	SessionKeepaliveRetry  time.Duration
	SessionPresenceHold    time.Duration
	// sessionPresence and sessionReady are test seams: the presence sender
	// (nil = the client) and "connected and logged in" (nil = the client).
	sessionPresence presenceSender
	sessionReady    func() bool
	// sessionNow is the wall clock for keepalive comparisons (nil = time.Now).
	sessionNow func() time.Time
	// keepaliveLoop is the keepalive's goroutine; Shutdown waits for it so a
	// blip in progress still ends with "unavailable".
	keepaliveLoop sync.WaitGroup
	// StreamReplacedDelay is how long the reconnect after a StreamReplaced event
	// waits, so this bridge does not ping-pong with the session that took its
	// slot (events.go). Set once at startup; tests shorten it on their own
	// Bridge instead of on a shared variable (issue #351).
	StreamReplacedDelay time.Duration
	// ReconnectInitialBackoff and ReconnectMaxBackoff bound the redial wait of
	// reconnectLoop (events.go): the first wait, doubled on every failure, capped
	// here, reset on success.
	ReconnectInitialBackoff time.Duration
	ReconnectMaxBackoff     time.Duration
	// HistoryVoteRetryDelays paces the retries of a history-sync poll vote whose
	// secret whatsmeow has not written yet, and its length is how many retries a
	// vote gets (polls.go) — empty or nil means none, and every vote that races
	// the secret is then recorded as undecodable. On the Bridge for the same
	// reason as the three timings above (issue #382): a test shortening a shared
	// variable races the goroutine reading it.
	HistoryVoteRetryDelays []time.Duration
	// StoreRetryDelays paces the retries of a live write that found the
	// database busy, and its length is how many retries it gets
	// (store_failures.go); empty means a busy write is lost at once.
	StoreRetryDelays []time.Duration
	// PurgeScanLimit bounds how many message rows one criteria purge examines
	// (media_purge.go); 0 means purgeMaxScan. On the Bridge so a test can shrink
	// it without a shared variable.
	PurgeScanLimit int

	// origTimes caches send-times of undecryptable first deliveries (see originalTimestamps).
	origTimes *originalTimestamps
	// mediaRetry routes MediaRetry events to waiting downloads (see mediaRetryHub).
	mediaRetry *mediaRetryHub
	// rosterFailures backs off groups whose roster refresh keeps failing (group_events.go).
	rosterFailures *rosterFailures
	// mediaTransfers keeps one transfer in flight per cached file, so callers
	// that miss the cache together share it (see media_inflight.go).
	mediaTransfers mediaTransferGroup
	// mediaTransfer streams one media file to disk (nil = downloadToPath);
	// tests inject a blocking fake (see Bridge.transferMedia).
	mediaTransfer mediaTransferFunc
	// mediaRetryDownload asks the sender's phone to re-upload a file and
	// downloads it (nil = downloadViaMediaRetry); tests inject a recorder
	// (see Bridge.retryMedia).
	mediaRetryDownload mediaRetryFunc
	// storeRetryWait waits before a store retry (nil = a timer that Shutdown
	// interrupts); tests use it to act between two attempts.
	storeRetryWait func(time.Duration) bool
	// autoDownloads is the bounded pool that caches inbound media; a full
	// queue drops the download instead of growing (see media_budget.go).
	autoDownloads *mediaJobQueue
	// startedAt feeds uptime_seconds in /api/health.
	startedAt time.Time
	// History vote batches form one FIFO chain outside the SDK callback.
	historyVotes  sync.WaitGroup
	historyVoteMu sync.Mutex
	// Vote decoding must drain for logout without blocking a client handoff.
	historyVoteSessionGate sync.RWMutex
	historyVoteTail        chan struct{}
	historyVoteStopped     bool
	// In-flight history attempts/retries, one latest candidate per tally key.
	historyVoteOrderMu  sync.Mutex
	historyPendingVotes map[historyVoteKey]*historyVoteWork
	// historyBatchWriter replaces the transaction runner in controlled tests.
	historyBatchWriter func(func(*messageBatch) error) error
	// Peer history has a separate one-worker, one-waiting-job budget.
	historyShareMu         sync.Mutex
	historyShares          *historyShareQueue
	historyShareStopped    bool
	historyShareJobTimeout time.Duration
	// nil uses the SDK; tests bypass paired media-connection discovery only.
	historyShareDownload func(context.Context, *waE2E.HistorySyncNotification, whatsmeow.File) error
	// httpServer is the REST listener, kept so Shutdown can drain it (rest.go).
	httpServer *http.Server
	// ctx is cancelled by Shutdown; long-lived goroutines (reconnect loop, retention
	// sweep, StreamReplaced timer) select on it instead of sleeping blindly.
	ctx    context.Context
	cancel context.CancelFunc
	// storeStats caches store/media sizes for /api/health (see media_retention.go).
	storeStats *storeStats
	// metrics feeds GET /metrics (metrics.go).
	metrics *metricsRegistry
}

// newBridge wires the production dependencies from a live client and store.
// bridgeToken is the REST bearer token, also attached to outbound webhooks;
// storeRoot is the open store directory (main() owns opening and closing it).
func newBridge(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger, bridgeToken string, storeRoot *os.Root, switches bridgeSwitches) *Bridge {
	client.EmitAppStateEventsOnFullSync = true
	b := &Bridge{
		Client:              client,
		Disconnect:          client.Disconnect,
		SendAppState:        client.SendAppState,
		appStateGate:        make(chan struct{}, 1),
		Store:               store,
		Log:                 logger,
		StoreRoot:           storeRoot,
		Policy:              loadChatPolicy(),
		PollVoteDecrypt:     whatsmeowPollVoteDecrypter(client),
		ForwardSelf:         switches.ForwardSelf,
		ForwardStatus:       switches.ForwardStatus,
		ForwardChannels:     switches.ForwardChannels,
		ForwardBroadcasts:   switches.ForwardBroadcasts,
		ForwardConnection:   switches.ForwardConnection,
		MetricsEnabled:      switches.Metrics,
		MediaAutoDownload:   switches.MediaAutoDownload,
		MediaMaxBytes:       defaultMediaMaxBytes, // main applies the validated configuration before events start
		Webhook:             newWebhookSender(bridgeToken, switches.WebhookEnabled),
		RESTBind:            defaultBridgeBind,
		GroupRosterSync:     groupRosterSyncInterval,
		SessionKeepalive:    sessionKeepaliveInterval,
		StreamReplacedDelay: defaultStreamReplacedDelay,

		ReconnectInitialBackoff: defaultReconnectInitialBackoff,
		ReconnectMaxBackoff:     defaultReconnectMaxBackoff,
		SessionKeepaliveSettle:  defaultSessionKeepaliveSettle,
		SessionKeepalivePoll:    defaultSessionKeepalivePoll,
		SessionKeepaliveRetry:   defaultSessionKeepaliveRetry,
		SessionPresenceHold:     defaultSessionPresenceHold,
		HistoryVoteRetryDelays:  defaultHistoryVoteRetryDelays(),
		StoreRetryDelays:        defaultStoreRetryDelays(),

		rosterFailures: newRosterFailures(),
		origTimes:      newOriginalTimestamps(),
		mediaRetry:     newMediaRetryHub(),
		startedAt:      time.Now(),
		storeStats:     newStoreStats(storeRoot),
		metrics:        newMetricsRegistry(),
	}
	b.LabelResyncTimeout = actionDeadline
	b.LabelResync = func(ctx context.Context) error {
		return b.currentClient().FetchAppState(ctx, appstate.WAPatchRegular, true, false)
	}
	b.Policy.warnInvalidEntries(logger)
	b.ctx, b.cancel = context.WithCancel(context.Background())
	if b.Webhook != nil {
		b.Webhook.failures = &b.metrics.webhookFailures
	}
	b.DownloadMedia = b.downloadMedia
	b.autoDownloads = newMediaJobQueue(b.ctx, autoDownloadWorkers, autoDownloadQueue, b.runAutoDownload)
	if b.MediaAutoDownload {
		logger.Infof("Automatic media downloads: %d at a time, up to %d queued; extra media is not cached on arrival and stays available through download_media",
			autoDownloadWorkers, autoDownloadQueue)
	}
	b.Connect = func() error { return client.ConnectContext(b.ctx) }
	b.Connected = func() bool { return b.Client != nil && b.Client.IsConnected() }
	b.Send = b.sendBackend()
	b.IsOnWhatsApp = client.IsOnWhatsApp
	b.chatPresence = client.SendChatPresence
	b.Exit = func(reason string, code int) {
		logger.Errorf("%s", reason)
		os.Exit(code)
	}
	return b
}

// sleep waits for d and reports false when the bridge is shutting down.
func (b *Bridge) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-b.ctx.Done():
		return false
	}
}

// Shutdown stops accepting REST requests, cancels background goroutines and
// waits (bounded by timeout) for in-flight work. Order matters: drain HTTP
// first so no handler touches the store after main closes it, then cancel
// the loops, then wait for history-vote decoding. Disconnecting the WhatsApp
// client and closing the store stay in main(), after this returns.
func (b *Bridge) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if b.operatorServer != nil {
		if err := b.operatorServer.Shutdown(ctx); err != nil {
			b.Log.Warnf("Operator server did not drain cleanly: %v", err)
		}
	}
	if b.httpServer != nil {
		if err := b.httpServer.Shutdown(ctx); err != nil {
			b.Log.Warnf("REST server did not drain cleanly: %v", err)
		}
	}
	b.labelSyncOnce.Do(func() {})
	b.cancel()
	b.historyVoteMu.Lock()
	b.historyVoteStopped = true
	b.historyVoteMu.Unlock()
	if b.operatorPairing != nil && b.operatorPairing.started.Load() {
		select {
		case <-b.operatorPairing.done:
		case <-ctx.Done():
			b.Log.Warnf("Pairing controller did not stop before shutdown deadline")
		}
	}
	b.stopConnectionEvents()
	// Seal submission before the bounded drain joins cancelled peer imports.
	b.historyShareMu.Lock()
	b.historyShareStopped = true
	shares := b.historyShares
	b.historyShareMu.Unlock()
	if shares != nil {
		shares.seal()
	}
	done := make(chan struct{})
	go func() {
		if shares != nil {
			shares.stop()
		}
		b.historyVotes.Wait()
		b.labelSyncWait.Wait()
		// Media transfers outlive the request that started them, so they are
		// waited on here too: the lifecycle context above already aborted them
		// and stopped the auto-download workers.
		if b.autoDownloads != nil {
			b.autoDownloads.wait()
		}
		b.mediaTransfers.wait()
		// A keepalive blip in progress finishes with "unavailable" before the
		// caller disconnects the client.
		b.keepaliveLoop.Wait()
		b.connectionEvents.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		b.Log.Warnf("Timed out waiting for shared history, history poll votes, media transfers and the session keepalive; exiting anyway")
	}
}
