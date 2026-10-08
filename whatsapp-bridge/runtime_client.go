package main

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// The SDK logs raw QR payloads and protocol frames at DEBUG. When pairing
// stdout is disabled or the operator listener is enabled, suppress SDK debug
// at its root, before NewClient caches
// its send/receive subloggers. Bridge and database debug logs remain available.
type privatePairingLogger struct{ waLog.Logger }

func (l privatePairingLogger) Debugf(string, ...any) {}
func (l privatePairingLogger) Sub(module string) waLog.Logger {
	return privatePairingLogger{l.Logger.Sub(module)}
}

// Client remains the initial client (and the test seam). Production handoffs
// publish a new pointer atomically; no consumer writes or retains Client.
func (b *Bridge) currentClient() *whatsmeow.Client {
	if client := b.runtimeClient.Load(); client != nil {
		return client
	}
	return b.Client
}

func (b *Bridge) isPaired() bool {
	if b.runtimeClient.Load() != nil {
		return b.runtimePaired.Load()
	}
	client := b.Client
	return client != nil && client.Store != nil && client.Store.ID != nil
}

func (b *Bridge) installClient(client *whatsmeow.Client, paired bool, reconnect chan bool) {
	b.clientGate.Lock()
	defer b.clientGate.Unlock()
	client.EnableAutoReconnect = false
	client.DisableLoginAutoReconnect = true
	if client.Log == nil {
		client.Log = b.Log
	}
	client.Log = connectionProblemLogger{Logger: client.Log, bridge: b, active: func() bool { return b.currentClient() == client }}
	client.PrePairCallback = func(types.JID, string, string) bool {
		if b.currentClient() != client {
			return false
		}
		return b.operatorPairing == nil || b.operatorPairing.beginCompletion(client)
	}
	b.connectionMu.Lock()
	b.forceReconnect.Store(false)
	b.runtimePaired.Store(paired)
	b.runtimeClient.Store(client)
	b.connectionMu.Unlock()
	client.AddEventHandler(func(evt interface{}) {
		b.handleClientEvent(client, evt, reconnect)
	})
}

func (b *Bridge) handleClientEvent(client *whatsmeow.Client, evt interface{}, reconnect chan bool) {
	b.clientGate.RLock()
	defer b.clientGate.RUnlock()
	if b.currentClient() != client {
		return
	} // Ignore a retired client's late events.
	switch evt.(type) {
	case *events.PairSuccess, *events.Connected:
		b.runtimePaired.Store(true)
	case *events.LoggedOut:
		b.runtimePaired.Store(false)
	}
	b.handleEvent(evt, reconnect)
	if b.operatorPairing != nil {
		b.operatorPairing.connectionEvent(evt)
	}
}

func (b *Bridge) bindRuntimeClient() {
	b.Connect = func() error {
		if b.operatorPairing != nil && !b.isPaired() {
			return errors.New("unpaired client requires the pairing controller")
		}
		return b.currentClient().ConnectContext(b.ctx)
	}
	b.Disconnect = func() { b.currentClient().Disconnect() }
	b.Connected = func() bool { c := b.currentClient(); return c != nil && c.IsConnected() }
	b.SendAppState = func(ctx context.Context, patch appstate.PatchInfo) error {
		return b.currentClient().SendAppState(ctx, patch)
	}
	b.PollVoteDecrypt = func(ctx context.Context, evt *events.Message) ([][]byte, error) {
		return whatsmeowPollVoteDecrypter(b.currentClient())(ctx, evt)
	}
	b.IsOnWhatsApp = func(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
		return b.currentClient().IsOnWhatsApp(ctx, phones)
	}
	b.chatPresence = func(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error {
		return b.currentClient().SendChatPresence(ctx, jid, state, media)
	}
	b.Store.groupInfo = func(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
		return b.currentClient().GetGroupInfo(ctx, jid)
	}
}

// Each mux captures one client. A handoff rebuilds the captured handlers, while
// requests already accepted finish with their own mux and disconnected client.
func (b *Bridge) runtimeRESTHandler(port int, token string) http.Handler {
	var mu sync.Mutex
	var previous *whatsmeow.Client
	var handler http.Handler
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.clientGate.RLock()
		defer b.clientGate.RUnlock()
		mu.Lock()
		client := b.currentClient()
		if handler == nil || client != previous {
			handler, previous = b.newRESTMux(port, token), client
		}
		current := handler
		mu.Unlock()
		current.ServeHTTP(w, r)
	})
}
