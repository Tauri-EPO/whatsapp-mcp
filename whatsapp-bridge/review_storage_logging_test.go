package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMediaRetryRefreshIsOneSnapshot(t *testing.T) {
	for _, action := range []string{"ABORT", "ROLLBACK"} {
		t.Run(action, func(t *testing.T) {
			ms := newTestMessageStore(t)
			chat := phonePN.String()
			if err := ms.StoreChat(chat, "Alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := ms.StoreMessage(storedMessage{ID: "REFRESH", ChatJID: chat, Sender: phonePN.User, Content: "media", Timestamp: time.Now(), MediaType: "image", URL: "https://example.invalid/old", MediaKey: []byte("old-key"), FileSHA256: []byte("old-hash"), FileEncSHA256: []byte("old-enc"), FileLength: 10, Media: messageMediaOptions{directPath: "/old"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := ms.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_refresh BEFORE UPDATE OF direct_path ON messages WHEN new.id='REFRESH' AND new.direct_path='/new' BEGIN SELECT RAISE(%s,'simulated direct path failure'); END", action)); err != nil {
				t.Fatal(err)
			}
			fresh := &MediaDownloader{URL: "https://example.invalid/new", DirectPath: "/new", MediaKey: []byte("new-key"), FileSHA256: []byte("new-hash"), FileEncSHA256: []byte("new-enc"), FileLength: 20}
			check := func(want *MediaDownloader) {
				t.Helper()
				var url, path string
				var key, hash, enc []byte
				var length uint64
				if err := ms.db.QueryRow("SELECT url,direct_path,media_key,file_sha256,file_enc_sha256,file_length FROM messages WHERE id='REFRESH' AND chat_jid=?", chat).Scan(&url, &path, &key, &hash, &enc, &length); err != nil {
					t.Fatal(err)
				}
				if url != want.URL || path != want.DirectPath || !bytes.Equal(key, want.MediaKey) || !bytes.Equal(hash, want.FileSHA256) || !bytes.Equal(enc, want.FileEncSHA256) || length != want.FileLength {
					t.Fatal("media retry stored a partial snapshot")
				}
			}
			old := &MediaDownloader{URL: "https://example.invalid/old", DirectPath: "/old", MediaKey: []byte("old-key"), FileSHA256: []byte("old-hash"), FileEncSHA256: []byte("old-enc"), FileLength: 10}
			check(old)
			storeRefreshedMedia(ms, "REFRESH", chat, fresh)
			check(old)
			if _, err := ms.db.Exec("DROP TRIGGER reject_refresh"); err != nil {
				t.Fatal(err)
			}
			storeRefreshedMedia(ms, "REFRESH", chat, fresh)
			check(fresh)
		})
	}
}

func TestTextCapKeepsEscapeBoundaries(t *testing.T) {
	budget := textLogMaxLine - 1 - len(textLogTruncated)
	for _, tc := range []struct {
		name, value string
		inside      int
	}{
		{"control", "\x1b", 2},
		{"backslash", `\`, 1},
		{"bidi", "\u202e", 3},
		{"invalid-byte", string([]byte{0xff}), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := strings.Repeat("a", budget-tc.inside)
			line := capTextLine(oneLine(prefix + tc.value + strings.Repeat("z", 30)))
			payload := strings.TrimSuffix(line, textLogTruncated+"\n")
			decoded, err := strconv.Unquote(`"` + payload + `"`)
			if err != nil || decoded != prefix || len(line) > textLogMaxLine {
				t.Fatalf("cap split an escape: bytes=%d err=%v", len(line), err)
			}
		})
	}
}

func TestTextLoggerDropsTerminalNewline(t *testing.T) {
	for _, tc := range []struct {
		message          string
		lines, continued int
	}{
		{"one\n", 1, 0}, {"one\ntwo\n", 2, 1}, {"one\n\n", 2, 1}, {"", 1, 0},
	} {
		var out bytes.Buffer
		logger := newTextWriter("fixture", "INFO", &out, false)
		logger.Infof("%s", tc.message)
		if strings.Count(out.String(), "\n") != tc.lines || strings.Count(out.String(), textLogContinuation) != tc.continued {
			t.Fatalf("terminal newline changed record count: %q", out.String())
		}
	}
}
