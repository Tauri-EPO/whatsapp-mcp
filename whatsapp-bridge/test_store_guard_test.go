package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Some tests override TMPDIR to inspect download spools. Keep the suite's
// original temporary root so their separate media fixtures remain valid.
var testTemporaryRoot = os.TempDir()

// An in-memory MessageStore does not isolate the filesystem media cache.
// Setenv restores the caller's environment after workers and roots are closed.
func testStoreDir(t testing.TB) {
	t.Helper()
	if os.Getenv(storeDirEnv) == "" {
		t.Setenv(storeDirEnv, t.TempDir())
	}
	requireTemporaryStore(t, storeDir())
}

func requireTemporaryStore(t testing.TB, dir string) {
	t.Helper()
	if err := validateTemporaryStore(dir); err != nil {
		t.Fatal(err)
	}
}

func validateTemporaryStore(dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	temp, err := filepath.Abs(testTemporaryRoot)
	if err != nil {
		return err
	}
	source, err := filepath.Abs(defaultStoreDir)
	if err != nil {
		return err
	}
	// Reject the package store even when a checkout itself lives under /tmp.
	if pathWithin(source, root) || !pathWithin(temp, root) || root == temp {
		return fmt.Errorf("test store must be a temporary directory outside package store/: %s", dir)
	}
	// An existing store-root symlink must not redirect a temp fixture elsewhere.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		resolvedTemp, err := filepath.EvalSymlinks(temp)
		if err != nil {
			return err
		}
		if !pathWithin(resolvedTemp, resolved) || resolved == resolvedTemp || pathWithin(source, resolved) {
			return fmt.Errorf("test store resolves outside its temporary directory: %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func assertTestCachedMedia(t *testing.T, chat, id, source string) {
	t.Helper()
	requireTemporaryStore(t, storeDir())
	matches, err := filepath.Glob(filepath.Join(storeDir(), chatMediaRel(chat), "*_"+id+".*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("temporary media cache for %s: %v, %v", id, matches, err)
	}
	want, err := os.ReadFile(source) //nolint:gosec // source is a fake media file created by the caller under t.TempDir().
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("temporary media cache for %s: got %q, want %q, error %v", id, got, want, err)
	}
}

// Include directories and symlinks as well as files; an empty leaked chat
// directory is still a leak. WalkDir does not traverse directory symlinks.
func storeEntries(root string) (map[string]bool, error) {
	entries := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries[rel] = true
		return nil
	})
	return entries, err
}

// Called by TestMain before/after all tests, without deleting anything in an
// existing developer store. Failure diagnostics include every new entry.
func runWithStoreGuard(run func() int) int {
	root, err := filepath.Abs(defaultStoreDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect package store/: %v\n", err)
		return 1
	}
	before, err := storeEntries(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect package store/ before tests: %v\n", err)
		return 1
	}
	code := run()
	after, err := storeEntries(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "inspect package store/ after tests: %v\n", err)
		return 1
	}
	var added []string
	for entry := range after {
		if !before[entry] {
			added = append(added, entry)
		}
	}
	if len(added) != 0 {
		slices.Sort(added)
		fmt.Fprintf(os.Stderr, "Go tests leaked new entries under package store/: %q\n", added)
		return 1
	}
	return code
}

func TestTestStoreDirIsolatesMediaFixtures(t *testing.T) {
	t.Setenv(storeDirEnv, "")
	ms := newTestMessageStore(t)
	root := storeDir()
	if root == defaultStoreDir || validateTemporaryStore(root) != nil {
		t.Fatalf("in-memory DB has unsafe media store %q", root)
	}
	b := testBridge(t, nil, ms, testLogger())
	if storeDir() != root || b.StoreRoot == nil {
		t.Fatal("bridge did not retain the test's temporary media store")
	}
	b.recordConnectionProblem(406, 0, 0)
	if problem, err := readConnectionProblem(b.StoreRoot); err != nil || problem == nil || problem.Kind != "banned" {
		t.Fatalf("connection marker not in the test store: %+v, %v", problem, err)
	}
	other := t.TempDir()
	t.Setenv(storeDirEnv, other)
	testStoreDir(t)
	if storeDir() != other {
		t.Fatal("explicit temporary media store was replaced")
	}
}

func TestTemporaryStoreRejectsSourceTree(t *testing.T) {
	// Put a fake checkout under /tmp too: merely checking the temp prefix is
	// insufficient to protect a checkout created in a temporary directory.
	t.Chdir(t.TempDir())
	for _, dir := range []string{defaultStoreDir, filepath.Join(defaultStoreDir, "chat"), os.TempDir(), filepath.Dir(os.TempDir())} {
		if err := validateTemporaryStore(dir); err == nil {
			t.Errorf("unsafe test store accepted: %s", dir)
		}
	}
	if err := validateTemporaryStore(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestStoreHelpersRefuseSourceTree(t *testing.T) {
	if helper := os.Getenv("TEST_SOURCE_STORE_HELPER"); helper != "" {
		switch helper {
		case "store":
			newTestMessageStore(t)
		case "bridge":
			testBridge(t, nil, nil, testLogger())
		case "root":
			storeRootAt(t, storeDir())
		}
		t.Fatal("helper accepted source-tree store")
	}
	for _, helper := range []string{"store", "bridge", "root"} {
		t.Run(helper, func(t *testing.T) {
			checkout := t.TempDir()
			root := filepath.Join(checkout, defaultStoreDir)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStoreHelpersRefuseSourceTree$") //nolint:gosec // Re-executes this test binary with a fixed helper selector.
			cmd.Dir = checkout
			cmd.Env = append(os.Environ(), "TEST_SOURCE_STORE_HELPER="+helper, storeDirEnv+"="+root)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "test store must be a temporary directory outside package store/") {
				t.Fatalf("helper did not refuse source store: %v\n%s", err, output)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("refused helper touched source store: %v, %v", entries, err)
			}
		})
	}
}

func TestTemporaryStoreRejectsSymlinkToSourceStore(t *testing.T) {
	t.Chdir(t.TempDir())
	source, err := filepath.Abs(defaultStoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "source")
	symlinkOrSkip(t, source, link)
	if err := validateTemporaryStore(link); err == nil {
		t.Fatal("temporary symlink to source store accepted as test store")
	}
}

func TestStoreGuardDetectsNewEntriesAndPreservesExisting(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir(defaultStoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(defaultStoreDir, "existing.db")
	if err := os.WriteFile(old, []byte("existing store"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runWithStoreGuard(func() int { return 0 }); got != 0 {
		t.Fatalf("untouched store: exit %d", got)
	}
	if got := runWithStoreGuard(func() int { return 7 }); got != 7 {
		t.Fatalf("test failure exit changed: %d", got)
	}
	if got := runWithStoreGuard(func() int {
		if err := os.Mkdir(filepath.Join(defaultStoreDir, "leaked-empty-chat"), 0o700); err != nil {
			t.Fatal(err)
		}
		return 0
	}); got != 1 {
		t.Fatalf("leaked directory did not fail the run: exit %d", got)
	}
	if got := runWithStoreGuard(func() int {
		if err := os.WriteFile(filepath.Join(defaultStoreDir, "leaked-empty-chat", "image.jpg"), []byte("fake image"), 0o600); err != nil {
			t.Fatal(err)
		}
		return 0
	}); got != 1 {
		t.Fatalf("leaked file did not fail the run: exit %d", got)
	}
	if data, err := os.ReadFile(old); err != nil || string(data) != "existing store" { //nolint:gosec // old is existing.db in this test's temporary working directory.
		t.Fatalf("existing store changed: %q, %v", data, err)
	}
}

func TestStoreGuardDetectsNewStore(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := runWithStoreGuard(func() int {
		if err := os.Mkdir(defaultStoreDir, 0o700); err != nil {
			t.Fatal(err)
		}
		return 0
	}); got != 1 {
		t.Fatalf("new package store did not fail the run: exit %d", got)
	}
}

func TestStoreGuardRejectsLeakingProcess(t *testing.T) {
	if os.Getenv("TEST_SOURCE_STORE_LEAK") == "1" {
		if err := os.Mkdir(defaultStoreDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(defaultStoreDir, "fake-image.jpg"), []byte("fake image"), 0o600); err != nil {
			t.Fatal(err)
		}
		return // The test passes; TestMain must turn the process result red.
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStoreGuardRejectsLeakingProcess$") //nolint:gosec // Re-executes this test binary with a fixed leak-regression selector.
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "TEST_SOURCE_STORE_LEAK=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Go tests leaked new entries under package store/") || !strings.Contains(string(output), "fake-image.jpg") {
		t.Fatalf("TestMain accepted a source-store leak: %v\n%s", err, output)
	}
}
