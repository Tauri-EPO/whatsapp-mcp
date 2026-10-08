package main

// The download, the purge and the webhook decide with one rule whether a media
// file is cached (issue #490, media_cache_path.go).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chatMediaDir is store/<chat_jid> as a file system path, for fixtures: the
// bridge itself only ever names a chat's directory through the store root
// (chatMediaRel), so this lives with the tests.
func chatMediaDir(chatJID string) string {
	return storePath(chatMediaRel(chatJID))
}

// cacheRuleTime and cacheRuleID name the one media row every case below is about.
var cacheRuleTime = time.Date(2026, 9, 4, 15, 4, 5, 0, time.Local)

const cacheRuleID = "IMG1"

// cacheRuleViews asks the three readers of the cache about the same row and
// reports what each of them sees: the download's lookup, a dry-run purge and
// the webhook's open.
func cacheRuleViews(t *testing.T, chat, id string) (download bool, purge PurgeResult, webhook error) {
	t.Helper()
	root := storeRootAt(t, storeDir())
	download = cachedMediaPath(root, chatMediaRel(chat), "image", cacheRuleTime, id, "") != ""
	purge = purgeOne(root, mediaRow{ID: id, ChatJID: chat, MediaType: "image", Timestamp: cacheRuleTime}, true)
	f, _, webhook := openStoreMedia(root, chatMediaRel(chat), mediaFileName("image", cacheRuleTime, id, ""))
	if webhook == nil {
		_ = f.Close()
	}
	return download, purge, webhook
}

// A regular file under the row's name in the chat's own directory is cached for
// all three. Everything else is cached for none of them: before, the download
// refused a link the purge followed, and the purge accepted a directory name
// the download refused.
func TestCachedMediaIsTheSameThingForDownloadPurgeAndWebhook(t *testing.T) {
	name := mediaFileName("image", cacheRuleTime, cacheRuleID, "")
	cases := []struct {
		name string
		// plant prepares the store and returns the row to ask about and the
		// reason the purge must give ("" = it would purge the file).
		plant func(t *testing.T, chatDir string, victims []string) (chat, id, purgeReason string)
	}{
		{"a regular file", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			writeTestFile(t, filepath.Join(chatDir, name), "image bytes")
			return mediaTestChat, cacheRuleID, ""
		}},
		{"nothing under the name", func(*testing.T, string, []string) (string, string, string) {
			return mediaTestChat, cacheRuleID, purgeReasonNotCached
		}},
		{"no directory for the chat", func(*testing.T, string, []string) (string, string, string) {
			return "nobody@g.us", cacheRuleID, purgeReasonNotCached
		}},
		{"the name is a symlink out of the store", func(t *testing.T, chatDir string, victims []string) (string, string, string) {
			symlinkOrSkip(t, victims[0], filepath.Join(chatDir, name))
			return mediaTestChat, cacheRuleID, purgeReasonNotResolvable
		}},
		{"the name is a symlink to another file of the store", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			symlinkOrSkip(t, "../victim.db", filepath.Join(chatDir, name))
			return mediaTestChat, cacheRuleID, purgeReasonNotResolvable
		}},
		{"the name is a symlink to another file of the same chat", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			writeTestFile(t, filepath.Join(chatDir, "other.jpg"), "image bytes")
			symlinkOrSkip(t, "other.jpg", filepath.Join(chatDir, name))
			return mediaTestChat, cacheRuleID, purgeReasonNotResolvable
		}},
		{"the name is a directory", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			if err := os.Mkdir(filepath.Join(chatDir, name), 0o700); err != nil {
				t.Fatal(err)
			}
			return mediaTestChat, cacheRuleID, purgeReasonNotResolvable
		}},
		{"the chat directory is a symlink out of the store", func(t *testing.T, _ string, victims []string) (string, string, string) {
			outside := filepath.Dir(victims[0])
			writeTestFile(t, filepath.Join(outside, name), "image bytes")
			symlinkOrSkip(t, outside, storePath("moved@g.us"))
			return "moved@g.us", cacheRuleID, purgeReasonNotResolvable
		}},
		{"the chat directory is a symlink to another chat of the store", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			writeTestFile(t, filepath.Join(chatDir, name), "image bytes")
			symlinkOrSkip(t, filepath.Base(chatDir), storePath("alias@g.us"))
			return "alias@g.us", cacheRuleID, purgeReasonNotResolvable
		}},
		{"the message ID climbs out of the chat directory", func(t *testing.T, _ string, _ []string) (string, string, string) {
			return mediaTestChat, "../../victim", "path outside the store directory"
		}},
		{"the message ID carries a line break", func(t *testing.T, _ string, _ []string) (string, string, string) {
			return mediaTestChat, "ID\nx", "path outside the store directory"
		}},
		{"the chat JID names a nested path", func(t *testing.T, chatDir string, _ []string) (string, string, string) {
			writeTestFile(t, filepath.Join(chatDir, name), "image bytes")
			return "other@g.us/" + mediaTestChat, cacheRuleID, "path outside the store directory"
		}},
		{"the chat JID is the parent directory", func(*testing.T, string, []string) (string, string, string) {
			return "..", cacheRuleID, "path outside the store directory"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatDir, victims := cacheRuleStore(t)
			chat, id, purgeReason := tc.plant(t, chatDir, victims)

			download, purge, webhook := cacheRuleViews(t, chat, id)
			want := purgeReason == ""
			if download != want {
				t.Errorf("download: cached = %v, want %v", download, want)
			}
			if purge.Purged != want || purge.Reason != purgeReason {
				t.Errorf("purge: purged = %v, reason = %q; want %v, %q", purge.Purged, purge.Reason, want, purgeReason)
			}
			if (webhook == nil) != want {
				t.Errorf("webhook: open error = %v, want cached = %v", webhook, want)
			}
		})
	}
}

// The acceptance case of #490 with a real purge: a cached name that is a
// symlink to another file of the store used to be a purge candidate (root.Stat
// followed it and the result carried the target's size). It is refused now, as
// the download refuses it; the link and its target stay.
func TestPurgeOne_RefusesANameLinkedToAnotherStoreFile(t *testing.T) {
	chatDir, victims := cacheRuleStore(t)
	link := filepath.Join(chatDir, mediaFileName("image", cacheRuleTime, cacheRuleID, ""))
	symlinkOrSkip(t, "../victim.db", link)

	res := purgeOne(storeRootAt(t, storeDir()), mediaRow{ID: cacheRuleID, ChatJID: mediaTestChat, MediaType: "image", Timestamp: cacheRuleTime}, false)
	if res.Purged || res.Bytes != 0 || res.Reason != purgeReasonNotResolvable {
		t.Errorf("result = %+v, want a refusal with no bytes", res)
	}
	if got, err := os.ReadFile(victims[1]); err != nil || string(got) != "victim-content" {
		t.Errorf("the target of the link changed: %q, %v", got, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link should be left in place: info=%v err=%v", info, err)
	}
}

// A real purge still removes the regular file and nothing else.
func TestPurgeOne_RemovesTheRegularFile(t *testing.T) {
	chatDir, _ := cacheRuleStore(t)
	file := filepath.Join(chatDir, mediaFileName("image", cacheRuleTime, cacheRuleID, ""))
	writeTestFile(t, file, "image bytes")

	res := purgeOne(storeRootAt(t, storeDir()), mediaRow{ID: cacheRuleID, ChatJID: mediaTestChat, MediaType: "image", Timestamp: cacheRuleTime}, false)
	if !res.Purged || res.Bytes != int64(len("image bytes")) || res.File != filepath.Base(file) || res.Reason != "" {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Errorf("the cached file is still there: %v", err)
	}
}

// The error that says "something is in the way" must not be the one for
// "nothing there": the purge tells the two apart for whoever reads its answer.
func TestFindCachedMediaTellsAbsenceFromRefusal(t *testing.T) {
	chatDir, _ := cacheRuleStore(t)
	root := storeRootAt(t, storeDir())
	chat := chatMediaRel(mediaTestChat)

	if name, _, err := findCachedMedia(root, chat, []string{"missing.jpg"}); name != "" || err != nil {
		t.Errorf("missing file: name=%q err=%v, want neither", name, err)
	}
	if name, _, err := findCachedMedia(root, "nobody@g.us", []string{"missing.jpg"}); name != "" || err != nil {
		t.Errorf("missing directory: name=%q err=%v, want neither", name, err)
	}
	symlinkOrSkip(t, "../victim.db", filepath.Join(chatDir, "linked.jpg"))
	if name, _, err := findCachedMedia(root, chat, []string{"linked.jpg"}); name != "" || err == nil {
		t.Errorf("linked file: name=%q err=%v, want a refusal", name, err)
	}
	// The second name is tried when the first is refused: a legacy file is
	// still found next to a link under the current name.
	writeTestFile(t, filepath.Join(chatDir, "legacy"), "x")
	if name, info, err := findCachedMedia(root, chat, []string{"linked.jpg", "legacy"}); name != "legacy" || info == nil || err != nil {
		t.Errorf("fallback: name=%q info=%v err=%v, want the legacy file", name, info, err)
	}
	if _, _, err := findCachedMedia(root, chat, []string{"../victim.db"}); err == nil || !strings.Contains(err.Error(), "refusing media path") {
		t.Errorf("a name with a separator: err=%v, want the component refusal", err)
	}
	if _, _, err := findCachedMedia(nil, chat, []string{"x.jpg"}); err == nil {
		t.Error("want an error without a store root")
	}
}

// cacheRuleStore builds a store with an empty directory for mediaTestChat and
// two files nothing may reach through a link: one outside the store, one in it.
func cacheRuleStore(t *testing.T) (chatDir string, victims []string) {
	t.Helper()
	scratch := storeInScratch(t)
	chatDir = chatMediaDir(mediaTestChat)
	if err := os.Mkdir(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(scratch, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	victims = []string{filepath.Join(outside, "outside.db"), storePath("victim.db")}
	for _, v := range victims {
		writeTestFile(t, v, "victim-content")
	}
	return chatDir, victims
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
