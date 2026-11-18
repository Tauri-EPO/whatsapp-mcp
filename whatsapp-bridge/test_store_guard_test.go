package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

type testStorePaths struct {
	temporary string
	source    string
}

type testStoreSuiteState struct {
	paths        testStorePaths
	err          error
	helper       bool
	fixtureStore string
}

// Write-once before TestMain: tests change TMPDIR and cwd, and resolving the
// suite roots in every fixture is both incorrect under aliases and expensive.
var testStoreSuite = loadTestStoreSuite()

func loadTestStoreSuite() testStoreSuiteState {
	paths, err := newTestStorePaths(os.TempDir(), defaultStoreDir)
	state := testStoreSuiteState{paths: paths, err: err}
	// These CLI children deliberately receive their parent's temporary store.
	// TestMain still clears host configuration; restore only a validated fixture.
	for _, child := range []struct {
		env, testName string
		store         bool
	}{
		{"W11_SNAPSHOT_CLI_TEST", "TestSnapshotCLIJSONStdout", true},
		{"WAMCP_TEST_OPERATOR_STARTUP", "TestOperatorStartupHelper", true},
		{"WAMCP_TEST_BLOCKED_SIGNAL", "TestStartupBlockedSignalHelper", true},
		{"WAMCP_TEST_UNLINK_RECOVERY", "TestUnlinkRecoveryRunBridgeHelper", true},
		{"TEST_SOURCE_STORE_HELPER", "TestStoreHelpersRefuseSourceTree", false},
		{"TEST_STORE_ENV_HELPER", "TestStoreGuardIgnoresInheritedStore", false},
		{"WAMCP_TEST_OPERATOR_EXIT", "TestOperatorLogoutExitProcessHelper", false},
		{"BRIDGE_LOCK_HELPER", "TestAcquireInstanceLock_ReleasedWhenHolderDies", false},
		{"WAMCP_UMASK_FIXTURE", "TestPrivateProcessUmaskIncludesSQLiteSidecars", false},
	} {
		if os.Getenv(child.env) == "" || !slices.Contains(os.Args[1:], "-test.run=^"+child.testName+"$") {
			continue
		}
		state.helper = true
		if child.store {
			state.fixtureStore = os.Getenv(storeDirEnv)
		}
	}
	// TEST_SOURCE_STORE_LEAK intentionally exercises the real TestMain walks.
	return state
}

func newTestStorePaths(temporary, source string) (testStorePaths, error) {
	temp, err := resolveTestStorePath(temporary)
	if err != nil {
		return testStorePaths{}, err
	}
	root, err := resolveTestStorePath(source)
	return testStorePaths{temporary: temp, source: root}, err
}

// Resolve missing suffixes through their nearest existing ancestor, including
// a dangling symlink whose target has not been created yet.
func resolveTestStorePath(name string) (string, error) {
	return resolveTestStorePathSeen(name, nil)
}

func resolveTestStorePathSeen(name string, seen map[string]bool) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil || !os.IsNotExist(err) {
		return resolved, err
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		if seen == nil {
			seen = make(map[string]bool)
		}
		if seen[abs] {
			return "", fmt.Errorf("cyclic temporary store symlink: %s", abs)
		}
		seen[abs] = true
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		return resolveTestStorePathSeen(target, seen)
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	resolved, err = resolveTestStorePathSeen(parent, seen)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(abs)), nil
}

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
	if testStoreSuite.err != nil {
		return testStoreSuite.err
	}
	return testStoreSuite.paths.validate(dir)
}

func (p testStorePaths) validate(dir string) error {
	root, err := resolveTestStorePath(dir)
	if err != nil {
		return err
	}
	// Compare canonical paths on both sides, even for an aliased TMPDIR/cwd.
	if pathWithin(p.source, root) || !pathWithin(p.temporary, root) || pathWithin(root, p.temporary) {
		return fmt.Errorf("test store must be a temporary directory outside package store/: %s", dir)
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

// Keys include directories and symlinks; true means an opaque, unreadable
// entry. Unknown contents under a previously opaque directory are not new.
type storeSnapshot map[string]bool

func storeEntries(root string) storeSnapshot {
	return snapshotStoreEntries(os.DirFS(root))
}

func snapshotStoreEntries(tree fs.FS) storeSnapshot {
	entries := make(storeSnapshot)
	_ = fs.WalkDir(tree, ".", func(name string, _ fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			delete(entries, name)
			return nil
		}
		// A read error makes this entry opaque, not a test failure. WalkDir
		// already stops descending when the directory cannot be read.
		entries[name] = err != nil
		return nil
	})
	return entries
}

func (s storeSnapshot) contains(name string) bool {
	if _, exists := s[name]; exists {
		return true
	}
	for parent := path.Dir(name); ; parent = path.Dir(parent) {
		if s[parent] {
			return true
		}
		if parent == "." {
			return false
		}
	}
}

// Probe only an existing file, without acquireInstanceLock's create/PID write.
func packageStoreInUse(root string) bool {
	store, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer func() { _ = store.Close() }()
	lock, err := store.OpenFile(".bridge.lock", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }()
	if err := lockFileExclusive(lock); err != nil {
		return errors.Is(err, errInstanceLocked)
	}
	_ = unlockFile(lock)
	return false
}

// Called by TestMain before/after all tests, without deleting anything in an
// existing developer store. Failure diagnostics include every new entry.
func runWithStoreGuard(run func() int) int {
	if testStoreSuite.err != nil {
		fmt.Fprintf(os.Stderr, "resolve test store roots: %v\n", testStoreSuite.err)
		return 1
	}
	if testStoreSuite.fixtureStore != "" {
		if err := validateTemporaryStore(testStoreSuite.fixtureStore); err != nil {
			fmt.Fprintf(os.Stderr, "invalid child fixture store: %v\n", err)
			return 1
		}
		if err := os.Setenv(storeDirEnv, testStoreSuite.fixtureStore); err != nil {
			fmt.Fprintf(os.Stderr, "restore child fixture store: %v\n", err)
			return 1
		}
	}
	if testStoreSuite.helper {
		return run() // Leaf re-execs need no two additional source-store walks.
	}
	return guardTestStore(testStoreSuite.paths.source, run, os.Stderr)
}

func guardTestStore(root string, run func() int, output io.Writer) int {
	notice := func() {
		_, _ = fmt.Fprintln(output, "Skipping package store leak comparison: a live bridge holds store/.bridge.lock")
	}
	if packageStoreInUse(root) {
		notice()
		return run()
	}
	before := storeEntries(root)
	code := run()
	if packageStoreInUse(root) {
		notice()
		return code
	}
	after := storeEntries(root)
	var added []string
	for entry := range after {
		if !before.contains(entry) {
			added = append(added, entry)
		}
	}
	if len(added) != 0 {
		slices.Sort(added)
		_, _ = fmt.Fprintf(output, "Go tests leaked new entries under package store/: %q\n", added)
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
	paths, err := newTestStorePaths(os.TempDir(), defaultStoreDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{defaultStoreDir, filepath.Join(defaultStoreDir, "chat"), os.TempDir(), filepath.Dir(os.TempDir())} {
		if err := paths.validate(dir); err == nil {
			t.Errorf("unsafe test store accepted: %s", dir)
		}
	}
	if err := paths.validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestStoreHelpersRefuseSourceTree(t *testing.T) {
	if helper := os.Getenv("TEST_SOURCE_STORE_HELPER"); helper != "" {
		// TestMain cleared the inherited host value; this is intentional input
		// supplied by the test itself and must still be rejected unconditionally.
		t.Setenv(storeDirEnv, testStoreSuite.paths.source)
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
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%v", helper, live), func(t *testing.T) {
				checkout := t.TempDir()
				root := filepath.Join(checkout, defaultStoreDir)
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if live {
					lock, err := acquireInstanceLock(filepath.Join(root, ".bridge.lock"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(lock.Release)
				}
				cmd := storeTestCommand(t, "^TestStoreHelpersRefuseSourceTree$")
				cmd.Dir = checkout
				cmd.Env = append(os.Environ(), "TEST_SOURCE_STORE_HELPER="+helper, storeDirEnv+"="+root)
				output, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(output), "test store must be a temporary directory outside package store/") {
					t.Fatalf("helper did not refuse source store: %v\n%s", err, output)
				}
				entries, err := os.ReadDir(root)
				if err != nil || (!live && len(entries) != 0) || (live && (len(entries) != 1 || entries[0].Name() != ".bridge.lock")) {
					t.Fatalf("refused helper touched source store: %v, %v", entries, err)
				}
			})
		}
	}
}

func TestTemporaryStoreRejectsSymlinkToSourceStore(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			physicalTemp := t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			symlinkOrSkip(t, physicalTemp, alias)
			source := filepath.Join(alias, "checkout", defaultStoreDir)
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			paths, err := newTestStorePaths(alias, source)
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(alias, "source-link")
			symlinkOrSkip(t, source, link)
			for _, candidate := range []string{source, link, filepath.Join(link, "not-created", "media")} {
				if err := paths.validate(candidate); err == nil {
					t.Errorf("temporary symlink into source store accepted: %s", candidate)
				}
			}
			if _, err := os.Stat(filepath.Join(source, "not-created")); !os.IsNotExist(err) {
				t.Fatalf("validation created source entries: %v", err)
			}
			if err := paths.validate(filepath.Join(alias, "safe-new-store", "media")); err != nil {
				t.Fatalf("valid missing temp store refused: %v", err)
			}
		})
	}
}

func TestTemporaryStoreRejectsDanglingSymlinkCycle(t *testing.T) {
	link := filepath.Join(t.TempDir(), "loop")
	// EvalSymlinks stops at the missing component; cleaning its target would
	// otherwise make the nearest-ancestor fallback recurse into itself.
	symlinkOrSkip(t, "missing/../loop", link)
	if _, err := resolveTestStorePath(link); err == nil || !strings.Contains(err.Error(), "cyclic temporary store symlink") {
		t.Fatalf("dangling cycle not refused: %v", err)
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
	if got := guardTestStore(defaultStoreDir, func() int { return 0 }, os.Stderr); got != 0 {
		t.Fatalf("untouched store: exit %d", got)
	}
	if got := guardTestStore(defaultStoreDir, func() int { return 7 }, os.Stderr); got != 7 {
		t.Fatalf("test failure exit changed: %d", got)
	}
	if got := guardTestStore(defaultStoreDir, func() int {
		if err := os.Mkdir(filepath.Join(defaultStoreDir, "leaked-empty-chat"), 0o700); err != nil {
			t.Fatal(err)
		}
		return 0
	}, os.Stderr); got != 1 {
		t.Fatalf("leaked directory did not fail the run: exit %d", got)
	}
	if got := guardTestStore(defaultStoreDir, func() int {
		if err := os.WriteFile(filepath.Join(defaultStoreDir, "leaked-empty-chat", "image.jpg"), []byte("fake image"), 0o600); err != nil {
			t.Fatal(err)
		}
		return 0
	}, os.Stderr); got != 1 {
		t.Fatalf("leaked file did not fail the run: exit %d", got)
	}
	if data, err := os.ReadFile(old); err != nil || string(data) != "existing store" { //nolint:gosec // old is existing.db in this test's temporary working directory.
		t.Fatalf("existing store changed: %q, %v", data, err)
	}
}

func TestStoreGuardDetectsNewStore(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := guardTestStore(defaultStoreDir, func() int {
		if err := os.Mkdir(defaultStoreDir, 0o700); err != nil {
			t.Fatal(err)
		}
		return 0
	}, os.Stderr); got != 1 {
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
	cmd := storeTestCommand(t, "^TestStoreGuardRejectsLeakingProcess$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "TEST_SOURCE_STORE_LEAK=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Go tests leaked new entries under package store/") || !strings.Contains(string(output), "fake-image.jpg") {
		t.Fatalf("TestMain accepted a source-store leak: %v\n%s", err, output)
	}
}

func storeTestCommand(t *testing.T, selector string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exec.CommandContext(t.Context(), self, "-test.run="+selector) //nolint:gosec // This binary and fixed selectors supplied by the test functions only.
}

func TestStoreGuardIgnoresInheritedStore(t *testing.T) {
	if os.Getenv("TEST_STORE_ENV_HELPER") == "1" {
		if _, inherited := os.LookupEnv(storeDirEnv); inherited {
			t.Fatal("TestMain retained the host store")
		}
		b := testBridge(t, nil, newTestMessageStore(t), testLogger())
		if b.StoreRoot == nil || validateTemporaryStore(storeDir()) != nil {
			t.Fatal("inherited host configuration prevented an isolated fixture")
		}
		return
	}
	cmd := storeTestCommand(t, "^TestStoreGuardIgnoresInheritedStore$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "TEST_STORE_ENV_HELPER=1", storeDirEnv+"=/app/store")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host store broke child tests: %v\n%s", err, output)
	}
}

type faultyStoreFS struct {
	fs.FS
	faults map[string]error
}

func (tree faultyStoreFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := tree.faults[name]; err != nil {
		return nil, err
	}
	return fs.ReadDir(tree.FS, name)
}

func TestStoreGuardToleratesUnreadableAndVanishingEntries(t *testing.T) {
	tree := fstest.MapFS{
		"existing.db":     &fstest.MapFile{Data: []byte("fake database")},
		"opaque/image":    &fstest.MapFile{},
		"vanishing/image": &fstest.MapFile{},
	}
	before := snapshotStoreEntries(faultyStoreFS{FS: tree, faults: map[string]error{"opaque": fs.ErrPermission, "vanishing": fs.ErrNotExist}})
	if !before["opaque"] || before.contains("vanishing") || before.contains("vanishing/image") {
		t.Fatalf("unreadable/vanishing entries mishandled: %v", before)
	}
	for _, name := range []string{"existing.db", "opaque", "opaque/image", "opaque/new-unknown-image"} {
		if !before.contains(name) {
			t.Errorf("previously observed/opaque entry treated as new: %s", name)
		}
	}
	rootOpaque := snapshotStoreEntries(faultyStoreFS{FS: tree, faults: map[string]error{".": fs.ErrPermission}})
	if !rootOpaque["."] || !rootOpaque.contains("existing.db") {
		t.Fatalf("unreadable root not opaque: %v", rootOpaque)
	}
	if before.contains("new-chat/image") {
		t.Fatal("a readable store's new chat was hidden")
	}
}

func TestStoreGuardUnreadableDirectoryCanBecomeReadable(t *testing.T) {
	root := t.TempDir()
	sr := storeRootAt(t, root)
	if err := sr.Mkdir("opaque", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sr.WriteFile("opaque/fake-image", []byte("pre-existing fake image"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "opaque")
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	restore := func() error {
		return os.Chmod(private, 0o700) //nolint:gosec // Restore owner-only directory traversal, not file permissions.
	}
	t.Cleanup(func() { _ = restore() })
	if _, err := os.ReadDir(private); err == nil {
		t.Skip("directory permissions are not enforced for this user")
	}
	if before := storeEntries(root); !before["opaque"] {
		t.Fatalf("unreadable directory not opaque: %v", before)
	}
	var output bytes.Buffer
	if result := guardTestStore(root, func() int {
		if err := restore(); err != nil {
			t.Fatal(err)
		}
		return 0
	}, &output); result != 0 {
		t.Fatalf("pre-existing contents became false leaks after chmod: %d, %s", result, &output)
	}
}

func TestStoreGuardProbeDoesNotCreateOrRewriteLock(t *testing.T) {
	root := t.TempDir()
	if packageStoreInUse(root) {
		t.Fatal("empty store marked busy")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("probe created an entry: %v, %v", entries, err)
	}
	sr := storeRootAt(t, root)
	if err := sr.WriteFile(".bridge.lock", []byte("fake pre-existing lock contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if packageStoreInUse(root) {
		t.Fatal("unheld lock file marked busy")
	}
	if data, err := sr.ReadFile(".bridge.lock"); err != nil || string(data) != "fake pre-existing lock contents" {
		t.Fatalf("probe rewrote lock contents: %q, %v", data, err)
	}
}

func TestStoreGuardSkipsLiveBridge(t *testing.T) {
	for _, heldAtStart := range []bool{false, true} {
		for _, code := range []int{0, 7} {
			t.Run(fmt.Sprintf("held=%v/exit=%d", heldAtStart, code), func(t *testing.T) {
				root := t.TempDir()
				var lock *instanceLock
				hold := func() {
					var err error
					lock, err = acquireInstanceLock(filepath.Join(root, ".bridge.lock"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(lock.Release)
				}
				if heldAtStart {
					hold()
				}
				sr := storeRootAt(t, root)
				var output bytes.Buffer
				result := guardTestStore(root, func() int {
					if !heldAtStart {
						hold()
					}
					if err := sr.WriteFile(".session-keepalive", []byte("fake live bridge state"), 0o600); err != nil {
						t.Fatal(err)
					}
					return code
				}, &output)
				if result != code || strings.Count(output.String(), "Skipping package store leak comparison") != 1 {
					t.Fatalf("busy store changed result/notice: %d, %s", result, &output)
				}
				if !packageStoreInUse(root) || readLockHolder(lock.f) != fmt.Sprint(os.Getpid()) {
					t.Fatal("probe took over or changed the live bridge lock")
				}
			})
		}
	}
}
