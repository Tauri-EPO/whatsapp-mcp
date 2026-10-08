package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestForwardPreservesStoredKindAndPresentation(t *testing.T) {
	ogg := append(oggPage(0, 0, opusHead(0, 48000)), oggPage(1, 48000*2, []byte{0xfc})...)
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name, kind, mime, filename, title string
			inputMIME, sentName, wireKind     string
			data                              []byte
			ptt, animated                     bool
			legacy                            bool
			refused                           bool
			refreshed                         bool
			mimeOnlyReplay                    bool
			missingAudioFields                bool
			boundedNameReplay, staleCache     bool
		}{
			{name: "PNG document", kind: "document", mime: "image/png", filename: "image.png", title: "Quarterly report", data: []byte("\x89PNG\r\n\x1a\nfake")},
			{name: "document path name", kind: "document", mime: "application/pdf", filename: `C:\fake\private\report.pdf`, title: "Report", data: []byte("%PDF-1.7 fake")},
			{name: "named document with generated prefix", kind: "document", mime: "application/pdf", filename: "document_report.pdf", title: "Report", data: []byte("%PDF-1.7 fake")},
			{name: "unnamed document", kind: "document", mime: "application/pdf", data: []byte("%PDF-1.7 fake")},
			{name: "MP3 normal audio", kind: "audio", mime: "audio/mpeg", data: []byte("ID3 fake audio")},
			{name: "Opus music", kind: "audio", mime: "audio/ogg; codecs=opus", inputMIME: "audio/ogg; codecs=opus", data: ogg},
			{name: "Opus voice note", kind: "audio", mime: "audio/ogg; codecs=opus", inputMIME: "audio/ogg; codecs=opus", data: ogg, ptt: true},
			{name: "Opus music missing duration and waveform", kind: "audio", mime: "audio/ogg; codecs=opus", data: ogg, missingAudioFields: true},
			{name: "static sticker", kind: "sticker", mime: "image/webp", data: []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")},
			{name: "animated sticker", kind: "sticker", mime: "image/webp", data: []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake"), animated: true},
			{name: "legacy MP3", kind: "audio", mime: "audio/mpeg", data: []byte("ID3 fake audio"), legacy: true},
			{name: "legacy MP3 without ID3", kind: "audio", mime: "audio/mpeg", data: []byte("\xff\xfb\x90\x64fake MPEG audio"), legacy: true},
			{name: "legacy Opus", kind: "audio", mime: "audio/ogg; codecs=opus", data: ogg, legacy: true, ptt: true},
			{name: "legacy other Ogg", kind: "audio", mime: "audio/ogg", data: []byte("OggS\x00fake non-Opus container"), legacy: true},
			{name: "legacy WAV", kind: "audio", mime: "audio/wav", data: []byte("RIFF\x10\x00\x00\x00WAVEfake"), legacy: true},
			{name: "legacy AAC", kind: "audio", mime: "audio/aac", data: []byte("\xff\xf1\x50\x80\x01\x7f\xfcfake"), legacy: true},
			{name: "legacy truncated AAC", kind: "audio", data: []byte("\xff\xf1"), legacy: true, refused: true},
			{name: "legacy unnamed document", kind: "document", mime: "application/octet-stream", data: []byte("fake document"), legacy: true},
			{name: "legacy generated-prefix original", kind: "document", mime: "application/pdf", filename: "document_report.pdf", title: "document_report.pdf", data: []byte("%PDF-1.7 fake"), legacy: true},
			{name: "legacy ambiguous timestamp name", kind: "document", mime: "application/pdf", filename: "document_20260904_100000_report.pdf", sentName: "file", data: []byte("%PDF-1.7 fake"), legacy: true},
			{name: "legacy other message cache name", kind: "document", mime: "application/pdf", filename: "document_20260904_100000_OTHER1.pdf", sentName: "file", data: []byte("%PDF-1.7 fake"), legacy: true},
			{name: "original name matches cache pattern", kind: "document", mime: "application/pdf", filename: "document_20260904_100000_OTHER1.pdf", title: "Report", data: []byte("%PDF-1.7 fake")},
			{name: "legacy PNG behind sticker name", kind: "sticker", wireKind: "image", mime: "image/png", inputMIME: "image/webp", data: []byte("\x89PNG\r\n\x1a\nfake"), legacy: true},
			{name: "legacy MIME-only replay", kind: "document", mime: "application/pdf", filename: "report.pdf", data: []byte("%PDF-1.7 fake"), legacy: true, mimeOnlyReplay: true},
			{name: "legacy unknown audio", kind: "audio", data: []byte("unknown audio bytes"), legacy: true, refused: true},
			{name: "phone retry changes hash", kind: "audio", mime: "audio/mpeg", data: []byte("ID3 fake audio"), refreshed: true},
			{name: "bounded name retains PDF cache", kind: "document", mime: "application/pdf", filename: strings.Repeat("n", 201) + ".pdf", title: "Report", sentName: strings.Repeat("n", 196) + ".pdf", data: []byte("%PDF-1.7 fake"), legacy: true, boundedNameReplay: true},
			{name: "stale document cache refused", kind: "document", mime: "application/pdf", filename: "new.pdf", title: "New title", data: []byte("%PDF-1.7 old fake"), staleCache: true, refused: true},
			{name: "stale audio cache refused", kind: "audio", mime: "audio/mpeg", data: []byte("ID3 old fake"), staleCache: true, refused: true},
			{name: "stale sticker cache refused", kind: "sticker", mime: "image/webp", data: []byte("RIFF\x10\x00\x00\x00WEBPVP8 old fake"), staleCache: true, refused: true},
		} {
			t.Run(fmt.Sprintf("%t/%s", batch, tc.name), func(t *testing.T) {
				t.Setenv(storeDirEnv, t.TempDir())
				wantKind := tc.wireKind
				if wantKind == "" {
					wantKind = tc.kind
				}
				wantName := tc.sentName
				if wantName == "" {
					wantName = outboundFileName(tc.filename)
				}
				inputMIME := tc.inputMIME
				if inputMIME == "" {
					inputMIME = tc.mime
				}
				ms := newTestMessageStore(t)
				client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
				b := testBridge(t, client, ms, testLogger())
				b.Connected = func() bool { return true }
				ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
				if err := ms.StoreChat(efChat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreChat("120363000000000001@g.us", "Bob", ts); err != nil {
					t.Fatal(err)
				}
				if err := ms.UpdateChatEphemeralSettings("120363000000000001@g.us", 86400, 1710000000); err != nil {
					t.Fatal(err)
				}
				common := testUpload()
				common.FileLength = uint64(len(tc.data))
				hash := sha256.Sum256(tc.data)
				common.FileSHA256 = hash[:]
				var packet *waE2E.Message
				switch tc.kind {
				case "document":
					packet = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String(inputMIME), FileName: proto.String(tc.filename), Title: proto.String(tc.title), URL: &common.URL, MediaKey: common.MediaKey, FileSHA256: common.FileSHA256, FileEncSHA256: common.FileEncSHA256, FileLength: &common.FileLength}}
				case "audio":
					packet = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String(inputMIME), PTT: proto.Bool(tc.ptt), Seconds: proto.Uint32(2), Waveform: bytes.Repeat([]byte{1}, 64), URL: &common.URL, MediaKey: common.MediaKey, FileSHA256: common.FileSHA256, FileEncSHA256: common.FileEncSHA256, FileLength: &common.FileLength}}
					if tc.missingAudioFields {
						packet.AudioMessage.Seconds, packet.AudioMessage.Waveform = nil, nil
					}
					if tc.refreshed {
						packet.AudioMessage.Mimetype, packet.AudioMessage.PTT = proto.String("audio/ogg; codecs=opus"), proto.Bool(true)
					}
				case "sticker":
					packet = &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String(inputMIME), IsAnimated: proto.Bool(tc.animated), URL: &common.URL, MediaKey: common.MediaKey, FileSHA256: common.FileSHA256, FileEncSHA256: common.FileEncSHA256, FileLength: &common.FileLength}}
				}
				ex := extractMessage(packet, ts, "KIND1")
				if tc.legacy {
					if tc.boundedNameReplay {
						ex.filename = tc.filename // exact filename kept by the old writer
					}
					if tc.kind == "document" && ex.filename == "" {
						ex.filename = "document_" + ts.Format("20060102_150405") + "_KIND1"
					}
					if err := ms.StoreMessage("KIND1", efChat, "x", "", ts, false, tc.kind, ex.filename, common.URL, common.MediaKey, common.FileSHA256, common.FileEncSHA256, common.FileLength, ""); err != nil {
						t.Fatal(err)
					}
				} else {
					replayWriter(t, ms, batch, func(w messageWriter) error {
						return persistMessage(w, "KIND1", efChat, "x", ts, false, ex, false, testLogger())
					})
				}
				if tc.mimeOnlyReplay || tc.boundedNameReplay || tc.staleCache {
					replay := proto.Clone(packet).(*waE2E.Message)
					if tc.mimeOnlyReplay {
						replay.DocumentMessage.FileName, replay.DocumentMessage.Title = nil, nil
					}
					if tc.staleCache {
						switch tc.kind {
						case "document":
							replay.DocumentMessage.FileSHA256 = bytes.Repeat([]byte{9}, 32)
						case "audio":
							replay.AudioMessage.FileSHA256 = bytes.Repeat([]byte{9}, 32)
						case "sticker":
							replay.StickerMessage.FileSHA256 = bytes.Repeat([]byte{9}, 32)
						}
					}
					replayWriter(t, ms, batch, func(w messageWriter) error {
						return persistMessage(w, "KIND1", efChat, "x", ts, false, extractMessage(replay, ts, "KIND1"), false, testLogger())
					})
				}
				folder := chatMediaDir(efChat)
				if err := os.MkdirAll(folder, 0o750); err != nil {
					t.Fatal(err)
				}
				cached := filepath.Join(folder, mediaFileName(tc.kind, ts, "KIND1", ex.filename))
				if err := os.WriteFile(cached, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
				b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
					t.Error("cache miss")
					return 0, fmt.Errorf("unexpected download")
				}
				calls, uploads := 0, 0
				network := messageSendNetwork{connected: func() bool { return true }, upload: func(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					uploads++
					want := whatsmeow.MediaImage
					if tc.kind == "document" {
						want = whatsmeow.MediaDocument
					}
					if tc.kind == "audio" {
						want = whatsmeow.MediaAudio
					}
					if kind != want || !bytes.Equal(data, tc.data) {
						t.Fatalf("upload=%s want=%s bytes changed=%t", kind, want, !bytes.Equal(data, tc.data))
					}
					return common, nil
				}, send: func(_ context.Context, _ types.JID, got *waE2E.Message) (whatsmeow.SendResponse, error) {
					calls++
					kind, part := mediaPartOf(got)
					if kind != wantKind {
						t.Fatalf("wire kind=%s want=%s", kind, wantKind)
					}
					ctx := part.(interface{ GetContextInfo() *waE2E.ContextInfo }).GetContextInfo()
					if ctx.GetExpiration() != 86400 || ctx.GetEphemeralSettingTimestamp() != 1710000000 {
						t.Fatalf("destination expiration lost for %s: %v", tc.kind, ctx)
					}
					if part.(interface{ GetMimetype() string }).GetMimetype() != tc.mime {
						t.Fatalf("wire MIME=%s want=%s", part.(interface{ GetMimetype() string }).GetMimetype(), tc.mime)
					}
					if doc := got.GetDocumentMessage(); doc != nil {
						wantTitle := tc.title
						if doc.GetFileName() != wantName || doc.GetTitle() != wantTitle {
							t.Fatalf("document name/title=%q/%q want=%q/%q", doc.GetFileName(), doc.GetTitle(), wantName, wantTitle)
						}
					}
					if audio := got.GetAudioMessage(); audio != nil {
						if audio.GetPTT() != tc.ptt {
							t.Fatalf("PTT=%t want=%t", audio.GetPTT(), tc.ptt)
						}
						if (tc.name == "legacy Opus" || tc.missingAudioFields) && (audio.GetSeconds() != 2 || len(audio.Waveform) != 64) {
							t.Fatal("Opus duration/waveform was not computed")
						}
						if !tc.legacy && !tc.refreshed && !tc.missingAudioFields && (audio.GetSeconds() != 2 || !bytes.Equal(audio.Waveform, bytes.Repeat([]byte{1}, 64))) {
							t.Fatal("audio presentation changed")
						}
					}
					if got.GetStickerMessage() != nil && got.GetStickerMessage().GetIsAnimated() != tc.animated {
						t.Fatal("sticker animation lost")
					}
					return whatsmeow.SendResponse{ID: "SENTKIND1", Timestamp: ts}, nil
				}}
				deps := forwardDeps{lookup: ms.messageContentLookup, download: b.downloadMedia, resolveRecipient: func(ctx context.Context, w http.ResponseWriter, to string) (string, bool) {
					return b.registeredRecipient(ctx, w, to, nil)
				}, send: func(ctx context.Context, to, text, path, qid, qs, qc string, mentions []string) (bool, string, sentMessage) {
					return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, text, path, qid, qs, qc, mentions, network)
				}}
				if tc.refreshed {
					deps.download = func(ctx context.Context, id, chat string) (bool, string, string, string, error) {
						ok, kind, filename, path, err := b.downloadMedia(ctx, id, chat)
						if err == nil {
							// Exercise the actual retry persistence helper while
							// forwarding is in flight, without a paired phone.
							storeRefreshedMedia(ms, id, chat, &MediaDownloader{URL: common.URL, MediaKey: common.MediaKey, FileSHA256: []byte("new plaintext sha"), FileEncSHA256: common.FileEncSHA256, FileLength: common.FileLength})
						}
						return ok, kind, filename, path, err
					}
				}
				code, response := efPost(t, handleForwardMessage(deps, chatPolicy{}), fmt.Sprintf(`{"chat_jid":%q,"message_id":"KIND1","to_chat_jid":"120363000000000001@g.us"}`, efChat))
				if tc.refused {
					if code != http.StatusBadGateway || calls != 0 || uploads != 0 {
						t.Fatalf("unknown audio was uploaded/sent: code=%d sends=%d uploads=%d", code, calls, uploads)
					}
					if tc.staleCache {
						source, found, err := ms.messageContentLookup("KIND1", efChat)
						if err != nil || !found {
							t.Fatal("stale-cache fixture missing")
						}
						ctx := context.WithValue(t.Context(), forwardSourceKey{}, &source)
						if wire, _, err := buildForwardMedia(ctx, whatsmeow.MediaDocument, "application/pdf", cached, tc.data, common, "", outboundQuote{}, nil); err == nil || wire != nil {
							t.Fatal("wire builder accepted presentation for different bytes")
						}
						data, err := os.ReadFile(cached) // #nosec G304 -- generated fixture cache path under t.TempDir, written above
						if err != nil || !bytes.Equal(data, tc.data) {
							t.Fatal("refusal modified the cache")
						}
						var rows int
						if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='SENTKIND1'").Scan(&rows); err != nil || rows != 0 {
							t.Fatal("refused cache produced an outbound row")
						}
					}
					return
				}
				if code != http.StatusOK || !response.Success || calls != 1 {
					t.Fatalf("forward=%d %+v sends=%d", code, response, calls)
				}
				var kind, name, presentation string
				if err := ms.db.QueryRow("SELECT media_type,filename,media_presentation FROM messages WHERE id='SENTKIND1'").Scan(&kind, &name, &presentation); err != nil {
					t.Fatal(err)
				}
				var p mediaPresentation
				if err := json.Unmarshal([]byte(presentation), &p); err != nil {
					t.Fatal(err)
				}
				if kind != wantKind || p.MIME != tc.mime {
					t.Fatalf("archive=%s/%s want=%s/%s", kind, p.MIME, wantKind, tc.mime)
				}
				if tc.kind == "document" && name != wantName {
					t.Fatalf("archive filename=%s", name)
				}
				if ok, _, _, path, err := b.downloadMedia(t.Context(), "KIND1", efChat); !ok || err != nil || path != cached {
					t.Fatalf("cache path=%s err=%v", path, err)
				}
				if code, response := purgeCall(t, b, fmt.Sprintf(`{"items":[{"chat_jid":%q,"message_id":"KIND1"}],"dry_run":false}`, efChat)); code != http.StatusOK || response.PurgedFiles != 1 {
					t.Fatalf("purge=%d %+v", code, response)
				}
			})
		}
	}
}
