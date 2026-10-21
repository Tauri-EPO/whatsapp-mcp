package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func TestMediaRetryCooldownDurations(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		wait time.Duration
	}{
		{"the phone answered without the file", phoneRetryAnswer{whatsmeow.ErrMediaNotAvailableOnPhone}, mediaRetryAnsweredCooldown},
		{"the phone said NOT_FOUND", fmt.Errorf("declined: NOT_FOUND: %w", errMediaUnavailable), mediaRetryAnsweredCooldown},
		{"no answer came back", errors.New("timed out waiting for the sender's phone"), mediaRetryUnansweredCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newMediaRetryCooldown()
			backoff := c.note("chat", "ID", tc.err, now)
			if !backoff.until.Equal(now.Add(tc.wait)) || !errors.Is(backoff, tc.err) {
				t.Fatalf("backoff = %v until %v, want %v wrapping the cause", backoff, backoff.until, now.Add(tc.wait))
			}
			if got, ok := c.active("chat", "ID", now.Add(tc.wait-time.Second)); !ok || !errors.Is(got, tc.err) {
				t.Fatalf("not cooling down just before the end: %v %v", got, ok)
			}
			if _, ok := c.active("other", "ID", now); ok {
				t.Fatal("the cooldown is per chat and ID")
			}
			if _, ok := c.active("chat", "ID", now.Add(tc.wait)); ok {
				t.Fatal("still cooling down after the end")
			}
			if len(c.entries) != 0 {
				t.Fatal("an expired entry is kept")
			}
		})
	}
}

func TestMediaRetryCooldownForgetAndBound(t *testing.T) {
	now := time.Now()
	c := newMediaRetryCooldown()
	_ = c.note("chat", "ID", errors.New("offline"), now)
	c.forget("chat", "ID")
	if _, ok := c.active("chat", "ID", now); ok {
		t.Fatal("a successful retry must clear the cooldown")
	}
	for i := range mediaRetryCooldownMax + 10 {
		_ = c.note("chat", strconv.Itoa(i), errors.New("offline"), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(c.entries) != mediaRetryCooldownMax {
		t.Fatalf("entries = %d, want the bound %d", len(c.entries), mediaRetryCooldownMax)
	}
	if _, ok := c.active("chat", strconv.Itoa(mediaRetryCooldownMax+9), now); !ok {
		t.Fatal("the newest entry was evicted instead of the one due soonest")
	}
	var nilCooldown *mediaRetryCooldown
	if _, ok := nilCooldown.active("chat", "ID", now); ok {
		t.Fatal("a nil cooldown never blocks")
	}
	_ = nilCooldown.note("chat", "ID", errors.New("offline"), now)
	nilCooldown.forget("chat", "ID")
}

// The loop of issue #743: an old message the CDN refuses, a phone that answers
// it has no file. The second /api/download must not reach the phone again,
// and both answers say when to come back instead of a bare 500.
func TestDownloadDoesNotAskThePhoneAgainDuringTheCooldown(t *testing.T) {
	old := time.Now().Add(-cdnFreshWindow - time.Hour)
	cases := []struct {
		name     string
		answer   error
		status   int
		code     string
		backoffs bool
	}{
		{"not available on phone", phoneRetryAnswer{whatsmeow.ErrMediaNotAvailableOnPhone}, http.StatusServiceUnavailable, "media_retry_backoff", true},
		{"the phone did not answer", errors.New("timed out after 30s waiting for the sender's phone"), http.StatusServiceUnavailable, "media_retry_backoff", true},
		{"NOT_FOUND stays media_unavailable", phoneRetryAnswer{fmt.Errorf("declined: NOT_FOUND: %w", errMediaUnavailable)}, http.StatusInternalServerError, "media_unavailable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			ms := newTestMessageStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			b.Connected = func() bool { return true }
			seedDocumentRow(t, ms, "LOOP1", fixtureDirectPath, old)
			b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
				return 0, whatsmeow.ErrMediaDownloadFailedWith403
			}
			retry := &retryRecorder{err: tc.answer}
			b.mediaRetryDownload = retry.retry
			for attempt := 1; attempt <= 3; attempt++ {
				rec := httptest.NewRecorder()
				b.handleDownload()(rec, httptest.NewRequest(http.MethodPost, "/api/download",
					strings.NewReader(`{"message_id":"LOOP1","chat_jid":"`+mediaTestChat+`"}`)))
				if rec.Code != tc.status || downloadErrorCode(t, rec) != tc.code {
					t.Fatalf("attempt %d: %d %s, want %d %s", attempt, rec.Code, rec.Body.String(), tc.status, tc.code)
				}
				retryAfter, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
				if tc.backoffs && retryAfter < int(mediaRetryUnansweredCooldown.Seconds())-60 {
					t.Fatalf("attempt %d: Retry-After = %q", attempt, rec.Header().Get("Retry-After"))
				}
			}
			if retry.calls != 1 {
				t.Fatalf("the phone was asked %d times, want once", retry.calls)
			}
		})
	}
}

func TestOnlyARetryThatReachedThePhoneCoolsDown(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	cases := []struct {
		name  string
		ctx   context.Context
		err   error
		cools bool
	}{
		{"receipt not sent", context.Background(), mediaRetryNotSent{errors.New("send media retry receipt: not connected")}, false},
		{"caller budget after the answer", context.Background(), fmt.Errorf("download after media retry: %w", errAutoMediaLimit), false},
		{"quota after the answer", context.Background(), errMediaQuota, false},
		{"deadline while the phone was awaited", expired, context.DeadlineExceeded, true},
		{"no answer", context.Background(), errors.New("timed out waiting for the sender's phone"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
			b.mediaRetryDownload = func(context.Context, string, string, *MediaDownloader, *os.Root, string) (int64, error) {
				return 0, tc.err
			}
			_, err := b.retryMedia(tc.ctx, "ID", mediaTestChat, &MediaDownloader{}, nil, "x")
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.err)
			}
			if _, ok := b.mediaRetryCooldown.active(mediaTestChat, "ID", time.Now()); ok != tc.cools {
				t.Fatalf("cooling down = %v, want %v", ok, tc.cools)
			}
		})
	}
}

func TestForwardAndStreamNameTheBackoff(t *testing.T) {
	backoff := &mediaRetryBackoffError{until: time.Now().Add(time.Hour), err: errors.New("timed out")}
	rec := httptest.NewRecorder()
	if !writeMediaRetryBackoff(rec, fmt.Errorf("failed to download media: %w", backoff), "cooling") {
		t.Fatal("a wrapped backoff was not answered")
	}
	if rec.Code != http.StatusServiceUnavailable || downloadErrorCode(t, rec) != "media_retry_backoff" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("answer = %d %s %q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	final := &mediaRetryBackoffError{until: time.Now().Add(time.Hour), err: errMediaUnavailable}
	if writeMediaRetryBackoff(httptest.NewRecorder(), final, "x") || writeMediaRetryBackoff(httptest.NewRecorder(), errors.New("other"), "x") {
		t.Fatal("media_unavailable and unrelated errors must keep their own answers")
	}

	ms := seedEditStore(t)
	deps := forwardDeps{
		lookup:           ms.messageContentLookup,
		resolveRecipient: forwardRecipientAsTyped,
		download: func(context.Context, string, string) (bool, string, string, string, error) {
			return false, "", "", "", fmt.Errorf("failed to download media: %w", backoff)
		},
		send: func(context.Context, string, string, string, string, string, string, []string) (bool, string, sentMessage) {
			t.Fatal("nothing to send without the bytes")
			return false, "", sentMessage{}
		},
	}
	rec = httptest.NewRecorder()
	handleForwardMessage(deps, chatPolicy{})(rec, httptest.NewRequest(http.MethodPost, "/api/forward",
		strings.NewReader(`{"chat_jid":"5511999999999@s.whatsapp.net","message_id":"PIC","to_chat_jid":"120363000000000001@g.us"}`)))
	if rec.Code != http.StatusServiceUnavailable || downloadErrorCode(t, rec) != "media_retry_backoff" {
		t.Fatalf("forward answer = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCancelledMediaRetryLeavesNoCooldown(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms := newTestMessageStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	b.mediaRetryDownload = func(context.Context, string, string, *MediaDownloader, *os.Root, string) (int64, error) {
		cancel()
		return 0, context.Canceled
	}
	if _, err := b.retryMedia(ctx, "ID", mediaTestChat, &MediaDownloader{}, nil, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := b.mediaRetryCooldown.active(mediaTestChat, "ID", time.Now()); ok {
		t.Fatal("a caller that gave up must not start a cooldown")
	}
}
