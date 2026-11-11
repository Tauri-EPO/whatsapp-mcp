package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestSendDocumentAndMediaQuotes(t *testing.T) {
	for _, ext := range []string{"png", "mp4", "ogg"} {
		for _, document := range []bool{false, true} {
			for _, sameChat := range []bool{false, true} {
				t.Run(ext+"/document="+strconv.FormatBool(document)+"/same_chat="+strconv.FormatBool(sameChat), func(t *testing.T) {
					ms := newTestMessageStore(t)
					client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
					b := testBridge(t, client, ms, testLogger())
					b.Connected = func() bool { return true }
					root := t.TempDir()
					b.MediaRoots = []string{root}
					path := filepath.Join(root, "original."+ext)
					data := []byte("original bytes")
					if ext == "ogg" {
						data = append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000, []byte{0x78})...)
					}
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
					const chat = "120363000000000001@g.us"
					quoteChat := chat
					if !sameChat {
						quoteChat = "5511999999999@s.whatsapp.net"
					}
					ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
					if err := ms.StoreChat(quoteChat, "Alice", ts); err != nil {
						t.Fatal(err)
					}
					if err := ms.StoreMessage(storedMessage{ID: "QUOTE", ChatJID: quoteChat, Sender: selfPhone.String(), Content: "same chat preview", Timestamp: ts}); err != nil {
						t.Fatal(err)
					}
					uploads, sends := 0, 0
					wantKind := map[string]string{"png": "image", "mp4": "video", "ogg": "audio"}[ext]
					wantUpload := map[string]whatsmeow.MediaType{"png": whatsmeow.MediaImage, "mp4": whatsmeow.MediaVideo, "ogg": whatsmeow.MediaAudio}[ext]
					if document {
						wantKind, wantUpload = "document", whatsmeow.MediaDocument
					}
					network := messageSendNetwork{
						connected: func() bool { return true },
						upload: func(_ context.Context, got []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
							uploads++
							if kind != wantUpload || !bytes.Equal(got, data) {
								t.Fatalf("upload=%s want=%s bytes changed=%t", kind, wantUpload, !bytes.Equal(got, data))
							}
							return whatsmeow.UploadResponse{URL: fixtureMediaURL, DirectPath: "/fake", MediaKey: []byte("key"), FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc"), FileLength: uint64(len(got))}, nil
						},
						send: func(_ context.Context, _ types.JID, packet *waE2E.Message) (whatsmeow.SendResponse, error) {
							sends++
							if kind, _ := mediaPartOf(packet); kind != wantKind {
								t.Fatalf("wire kind=%s want=%s", kind, wantKind)
							}
							q := *mediaContextInfo(packet)
							preview := "fallback"
							if sameChat {
								preview = "same chat preview"
							}
							if q.GetStanzaID() != "QUOTE" || q.GetQuotedMessage().GetConversation() != preview {
								t.Fatalf("quote=%v", q)
							}
							if !sameChat && q.GetParticipant() != "" {
								t.Fatal("cross-chat sender leaked")
							}
							if document {
								doc := packet.GetDocumentMessage()
								if doc.GetFileName() != "original."+ext || doc.GetMimetype() == "" || doc.GetCaption() != "caption" {
									t.Fatalf("document=%v", doc)
								}
							}
							return whatsmeow.SendResponse{ID: "SENT", Timestamp: ts}, nil
						},
					}
					b.Send = func(ctx context.Context, to, text, path, qid, qs, qc string, mentions []string) (bool, string, sentMessage) {
						return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, text, path, qid, qs, qc, mentions, network)
					}
					body, err := json.Marshal(map[string]any{"recipient": chat, "media_path": path, "message": "caption", "as_document": document, "quoted_message_id": "QUOTE", "quoted_content": "fallback"})
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", bytes.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
					rec := httptest.NewRecorder()
					b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
					if rec.Code != 200 || uploads != 1 || sends != 1 {
						t.Fatalf("status=%d %s uploads=%d sends=%d", rec.Code, rec.Body.String(), uploads, sends)
					}
					var kind, quoteID string
					if err := ms.db.QueryRow("SELECT media_type, quoted_message_id FROM messages WHERE id=? AND chat_jid=?", "SENT", chat).Scan(&kind, &quoteID); err != nil {
						t.Fatal(err)
					}
					if kind != wantKind || quoteID != "QUOTE" {
						t.Fatalf("stored kind=%s quote=%s", kind, quoteID)
					}
				})
			}
		}
	}
}

func TestSendDocumentDeniedBeforeEffects(t *testing.T) {
	for _, denial := range []string{"read_only", "chat", "tool", "path", "sticker_conflict"} {
		t.Run(denial, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.MediaRoots = []string{t.TempDir()}
			b.Send = func(context.Context, string, string, string, string, string, string, []string) (bool, string, sentMessage) {
				t.Fatal("denied send reached backend")
				return false, "", sentMessage{}
			}
			switch denial {
			case "read_only":
				b.ReadOnly = readOnlyPolicy{enabled: true}
			case "chat":
				b.Policy = b.runtimeChatPolicy(parseChatPolicy("5511999999999@s.whatsapp.net"))
			case "tool":
				var err error
				b.Tools, err = newToolPolicy("send_message", "send_message,send_file,send_audio_message")
				if err != nil {
					t.Fatal(err)
				}
			}
			body := `{"recipient":"120363000000000001@g.us","media_path":"/outside/photo.png","as_document":true,"quoted_message_id":"QUOTE"}`
			if denial == "sticker_conflict" {
				body = strings.Replace(body, `"as_document":true`, `"as_document":true,"as_sticker":true`, 1)
			}
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			want := 403
			if denial == "sticker_conflict" {
				want = 400
			}
			if rec.Code != want {
				t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCachedMediaAsDocument(t *testing.T) {
	for _, tc := range []struct{ kind, name, title, wantName, wantTitle string }{
		{"image", "image_20261101_000000_SOURCE.jpg", "", "attachment", "attachment"},
		{"document", "report.pdf", "Original title", "report.pdf", "Original title"},
		{"audio", "voice.ogg", "", "attachment", "attachment"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
			data := []byte("\xff\xd8\xfffake")
			b.MediaStorage = secondReviewMediaStorage{data: data}
			if err := b.Store.StoreMessage(storedMessage{ID: "SOURCE", ChatJID: testGroupJID, Sender: selfPhone.String(), Timestamp: time.Now(), MediaType: tc.kind, Filename: tc.name}); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), documentSendKey{}, true)
			ctx, path, cleanup, err := b.resolveSendMedia(ctx, "whatsapp://media/"+testGroupJID+"/SOURCE")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if tc.title != "" {
				source := ctx.Value(forwardSourceKey{}).(*forwardSource)
				source.presentation = &mediaPresentation{Title: proto.String(tc.title)}
			}
			kind, mimeType, err := forwardMediaType(ctx, path, data)
			if err != nil || kind != whatsmeow.MediaDocument {
				t.Fatalf("kind=%s err=%v", kind, err)
			}
			packet, _, err := buildForwardMedia(ctx, kind, mimeType, path, data, whatsmeow.UploadResponse{}, "caption", outboundQuote{id: "QUOTE"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			doc := packet.GetDocumentMessage()
			wantName, wantTitle := tc.wantName, tc.wantTitle
			if tc.kind != "document" {
				extensions, err := mime.ExtensionsByType(mimeType)
				if err != nil || len(extensions) == 0 {
					t.Fatalf("extensions: %v", err)
				}
				wantName, wantTitle = "attachment"+extensions[0], "attachment"+extensions[0]
			}
			if doc == nil || doc.GetFileName() != wantName || doc.GetTitle() != wantTitle || doc.GetContextInfo().GetStanzaID() != "QUOTE" {
				t.Fatalf("document=%v", doc)
			}
		})
	}
}

func TestDocumentRequiresMediaPath(t *testing.T) {
	b := testBridge(t, nil, newTestMessageStore(t), testLogger())
	b.Send = func(context.Context, string, string, string, string, string, string, []string) (bool, string, sentMessage) {
		t.Fatal("invalid document reached sender")
		return false, "", sentMessage{}
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"120363000000000001@g.us","message":"text","as_document":true}`))
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "as_document requires media_path") {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
}
