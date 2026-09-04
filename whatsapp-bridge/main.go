package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"google.golang.org/protobuf/proto"
)

// Whether to forward messages sent by self via webhook.
// Defaults to true. Override with env FORWARD_SELF=false.

// CLI flag: request a full history sync at pair time.
// Only meaningful on a fresh pair (whatsapp.db deleted). See the usage block
// near NewClient for the full rationale and caveats.
var fullHistoryPairFlag = flag.Bool("full-history-pair", false,
	"Request full history at pair time (only effective when re-pairing; no-op for existing sessions)")

// getEnvBool reads a boolean env var with a default.
// Accepts: 1/true/yes/on and 0/false/no/off (case-insensitive)
func getEnvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// resolveDeviceName returns the operator-configured linked-device label from
// WHATSAPP_DEVICE_NAME, trimmed of surrounding whitespace. An empty or unset
// value returns "", which callers treat as "keep the whatsmeow default".
func resolveDeviceName() string {
	return strings.TrimSpace(os.Getenv("WHATSAPP_DEVICE_NAME"))
}

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

func webhookStartupMessage(forwardSelf bool) string {
	if !webhooksEnabled() {
		return "WEBHOOK_ENABLED=false: outbound webhooks disabled"
	}
	if forwardSelf {
		return "FORWARD_SELF enabled: forwarding self messages to webhook"
	}
	return "FORWARD_SELF disabled: self messages will NOT be forwarded"
}

func main() {
	flag.Parse()

	// One level for the bridge and the whatsmeow client (WHATSAPP_LOG_LEVEL, default INFO).
	logger, clientLog, dbLog := initLogging()
	logger.Infof("Starting WhatsApp client...")
	logger.Infof("%s", buildInfo(false).String())

	logger.Infof("%s", webhookStartupMessage(getEnvBool("FORWARD_SELF", true)))

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), 0o750); err != nil {
		logger.Errorf("Failed to create store directory %q: %v", storeDir(), err)
		return
	}
	if abs, err := filepath.Abs(storeDir()); err == nil {
		logger.Infof("Store directory: %s", abs)
	}

	// Refuse to run alongside another bridge on the same store. Two processes
	// sharing one WhatsApp session evict each other forever (StreamReplaced)
	// and neither persists messages reliably. Must happen before the session
	// database is opened or WhatsApp is dialled. See instance_lock.go.
	lock, lockErr := acquireInstanceLock(instanceLockPath())
	if lockErr != nil {
		logger.Errorf("Refusing to start: %v", lockErr)
		logger.Errorf("Stop the other bridge (or point this one at a different store directory) and retry.")
		os.Exit(1)
	}
	defer lock.Release()

	container, err := sqlstore.New(context.Background(), "sqlite3", sqliteURI(whatsmeowDBPath(), "_foreign_keys=on"), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
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
			return
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
	if name := resolveDeviceName(); name != "" {
		store.DeviceProps.Os = proto.String(name)
		logger.Infof("Linked-device name set to %q (WHATSAPP_DEVICE_NAME)", name)
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, clientLog)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	messageStore.groupInfo = client.GetGroupInfo
	defer func() { _ = messageStore.Close() }()

	if err := messageStore.MigrateLegacyLIDChatsToPhoneJIDs(whatsmeowDBPath(), logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID chat rows: %v", err)
		return
	}

	if err := messageStore.MigrateLegacyLIDSendersToPhones(whatsmeowDBPath(), logger); err != nil {
		logger.Errorf("Failed to migrate legacy LID sender rows: %v", err)
		return
	}

	// Resolve the REST API port. Pure env parsing with no dependency on the
	// WhatsApp connection, so it's safe to do this early alongside the token
	// load below — and failing fast here means we don't run a QR-pairing
	// flow only to error out on an invalid port afterwards.
	port := 8080
	if p := os.Getenv("WHATSAPP_BRIDGE_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			logger.Errorf("Invalid WHATSAPP_BRIDGE_PORT=%q, must be 1-65535", p)
			return
		}
		port = v
	}

	restBind, restAllowedHosts, bindErr := loadRESTBindConfig()
	if bindErr != nil {
		logger.Errorf("%v", bindErr)
		return
	}

	// Load (or generate on first run) the bearer token used to authenticate
	// REST callers; the Bridge attaches it to outbound webhook POSTs too.
	bridgeToken, fresh, tokErr := loadOrCreateBridgeToken()
	if tokErr != nil {
		logger.Errorf("Failed to initialize bridge token: %v", tokErr)
		return
	}

	mediaRetention, retErr := resolveMediaRetention(os.Getenv(mediaRetentionEnv))
	if retErr != nil {
		logger.Errorf("%v", retErr)
		return
	}

	bridge := newBridge(client, messageStore, logger, bridgeToken)
	// Unrecoverable conditions (LoggedOut, ClientOutdated) end the process here so
	// the store is closed and the lock released before the supervisor restarts us.
	bridge.Exit = func(reason string, code int) {
		logger.Errorf("%s", reason)
		_ = messageStore.Close()
		lock.Release()
		os.Exit(code)
	}
	bridge.RESTBind, bridge.RESTAllowedHosts = restBind, restAllowedHosts

	// Resolve the allow-listed roots that media_path values in /api/send must
	// live under. See media_path.go for the rationale.
	allowedMediaRoots, mrErr := resolveMediaRoots()
	if mrErr != nil {
		logger.Errorf("Failed to resolve media roots: %v", mrErr)
		return
	}
	logger.Infof("Allowed media roots: %v", allowedMediaRoots)

	// Serve the REST API before pairing/connecting: /api/health answers as soon
	// as the process is up (a container waiting for its QR scan is alive, not
	// broken), /api/ready reports the WhatsApp connection, and endpoints that
	// need WhatsApp check client.IsConnected() themselves.
	bridge.startRESTServer(port, bridgeToken, allowedMediaRoots)
	logger.Infof("%s", bridge.Policy.Summary())
	logger.Infof("Media auto-download: %v; retention: %s", bridge.MediaAutoDownload, retentionSummary(mediaRetention))
	retentionStop := make(chan struct{})
	go bridge.runMediaRetention(mediaRetention, retentionStop)

	// Print the one-time setup banner immediately, before attempting to
	// connect/pair. loadOrCreateBridgeToken() already persisted the token to
	// disk as soon as it generated one; if the banner instead waited until
	// after a successful connection (as it used to), a QR-pairing timeout or
	// early exit would leave a token on disk that was never shown to the
	// user — and loadOrCreateBridgeToken() would report fresh=false on every
	// later run, so the banner would never get a second chance to print it.
	if fresh {
		printTokenBanner(bridgeToken, port)
	}

	// Channel to signal reconnection needs
	reconnectChan := make(chan bool, 1)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) { bridge.handleEvent(evt, reconnectChan) })

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Add connection retry logic
	maxRetries := 3
	var connErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		logger.Infof("Connection attempt %d/%d...", attempt, maxRetries)

		// Connect to WhatsApp
		if client.Store.ID == nil {
			// No ID stored, this is a new client, need to pair with phone
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			qrChan, connErr := client.GetQRChannel(ctx)
			if connErr != nil {
				logger.Errorf("Failed to get QR channel: %v", connErr)
				if attempt == maxRetries {
					return
				}
				time.Sleep(5 * time.Second)
				continue
			}

			connErr = client.Connect()
			if connErr != nil {
				logger.Errorf("Failed to connect (attempt %d): %v", attempt, connErr)
				if attempt == maxRetries {
					return
				}
				time.Sleep(5 * time.Second)
				continue
			}

			// Print the QR code for pairing with the phone. whatsmeow rotates the
			// code roughly every 20 seconds and a scan of an expired one is
			// rejected by the phone, so every "code" event must be redrawn — not
			// just the first (see #14).
			qrCodesShown := 0
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					qrCodesShown++
					printQRCode(os.Stdout, evt.Code, qrCodesShown)
				case "success":
					connected <- true
				case "timeout":
					logger.Warnf("QR pairing timed out after %d code(s)", qrCodesShown)
				default:
					if evt.Error != nil {
						logger.Errorf("QR pairing error (%s): %v", evt.Event, evt.Error)
					} else {
						logger.Warnf("QR pairing event: %s", evt.Event)
					}
				}
				if evt.Event == "success" || evt.Event == "timeout" || evt.Error != nil {
					break
				}
			}
			// Wait for connection with timeout
			select {
			case <-connected:
				bridgeLog.Infof("Successfully connected and authenticated!")
				goto connectionSuccess
			case <-ctx.Done():
				logger.Errorf("Timeout waiting for QR code scan (attempt %d)", attempt)
				client.Disconnect()
				if attempt == maxRetries {
					return
				}
				time.Sleep(10 * time.Second)
				continue
			}
		} else {
			// Already logged in, just connect
			connErr = client.Connect()
			if connErr != nil {
				logger.Errorf("Failed to connect (attempt %d): %v", attempt, connErr)
				if attempt == maxRetries {
					return
				}
				time.Sleep(5 * time.Second)
				continue
			}
			connected <- true
			break
		}
	}

connectionSuccess:

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	bridgeLog.Infof("Connected to WhatsApp! Type 'help' for commands.")

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	bridgeLog.Infof("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Start reconnection handler goroutine
	go bridge.reconnectLoop(reconnectChan, exitChan)

	// Wait for termination signal
	<-exitChan

	bridgeLog.Infof("Disconnecting...")
	close(retentionStop)
	// Disconnect client
	client.Disconnect()
}
