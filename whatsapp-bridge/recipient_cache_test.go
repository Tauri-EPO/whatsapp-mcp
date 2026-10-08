package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestRecipientCacheRESTAndCurrentPolicy(t *testing.T) {
	for _, route := range []string{"/api/send", "/api/forward"} {
		t.Run(route, func(t *testing.T) {
			ask := registeredWithoutNinthDigit()
			b, _, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
			b.Store = seedEditStore(t)
			b.Policy = parseChatPolicy(efChat + "," + dialledNumber + "," + registeredNumber)
			post := func() *httptest.ResponseRecorder {
				mux := b.newRESTMux(8080, sendRecipientToken)
				if route == "/api/send" {
					return postSend(mux, dialledNumber)
				}
				body, _ := json.Marshal(map[string]string{"chat_jid": efChat, "message_id": "THEIRS", "to_chat_jid": dialledNumber})
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, seamRequest(http.MethodPost, route, string(body), sendRecipientToken))
				return rec
			}
			for range 2 {
				if rec := post(); rec.Code != http.StatusOK {
					t.Fatalf("send status=%d: %s", rec.Code, rec.Body.String())
				}
			}
			if len(ask.calls) != 1 || len(*sent) != 2 || (*sent)[1] != registeredJID.String() {
				t.Fatalf("queries=%v sent=%v", ask.calls, *sent)
			}
			b.Policy = parseChatPolicy(efChat + "," + dialledNumber)
			if rec := post(); rec.Code != http.StatusForbidden || len(ask.calls) != 1 || len(*sent) != 2 {
				t.Fatalf("registered deny status=%d queries=%v sent=%v", rec.Code, ask.calls, *sent)
			}
			b.Policy = parseChatPolicy(efChat + "," + registeredNumber)
			if rec := post(); rec.Code != http.StatusForbidden || len(ask.calls) != 1 || len(*sent) != 2 {
				t.Fatalf("typed deny status=%d queries=%v sent=%v", rec.Code, ask.calls, *sent)
			}
		})
	}
}

func TestRecipientCacheExpiryAndBridgeIsolation(t *testing.T) {
	ask := registeredWithoutNinthDigit()
	b, mux, sent := sendRecipientBridge(t, &mockLIDStore{}, ask)
	b.Policy = parseChatPolicy(dialledNumber + "," + registeredNumber)
	now := time.Unix(1000, 0)
	b.recipientNumbers.now = func() time.Time { return now }
	if rec := postSend(mux, dialledNumber); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	now = now.Add(recipientCacheTTL - time.Second)
	if rec := postSend(mux, dialledNumber); rec.Code != http.StatusOK || len(ask.calls) != 1 {
		t.Fatalf("pre-expiry status=%d queries=%v", rec.Code, ask.calls)
	}
	now = now.Add(time.Second)
	ask.answers["+"+dialledNumber] = types.IsOnWhatsAppResponse{IsIn: false}
	if rec := postSend(mux, dialledNumber); rec.Code != http.StatusNotFound || len(ask.calls) != 2 || len(*sent) != 2 {
		t.Fatalf("expiry status=%d queries=%v sent=%v", rec.Code, ask.calls, *sent)
	}
	otherAsk := registeredWithoutNinthDigit()
	_, otherMux, _ := sendRecipientBridge(t, &mockLIDStore{}, otherAsk)
	if rec := postSend(otherMux, dialledNumber); rec.Code != http.StatusOK || len(otherAsk.calls) != 1 {
		t.Fatalf("separate bridge reused an answer: %d %v", rec.Code, otherAsk.calls)
	}
}

func TestRecipientCacheDoesNotRememberUnknownAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  *fakeIsOnWhatsApp
	}{
		{"negative", &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{"+" + dialledNumber: {IsIn: false}}}},
		{"empty", &fakeIsOnWhatsApp{}},
		{"failed", &fakeIsOnWhatsApp{err: errors.New("query unavailable")}},
		{"LID only", &fakeIsOnWhatsApp{answers: map[string]types.IsOnWhatsAppResponse{"+" + dialledNumber: {IsIn: true, JID: registeredLID}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bridge{IsOnWhatsApp: tc.ask.ask}
			for range 2 {
				_, _ = b.queryRegisteredNumber(context.Background(), []string{"+" + dialledNumber})
			}
			if len(tc.ask.calls) != 2 || len(b.recipientNumbers.entries) != 0 {
				t.Fatalf("queries=%v entries=%v", tc.ask.calls, b.recipientNumbers.entries)
			}
		})
	}
	ask := registeredWithoutNinthDigit()
	ask.err = errors.New("LID persistence failed after registry answer")
	b := &Bridge{IsOnWhatsApp: ask.ask}
	for range 2 {
		answers, _ := b.queryRegisteredNumber(context.Background(), []string{"+" + dialledNumber})
		if len(answers) != 1 || registeredPhoneJID(answers[0]) != registeredJID {
			t.Fatalf("lost authoritative answer: %v", answers)
		}
	}
	if len(ask.calls) != 1 {
		t.Fatalf("positive answer was not remembered: %v", ask.calls)
	}
}

func TestRecipientCacheBoundAndCanceledHit(t *testing.T) {
	calls := 0
	b := &Bridge{IsOnWhatsApp: func(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
		calls++
		return []types.IsOnWhatsAppResponse{{Query: phones[0], IsIn: true, PhoneNumber: registeredJID}}, nil
	}}
	for index := range recipientCacheLimit + 17 {
		_, _ = b.queryRegisteredNumber(context.Background(), []string{fmt.Sprintf("fake-key-%d", index)})
		if len(b.recipientNumbers.entries) > recipientCacheLimit {
			t.Fatalf("unbounded entries: %d", len(b.recipientNumbers.entries))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	answers, err := b.queryRegisteredNumber(ctx, []string{fmt.Sprintf("fake-key-%d", recipientCacheLimit+16)})
	if !errors.Is(err, context.Canceled) || len(answers) != 0 || calls != recipientCacheLimit+17 {
		t.Fatalf("canceled hit=%v %v calls=%d", answers, err, calls)
	}
}

func TestRecipientCacheConnectionEventsAndInFlightClear(t *testing.T) {
	for _, event := range []any{&events.Connected{}, &events.Disconnected{}, &events.LoggedOut{}} {
		b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
		b.Exit = func(string, int) {}
		_, _ = b.queryRegisteredNumber(context.Background(), []string{"+" + dialledNumber})
		b.handleEvent(event, make(chan bool, 1))
		if len(b.recipientNumbers.entries) != 0 {
			t.Fatalf("%T did not clear the answer", event)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	b := &Bridge{IsOnWhatsApp: func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
		close(entered)
		<-release
		return []types.IsOnWhatsAppResponse{{IsIn: true, PhoneNumber: registeredJID}}, nil
	}}
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		_, _ = b.queryRegisteredNumber(context.Background(), []string{"+" + dialledNumber})
	}()
	<-entered
	b.recipientNumbers.clear()
	close(release)
	done.Wait()
	if len(b.recipientNumbers.entries) != 0 {
		t.Fatal("an answer from before the connection change repopulated the cache")
	}
}
