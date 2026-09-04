package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendHandlerReturnsMessageID(t *testing.T) {
	const token = "test-token-0123456789"
	b := testBridge(newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.Send = func(recipient, message, mediaPath, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage) {
		return true, "Message sent to " + recipient, sentMessage{
			ID: "3EB0ABCDEF", ChatJID: "5511999999999@s.whatsapp.net",
			Timestamp: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		}
	}
	mux := b.newRESTMux(8080, token, nil)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"5511999999999","message":"hi"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp SendMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.MessageID != "3EB0ABCDEF" || resp.ChatJID != "5511999999999@s.whatsapp.net" || resp.Timestamp != "2026-09-04T12:00:00Z" {
		t.Fatalf("response = %+v", resp)
	}

	// Failure: no id fields leak into the body.
	b.Send = func(recipient, message, mediaPath, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage) {
		return false, "Error sending message: offline", sentMessage{}
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"5511999999999","message":"hi"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+token)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "message_id") {
		t.Fatalf("failure response = %d %s", rec.Code, rec.Body.String())
	}
}
