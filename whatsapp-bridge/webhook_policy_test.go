package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestWebhookChatBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		chat         types.JID
		allowed      bool
	}{
		{"denied", mgGroup, phonePN, false},
		{"allowed phone", phonePN.String(), phonePN, true},
		{"allowed LID twin", phonePN.String(), phoneLID, true},
		{"allowed phone twin", phoneLID.String(), phonePN, true},
		{"allowed ninth digit", "5511999999999", types.NewJID("551199999999", types.DefaultUserServer), true},
		{"unrestricted", "", phonePN, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, payloads := captureWebhook(t)
			t.Setenv("WEBHOOK_URL", srv.URL)
			logger := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{
				lidByPN: map[types.JID]types.JID{phonePN: phoneLID},
				pnByLID: map[types.JID]types.JID{phoneLID: phonePN},
			}), newTestMessageStore(t), logger)
			b.Policy = parseChatPolicy(tc.policy)
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			b.StoreRoot = root
			t.Cleanup(func() { _ = root.Close() })
			// Keep ordinary archival caching queued; only the webhook may read now.
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 4, b.runAutoDownload)
			data := []byte("image bytes for the allowed webhook")
			sum := sha256.Sum256(data)
			msg := buildImageMessage(tc.chat, tc.chat, false, "caption")
			msg.Message.ImageMessage.URL = proto.String("https://example.invalid/image")
			msg.Message.ImageMessage.MediaKey = []byte("test-media-key")
			msg.Message.ImageMessage.FileSHA256 = sum[:]
			msg.Message.ImageMessage.FileEncSHA256 = []byte("test-enc-sha256")
			msg.Message.ImageMessage.FileLength = proto.Uint64(uint64(len(data)))
			var downloads atomic.Int32
			b.DownloadMedia = func(_ context.Context, _, chat string) (bool, string, string, string, error) {
				downloads.Add(1)
				dir := filepath.Join(b.StoreRoot.Name(), chatMediaRel(chat))
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "image.jpg")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				return true, "", "image.jpg", path, nil
			}
			b.handleMessage(msg)
			if !tc.allowed {
				if downloads.Load() != 0 {
					t.Fatal("denied chat triggered webhook media work")
				}
				select {
				case <-payloads:
					t.Fatal("denied chat sent a POST")
				default:
				}
				return
			}
			if downloads.Load() != 1 {
				t.Fatal("allowed chat did not read its image")
			}
			select {
			case p := <-payloads:
				wantChat := tc.chat.String()
				if tc.chat == phoneLID {
					wantChat = phonePN.String()
				}
				if p.Content != "caption" || p.ChatJID != wantChat || p.MediaBase64 != base64.StdEncoding.EncodeToString(data) {
					t.Fatalf("allowed payload changed: %+v", p)
				}
			default:
				t.Fatal("allowed chat did not send its payload")
			}
		})
	}
}

func TestWebhookAllowListAndFeedGates(t *testing.T) {
	for _, chat := range []types.JID{phonePN, types.StatusBroadcastJID, types.NewJID("example", types.NewsletterServer), types.NewJID("example", types.BroadcastServer)} {
		for _, entry := range []string{chat.String(), mgGroup} {
			for flags := 0; flags < 32; flags++ {
				b := &Bridge{ctx: context.Background(), Webhook: newWebhookSender("", true)}
				b.Policy = parseChatPolicy(entry)
				b.Webhook.enabled, b.ForwardSelf = flags&1 != 0, flags&2 != 0
				b.ForwardStatus, b.ForwardChannels, b.ForwardBroadcasts = flags&4 != 0, flags&8 != 0, flags&16 != 0
				feedAllowed := chat == phonePN
				switch {
				case isStatusChat(chat):
					feedAllowed = b.ForwardStatus
				case chat.Server == types.NewsletterServer:
					feedAllowed = b.ForwardChannels
				case chat.Server == types.BroadcastServer:
					feedAllowed = b.ForwardBroadcasts
				}
				for _, fromMe := range []bool{false, true} {
					want := entry == chat.String() && b.Webhook.enabled && (!fromMe || b.ForwardSelf) && feedAllowed
					if got := b.forwardsToWebhook(chat, fromMe); got != want {
						t.Fatalf("chat %s entry %s flags %d fromMe %t: %t want %t", chat, entry, flags, fromMe, got, want)
					}
				}
			}
		}
	}
}
