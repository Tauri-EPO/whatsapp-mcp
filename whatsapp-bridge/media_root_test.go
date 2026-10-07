package main

// The inbound media write path is confined twice (issue #453): the chat
// directory and the file name must each be one plain path component, and every
// filesystem step goes through the store root (os.Root).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow"
)

// countingTransfer is a fake transfer that writes a payload the way the real
// one does (store root, ".part", rename) and counts its runs.
func countingTransfer(runs *atomic.Int32) mediaTransferFunc {
	return func(_ context.Context, _ whatsmeow.DownloadableMessage, relPath string) (int64, error) {
		runs.Add(1)
		return writeLikeDownloadToPath(relPath, []byte("media bytes"))
	}
}

// treeEntries lists every file and directory under dir, relative to it.
func treeEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// storeInScratch points WHATSAPP_STORE_DIR at <scratch>/store and returns the
// scratch directory, so a test can see what was created next to the store.
func storeInScratch(t *testing.T) string {
	t.Helper()
	scratch := t.TempDir()
	store := filepath.Join(scratch, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(storeDirEnv, store)
	return scratch
}

func TestCheckMediaPathComponents(t *testing.T) {
	const okDir, okFile = "5511999999999@s.whatsapp.net", "image_20260904_150405_3EB0C0FFEE.jpg"
	for _, dir := range []string{okDir, "5511999999999_12@s.whatsapp.net", "120363041234567890@g.us", "5511-1400000000@g.us", "status@broadcast", "123456789012345@lid"} {
		for _, file := range []string{okFile, "document_20260904_150405_3EB0C0FFEE", "document_20260904_150405_ABC-def_1.2.pdf"} {
			if err := checkMediaPathComponents(dir, file); err != nil {
				t.Errorf("(%q, %q) refused: %v", dir, file, err)
			}
		}
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "/abs", "../up", `..\up`, "a..b", "a\x00b", "a/../b"}
	for _, name := range bad {
		if err := checkMediaPathComponents(name, okFile); err == nil || !strings.Contains(err.Error(), "chat JID") {
			t.Errorf("chat directory %q: err = %v, want a refusal naming the chat JID", name, err)
		}
		if err := checkMediaPathComponents(okDir, name); err == nil || !strings.Contains(err.Error(), "message ID") {
			t.Errorf("file name %q: err = %v, want a refusal naming the message ID", name, err)
		}
	}
}

// A message ID or a chat JID that is more than one path component is refused
// before anything touches the disk: no transfer runs, no directory is created
// in the store, and nothing appears next to it.
func TestDownloadMedia_RefusesUnsafePathComponents(t *testing.T) {
	scratch := storeInScratch(t)
	ms := newConcurrentTestStore(t)
	rec := installRecordingLogger(t)
	b := testBridge(t, nil, ms, rec)
	var transfers atomic.Int32
	b.mediaTransfer = countingTransfer(&transfers)

	cases := []struct{ name, chat, id string }{
		{"message ID with a slash", mediaTestChat, "../../evil/ID"},
		{"message ID with a backslash", mediaTestChat, `..\..\evil\ID`},
		{"message ID that is dot-dot", mediaTestChat, ".."},
		{"message ID with an inner dot-dot", mediaTestChat, "ID..ID"},
		{"message ID with a nested path", mediaTestChat, "sub/ID"},
		{"chat JID with a slash", "../escape@s.whatsapp.net", "ID1"},
		{"chat JID with a backslash", `..\escape@s.whatsapp.net`, "ID2"},
		{"chat JID naming a nested path", "other@g.us/5511999999999@s.whatsapp.net", "ID3"},
		{"chat JID that is dot-dot", "..", "ID4"},
	}
	for _, tc := range cases {
		seedMediaRowIn(t, ms, tc.chat, tc.id)
		ok, _, _, path, err := b.downloadMedia(context.Background(), tc.id, tc.chat)
		if ok || path != "" || err == nil || !strings.Contains(err.Error(), "refusing media path") {
			t.Errorf("%s: ok=%v path=%q err=%v, want a refusal", tc.name, ok, path, err)
		}
		if got := treeEntries(t, scratch); !slices.Equal(got, []string{"store"}) {
			t.Fatalf("%s: the refusal still touched the disk: %v", tc.name, got)
		}
	}
	if got := transfers.Load(); got != 0 {
		t.Errorf("transfers = %d, want 0", got)
	}
	if got := strings.Count(rec.String(), "[WARN] Refusing to cache media"); got != len(cases) {
		t.Errorf("WARN lines = %d, want %d:\n%s", got, len(cases), rec.String())
	}

	// The control: the same fixture does write for an ordinary message, to the
	// name it has always had, so the refusals above are not an inert fake.
	dest := seedMediaRowIn(t, ms, mediaTestChat, "3EB0C0FFEE")
	ok, _, name, path, err := b.downloadMedia(context.Background(), "3EB0C0FFEE", mediaTestChat)
	if !ok || err != nil || path != dest || name != "image_20260904_150405_3EB0C0FFEE.jpg" {
		t.Fatalf("ordinary download: ok=%v name=%q path=%q err=%v (want %q)", ok, name, path, err, dest)
	}
	want := []string{"store", "store/" + mediaTestChat, "store/" + mediaTestChat + "/" + name}
	if got := treeEntries(t, scratch); !slices.Equal(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
}

// A chat directory that is a symlink out of the store does not receive the
// file: the store root refuses to resolve it, where os.MkdirAll and os.OpenFile
// followed the link and wrote wherever it pointed.
func TestDownloadMedia_SymlinkedChatDirDoesNotReceiveFile(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute link", ""}, // filled in below: the outside directory itself
		{"relative link", "../outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := storeInScratch(t)
			outside := filepath.Join(scratch, "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if target == "" {
				target = outside
			}
			link := chatMediaDir(mediaTestChat)
			symlinkOrSkip(t, target, link)

			ms := newConcurrentTestStore(t)
			b := testBridge(t, nil, ms, installRecordingLogger(t))
			var transfers atomic.Int32
			b.mediaTransfer = countingTransfer(&transfers)
			seedMediaRowIn(t, ms, mediaTestChat, "IMG1")

			ok, _, _, path, err := b.downloadMedia(context.Background(), "IMG1", mediaTestChat)
			if ok || path != "" || err == nil {
				t.Fatalf("ok=%v path=%q err=%v, want the download refused", ok, path, err)
			}
			if got := treeEntries(t, outside); len(got) != 0 {
				t.Fatalf("the download followed the symlink out of the store: %v", got)
			}
			if got := transfers.Load(); got != 0 {
				t.Errorf("transfers = %d, want 0", got)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the symlink should be left in place: info=%v err=%v", info, err)
			}
		})
	}
}

func TestWriteMediaFile(t *testing.T) {
	payload := []byte("decrypted media bytes")
	fill := func(f *os.File) error {
		_, err := f.Write(payload)
		return err
	}
	// newStore returns a store with one chat directory, its root, and a file
	// outside the store that no write may reach.
	newStore := func(t *testing.T) (string, *os.Root, string) {
		t.Helper()
		scratch := storeInScratch(t)
		if err := os.Mkdir(storePath("chat"), 0o700); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(scratch, "secret.bin")
		if err := os.WriteFile(secret, []byte("secret-content"), 0o600); err != nil {
			t.Fatal(err)
		}
		return storePath("chat"), storeRootAt(t, storeDir()), secret
	}
	assertSecretIntact := func(t *testing.T, secret string) {
		t.Helper()
		if got, err := os.ReadFile(secret); err != nil || string(got) != "secret-content" { //nolint:gosec // path built by the test under t.TempDir()
			t.Fatalf("the write reached the file outside the store: %q, %v", got, err)
		}
	}

	t.Run("writes through the temp file", func(t *testing.T) {
		chat, root, _ := newStore(t)
		n, err := writeMediaFile(root, "chat/file.jpg", fill)
		if err != nil || n != int64(len(payload)) {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if got, err := os.ReadFile(filepath.Join(chat, "file.jpg")); err != nil || string(got) != string(payload) { //nolint:gosec // path built by the test under t.TempDir()
			t.Fatalf("content = %q, %v", got, err)
		}
		if got := treeEntries(t, chat); !slices.Equal(got, []string{"file.jpg"}) {
			t.Errorf("chat directory = %v, want only the finished file", got)
		}
		if info, err := os.Stat(filepath.Join(chat, "file.jpg")); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o600) {
			t.Errorf("file mode = %v, %v, want 0600", info.Mode().Perm(), err)
		}
	})

	t.Run("a failed fill leaves nothing behind", func(t *testing.T) {
		chat, root, _ := newStore(t)
		boom := errors.New("cdn went away")
		if _, err := writeMediaFile(root, "chat/file.jpg", func(*os.File) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the fill error", err)
		}
		if got := treeEntries(t, chat); len(got) != 0 {
			t.Errorf("left behind: %v", got)
		}
	})

	t.Run("no store root", func(t *testing.T) {
		if _, err := writeMediaFile(nil, "chat/file.jpg", fill); err == nil {
			t.Fatal("expected an error without a store root")
		}
	})

	// os.OpenFile followed a ".part" that was a symlink and truncated its
	// target; the root refuses to open it.
	t.Run("a temp name that is a symlink out of the store is refused", func(t *testing.T) {
		chat, root, secret := newStore(t)
		symlinkOrSkip(t, secret, filepath.Join(chat, "file.jpg.part"))
		if _, err := writeMediaFile(root, "chat/file.jpg", fill); err == nil {
			t.Fatal("expected the write to be refused")
		}
		assertSecretIntact(t, secret)
		if _, err := os.Lstat(filepath.Join(chat, "file.jpg")); !os.IsNotExist(err) {
			t.Errorf("final file must not exist: %v", err)
		}
	})

	// A symlink planted under the final name is replaced by the rename, never
	// written through.
	t.Run("a final name that is a symlink out of the store is replaced", func(t *testing.T) {
		chat, root, secret := newStore(t)
		final := filepath.Join(chat, "file.jpg")
		symlinkOrSkip(t, secret, final)
		if _, err := writeMediaFile(root, "chat/file.jpg", fill); err != nil {
			t.Fatalf("writeMediaFile: %v", err)
		}
		assertSecretIntact(t, secret)
		info, err := os.Lstat(final)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("final name should be a regular file now: info=%v err=%v", info, err)
		}
		if got, _ := os.ReadFile(final); string(got) != string(payload) { //nolint:gosec // path built by the test under t.TempDir()
			t.Errorf("content = %q", got)
		}
	})
}

// Directories the bridge creates in the store are owner-only; one that already
// exists keeps the mode it has.
func TestStoreDirectoriesAreCreatedOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	mode := func(path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	scratch := t.TempDir()

	// The store directory itself, from the message store...
	t.Setenv(storeDirEnv, filepath.Join(scratch, "from-store"))
	created, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	_ = created.Close()
	if got := mode(storeDir()); got != 0o700 {
		t.Errorf("store directory created by NewMessageStore = %o, want 700", got)
	}
	// ...and from the token helper.
	t.Setenv(storeDirEnv, filepath.Join(scratch, "from-token"))
	t.Setenv("WHATSAPP_BRIDGE_TOKEN", "")
	if _, _, err := loadOrCreateBridgeToken(); err != nil {
		t.Fatalf("token: %v", err)
	}
	if got := mode(storeDir()); got != 0o700 {
		t.Errorf("store directory created by the token helper = %o, want 700", got)
	}

	// A chat's media directory, and one that predates this rule.
	const oldChat = "5511888888888@s.whatsapp.net"
	if err := os.Mkdir(storePath(oldChat), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storePath(oldChat), 0o755); err != nil { //nolint:gosec // the looser mode an older bridge left behind is the fixture
		t.Fatal(err)
	}
	ms := newConcurrentTestStore(t)
	b := testBridge(t, nil, ms, installRecordingLogger(t))
	var transfers atomic.Int32
	b.mediaTransfer = countingTransfer(&transfers)
	for _, chat := range []string{mediaTestChat, oldChat} {
		seedMediaRowIn(t, ms, chat, "IMG1")
		if ok, _, _, _, err := b.downloadMedia(context.Background(), "IMG1", chat); !ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", chat, ok, err)
		}
	}
	if got := mode(storePath(mediaTestChat)); got != 0o700 {
		t.Errorf("new chat directory = %o, want 700", got)
	}
	if got := mode(storePath(oldChat)); got != 0o755 {
		t.Errorf("existing chat directory was re-moded to %o, want it left at 755", got)
	}
}
