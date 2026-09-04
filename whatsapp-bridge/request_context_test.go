package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendHandlerPassesRequestContextWithDeadline(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	var seen context.Context
	b.Send = func(ctx context.Context, recipient, message, mediaPath, _, _, _ string, _ []string) (bool, string, sentMessage) {
		seen = ctx
		return true, "ok", sentMessage{ID: "X", ChatJID: recipient}
	}
	mux := b.newRESTMux(8080, token)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"5511999999999","message":"hi"}`)).WithContext(parent)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen == nil {
		t.Fatalf("status %d, ctx %v", rec.Code, seen)
	}
	deadline, ok := seen.Deadline()
	if !ok || time.Until(deadline) > sendDeadline || time.Until(deadline) < sendDeadline-5*time.Second {
		t.Fatalf("send context should carry the %s deadline, got ok=%v deadline=%v", sendDeadline, ok, deadline)
	}
	// Once the handler returned, its context is cancelled: nothing keeps running.
	select {
	case <-seen.Done():
	default:
		t.Fatal("request-scoped context should be cancelled after the handler returns")
	}
}
