package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestSendKeepsTheCallerFilenameMIME(t *testing.T) {
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{
		{"image.jpg", "image/jpeg", []byte("\x89PNG\r\n\x1a\nfake")},
		{"image.png", "image/png", []byte("\xff\xd8\xfffake")},
		{"video.mov", "video/quicktime", []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42\x00\x00\x00\x00")},
		{"video.mp4", "video/mp4", []byte("\x1a\x45\xdf\xa3fake")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(storeDirEnv, dir)
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			ms := newTestMessageStore(t)
			client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
			b := testBridge(t, client, ms, testLogger())
			b.Connected = func() bool { return true }
			ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
			sends := 0
			network := messageSendNetwork{
				connected: func() bool { return true },
				upload: func(_ context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					return whatsmeow.UploadResponse{URL: fixtureMediaURL, MediaKey: []byte("key"), FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc"), FileLength: uint64(len(data))}, nil
				},
				send: func(_ context.Context, _ types.JID, packet *waE2E.Message) (whatsmeow.SendResponse, error) {
					sends++
					mime := packet.GetImageMessage().GetMimetype()
					if strings.HasPrefix(tc.mime, "video/") {
						mime = packet.GetVideoMessage().GetMimetype()
					}
					if mime != tc.mime {
						t.Errorf("/api/send sniffed caller bytes: mime=%s want=%s", mime, tc.mime)
					}
					return whatsmeow.SendResponse{ID: "SENTMIME1", Timestamp: ts}, nil
				},
			}
			b.Send = func(ctx context.Context, to, caption, path, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage) {
				return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, caption, path, quotedID, quotedSender, quotedContent, mentions, network)
			}
			body, err := json.Marshal(map[string]string{"recipient": "120363000000000001@g.us", "message": "caption", "media_path": path})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			b.handleSend([]string{dir})(response, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(string(body))))
			if response.Code != http.StatusOK || sends != 1 {
				t.Fatalf("send=%d %s sends=%d", response.Code, response.Body.String(), sends)
			}
		})
	}
}
