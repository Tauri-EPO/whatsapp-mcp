package main

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func replayWriter(t *testing.T, store *MessageStore, batch bool, write func(messageWriter) error) {
	t.Helper()
	var err error
	if batch {
		err = store.Batch(func(w *messageBatch) error { return write(w) })
	} else {
		err = write(store)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestMediaReplayAcceptsOnlyFirstPartialAndKeepsChatsIndependent(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history batch"}[batch], func(t *testing.T) {
			ms := newTestMessageStore(t)
			ts := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
			otherChat := "5511888888888@s.whatsapp.net"
			for _, chat := range []string{mediaTestChat, otherChat} {
				if err := ms.StoreChat(chat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
			}
			persist := func(chat string, msg *waE2E.Message) {
				t.Helper()
				ex := extractMessage(msg, ts, "PARTIAL1")
				replayWriter(t, ms, batch, func(w messageWriter) error {
					return persistMessage(w, "PARTIAL1", chat, "x", ts, false, ex, false, testLogger())
				})
			}
			persist(mediaTestChat, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}})
			ts = ts.Add(time.Hour)
			firstTimestamp := ts
			first := &waE2E.DocumentMessage{FileName: proto.String("first.txt"), URL: proto.String("https://example.invalid/first"), DirectPath: proto.String("/first"), MediaKey: []byte("first key"), FileSHA256: []byte("first sha")}
			persist(mediaTestChat, &waE2E.Message{DocumentMessage: first})
			check := func() {
				t.Helper()
				var kind, name, url string
				var path sql.NullString
				var key, sha, enc []byte
				var length uint64
				var storedTimestamp time.Time
				if err := ms.db.QueryRow("SELECT media_type,filename,url,direct_path,media_key,file_sha256,file_enc_sha256,file_length,timestamp FROM messages WHERE id='PARTIAL1' AND chat_jid=?", mediaTestChat).Scan(&kind, &name, &url, &path, &key, &sha, &enc, &length, &storedTimestamp); err != nil {
					t.Fatal(err)
				}
				if kind != "document" || name != first.GetFileName() || url != first.GetURL() || path.String != first.GetDirectPath() || !bytes.Equal(key, first.GetMediaKey()) || !bytes.Equal(sha, first.GetFileSHA256()) || len(enc) != 0 || length != 0 || !storedTimestamp.Equal(firstTimestamp) {
					t.Fatalf("first partial snapshot lost: kind=%s name=%s path=%s length=%d timestamp=%v", kind, name, path.String, length, storedTimestamp)
				}
			}
			check()
			ts = ts.Add(time.Hour)
			persist(mediaTestChat, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: proto.String("https://example.invalid/second"), DirectPath: proto.String("/second"), MediaKey: []byte("second key"), FileEncSHA256: []byte("second enc"), FileLength: proto.Uint64(14)}})
			check()
			other := fixtureDocument()
			persist(otherChat, &waE2E.Message{DocumentMessage: other})
			check()
			var otherURL string
			if err := ms.db.QueryRow("SELECT url FROM messages WHERE id='PARTIAL1' AND chat_jid=?", otherChat).Scan(&otherURL); err != nil || otherURL != other.GetURL() {
				t.Fatalf("other chat=%s err=%v", otherURL, err)
			}
		})
	}
}

func TestIncompleteMediaReplayPreservesCaptionAndFTS(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history batch"}[batch], func(t *testing.T) {
			ms := newTestMessageStore(t)
			if _, err := ms.db.Exec(ftsSchema); err != nil {
				t.Fatal(err)
			}
			ts := time.Now().Truncate(time.Second)
			if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
				t.Fatal(err)
			}
			persist := func(doc *waE2E.DocumentMessage) {
				t.Helper()
				ex := extractMessage(&waE2E.Message{DocumentMessage: doc}, ts, "CAPTION1")
				replayWriter(t, ms, batch, func(w messageWriter) error {
					return persistMessage(w, "CAPTION1", mediaTestChat, "x", ts, false, ex, false, testLogger())
				})
			}
			check := func(want string, matches int) {
				t.Helper()
				var content string
				var count int
				if err := ms.db.QueryRow("SELECT content FROM messages WHERE id='CAPTION1'").Scan(&content); err != nil || content != want {
					t.Fatalf("caption=%q err=%v", content, err)
				}
				if err := ms.db.QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'preservedcaption'").Scan(&count); err != nil || count != matches {
					t.Fatalf("FTS matches=%d err=%v", count, err)
				}
			}
			original := fixtureDocument()
			original.Caption = proto.String("preservedcaption")
			persist(original)
			persist(&waE2E.DocumentMessage{FileName: proto.String("stub.txt")})
			check("preservedcaption", 1)
			complete := fixtureDocument()
			complete.Caption = nil
			persist(complete)
			check("", 0)
		})
	}
}

func TestPointerReplaysKeepTheirUpdateSemantics(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, kind := range []string{"reaction", "poll_vote"} {
			t.Run(kind+map[bool]string{false: "/live", true: "/history batch"}[batch], func(t *testing.T) {
				ms := newTestMessageStore(t)
				ts := time.Now().Truncate(time.Second)
				if err := ms.StoreChat(mediaTestChat, "Alice", ts); err != nil {
					t.Fatal(err)
				}
				write := func(content string) {
					replayWriter(t, ms, batch, func(w messageWriter) error {
						return w.StoreMessage("POINTER1", mediaTestChat, "x", content, ts, false, kind, "TARGET1", "", nil, nil, nil, 0, "")
					})
				}
				write("initial")
				if err := ms.SetTargetMessageID("POINTER1", mediaTestChat, "TARGET1"); err != nil {
					t.Fatal(err)
				}
				ts = ts.Add(time.Hour)
				want := "updated vote"
				if kind == "reaction" {
					want = ""
				} // Removal is still an empty emoji.
				write(want)
				var gotKind, name, target, content string
				var storedTimestamp time.Time
				if err := ms.db.QueryRow("SELECT media_type,filename,target_message_id,content,timestamp FROM messages WHERE id='POINTER1'").Scan(&gotKind, &name, &target, &content, &storedTimestamp); err != nil {
					t.Fatal(err)
				}
				if gotKind != kind || name != "TARGET1" || target != "TARGET1" || content != want || !storedTimestamp.Equal(ts) {
					t.Fatalf("pointer replay changed: %s %s %s %q %v", gotKind, name, target, content, storedTimestamp)
				}
			})
		}
	}
}
