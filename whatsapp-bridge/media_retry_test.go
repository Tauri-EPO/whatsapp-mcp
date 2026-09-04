package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestIsExpiredMediaError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"403", whatsmeow.ErrMediaDownloadFailedWith403, true},
		{"404", whatsmeow.ErrMediaDownloadFailedWith404, true},
		{"410", whatsmeow.ErrMediaDownloadFailedWith410, true},
		{"wrapped 403", fmt.Errorf("failed to download media from last host: %w", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 403}}), true},
		{"500", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, false},
		{"hash mismatch", whatsmeow.ErrInvalidMediaSHA256, false},
		{"other", errors.New("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpiredMediaError(tc.err); got != tc.want {
				t.Fatalf("isExpiredMediaError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMediaURLFromDirectPathRoundTrips(t *testing.T) {
	directPath := "/v/t62.7118-24/123_456_789_n.enc?ccb=11-4&oh=abc&oe=def&_nc_sid=5e03e0"
	url := mediaURLFromDirectPath(directPath)
	if url != "https://mmg.whatsapp.net"+directPath {
		t.Fatalf("unexpected url %q", url)
	}
	// The persisted URL must decode back to the same direct path so the next
	// downloadMedia call (which reads messages.url) hits the refreshed CDN object.
	if got := extractDirectPathFromURL(url); got != directPath {
		t.Fatalf("extractDirectPathFromURL(%q) = %q, want %q", url, got, directPath)
	}
	if got := mediaURLFromDirectPath("v/no-leading-slash.enc"); got != "https://mmg.whatsapp.net/v/no-leading-slash.enc" {
		t.Fatalf("missing leading slash not normalised: %q", got)
	}
}

func TestMediaRetryMessageInfo(t *testing.T) {
	t.Run("direct chat with bare phone sender", func(t *testing.T) {
		info, err := mediaRetryMessageInfo("MSG1", "5511999999999@s.whatsapp.net", "5511999999999", false)
		if err != nil {
			t.Fatal(err)
		}
		if info.ID != "MSG1" || info.IsGroup || info.IsFromMe {
			t.Fatalf("unexpected info: %+v", info)
		}
		if info.Sender.String() != "5511999999999@s.whatsapp.net" {
			t.Fatalf("sender = %s", info.Sender)
		}
	})
	t.Run("group chat marks IsGroup and keeps participant", func(t *testing.T) {
		info, err := mediaRetryMessageInfo("MSG2", "120363000000000000@g.us", "5511888888888", false)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsGroup {
			t.Fatal("expected IsGroup for @g.us chat")
		}
		if info.Sender.User != "5511888888888" || info.Sender.Server != types.DefaultUserServer {
			t.Fatalf("sender = %s", info.Sender)
		}
	})
	t.Run("full sender JID is parsed as-is", func(t *testing.T) {
		info, err := mediaRetryMessageInfo("MSG3", "120363000000000000@g.us", "123456789@lid", true)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sender.Server != "lid" || !info.IsFromMe {
			t.Fatalf("unexpected info: %+v", info)
		}
	})
	t.Run("empty sender falls back to chat", func(t *testing.T) {
		info, err := mediaRetryMessageInfo("MSG4", "5511999999999@s.whatsapp.net", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if info.Sender != info.Chat {
			t.Fatalf("sender = %s, chat = %s", info.Sender, info.Chat)
		}
	})
	t.Run("invalid chat JID errors", func(t *testing.T) {
		if _, err := mediaRetryMessageInfo("MSG5", "1.2.3@s.whatsapp.net", "x", false); err == nil {
			t.Fatal("expected error for invalid chat JID")
		}
	})
}

func TestMediaRetryWaiterDispatch(t *testing.T) {
	ch, cancel := registerMediaRetryWaiter("WAIT1")
	defer cancel()

	if dispatchMediaRetry(&events.MediaRetry{MessageID: "OTHER"}) {
		t.Fatal("dispatch for unregistered ID should return false")
	}
	if dispatchMediaRetry(nil) {
		t.Fatal("nil event should be ignored")
	}

	evt := &events.MediaRetry{MessageID: "WAIT1"}
	if !dispatchMediaRetry(evt) {
		t.Fatal("dispatch for registered ID should succeed")
	}
	select {
	case got := <-ch:
		if got != evt {
			t.Fatal("received a different event")
		}
	default:
		t.Fatal("event was not delivered to the waiter channel")
	}

	// Buffer is 1: a second undelivered event is dropped instead of blocking the event loop.
	if !dispatchMediaRetry(evt) {
		t.Fatal("first buffered dispatch should succeed")
	}
	if dispatchMediaRetry(evt) {
		t.Fatal("dispatch into a full buffer must not block or succeed")
	}

	cancel()
	if dispatchMediaRetry(evt) {
		t.Fatal("dispatch after cancel should return false")
	}
}

func TestMediaRetryWaiterCancelDoesNotRemoveReplacement(t *testing.T) {
	_, cancelOld := registerMediaRetryWaiter("DUP")
	chNew, cancelNew := registerMediaRetryWaiter("DUP")
	defer cancelNew()

	cancelOld() // stale cancel must not evict the newer waiter
	if !dispatchMediaRetry(&events.MediaRetry{MessageID: "DUP"}) {
		t.Fatal("newer waiter should still receive events")
	}
	<-chNew
}
