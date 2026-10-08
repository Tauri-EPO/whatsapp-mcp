package main

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

func TestLiveViewOnceThroughSDKUnwrapIsFlaggedExactlyOnce(t *testing.T) {
	for _, name := range []string{"v1", "v2", "v2 extension", "plain", "still wrapped"} {
		for _, caption := range []string{"", "fake caption"} {
			t.Run(name+"/"+caption, func(t *testing.T) {
				t.Setenv(storeDirEnv, t.TempDir())
				t.Setenv("WEBHOOK_ENABLED", "false")
				ms := newTestMessageStore(t)
				b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, selfPhone), ms, testLogger())
				b.MediaAutoDownload = false
				evt := buildImageMessage(phonePN, phonePN, false, caption)
				evt.Info.ID = "LIVEVO1"
				inner := evt.Message
				envelope := &waE2E.FutureProofMessage{Message: inner}
				switch name {
				case "v1":
					evt.RawMessage = &waE2E.Message{ViewOnceMessage: envelope}
				case "v2", "still wrapped":
					evt.RawMessage = &waE2E.Message{ViewOnceMessageV2: envelope}
				case "v2 extension":
					evt.RawMessage = &waE2E.Message{ViewOnceMessageV2Extension: envelope}
				case "plain":
					evt.RawMessage = inner
				}
				if name == "still wrapped" {
					evt.Message = evt.RawMessage
					evt.IsViewOnce = true
				} else {
					evt.UnwrapRaw()
					if evt.Message != inner || evt.IsViewOnce != (name != "plain") {
						t.Fatal("SDK fixture did not reproduce a live event")
					}
				}
				b.handleMessage(evt)
				var content, kind string
				var flagged bool
				if err := ms.db.QueryRow("SELECT content,media_type,view_once FROM messages WHERE id='LIVEVO1'").Scan(&content, &kind, &flagged); err != nil {
					t.Fatal(err)
				}
				want := caption
				if name != "plain" {
					want = "🔒 " + caption
					if caption == "" {
						want = "🔒 view-once image"
					}
				}
				if kind != "image" || flagged != (name != "plain") || content != want {
					t.Fatalf("kind=%s flag=%v content=%q want=%q", kind, flagged, content, want)
				}
				wantLocks := 1
				if name == "plain" {
					wantLocks = 0
				}
				if strings.Count(content, "🔒") != wantLocks {
					t.Fatal("incorrect view-once marker count")
				}
				// Keep protobuf's inner content intact for webhook/media extraction.
				if inner.GetImageMessage().GetCaption() != caption || inner.GetImageMessage().GetViewOnce() {
					t.Fatal("event payload was mutated")
				}
			})
		}
	}
}

func TestViewOnceRawHistoryBatchStillFlagged(t *testing.T) {
	for _, version := range []string{"v1", "v2", "v2 extension"} {
		for _, caption := range []string{"", "fake caption"} {
			t.Run(version+"/"+caption, func(t *testing.T) {
				ms := newTestMessageStore(t)
				ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
				if err := ms.StoreChat(phonePN.String(), "Alice", ts); err != nil {
					t.Fatal(err)
				}
				inner := buildImageMessage(phonePN, phonePN, false, caption).Message
				envelope := &waE2E.FutureProofMessage{Message: inner}
				raw := &waE2E.Message{}
				switch version {
				case "v1":
					raw.ViewOnceMessage = envelope
				case "v2":
					raw.ViewOnceMessageV2 = envelope
				default:
					raw.ViewOnceMessageV2Extension = envelope
				}
				err := ms.Batch(func(w *messageBatch) error {
					return persistMessage(w, "HISTORYVO1", phonePN.String(), phonePN.String(), ts, false, extractMessage(raw, ts, "HISTORYVO1"), false, testLogger())
				})
				if err != nil {
					t.Fatal(err)
				}
				var flagged bool
				var content, kind string
				if err := ms.db.QueryRow("SELECT view_once,content,media_type FROM messages WHERE id='HISTORYVO1'").Scan(&flagged, &content, &kind); err != nil {
					t.Fatal(err)
				}
				want := "🔒 " + caption
				if caption == "" {
					want = "🔒 view-once image"
				}
				if !flagged || content != want || kind != "image" || inner.GetImageMessage().GetCaption() != caption {
					t.Fatalf("history flag=%v kind=%s content=%q want=%q", flagged, kind, content, want)
				}
			})
		}
	}
}
