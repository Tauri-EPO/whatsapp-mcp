package main

// The download, the purge and the webhook decide with one rule whether a media
// file is cached (issue #490, media_cache_path.go).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	cached, _ := cachedMediaPath(root, chatMediaRel(chat), "image", cacheRuleTime, id, "")
	download = cached != ""
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
// "nothing there": the purge tells the two apart for whoever reads its answer,
// and the download logs the first.
func TestFindCachedMediaTellsAbsenceFromRefusal(t *testing.T) {
	chatDir, _ := cacheRuleStore(t)
	root := storeRootAt(t, storeDir())
	chat := chatMediaRel(mediaTestChat)
	find := func(dir string, names ...string) (string, error) {
		t.Helper()
		found, err := findCachedMedia(root, dir, names)
		if found == nil {
			return "", err
		}
		defer found.Close()
		if found.info == nil || err != nil {
			t.Errorf("a found file must come with its info and no error: info=%v err=%v", found.info, err)
		}
		return found.name, err
	}

	if name, err := find(chat, "missing.jpg"); name != "" || err != nil {
		t.Errorf("missing file: name=%q err=%v, want neither", name, err)
	}
	if name, err := find("nobody@g.us", "missing.jpg"); name != "" || err != nil {
		t.Errorf("missing directory: name=%q err=%v, want neither", name, err)
	}
	symlinkOrSkip(t, "../victim.db", filepath.Join(chatDir, "linked.jpg"))
	if name, err := find(chat, "linked.jpg"); name != "" || !errors.Is(err, errMediaNotRegular) {
		t.Errorf("linked file: name=%q err=%v, want the not-a-regular-file refusal", name, err)
	}
	// The second name is tried when the first is refused: a legacy file is
	// still found next to a link under the current name.
	writeTestFile(t, filepath.Join(chatDir, "legacy"), "x")
	if name, err := find(chat, "linked.jpg", "legacy"); name != "legacy" || err != nil {
		t.Errorf("fallback: name=%q err=%v, want the legacy file", name, err)
	}
	// The same name twice, as every type but a document lists it, is one lookup
	// with one answer.
	if name, err := find(chat, "legacy", "legacy"); name != "legacy" || err != nil {
		t.Errorf("repeated name: name=%q err=%v", name, err)
	}
	if _, err := find(chat, "../victim.db"); !errors.Is(err, errMediaPath) || !strings.Contains(err.Error(), "refusing media path") {
		t.Errorf("a name with a separator: err=%v, want the component refusal", err)
	}
	if found, err := findCachedMedia(nil, chat, []string{"x.jpg"}); found != nil || err == nil {
		t.Error("want an error without a store root")
	}
}

// A lookup that was refused is not a silent cache miss: the download says why
// it fetches a file that has something under its name.
func TestDownloadMedia_SaysWhenTheCacheLookupWasRefused(t *testing.T) {
	storeInScratch(t)
	ms := newConcurrentTestStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, ms, rec)
	var transfers atomic.Int32
	b.mediaTransfer = countingTransfer(&transfers)
	dest := seedMediaRowIn(t, ms, mediaTestChat, "IMG1")
	if err := os.Mkdir(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, storePath("victim.db"), "victim-content")
	symlinkOrSkip(t, "../victim.db", dest)

	if ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG1", mediaTestChat); !ok || err != nil {
		t.Fatalf("ok=%v err=%v, want a fresh download", ok, err)
	}
	if got := transfers.Load(); got != 1 {
		t.Errorf("transfers = %d, want 1", got)
	}
	if !strings.Contains(rec.String(), "[WARN] Cache lookup for message \"IMG1\"") || !strings.Contains(rec.String(), "was refused") {
		t.Errorf("want a WARN naming the refused lookup, got:\n%s", rec.String())
	}

	// An ordinary miss says nothing.
	rec2 := installRecordingLogger(t)
	b.Log = rec2
	seedMediaRowIn(t, ms, mediaTestChat, "IMG2")
	if ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG2", mediaTestChat); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if strings.Contains(rec2.String(), "Cache lookup") {
		t.Errorf("an ordinary cache miss logged a refusal:\n%s", rec2.String())
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
