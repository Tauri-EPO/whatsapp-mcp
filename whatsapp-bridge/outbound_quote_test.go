package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

var outboundReplyGroup = types.NewJID("120363000000000001", types.GroupServer)

func replyTestBridge(t *testing.T, lids store.LIDStore) *Bridge {
	t.Helper()
	b := testBridge(t, newTestClientWithSelf(lids, phonePN), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	b.RESTBind = defaultBridgeBind
	b.Send = b.sendBackend()
	return b
}

func TestReplyRESTWireStoredPreview(t *testing.T) {
	for _, kind := range []string{"image", "video", "document", "audio", "sticker", "text", "missing", "other-chat", "legacy", "invalid-presentation"} {
		for _, outgoing := range []string{"text", "image"} {
			t.Run(kind+"/"+outgoing, func(t *testing.T) {
				b := replyTestBridge(t, &mockLIDStore{})
				chat := outboundReplyGroup.String()
				if err := b.Store.StoreChat(chat, "Example group", time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := b.Store.UpdateChatEphemeralSettings(chat, 86400, 1710000000); err != nil {
					t.Fatal(err)
				}
				mediaKind, caption := kind, "stored caption"
				if kind == "text" {
					mediaKind, caption = "", "stored text"
				}
				if kind == "image" || kind == "audio" || kind == "sticker" {
					caption = ""
				}
				if kind == "legacy" || kind == "invalid-presentation" {
					mediaKind = "document"
				}
				mime := map[string]string{"image": "image/png", "video": "video/mp4", "document": "application/pdf", "audio": "audio/ogg; codecs=opus", "sticker": "image/webp"}[mediaKind]
				p := (&mediaPresentation{MIME: mime, Name: proto.String("Report.pdf"), Title: proto.String("Report title"), PTT: proto.Bool(true), Seconds: proto.Uint32(7), Waveform: bytes.Repeat([]byte{1}, 64), Animated: proto.Bool(true)}).forFile(mediaKind, []byte("fake-sha"))
				if kind == "legacy" || kind == "invalid-presentation" {
					p = nil
				}
				if kind != "missing" {
					quoteChat := chat
					if kind == "other-chat" {
						quoteChat = phonePN.String()
					}
					if err := b.Store.StoreMessage("QPRE", quoteChat, phonePN.String(), caption, time.Now(), false, mediaKind, "document_20261008_120000_QPRE", "https://example.com/media", []byte("secret-key"), []byte("fake-sha"), []byte("encrypted-sha"), uint64(10), "", messageMediaOptions{presentation: p}); err != nil {
						t.Fatal(err)
					}
					if kind == "invalid-presentation" {
						if _, err := b.Store.db.Exec("UPDATE messages SET media_presentation = ? WHERE id = ? AND chat_jid = ?", `{"sha256":"wrong","mime":"image/png","name":"wrong.pdf"}`, "QPRE", quoteChat); err != nil {
							t.Fatal(err)
						}
					}
				}
				path := ""
				if outgoing == "image" {
					path = filepath.Join(t.TempDir(), "sample.png")
					if err := os.WriteFile(path, []byte("fake image"), 0o600); err != nil {
						t.Fatal(err)
					}
					b.MediaRoots = []string{filepath.Dir(path)}
				}
				var wire []byte
				uploads := 0
				b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					uploads++
					return testUpload(), nil
				}
				b.sendMessage = func(ctx context.Context, jid types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
					var err error
					wire, err = proto.Marshal(message)
					return whatsmeow.SendResponse{ID: "REPLY1", Timestamp: time.Now()}, err
				}
				mux := b.newRESTMux(8080, sendRecipientToken)
				handlerDone := make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(handlerDone)
					mux.ServeHTTP(w, r)
				}))
				defer srv.Close()
				body, _ := json.Marshal(SendMessageRequest{Recipient: chat, Message: "reply", MediaPath: path, QuotedMessageID: "QPRE", QuotedContent: "caller fallback", Mentions: []string{phoneLID.String()}})
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/send", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+sendRecipientToken)
				req.Host = "localhost:8080"
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				responseBody, _ := io.ReadAll(resp.Body)
				<-handlerDone
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP %d: %s", resp.StatusCode, responseBody)
				}
				m := &waE2E.Message{}
				if err := proto.Unmarshal(wire, m); err != nil {
					t.Fatal(err)
				}
				ctx := sharedContextInfo(m)
				wantAuthor := phonePN.String()
				if kind == "missing" || kind == "other-chat" {
					wantAuthor = ""
				}
				if ctx == nil || ctx.GetParticipant() != wantAuthor || (wantAuthor == "" && ctx.Participant != nil) || ctx.GetStanzaID() != "QPRE" || ctx.GetExpiration() != 86400 || len(ctx.GetMentionedJID()) != 1 || ctx.GetMentionedJID()[0] != phoneLID.String() {
					t.Fatalf("wire context=%v", ctx)
				}
				quoted := ctx.GetQuotedMessage()
				switch kind {
				case "image":
					if quoted.GetImageMessage() == nil || quoted.GetImageMessage().GetCaption() != "" || quoted.GetImageMessage().GetMimetype() != mime {
						t.Errorf("captionless image preview=%v", quoted)
					}
				case "video":
					if quoted.GetVideoMessage() == nil || quoted.GetVideoMessage().GetCaption() != caption || quoted.GetVideoMessage().GetMimetype() != mime {
						t.Errorf("video preview=%v", quoted)
					}
				case "document":
					if quoted.GetDocumentMessage() == nil || quoted.GetDocumentMessage().GetCaption() != caption || quoted.GetDocumentMessage().GetFileName() != "Report.pdf" || quoted.GetDocumentMessage().GetTitle() != "Report title" {
						t.Errorf("document preview=%v", quoted)
					}
				case "audio":
					if quoted.GetAudioMessage() == nil || !quoted.GetAudioMessage().GetPTT() || quoted.GetAudioMessage().GetSeconds() != 7 || !bytes.Equal(quoted.GetAudioMessage().GetWaveform(), bytes.Repeat([]byte{1}, 64)) {
						t.Errorf("voice preview=%v", quoted)
					}
				case "sticker":
					if quoted.GetStickerMessage() == nil || !quoted.GetStickerMessage().GetIsAnimated() || quoted.GetStickerMessage().GetMimetype() != mime {
						t.Errorf("sticker preview=%v", quoted)
					}
				case "text":
					if quoted.GetConversation() != caption {
						t.Errorf("stored text preview=%v", quoted)
					}
				case "missing", "other-chat":
					if quoted.GetConversation() != "caller fallback" {
						t.Errorf("fallback/cross-chat preview=%v", quoted)
					}
				case "legacy", "invalid-presentation":
					if quoted.GetDocumentMessage() == nil || quoted.GetDocumentMessage().GetFileName() != "" || quoted.GetDocumentMessage().GetTitle() != "" || quoted.GetDocumentMessage().GetMimetype() != "" {
						t.Errorf("legacy/invalid preview=%v", quoted)
					}
				}
				if _, part := mediaPartOf(quoted); part != nil && (part.GetURL() != "" || part.GetDirectPath() != "" || len(part.GetMediaKey()) != 0) {
					t.Error("preview contains download credentials")
				}
				var quotedID string
				if err := b.Store.db.QueryRow("SELECT quoted_message_id FROM messages WHERE id = ? AND chat_jid = ?", "REPLY1", chat).Scan(&quotedID); err != nil || quotedID != "QPRE" {
					t.Errorf("outbound row quote=%q err=%v", quotedID, err)
				}
				if (outgoing == "image" && uploads != 1) || (outgoing == "text" && uploads != 0) {
					t.Errorf("uploads=%d", uploads)
				}
			})
		}
	}
}

func TestReplyRESTWireStoredAuthor(t *testing.T) {
	for _, tc := range []struct {
		name, sender, explicit, want        string
		mapped, own, full, foreign, missing bool
	}{
		{name: "PN", sender: phonePN.String(), want: phonePN.String()},
		{name: "PN mapped uses wire LID", sender: phonePN.String(), mapped: true, want: phoneLID.String()},
		{name: "LID mapped", sender: phoneLID.String(), mapped: true, want: phoneLID.String()},
		{name: "LID unmapped", sender: phoneLID.String(), want: phoneLID.String()},
		{name: "own overrides stored peer", sender: phoneLID.String(), own: true, mapped: true, want: phoneLID.String()},
		{name: "own without stored sender", own: true, want: phonePN.String()},
		{name: "unknown sender"},
		{name: "unknown bare namespace", sender: phoneLID.User},
		{name: "full LID legacy", sender: phoneLID.String(), full: true, want: phoneLID.String()},
		{name: "full PN legacy", sender: phonePN.String(), full: true, want: phonePN.String()},
		{name: "full other namespace", sender: outboundReplyGroup.String(), full: true, want: outboundReplyGroup.String()},
		{name: "other chat", sender: phoneLID.String(), mapped: true, foreign: true},
		{name: "missing row", missing: true},
		{name: "explicit PN upgrades", sender: phoneLID.String(), mapped: true, explicit: phonePN.User, want: phoneLID.String()},
		{name: "explicit LID preserved", sender: phonePN.String(), mapped: true, explicit: phoneLID.String(), want: phoneLID.String()},
		{name: "invalid explicit stays omitted", sender: phonePN.String(), explicit: "1.2.3@s.whatsapp.net"},
	} {
		for _, outgoing := range []string{"text", "image"} {
			t.Run(tc.name+"/"+outgoing, func(t *testing.T) {
				lids := &mockLIDStore{}
				if tc.mapped {
					lids.pnByLID = map[types.JID]types.JID{phoneLID: phonePN}
					lids.lidByPN = map[types.JID]types.JID{phonePN: phoneLID}
				}
				b := replyTestBridge(t, lids)
				b.Client.Store.ID.Device = 7
				chat := outboundReplyGroup.String()
				quoteChat := chat
				if tc.foreign {
					quoteChat = phonePN.String()
				}
				if !tc.missing {
					if err := b.Store.StoreMessage("QAUTHOR", quoteChat, tc.sender, "private stored text", time.Now(), tc.own, "", "", "", nil, nil, nil, nil, ""); err != nil {
						t.Fatal(err)
					}
					if tc.full {
						if _, err := b.Store.db.Exec("UPDATE messages SET sender = ?, sender_server = NULL WHERE id = ? AND chat_jid = ?", tc.sender, "QAUTHOR", quoteChat); err != nil {
							t.Fatal(err)
						}
					}
				}
				path := ""
				if outgoing == "image" {
					path = filepath.Join(t.TempDir(), "sample.png")
					if err := os.WriteFile(path, []byte("fake image"), 0o600); err != nil {
						t.Fatal(err)
					}
					b.MediaRoots = []string{filepath.Dir(path)}
				}
				var wire []byte
				b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					return testUpload(), nil
				}
				b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
					var err error
					wire, err = proto.Marshal(message)
					return whatsmeow.SendResponse{ID: "REPLYAUTHOR", Timestamp: time.Now()}, err
				}
				handlerDone := make(chan struct{})
				mux := b.newRESTMux(8080, sendRecipientToken)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(handlerDone); mux.ServeHTTP(w, r) }))
				defer srv.Close()
				body, err := json.Marshal(SendMessageRequest{Recipient: chat, Message: "reply", MediaPath: path, QuotedMessageID: "QAUTHOR", QuotedSenderJID: tc.explicit, QuotedContent: "caller fallback"})
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/send", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+sendRecipientToken)
				req.Host = "localhost:8080"
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				responseBody, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				<-handlerDone
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP %d: %s", resp.StatusCode, responseBody)
				}
				m := &waE2E.Message{}
				if err := proto.Unmarshal(wire, m); err != nil {
					t.Fatal(err)
				}
				ctx := sharedContextInfo(m)
				if ctx == nil || ctx.GetParticipant() != tc.want || (tc.want == "" && ctx.Participant != nil) || ctx.GetStanzaID() != "QAUTHOR" {
					t.Fatalf("wire author=%v, want %q", ctx, tc.want)
				}
				wantPreview := "private stored text"
				if tc.foreign || tc.missing {
					wantPreview = "caller fallback"
				}
				if ctx.GetQuotedMessage().GetConversation() != wantPreview {
					t.Errorf("wire preview=%v, want %q", ctx.GetQuotedMessage(), wantPreview)
				}
			})
		}
	}
}

type cancelReplyLIDs struct {
	*mockLIDStore
	blockPN, blockLID types.JID
	entered           chan context.Context
	skipFirstPN       bool
}

func (l *cancelReplyLIDs) GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error) {
	if pn == l.blockPN && l.skipFirstPN {
		l.skipFirstPN = false
		return phoneLID, nil
	}
	if pn == l.blockPN {
		l.entered <- ctx
		<-ctx.Done()
		return types.EmptyJID, ctx.Err()
	}
	return l.mockLIDStore.GetLIDForPN(ctx, pn)
}

func (l *cancelReplyLIDs) GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error) {
	if lid == l.blockLID {
		l.entered <- ctx
		<-ctx.Done()
		return types.EmptyJID, ctx.Err()
	}
	return l.mockLIDStore.GetPNForLID(ctx, lid)
}

func TestReplyHTTPDisconnectCancelsLIDLookupsBeforeUpload(t *testing.T) {
	for _, phase := range []string{"quote", "stored-author", "mention", "recipient", "recipient-send", "settings"} {
		t.Run(phase, func(t *testing.T) {
			lids := &cancelReplyLIDs{mockLIDStore: &mockLIDStore{}, entered: make(chan context.Context, 1)}
			b := replyTestBridge(t, lids)
			path := filepath.Join(t.TempDir(), "sample.png")
			if err := os.WriteFile(path, []byte("fake image"), 0o600); err != nil {
				t.Fatal(err)
			}
			b.MediaRoots = []string{filepath.Dir(path)}
			var uploads, sends atomic.Int64
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads.Add(1)
				return testUpload(), nil
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				sends.Add(1)
				return whatsmeow.SendResponse{}, nil
			}
			payload := SendMessageRequest{Recipient: outboundReplyGroup.String(), Message: "reply", MediaPath: path}
			switch phase {
			case "quote":
				payload.QuotedMessageID, payload.QuotedSenderJID, lids.blockPN = "Q1", phonePN.String(), phonePN
			case "stored-author":
				payload.QuotedMessageID, lids.blockPN = "Q1", phonePN
				if err := b.Store.StoreMessage("Q1", payload.Recipient, phonePN.String(), "original", time.Now(), false, "", "", "", nil, nil, nil, nil, ""); err != nil {
					t.Fatal(err)
				}
			case "mention":
				payload.Mentions, lids.blockPN = []string{phonePN.String()}, phonePN
			case "recipient":
				payload.Recipient, lids.blockPN = phonePN.String(), phonePN
			case "recipient-send":
				payload.Recipient, lids.blockPN, lids.skipFirstPN = phonePN.String(), phonePN, true
			case "settings":
				payload.Recipient, lids.blockLID = phoneLID.String(), phoneLID
			}
			mux := b.newRESTMux(8080, sendRecipientToken)
			handlerDone := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(handlerDone); mux.ServeHTTP(w, r) }))
			defer srv.Close()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			body, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(parent, http.MethodPost, srv.URL+"/api/send", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+sendRecipientToken)
			req.Host = "localhost:8080"
			requestDone := make(chan error, 1)
			go func() {
				resp, err := srv.Client().Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				requestDone <- err
			}()
			select {
			case lookupCtx := <-lids.entered:
				if _, ok := lookupCtx.Deadline(); !ok {
					t.Error("LID lookup lost request deadline")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lookup not reached")
			}
			cancel()
			if err := <-requestDone; !errors.Is(err, context.Canceled) {
				t.Errorf("HTTP canceled error=%v", err)
			}
			select {
			case <-handlerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not stop with HTTP disconnect")
			}
			if uploads.Load() != 0 || sends.Load() != 0 {
				t.Errorf("side effects after cancellation: upload=%d send=%d", uploads.Load(), sends.Load())
			}
			wantRows := 0
			if phase == "stored-author" {
				wantRows = 1
			}
			if rows := queryMessageCount(b.Store, payload.Recipient); rows != wantRows {
				t.Errorf("messages after cancellation=%d, want unchanged count %d", rows, wantRows)
			}
		})
	}
}

func TestReplySQLLookupsCanceledBeforeUpload(t *testing.T) {
	for _, phase := range []string{"quote", "settings"} {
		t.Run(phase, func(t *testing.T) {
			b := replyTestBridge(t, &mockLIDStore{})
			b.Store.db.SetMaxOpenConns(1)
			conn, err := b.Store.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				t.Error("upload after canceled SQL lookup")
				return testUpload(), nil
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				t.Error("send after canceled SQL lookup")
				return whatsmeow.SendResponse{}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			quote := ""
			if phase == "quote" {
				quote = "Q1"
			}
			done := make(chan string, 1)
			go func() {
				_, status, _ := b.sendWhatsAppMessage(ctx, b.persistOutbound, outboundReplyGroup.String(), "reply", "unused.png", quote, "", "", nil)
				done <- status
			}()
			// WaitCount proves QueryRowContext entered the real pool wait.
			deadline := time.Now().Add(2 * time.Second)
			for b.Store.db.Stats().WaitCount == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if b.Store.db.Stats().WaitCount == 0 {
				t.Fatal("SQL lookup never waited")
			}
			cancel()
			select {
			case status := <-done:
				if !strings.Contains(status, "context canceled") {
					t.Errorf("status=%s", status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SQL lookup ignored request cancellation")
			}
		})
	}
}
