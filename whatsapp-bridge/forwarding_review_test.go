package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"math"
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

func TestOwnForwardLabelsWireAndArchive(t *testing.T) {
	for _, kind := range []string{"text", "image", "video", "audio", "document", "sticker"} {
		for _, original := range []*uint32{nil, proto.Uint32(0), proto.Uint32(4), proto.Uint32(math.MaxUint32)} {
			t.Run(kind+"/"+func() string {
				if original == nil {
					return "unknown"
				}
				return strconv.FormatUint(uint64(*original), 10)
			}(), func(t *testing.T) {
				ms := newTestMessageStore(t)
				client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
				b := testBridge(t, client, ms, testLogger())
				b.Connected = func() bool { return true }
				ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
				storedKind := kind
				data := []byte("file")
				ext := map[string]string{"image": ".jpg", "video": ".mp4", "audio": ".ogg", "document": ".pdf", "sticker": ".webp"}[kind]
				if kind == "text" {
					storedKind = ""
				}
				if kind == "audio" {
					data = append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000, []byte{0x78})...)
				}
				if kind == "sticker" {
					data = stickerFixture(false)
				}
				path := filepath.Join(t.TempDir(), "cached"+ext)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreChat(efChat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "SOURCE", ChatJID: efChat, Sender: selfPhone.String(), Timestamp: ts, Content: "forwarded text", MediaType: storedKind, Filename: "source" + ext, ForwardingScore: original}); err != nil {
					t.Fatal(err)
				}
				want := uint32(1)
				if original != nil {
					want = *original
					if want < math.MaxUint32 {
						want++
					}
				}
				network := messageSendNetwork{connected: func() bool { return true }, upload: func(_ context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					return whatsmeow.UploadResponse{FileLength: uint64(len(data))}, nil
				}, send: func(_ context.Context, _ types.JID, packet *waE2E.Message) (whatsmeow.SendResponse, error) {
					info := sharedContextInfo(packet)
					if !info.GetIsForwarded() || info.GetForwardingScore() != want {
						t.Fatalf("wire forwarding=%v", info)
					}
					return whatsmeow.SendResponse{ID: "SENT", Timestamp: ts}, nil
				}}
				b.Send = func(ctx context.Context, to, text, path, qid, qs, qc string, mentions []string) (bool, string, sentMessage) {
					return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, text, path, qid, qs, qc, mentions, network)
				}
				b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
					return true, storedKind, "", path, nil
				}
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/forward", strings.NewReader(`{"chat_jid":"`+efChat+`","message_id":"SOURCE","to_chat_jid":"120363000000000001@g.us"}`))
				req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
				rec := httptest.NewRecorder()
				b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
				}
				var score uint32
				if err := ms.db.QueryRow("SELECT forwarding_score FROM messages WHERE id=? AND chat_jid=?", "SENT", testGroupJID).Scan(&score); err != nil || score != want {
					t.Fatalf("archive score=%d want=%d err=%v", score, want, err)
				}
			})
		}
	}
}

func TestCachedPNGDocumentNameReachesWireArchiveAndCache(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	root, err := os.OpenRoot(storeDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	ms := newTestMessageStore(t)
	client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
	b := testBridge(t, client, ms, testLogger())
	b.StoreRoot = root
	b.MediaAutoDownload = true
	b.Connected = func() bool { return true }
	ts := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	data := []byte("\x89PNG\r\n\x1a\nfake original PNG")
	if err := ms.StoreChat(testGroupJID, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "SOURCE", ChatJID: testGroupJID, Sender: selfPhone.String(), Timestamp: ts, MediaType: "image", Filename: mediaFileName("image", ts, "SOURCE", "")}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(chatMediaDir(testGroupJID), 0700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(chatMediaDir(testGroupJID), mediaFileName("image", ts, "SOURCE", ""))
	if err := os.WriteFile(sourcePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	network := messageSendNetwork{connected: func() bool { return true }, cache: b.cacheOutboundMedia, upload: func(_ context.Context, got []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		if kind != whatsmeow.MediaDocument || !bytes.Equal(got, data) {
			t.Fatal("wrong upload kind/bytes")
		}
		return whatsmeow.UploadResponse{URL: fixtureMediaURL, MediaKey: []byte("key"), FileSHA256: hash[:], FileEncSHA256: []byte("enc"), FileLength: uint64(len(got))}, nil
	}, send: func(_ context.Context, _ types.JID, packet *waE2E.Message) (whatsmeow.SendResponse, error) {
		doc := packet.GetDocumentMessage()
		if doc.GetFileName() != "attachment.png" || doc.GetMimetype() != "image/png" {
			t.Fatalf("wire document=%v", doc)
		}
		return whatsmeow.SendResponse{ID: "SENT", Timestamp: ts}, nil
	}}
	b.Send = func(ctx context.Context, to, text, path, qid, qs, qc string, mentions []string) (bool, string, sentMessage) {
		ctx, path, cleanup, err := b.resolveSendMedia(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, text, path, qid, qs, qc, mentions, network)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"120363000000000001@g.us","media_path":"whatsapp://media/120363000000000001@g.us/SOURCE","as_document":true}`))
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	var name, kind, raw string
	var score sql.NullInt64
	if err := ms.db.QueryRow("SELECT filename,media_type,media_presentation,forwarding_score FROM messages WHERE id=? AND chat_jid=?", "SENT", testGroupJID).Scan(&name, &kind, &raw, &score); err != nil {
		t.Fatal(err)
	}
	var presentation map[string]any
	if err := json.Unmarshal([]byte(raw), &presentation); err != nil {
		t.Fatal(err)
	}
	if name != "attachment.png" || kind != "document" || presentation["mime"] != "image/png" || score.Valid {
		t.Fatalf("archive name=%s kind=%s presentation=%v score=%v", name, kind, presentation, score)
	}
	cached := filepath.Join(testGroupJID, mediaFileName("document", ts, "SENT", name))
	got, err := root.ReadFile(cached)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("outbound cache %s bytes=%x err=%v", cached, got, err)
	}
}

func TestCachedDocumentRefusesMismatchedPresentation(t *testing.T) {
	for _, kind := range []string{"image", "video", "audio", "sticker"} {
		t.Run(kind, func(t *testing.T) {
			source := &forwardSource{mediaType: kind, presentation: &mediaPresentation{Hash: strings.Repeat("0", 64)}}
			ctx := context.WithValue(t.Context(), documentSendKey{}, true)
			ctx = context.WithValue(ctx, forwardSourceKey{}, source)
			data := []byte("changed cached bytes")
			if _, _, err := forwardMediaType(ctx, "cached", data); err == nil {
				t.Fatal("classification accepted mismatched cached bytes")
			}
			if packet, _, err := buildForwardMedia(ctx, whatsmeow.MediaDocument, "image/png", "cached", data, whatsmeow.UploadResponse{}, "", outboundQuote{}, nil); err == nil || packet != nil {
				t.Fatal("document builder accepted mismatched cached bytes")
			}
		})
	}
}
