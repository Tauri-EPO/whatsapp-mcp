package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type fullSendMedia interface {
	whatsmeow.DownloadableMessage
	GetURL() string
	GetFileLength() uint64
}

func TestFullRESTSendPersistsWireContentAndUpload(t *testing.T) {
	for _, kind := range []string{"text", "image", "document", "video", "audio"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			// Forward lookup can resolve PN -> LID; the reverse map is deliberately
			// absent, so persisting the wire LID would create the wrong chat.
			lids := &mockLIDStore{lidByPN: map[types.JID]types.JID{registeredJID: registeredLID}}
			ask := registeredWithoutNinthDigit()
			b, _, _ := sendRecipientBridge(t, lids, ask)
			b.Send = b.sendBackend()
			b.Policy = parseChatPolicy(dialledNumber + "," + registeredNumber)
			ts := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			if err := b.Store.StoreChat(registeredJID.String(), "Existing contact", ts.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := b.Store.UpdateChatEphemeralSettings(registeredJID.String(), 3600, 123000); err != nil {
				t.Fatal(err)
			}
			mediaData := []byte("synthetic file bytes")
			mediaPath, filename := "", ""
			wantUpload := whatsmeow.MediaImage
			if kind != "text" {
				ext := map[string]string{"image": "jpg", "document": "pdf", "video": "mp4", "audio": "ogg"}[kind]
				filename = "sample." + ext
				mediaPath = filepath.Join(t.TempDir(), filename)
				b.MediaRoots = []string{filepath.Dir(mediaPath)}
				if kind == "audio" {
					mediaData = append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*7, []byte{0xfc})...)
				}
				if err := os.WriteFile(mediaPath, mediaData, 0o600); err != nil {
					t.Fatal(err)
				}
				wantUpload = map[string]whatsmeow.MediaType{"image": whatsmeow.MediaImage, "document": whatsmeow.MediaDocument, "video": whatsmeow.MediaVideo, "audio": whatsmeow.MediaAudio}[kind]
			}
			up := outboundUpload()
			uploads, sends := 0, 0
			b.uploadMedia = func(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads++
				if kind == "text" || !bytes.Equal(data, mediaData) || mediaType != wantUpload {
					t.Fatalf("upload type=%s bytes=%q", mediaType, data)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("upload lost the HTTP request deadline")
				}
				return up, nil
			}
			b.sendMessage = func(ctx context.Context, jid types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
				sends++
				if jid != registeredLID {
					t.Fatalf("wire recipient=%s, want registered LID=%s", jid, registeredLID)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("send lost the HTTP request deadline")
				}
				var info *waE2E.ContextInfo
				if kind == "text" {
					if message.GetExtendedTextMessage().GetText() != "hello" {
						t.Fatalf("text stanza=%v", message)
					}
					info = message.GetExtendedTextMessage().GetContextInfo()
				} else {
					var downloadable fullSendMedia
					switch kind {
					case "image":
						downloadable = message.GetImageMessage()
					case "document":
						downloadable = message.GetDocumentMessage()
					case "video":
						downloadable = message.GetVideoMessage()
					case "audio":
						downloadable = message.GetAudioMessage()
					}
					if downloadable == nil || downloadable.GetURL() != up.URL || downloadable.GetDirectPath() != up.DirectPath || !bytes.Equal(downloadable.GetMediaKey(), up.MediaKey) || !bytes.Equal(downloadable.GetFileSHA256(), up.FileSHA256) || !bytes.Equal(downloadable.GetFileEncSHA256(), up.FileEncSHA256) || downloadable.GetFileLength() != up.FileLength {
						t.Fatalf("stanza lost upload fields: %v", message)
					}
					info = *mediaContextInfo(message)
					if kind == "audio" && (!message.GetAudioMessage().GetPTT() || message.GetAudioMessage().GetSeconds() != 7) {
						t.Fatalf("voice note=%v", message.GetAudioMessage())
					}
				}
				wantMentions := []string{registeredJID.String(), registeredLID.String()}
				if kind == "audio" {
					wantMentions = nil
				}
				if info == nil || info.GetStanzaID() != "ORIGINAL" || info.GetParticipant() != registeredLID.String() || info.GetQuotedMessage().GetConversation() != "quoted text" || !reflect.DeepEqual(info.GetMentionedJID(), wantMentions) || info.GetExpiration() != 3600 || info.GetEphemeralSettingTimestamp() != 123000 {
					t.Fatalf("quote/mentions/ephemeral context=%v", info)
				}
				return whatsmeow.SendResponse{ID: "FULL1", Timestamp: ts}, nil
			}
			payload := SendMessageRequest{Recipient: "+55 (11) 98888-7777", Message: "hello", MediaPath: mediaPath, QuotedMessageID: "ORIGINAL", QuotedSenderJID: "+55 (11) 8888-7777", QuotedContent: "quoted text", Mentions: []string{"+55 11 8888-7777", "", registeredJID.String() + "@g.us"}}
			body, _ := json.Marshal(payload)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
			var response SendMessageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			wantUploads := 1
			if kind == "text" {
				wantUploads = 0
			}
			if rec.Code != http.StatusOK || !response.Success || response.MessageID != "FULL1" || response.ChatJID != registeredJID.String() || response.Timestamp != ts.Format(time.RFC3339) || sends != 1 || uploads != wantUploads || len(ask.calls) != 1 {
				t.Fatalf("status=%d response=%+v sends=%d uploads=%d queries=%v", rec.Code, response, sends, uploads, ask.calls)
			}
			var chat, content, sender, namespace, timestamp, quoted, mediaType, gotFilename, url, direct string
			var fromMe bool
			var key, sha, enc []byte
			var length uint64
			if err := b.Store.db.QueryRow(`SELECT chat_jid, content, sender, sender_server, CAST(timestamp AS TEXT), quoted_message_id, is_from_me,
				COALESCE(media_type,''), COALESCE(filename,''), COALESCE(url,''), COALESCE(direct_path,''), media_key, file_sha256, file_enc_sha256, COALESCE(file_length,0)
				FROM messages WHERE id='FULL1'`).Scan(&chat, &content, &sender, &namespace, &timestamp, &quoted, &fromMe, &mediaType, &gotFilename, &url, &direct, &key, &sha, &enc, &length); err != nil {
				t.Fatal(err)
			}
			wantContent := "hello"
			if kind == "audio" {
				wantContent = ""
			}
			if chat != registeredJID.String() || content != wantContent || sender != "5511900000000" || namespace != types.DefaultUserServer || timestamp != dbTime(ts) || quoted != "ORIGINAL" || !fromMe {
				t.Fatalf("stored identity/content=%s %q %s %s %s %s %v", chat, content, sender, namespace, timestamp, quoted, fromMe)
			}
			if kind != "text" && (mediaType != kind || gotFilename != filename || url != up.URL || direct != up.DirectPath || !bytes.Equal(key, up.MediaKey) || !bytes.Equal(sha, up.FileSHA256) || !bytes.Equal(enc, up.FileEncSHA256) || length != up.FileLength) {
				t.Fatalf("stored upload=%s %s %s %s %x %x %x %d", mediaType, gotFilename, url, direct, key, sha, enc, length)
			}
			var name string
			if err := b.Store.db.QueryRow("SELECT name FROM chats WHERE jid=?", registeredJID.String()).Scan(&name); err != nil || name != "Existing contact" {
				t.Fatalf("chat name=%s %v", name, err)
			}
			if kind != "text" {
				b.mediaTransfer = func(_ context.Context, message whatsmeow.DownloadableMessage, path string) (int64, error) {
					if !bytes.Equal(message.GetMediaKey(), up.MediaKey) || !bytes.Equal(message.GetFileSHA256(), up.FileSHA256) || !bytes.Equal(message.GetFileEncSHA256(), up.FileEncSHA256) {
						t.Fatal("download did not use stored upload credentials")
					}
					return writeLikeDownloadToPath(path, mediaData)
				}
				ok, gotType, _, path, err := b.downloadMedia(context.Background(), "FULL1", registeredJID.String())
				if err != nil || !ok || gotType != kind {
					t.Fatalf("download ok=%v type=%s error=%v", ok, gotType, err)
				}
				got, err := os.ReadFile(path) //nolint:gosec // downloadMedia returned a file in this test's temporary store
				if err != nil || !bytes.Equal(got, mediaData) {
					t.Fatalf("download bytes=%q error=%v", got, err)
				}
			}
		})
	}
}

func TestFullSendFailureHasNoStoredMessage(t *testing.T) {
	for _, stage := range []string{"offline", "upload", "send"} {
		t.Run(stage, func(t *testing.T) {
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{registeredJID: registeredLID}}, registeredWithoutNinthDigit())
			b.Connected = func() bool { return stage != "offline" }
			path := filepath.Join(t.TempDir(), "sample.pdf")
			if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			uploads, sends := 0, 0
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads++
				if stage == "upload" {
					return whatsmeow.UploadResponse{}, errors.New("upload unavailable")
				}
				return outboundUpload(), nil
			}
			b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
				sends++
				return whatsmeow.SendResponse{}, errors.New("send unavailable")
			}
			ok, _, _ := b.sendWhatsAppMessage(context.Background(), b.persistOutbound, registeredNumber, "hello", path, "", "", "", nil)
			wantUploads, wantSends := 1, 0
			switch stage {
			case "offline":
				wantUploads = 0
			case "send":
				wantSends = 1
			}
			var count int
			if err := b.Store.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 0 || ok || uploads != wantUploads || sends != wantSends {
				t.Fatalf("success=%v uploads=%d sends=%d rows=%d error=%v", ok, uploads, sends, count, err)
			}
		})
	}
}

func TestFullSendArchiveFailureKeepsRemoteSuccess(t *testing.T) {
	b, _, _ := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{registeredJID: registeredLID}}, registeredWithoutNinthDigit())
	sends, persists := 0, 0
	b.sendMessage = func(_ context.Context, jid types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		sends++
		if jid != registeredLID || message.GetConversation() != "hi" {
			t.Fatalf("wire recipient=%s message=%v", jid, message)
		}
		return whatsmeow.SendResponse{ID: "OUT-ARCHIVE1", Timestamp: time.Now()}, nil
	}
	persist := func(jid types.JID, sent sentMessage, content string, _ outboundMedia, _ string) (string, error) {
		persists++
		if jid != registeredJID || sent.ID != "OUT-ARCHIVE1" || content != "hi" {
			t.Fatalf("archive arguments: recipient=%s sent=%v content=%q", jid, sent, content)
		}
		return jid.String(), errors.New("synthetic archive failure")
	}
	b.Send = func(ctx context.Context, recipient, message, mediaPath, quotedID, quotedSender, quotedContent string, mentions []string) (bool, string, sentMessage) {
		return b.sendWhatsAppMessage(ctx, persist, recipient, message, mediaPath, quotedID, quotedSender, quotedContent, mentions)
	}
	rec := postSend(b.newRESTMux(8080, sendRecipientToken), dialledNumber)
	var response SendMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !response.Success || response.MessageID != "OUT-ARCHIVE1" || response.ChatJID != registeredJID.String() || !strings.Contains(response.Message, outboundArchiveWarning) || sends != 1 || persists != 1 {
		t.Fatalf("status=%d body=%s sends=%d persists=%d", rec.Code, rec.Body.String(), sends, persists)
	}
	var count int
	if err := b.Store.db.QueryRow("SELECT count(*) FROM messages WHERE id='OUT-ARCHIVE1'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rows=%d error=%v", count, err)
	}
}

func TestFullSendWithoutLIDBackendUsesPhone(t *testing.T) {
	b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
	b.Client.Store.LIDs = nil
	b.Send = b.sendBackend()
	b.sendMessage = func(_ context.Context, jid types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		if jid != registeredJID || message.GetConversation() != "hi" {
			t.Fatalf("fallback wire=%s %v", jid, message)
		}
		return whatsmeow.SendResponse{ID: "NILMAP1", Timestamp: time.Now()}, nil
	}
	if rec := postSend(b.newRESTMux(8080, sendRecipientToken), dialledNumber); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var chat string
	if err := b.Store.db.QueryRow("SELECT chat_jid FROM messages WHERE id='NILMAP1'").Scan(&chat); err != nil || chat != registeredJID.String() {
		t.Fatalf("phone fallback persistence=%s error=%v", chat, err)
	}
	var wrong string
	if err := b.Store.db.QueryRow("SELECT chat_jid FROM messages WHERE chat_jid=?", dialledJID.String()).Scan(&wrong); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dialled spelling created a separate row: %s %v", wrong, err)
	}
}

func TestFullRESTForwardResolvesBeforeMediaAndPersists(t *testing.T) {
	for _, tc := range []struct {
		name, sourceID string
		media, denied  bool
	}{
		{"text", "THEIRS", false, false},
		{"image", "PIC", true, false},
		{"registered deny before download and upload", "PIC", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := registeredWithoutNinthDigit()
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{lidByPN: map[types.JID]types.JID{registeredJID: registeredLID}}, ask)
			b.Store = seedEditStore(t)
			b.Send = b.sendBackend()
			b.Policy = parseChatPolicy(efChat + "," + dialledNumber)
			if !tc.denied {
				b.Policy = parseChatPolicy(efChat + "," + dialledNumber + "," + registeredNumber)
			}
			mediaPath := filepath.Join(t.TempDir(), "sample.jpg")
			if err := os.WriteFile(mediaPath, []byte("synthetic image"), 0o600); err != nil {
				t.Fatal(err)
			}
			downloads, uploads, sends := 0, 0, 0
			b.DownloadMedia = func(context.Context, string, string) (bool, string, string, string, error) {
				downloads++
				return true, "image", "sample.jpg", mediaPath, nil
			}
			up := outboundUpload()
			b.uploadMedia = func(_ context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads++
				if !bytes.Equal(data, []byte("synthetic image")) || mediaType != whatsmeow.MediaImage {
					t.Fatalf("forward upload=%q %s", data, mediaType)
				}
				return up, nil
			}
			b.sendMessage = func(_ context.Context, jid types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
				sends++
				if jid != registeredLID {
					t.Fatalf("forward wire recipient=%s", jid)
				}
				if tc.media && (message.GetImageMessage().GetURL() != up.URL || !bytes.Equal(message.GetImageMessage().GetMediaKey(), up.MediaKey)) {
					t.Fatalf("forward image=%v", message)
				}
				return whatsmeow.SendResponse{ID: "FORWARDED1", Timestamp: time.Now()}, nil
			}
			body, _ := json.Marshal(forwardRequest{ChatJID: efChat, MessageID: tc.sourceID, ToChatJID: "+55 (11) 98888-7777"})
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/forward", string(body), sendRecipientToken))
			wantStatus, wantSends, wantMedia := http.StatusOK, 1, 0
			if tc.media {
				wantMedia = 1
			}
			if tc.denied {
				wantStatus, wantSends, wantMedia = http.StatusForbidden, 0, 0
			}
			if rec.Code != wantStatus || sends != wantSends || downloads != wantMedia || uploads != wantMedia || len(ask.calls) != 1 {
				t.Fatalf("status=%d body=%s sends=%d download=%d upload=%d queries=%v", rec.Code, rec.Body.String(), sends, downloads, uploads, ask.calls)
			}
			var chat string
			err := b.Store.db.QueryRow("SELECT chat_jid FROM messages WHERE id='FORWARDED1'").Scan(&chat)
			if tc.denied {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("refused forward stored a row: %s %v", chat, err)
				}
			} else if err != nil || chat != registeredJID.String() {
				t.Fatalf("forward stored recipient=%s error=%v", chat, err)
			}
		})
	}
}
