package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func presentationPacket(kind string, p *mediaPresentation) *waE2E.Message {
	doc := fixtureDocument()
	switch kind {
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: &p.MIME, PTT: p.PTT, Seconds: p.Seconds, Waveform: p.Waveform, URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, FileSHA256: doc.FileSHA256, FileEncSHA256: doc.FileEncSHA256, FileLength: doc.FileLength}}
	case "sticker":
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: &p.MIME, IsAnimated: p.Animated, URL: doc.URL, DirectPath: doc.DirectPath, MediaKey: doc.MediaKey, FileSHA256: doc.FileSHA256, FileEncSHA256: doc.FileEncSHA256, FileLength: doc.FileLength}}
	default:
		doc.Mimetype, doc.FileName, doc.Title = &p.MIME, p.Name, p.Title
		return &waE2E.Message{DocumentMessage: doc}
	}
}

func TestMediaPresentationValidationIngressAndEgress(t *testing.T) {
	for _, tc := range []struct {
		name, kind, mime string
		change           func(*mediaPresentation)
		check            func(*mediaPresentation) bool
	}{
		{"waveform-megabyte", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Waveform = bytes.Repeat([]byte{1}, 1<<20) }, func(p *mediaPresentation) bool { return len(p.Waveform) == 0 }},
		{"waveform-63", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Waveform = make([]byte, 63) }, func(p *mediaPresentation) bool { return len(p.Waveform) == 0 }},
		{"waveform-65", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Waveform = make([]byte, 65) }, func(p *mediaPresentation) bool { return len(p.Waveform) == 0 }},
		{"waveform-64", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Waveform = bytes.Repeat([]byte{3}, 64) }, func(p *mediaPresentation) bool { return bytes.Equal(p.Waveform, bytes.Repeat([]byte{3}, 64)) }},
		{"seconds-max-uint", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Seconds = proto.Uint32(4294967295) }, func(p *mediaPresentation) bool { return p.Seconds == nil }},
		{"seconds-zero", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Seconds = proto.Uint32(0) }, func(p *mediaPresentation) bool { return p.Seconds != nil && *p.Seconds == 0 }},
		{"seconds-max-day", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Seconds = proto.Uint32(86400) }, func(p *mediaPresentation) bool { return p.Seconds != nil && *p.Seconds == 86400 }},
		{"seconds-over-day", "audio", "audio/mpeg", func(p *mediaPresentation) { p.Seconds = proto.Uint32(86401) }, func(p *mediaPresentation) bool { return p.Seconds == nil }},
		{"name-10kb", "document", "application/pdf", func(p *mediaPresentation) { p.Name = proto.String(strings.Repeat("n", 10000)) }, func(p *mediaPresentation) bool { return p.Name != nil && *p.Name == strings.Repeat("n", 200) }},
		{"title-10kb", "document", "application/pdf", func(p *mediaPresentation) { p.Title = proto.String(strings.Repeat("t", 10000)) }, func(p *mediaPresentation) bool { return p.Title != nil && *p.Title == strings.Repeat("t", 200) }},
		{"title-unicode-bound", "document", "application/pdf", func(p *mediaPresentation) { p.Title = proto.String(strings.Repeat("界", 1000)) }, func(p *mediaPresentation) bool { return p.Title != nil && *p.Title == strings.Repeat("界", 200) }},
		{"name-path-control-bidi", "document", "application/pdf", func(p *mediaPresentation) { p.Name = proto.String("C:\\fake\\private\\re\u202eport\x00.pdf") }, func(p *mediaPresentation) bool { return p.Name != nil && *p.Name == "C:\\fake\\private\\report.pdf" }},
		{"title-path-control-bidi", "document", "application/pdf", func(p *mediaPresentation) { p.Title = proto.String("C:\\fake\\private\\su\u2066mmary\x1f.txt") }, func(p *mediaPresentation) bool { return p.Title != nil && *p.Title == "C:\\fake\\private\\summary.txt" }},
		{"name-clock-punctuation", "document", "application/pdf", func(p *mediaPresentation) { p.Name = proto.String("Meeting 10:30.pdf") }, func(p *mediaPresentation) bool { return p.Name != nil && *p.Name == "Meeting 10:30.pdf" }},
		{"name-display-punctuation", "document", "application/pdf", func(p *mediaPresentation) { p.Name = proto.String("a|b.pdf") }, func(p *mediaPresentation) bool { return p.Name != nil && *p.Name == "a|b.pdf" }},
		{"title-display-punctuation", "document", "application/pdf", func(p *mediaPresentation) { p.Title = proto.String("Q3: results") }, func(p *mediaPresentation) bool { return p.Title != nil && *p.Title == "Q3: results" }},
		{"document-mime-header-injection", "document", "application/pdf", func(p *mediaPresentation) { p.MIME = "text/html\r\nX-Evil: 1" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"document-mime-parameters", "document", "application/pdf", func(p *mediaPresentation) { p.MIME = "text/html;charset=utf-8" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"document-mime-whitespace", "document", "application/pdf", func(p *mediaPresentation) { p.MIME = " text/html " }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"document-mime-disposition", "document", "application/pdf", func(p *mediaPresentation) { p.MIME = "attachment" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"document-mime-range", "document", "application/pdf", func(p *mediaPresentation) { p.MIME = "*/*" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"audio-mime-range", "audio", "audio/mpeg", func(p *mediaPresentation) { p.MIME = "audio/*" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"document-any-valid-mime", "document", "text/html", func(p *mediaPresentation) { p.MIME = "text/html" }, func(p *mediaPresentation) bool { return p.MIME == "text/html" }},
		{"sticker-executable-mime", "sticker", "image/webp", func(p *mediaPresentation) { p.MIME = "application/x-msdownload" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
		{"audio-nonaudio-mime", "audio", "audio/mpeg", func(p *mediaPresentation) { p.MIME = "text/html" }, func(p *mediaPresentation) bool { return p.MIME == "" }},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", tc.name, batch), func(t *testing.T) {
				p := &mediaPresentation{MIME: "application/pdf", Name: proto.String("report.pdf"), Title: proto.String("Report")}
				if tc.kind == "audio" {
					p = &mediaPresentation{MIME: "audio/mpeg", PTT: proto.Bool(false), Seconds: proto.Uint32(2), Waveform: bytes.Repeat([]byte{1}, 64)}
				}
				if tc.kind == "sticker" {
					p = &mediaPresentation{MIME: "image/webp", Animated: proto.Bool(true)}
				}
				tc.change(p)
				packet := presentationPacket(tc.kind, p)
				ms := newTestMessageStore(t)
				ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
				if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				replayWriter(t, ms, batch, func(w messageWriter) error {
					return persistMessage(w, "BOUNDP1", mediaTestChat, "x", ts, false, extractMessage(packet, ts, "BOUNDP1"), false, testLogger())
				})
				var raw, filename string
				if err := ms.db.QueryRow("SELECT media_presentation,filename FROM messages WHERE id='BOUNDP1'").Scan(&raw, &filename); err != nil {
					t.Fatal(err)
				}
				var archived mediaPresentation
				if err := json.Unmarshal([]byte(raw), &archived); err != nil {
					t.Fatal(err)
				}
				if !tc.check(&archived) {
					t.Fatalf("%s ingress validation failed", tc.name)
				}
				if len(raw) > 4096 || utf8.RuneCountInString(filename) > 200 {
					t.Fatal("presentation/name storage is unbounded")
				}
				if tc.kind == "document" && archived.Name != nil && filename != *archived.Name {
					t.Fatal("filename column differs from the validated presentation name")
				}
				// Bypass the database decoder deliberately: the wire sink must
				// validate an old or directly supplied presentation too.
				ctx := context.WithValue(t.Context(), forwardSourceKey{}, &forwardSource{mediaType: tc.kind, filename: "report.pdf", presentation: p})
				data := []byte("%PDF-1.7 fake")
				if tc.kind == "audio" {
					data = []byte("ID3 fake")
				}
				if tc.kind == "sticker" {
					data = []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")
				}
				kind, mimeType, err := forwardMediaType(ctx, "cached.bin", data)
				if err != nil {
					t.Fatal(err)
				}
				wire, _, err := buildForwardMedia(ctx, kind, mimeType, "cached.bin", data, testUpload(), "", outboundQuote{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var sent mediaPresentation
				switch tc.kind {
				case "audio":
					sent = mediaPresentation{Seconds: wire.AudioMessage.Seconds, Waveform: wire.AudioMessage.Waveform, MIME: wire.AudioMessage.GetMimetype()}
				case "sticker":
					sent = mediaPresentation{MIME: wire.StickerMessage.GetMimetype()}
				default:
					sent = mediaPresentation{Name: wire.DocumentMessage.FileName, Title: wire.DocumentMessage.Title, MIME: wire.DocumentMessage.GetMimetype()}
				}
				if strings.Contains(tc.name, "mime") {
					if sent.MIME != tc.mime {
						t.Fatalf("wire MIME=%q want=%q", sent.MIME, tc.mime)
					}
				} else if tc.name == "name-path-control-bidi" || tc.name == "name-clock-punctuation" || tc.name == "name-display-punctuation" {
					want := map[string]string{"name-path-control-bidi": "report.pdf", "name-clock-punctuation": "30.pdf", "name-display-punctuation": "ab.pdf"}[tc.name]
					if sent.Name == nil || *sent.Name != want {
						t.Fatalf("wire filename=%v want=%s", sent.Name, want)
					}
				} else if !tc.check(&sent) {
					t.Fatalf("%s wire validation failed", tc.name)
				}
			})
		}
	}
}

func TestLegacyForwardDocumentFilenameSanitizedAtSink(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"C:\\fake\\private\\re\u202eport\x00.pdf", "report.pdf"},
		{strings.Repeat("x", 10000), strings.Repeat("x", 200)},
	} {
		ctx := context.WithValue(t.Context(), forwardSourceKey{}, &forwardSource{mediaType: "document", filename: tc.raw})
		msg, _, err := buildForwardMedia(ctx, whatsmeow.MediaDocument, "application/pdf", "cache.bin", []byte("fake"), testUpload(), "", outboundQuote{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if msg.GetDocumentMessage().GetFileName() != tc.want || msg.GetDocumentMessage().GetTitle() != tc.want {
			t.Fatal("legacy filename/title bypassed the wire sanitizer")
		}
	}
}

func TestMediaPresentationCorruptionFallsBackAndReplayRepairs(t *testing.T) {
	for _, raw := range []string{"{bad", "[]", `"str"`, `{"seconds":"x"}`, `{"waveform":"%%%"}`, fmt.Sprintf(`{"sha256":%q,"seconds":"x"}`, hex.EncodeToString(fixtureDocument().FileSHA256))} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", raw, batch), func(t *testing.T) {
				t.Setenv(storeDirEnv, t.TempDir())
				ms := newTestMessageStore(t)
				ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
				if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				doc := fixtureDocument()
				doc.Mimetype, doc.Title = proto.String("application/pdf"), proto.String("Report")
				write := func() {
					replayWriter(t, ms, batch, func(w messageWriter) error {
						return persistMessage(w, "CORRUPTP1", mediaTestChat, "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "CORRUPTP1"), false, testLogger())
					})
				}
				write()
				if _, err := ms.db.Exec("UPDATE messages SET media_presentation=? WHERE id='CORRUPTP1'", raw); err != nil {
					t.Fatal(err)
				}
				recorder := installRecordingLogger(t)
				source, found, err := ms.messageContentLookup("CORRUPTP1", mediaTestChat)
				if err != nil || !found || source.presentation != nil {
					t.Fatalf("corrupt JSON blocked legacy fallback: found=%v err=%v", found, err)
				}
				if strings.Count(recorder.String(), "[DEBUG]") != 1 || !strings.Contains(recorder.String(), "Ignoring invalid stored media presentation") {
					t.Fatal("corruption did not emit exactly one bounded DEBUG line")
				}
				folder := chatMediaDir(mediaTestChat)
				if err := os.MkdirAll(folder, 0o750); err != nil {
					t.Fatal(err)
				}
				cached := filepath.Join(folder, mediaFileName("document", ts, "CORRUPTP1", doc.GetFileName()))
				if err := os.WriteFile(cached, []byte("fake document bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
				b := testBridge(t, client, ms, testLogger())
				b.Connected = func() bool { return true }
				calls, uploads := 0, 0
				network := messageSendNetwork{connected: func() bool { return true }, upload: func(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
					uploads++
					if kind != whatsmeow.MediaDocument || string(data) != "fake document bytes" {
						t.Fatal("legacy fallback uploaded wrong bytes/category")
					}
					return testUpload(), nil
				}, send: func(_ context.Context, _ types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
					calls++
					if msg.GetDocumentMessage().GetFileName() != doc.GetFileName() || msg.GetDocumentMessage().GetTitle() != doc.GetFileName() {
						t.Fatal("corrupt presentation escaped legacy filename fallback")
					}
					return whatsmeow.SendResponse{ID: "CORRUPTOUT1", Timestamp: ts}, nil
				}}
				deps := forwardDeps{lookup: ms.messageContentLookup, download: b.downloadMedia, resolveRecipient: func(ctx context.Context, w http.ResponseWriter, to string) (string, bool) {
					return b.registeredRecipient(ctx, w, to, nil)
				}, send: func(ctx context.Context, to, text, path, qid, qs, qc string, mentions []string) (bool, string, sentMessage) {
					return sendWhatsAppMessageWithNetwork(ctx, client, ms, b.persistOutbound, to, text, path, qid, qs, qc, mentions, network)
				}}
				code, response := efPost(t, handleForwardMessage(deps, chatPolicy{}), fmt.Sprintf(`{"chat_jid":%q,"message_id":"CORRUPTP1","to_chat_jid":"120363000000000001@g.us"}`, mediaTestChat))
				if code != http.StatusOK || !response.Success || calls != 1 || uploads != 1 {
					t.Fatalf("corrupt JSON forward=%d sends=%d uploads=%d", code, calls, uploads)
				}
				write()
				source, found, err = ms.messageContentLookup("CORRUPTP1", mediaTestChat)
				if err != nil || !found || source.presentation == nil || source.presentation.MIME != "application/pdf" {
					t.Fatalf("replay did not repair corruption: found=%v err=%v", found, err)
				}
			})
		}
	}
}

func TestMediaPresentationHeadMainHeadSequence(t *testing.T) {
	ms := newTestMessageStore(t)
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	doc.Mimetype, doc.FileName, doc.Title = proto.String("application/pdf"), proto.String("old.pdf"), proto.String("Old title")
	if err := persistMessage(ms, "ROLLBACKP1", mediaTestChat, "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "ROLLBACKP1"), false, testLogger()); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := ms.db.QueryRow("SELECT media_presentation FROM messages WHERE id='ROLLBACKP1'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var identity map[string]any
	if err := json.Unmarshal([]byte(before), &identity); err != nil {
		t.Fatal(err)
	}
	if identity["sha256"] != hex.EncodeToString(doc.FileSHA256) {
		t.Fatal("presentation is not tied to the plaintext hash")
	}
	// Execute the actual production SQL captured from main before this PR,
	// against this same store: the old writer cannot touch the new column.
	legacySQL, err := os.ReadFile("testdata/presentation_legacy_upsert.sql")
	if err != nil {
		t.Fatal(err)
	}
	newHash := bytes.Repeat([]byte{2}, 32)
	args := messageArgs(storedMessage{
		ID:            "ROLLBACKP1",
		ChatJID:       mediaTestChat,
		Sender:        "x",
		Content:       "new caption",
		Timestamp:     ts,
		MediaType:     "document",
		Filename:      "new.pdf",
		URL:           doc.GetURL(),
		MediaKey:      doc.MediaKey,
		FileSHA256:    newHash,
		FileEncSHA256: doc.FileEncSHA256,
		FileLength:    doc.GetFileLength(),
		Media:         messageMediaOptions{directPath: doc.GetDirectPath()},
	})
	args = append(args[:16:16], sql.Named("complete_media", true))
	if _, err := ms.db.Exec(string(legacySQL), args...); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := ms.db.QueryRow("SELECT media_presentation FROM messages WHERE id='ROLLBACKP1'").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("fixture did not reproduce the old image leaving JSON untouched")
	}
	source, found, err := ms.messageContentLookup("ROLLBACKP1", mediaTestChat)
	if err != nil || !found || source.presentation != nil {
		t.Fatal("rolled-forward reader trusted the old file's metadata")
	}
	ctx := context.WithValue(t.Context(), forwardSourceKey{}, &source)
	wire, _, err := buildForwardMedia(ctx, whatsmeow.MediaDocument, "application/pdf", "cached.bin", []byte("%PDF-1.7 fake"), testUpload(), "", outboundQuote{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wire.DocumentMessage.GetFileName() != "new.pdf" || wire.DocumentMessage.GetTitle() != "new.pdf" {
		t.Fatal("rolled-forward send exposed the old file's name/title")
	}
}

func TestLegacyLIDCopyPreservesMediaPresentation(t *testing.T) {
	ms := newTestMessageStore(t)
	ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if err := ms.StoreChat("111@lid", "Alice", ts); err != nil {
		t.Fatal(err)
	}
	doc := fixtureDocument()
	doc.Mimetype, doc.Title = proto.String("application/pdf"), proto.String("Report")
	if err := persistMessage(ms, "LIDP1", "111@lid", "x", ts, false, extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "LIDP1"), false, testLogger()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	waDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waDB.Close() }()
	if _, err := waDB.Exec("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT NOT NULL); INSERT INTO whatsmeow_lid_map VALUES ('111','222')"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	source, found, err := ms.messageContentLookup("LIDP1", "222@s.whatsapp.net")
	if err != nil || !found || source.presentation == nil || source.presentation.MIME != "application/pdf" || source.presentation.Title == nil || *source.presentation.Title != "Report" {
		t.Fatal("LID row copy lost the presentation")
	}
	if got := storedDirectPath(t, ms, "LIDP1", "222@s.whatsapp.net"); got.String != doc.GetDirectPath() {
		t.Fatal("LID row copy lost the matching direct path")
	}
}
