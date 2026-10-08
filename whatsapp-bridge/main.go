package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// CLI flag: request a full history sync at pair time.
// Only meaningful on a fresh pair (whatsapp.db deleted). See the usage block
// near NewClient for the full rationale and caveats.
var fullHistoryPairFlag = flag.Bool("full-history-pair", false,
	"Request full history at pair time (only effective when re-pairing; no-op for existing sessions)")

// printQRCode renders one pairing QR code to out. index is 1 for the first
// code of a pairing session; later codes are redraws after whatsmeow rotated
// the previous one, and are labelled so a reader of a scrolling log (e.g.
// `docker compose logs -f bridge`) knows the latest block is the live one.
func printQRCode(out io.Writer, code string, index int) {
	if index <= 1 {
		_, _ = fmt.Fprintln(out, "\nScan this QR code with your WhatsApp app:")
	} else {
		_, _ = fmt.Fprintf(out, "\nQR code refreshed (#%d) — the previous one has expired, scan this one:\n", index)
	}
	qrterminal.GenerateHalfBlock(code, qrterminal.L, out)
	_, _ = fmt.Fprintln(out, "\nWaiting for QR code scan... (a new code is printed each time WhatsApp rotates it)")
}

func webhookStartupMessage(switches bridgeSwitches) string {
	if !switches.WebhookEnabled {
		return "WEBHOOK_ENABLED=false: outbound webhooks disabled"
	}
	status := "; status updates are not forwarded (WEBHOOK_FORWARD_STATUS)"
	if switches.ForwardStatus {
		status = "; WEBHOOK_FORWARD_STATUS enabled: status updates are forwarded too"
	}
	status += fmt.Sprintf("; channels forwarded=%t (%s); broadcast lists forwarded=%t (%s)", switches.ForwardChannels, webhookForwardChannelsEnv, switches.ForwardBroadcasts, webhookForwardBroadcastsEnv)
	if switches.ForwardSelf {
		return "FORWARD_SELF enabled: forwarding self messages to webhook" + status
	}
	return "FORWARD_SELF disabled: self messages will NOT be forwarded" + status
}

// shutdownTimeout bounds the drain on SIGTERM; compose's stop_grace_period is 30s.
const shutdownTimeout = 10 * time.Second

func main() {
	flag.Parse()
	os.Exit(run())
}

// run validates startup before any filesystem or network effect. Logging is
// configured first because even refusal must retain the selected log format.
func run() int {
	_, _, _ = initLogging()
	cfg, err := loadBridgeConfig()
	if err != nil {
		bridgeLog.Errorf("Refusing to start: %s", err)
		return 1
	}
	return runBridge(cfg)
}

func runBridge(cfg bridgeConfig) int {

	// One level for the bridge and the whatsmeow client (WHATSAPP_LOG_LEVEL, default INFO).
	logger, clientLog, dbLog := newLoggerSet(cfg.LogLevel, cfg.JSONLogs)
	bridgeLog = logger
	logger.Infof("Starting WhatsApp client...")
	logger.Infof("%s", buildInfo().String())

	logger.Infof("%s", webhookStartupMessage(cfg.Switches))

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), storeDirMode); err != nil {
		logger.Errorf("Failed to create store directory %q: %v", storeDir(), err)
		return 1
	}
	if abs, err := filepath.Abs(storeDir()); err == nil {
		logger.Infof("Store directory: %s", abs)
	}

	// One handle on the store for its whole lifetime. Everything that walks,
	// measures, deletes, writes media or reads it back for the webhook goes
	// through this os.Root, which confines those operations to the directory at
	// the kernel level (store_dir.go).
	storeRoot, rootErr := openStoreRoot()
	if rootErr != nil {
		logger.Errorf("Failed to open store directory %q: %v", storeDir(), rootErr)
		return 1
	}
	defer func() { _ = storeRoot.Close() }()

	// Refuse to run alongside another bridge on the same store. Two processes
	// sharing one WhatsApp session evict each other forever (StreamReplaced)
	// and neither persists messages reliably. Must happen before the session
	// database is opened or WhatsApp is dialled. See instance_lock.go.
	lock, lockErr := acquireInstanceLock(instanceLockPath())
	if lockErr != nil {
		logger.Errorf("Refusing to start: %v", lockErr)
		logger.Errorf("Stop the other bridge (or point this one at a different store directory) and retry.")
		return 1
	}
	defer lock.Release()

	// Only filesystem operations remain: all environment values were checked.
	bridgeToken, fresh, tokErr := loadOrCreateBridgeToken()
	if tokErr != nil {
		logger.Errorf("Failed to initialize bridge token: %v", tokErr)
		return 1
	}
	if err := cfg.Operator.refuseBridgeToken(bridgeToken); err != nil {
		logger.Errorf("Refusing to start: %v", err)
		return 1
	}
	// Print the one-time setup banner immediately, before binding REST or attempting to
	// connect/pair. loadOrCreateBridgeToken() already persisted the token to
	// disk as soon as it generated one; if the banner instead waited until
	// after a successful connection (as it used to), a QR-pairing timeout or
	// early exit would leave a token on disk that was never shown to the
	// user — and loadOrCreateBridgeToken() would report fresh=false on every
	// later run, so the banner would never get a second chance to print it.
	if fresh {
		printTokenBanner(bridgeToken, cfg.Port)
	}

	// The session keys live here: owner-only before whatsmeow creates or opens it.
	privateDatabase(whatsmeowDBPath())
	sessionDB, err := openSessionDB()
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return 1
	}
	container := sqlstore.NewWithDB(sessionDB, "sqlite", dbLog)
	defer func() { _ = container.Close() }()
	if err = container.Upgrade(context.Background()); err != nil {
		logger.Errorf("Failed to upgrade the session database: %v", err)
		return 1
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return 1
		}
	}

	// Optionally request a full history sync at pair time.
	//
	// whatsmeow's default DeviceProps has RequireFullSync=false, which asks the
	// primary device for "recent" history only (typically ~3 months, decided by
	// the phone). Setting RequireFullSync=true with a large FullSyncDaysLimit
	// flips the handshake to request full-history mode. The phone still decides
	// the actual cap — iPad companion is documented at ~1 year max
	// (https://wabetainfo.com/...). Only meaningful at pair time: for an
	// already-paired session (whatsapp.db present), this is a no-op because no
	// new pair handshake fires.
	//
	// Enable by passing --full-history-pair on the command line BEFORE deleting
	// whatsapp.db and re-scanning the QR code. The flag defaults to false so
	// normal launchd-managed restarts don't accidentally trigger a huge sync.
	if *fullHistoryPairFlag {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
		logger.Infof("--full-history-pair enabled: requesting full history (days=3650, sizeMb=102400)")
	}

	// Set the linked-device label shown in WhatsApp's "Linked Devices" list.
	// whatsmeow's built-in default is the literal string "whatsmeow", which is
	// opaque to end users who then see an unfamiliar name attached to their
	// account. WHATSAPP_DEVICE_NAME lets an operator show a recognisable label
	// (e.g. a product or company name) instead. Empty/unset keeps the whatsmeow
	// default. This only takes effect at pair time — an already-paired session
	// (whatsapp.db present) keeps the name captured when the QR was scanned; to
	// change it, re-pair. The platform icon (DeviceProps.PlatformType) is left
	// at whatsmeow's default on purpose: this is a labelling convenience, not a
	// way to impersonate an official WhatsApp client.
	if name := cfg.DeviceName; name != "" {
		store.DeviceProps.Os = proto.String(name)
		logger.Infof("Linked-device name set to %q (WHATSAPP_DEVICE_NAME)", name)
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, sdkSafeLogger{clientLog})
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return 1
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return 1
	}
	defer func() { _ = messageStore.Close() }()

	if err := messageStore.MigrateLegacyLIDChatsToPhoneJIDs(whatsmeowDBPath(), logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID chat rows: %v", err)
		return 1
	}

	if err := messageStore.MigrateLegacyLIDSendersToPhones(whatsmeowDBPath(), logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID sender rows: %v", err)
		return 1
	}

	// Runs last: it classifies the senders the rewrite above could not turn
	// into phone numbers (sender_namespace.go).
	if err := messageStore.MigrateSenderNamespaces(whatsmeowDBPath(), logger); err != nil {
		logger.Errorf("Failed to backfill sender namespaces: %v", err)
		return 1
	}

	// Chats an older bridge named after our own number get their placeholder
	// back, so the normal resolution names them (chat_names.go, issue #448).
	// Names only: a failure is logged and the bridge starts anyway.
	if renamed, err := messageStore.ResetSelfNamedChats(ownUsers(client)); err != nil {
		logger.Warnf("%v", err)
	} else if renamed > 0 {
		logger.Infof("Reset %d chats that were named after our own number", renamed)
	}

	mediaRoots, err := resolveMediaRootsValue(cfg.MediaRoots, false)
	if err != nil {
		logger.Errorf("Failed to resolve media roots: %v", err)
		return 1
	}

	bridge := newBridge(client, messageStore, logger, bridgeToken, storeRoot, cfg.Switches)
	exitCtx, stopSignals := signal.NotifyContext(bridge.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	reconnectChan := make(chan bool, 1)
	bridge.bindRuntimeClient()
	bridge.installClient(client, client.Store.ID != nil, reconnectChan)
	defer func() { bridge.Disconnect() }()
	bridge.connectionProblem, err = readConnectionProblem(storeRoot)
	if err != nil {
		logger.Errorf("Refusing to connect with unreadable saved connection state: %v", err)
		return 1
	}
	bridge.RESTBind, bridge.RESTAllowedHosts = cfg.Bind, cfg.AllowedHosts
	bridge.MediaRetention, bridge.MediaAutoDownloadStatus = cfg.MediaRetention, cfg.StatusMedia
	bridge.GroupRosterSync, bridge.SessionKeepalive = cfg.RosterSync, cfg.SessionKeepalive
	bridge.ReadOnly, bridge.Tools = cfg.ReadOnly, cfg.Tools
	bridge.MediaMaxBytes, bridge.MediaRoots = cfg.MediaMaxBytes, mediaRoots
	defer bridge.Shutdown(shutdownTimeout)
	pairingOut := io.Writer(os.Stdout)
	if !cfg.PairingStdout {
		pairingOut = io.Discard
	}
	if cfg.Operator.Bind != "" {
		bridge.operatorPairing = newOperatorPairing(bridge.ctx, bridge, client, func() (operatorPairingClient, error) {
			// Only an explicitly unpaired restart reaches this factory. Deleted
			// SDK clients cannot be reused, and old QR contexts cannot disconnect
			// this new client's socket.
			freshClient := whatsmeow.NewClient(container.NewDevice(), clientLog)
			bridge.installClient(freshClient, false, reconnectChan)
			return freshClient, nil
		}, bridge.isPaired, bridge.Connected, pairingOut, reconnectChan)
		bridge.operatorServer, err = startOperatorServer(cfg.Operator, bridge.operatorPairing.routes(), logger)
		if err != nil {
			logger.Errorf("Failed to start operator listener: %v", err)
			return 1
		}
	}
	// Unrecoverable conditions (LoggedOut, ClientOutdated) end the process here so
	// the store is closed and the lock released before the supervisor restarts us.
	bridge.Exit = func(reason string, code int) {
		logger.Errorf("%s", reason)
		_ = messageStore.Close()
		lock.Release()
		os.Exit(code)
	}
	logger.Infof("Allowed media roots: %v", bridge.MediaRoots)

	// Serve the REST API before pairing/connecting: /api/health answers as soon
	// as the process is up (a container waiting for its QR scan is alive, not
	// broken), /api/ready reports the WhatsApp connection, and endpoints that
	// need WhatsApp check client.IsConnected() themselves.
	if err := bridge.startRESTServer(cfg.Port, bridgeToken); err != nil {
		logger.Errorf("Failed to start REST API: %v", err)
		return 1
	}
	logger.Infof("%s", bridge.Policy.Summary())
	logger.Infof("%s", bridge.ReadOnly.Summary())
	logger.Infof("%s", bridge.Tools.Summary())
	logger.Infof("Media auto-download: %v (status updates: %v); retention: %s", bridge.MediaAutoDownload,
		bridge.MediaAutoDownload && bridge.MediaAutoDownloadStatus, retentionSummary(bridge.MediaRetention))
	logger.Infof("Group roster sync: %s", groupRosterSyncSummary(bridge.GroupRosterSync))
	go bridge.runMediaRetention()
	logger.Infof("Session keepalive: %s", sessionKeepaliveSummary(bridge.SessionKeepalive))
	go bridge.runGroupRosterSync()
	bridge.startSessionKeepalive()

	if !bridge.isPaired() {
		bridge.notifyConnection("pairing_required", "unpaired", true, false)
	}
	if bridge.operatorPairing != nil {
		bridge.operatorPairing.start()
	}

	// Connect, or pair over QR on a fresh store. Each attempt has its own
	// deadline; a rotated-code timeout starts the next attempt at once
	// (pairing.go). The signal context aborts pairing while the bridge lifecycle
	// remains alive until Shutdown drains accepted REST requests.
	if bridge.operatorPairing == nil {
		if err := connectOrPair(exitCtx, client, bridge.isPaired(), pairingOptions{
			attempts:          3,
			attemptTimeout:    5 * time.Minute,
			retryDelay:        5 * time.Second,
			out:               pairingOut,
			log:               logger,
			beforeDial:        func() error { return bridge.waitConnectionAllowedContext(exitCtx) },
			connectionContext: bridge.ctx,
			state: func(state string) {
				bridge.setPairingState(state)
				if state == "" {
					bridge.notifyConnection("paired", "pairing_succeeded", true, false)
				}
			},
		}); err != nil {
			if errors.Is(err, context.Canceled) && exitCtx.Err() != nil {
				logger.Infof("Shutting down during connection startup")
				return 0
			}
			if !errors.Is(err, errPairingOperator) {
				logger.Errorf("%v", err)
				return 1
			}
			logger.Warnf("%v", err)
		}
	}

	// Authentication can still need the queued 515 handshake. Consume it in
	// the gated reconnect loop while REST reports readiness, rather than
	// exiting before the loop has had a chance to establish the session.
	if client.IsLoggedIn() {
		bridgeLog.Infof("Connected to WhatsApp! Type 'help' for commands.")
	}

	bridgeLog.Infof("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Start reconnection handler goroutine
	go bridge.reconnectLoop(reconnectChan)

	// Wait for termination signal
	<-exitCtx.Done()

	bridgeLog.Infof("Shutting down: draining REST, stopping loops, disconnecting...")
	return 0
}
