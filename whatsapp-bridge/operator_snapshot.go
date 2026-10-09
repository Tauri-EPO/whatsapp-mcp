package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type snapshotFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Open only an existing regular store file. In particular notes.db is never
// created, upgraded, attached writable or opened with immutable (WAL matters).
type archiveDatabase struct {
	*sql.DB
	directory *os.File
}

func (db *archiveDatabase) Close() error { err := db.DB.Close(); _ = db.directory.Close(); return err }

func openArchiveDB(root *os.Root, name string, optional bool) (*archiveDatabase, error) {
	if root == nil {
		return nil, errors.New("store unavailable")
	}
	info, err := root.Lstat(name)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("archive source must be a regular database")
	}
	abs, err := filepath.Abs(filepath.Join(root.Name(), name))
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "linux" {
		abs = fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), name)
	}
	uri := archiveReadOnlyURI(abs)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	boundPool(db, 1)
	opened := &archiveDatabase{DB: db, directory: directory}
	if err := db.Ping(); err != nil {
		_ = opened.Close()
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, after) {
		_ = opened.Close()
		return nil, errors.New("archive source changed while opening")
	}
	return opened, nil
}

func archiveReadOnlyURI(name string) string {
	name = filepath.ToSlash(name)
	// A drive-letter path must be file:///C:/..., with an empty authority.
	// Without the leading slash net/url emits file://C:/..., rejected by SQLite.
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return (&url.URL{Scheme: "file", Path: name}).String() + "?" + sqliteReadOnlyOptions
}

func privateSnapshotDir(directory string) (*os.Root, *os.File, error) {
	if directory == "" {
		return nil, nil, errors.New("WHATSAPP_SNAPSHOT_DIR is not configured")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil, err
	}
	// Refuse links in every existing component, including a link to a directory
	// inside the store. Never chmod a symlink target.
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return nil, nil, errors.New("snapshot directory cannot contain symlinks")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() {
		return nil, nil, errors.New("snapshot directory is not a real directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o002 != 0 {
		return nil, nil, errors.New("snapshot directory must not be world-writable")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, nil, err
	}
	f, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	pinned, err := f.Stat()
	if err != nil || !os.SameFile(info, pinned) {
		_ = root.Close()
		_ = f.Close()
		return nil, nil, errors.New("snapshot directory changed while opening")
	}
	if runtime.GOOS != "windows" {
		if err := f.Chmod(0o700); err != nil {
			_ = root.Close()
			_ = f.Close()
			return nil, nil, err
		}
		if mode, err := f.Stat(); err != nil || mode.Mode().Perm() != 0o700 {
			_ = root.Close()
			_ = f.Close()
			return nil, nil, errors.New("snapshot directory must enforce 0700")
		}
	}
	return root, f, nil
}

var errSnapshotSpace = errors.New("insufficient snapshot filesystem space")
var errSnapshotRetention = errors.New("snapshot retained; retention cleanup failed")

type snapshotOptions struct {
	keep      int
	freeBytes func(*os.File) (uint64, error)
	prune     func(context.Context, *os.Root, *os.File, string, int) error
	copy      func(context.Context, io.Writer, io.Reader) (int64, error)
}

func snapshotRequiredBytes(source *os.Root, session bool) (uint64, error) {
	if source == nil {
		return 0, errors.New("store unavailable")
	}
	var size uint64
	for _, name := range []string{"messages.db", "notes.db", "whatsapp.db"} {
		if name == "whatsapp.db" && !session {
			continue
		}
		for _, suffix := range []string{"", "-wal"} {
			info, err := source.Lstat(name + suffix)
			if errors.Is(err, os.ErrNotExist) && (name == "notes.db" || suffix != "") {
				continue
			}
			if err != nil {
				return 0, err
			}
			if !info.Mode().IsRegular() || info.Size() < 0 {
				return 0, errors.New("invalid snapshot database file")
			}
			if uint64(info.Size()) > (^uint64(0)-size)/2 { //nolint:gosec // Negative sizes rejected above.
				return 0, errSnapshotSpace
			}
			size += uint64(info.Size()) //nolint:gosec // Negative sizes rejected above.
		}
	}
	// VACUUM needs a compact output and SQLite work space. Reserve twice the
	// source DB+WAL size, with a further fixed 16 MiB for growth and metadata.
	if size > (^uint64(0)-(16<<20))/2 {
		return 0, errSnapshotSpace
	}
	return 2*size + (16 << 20), nil
}

func snapshotStores(ctx context.Context, source *os.Root, directory string, session bool, options ...snapshotOptions) (files []snapshotFile, err error) {
	root, dir, err := privateSnapshotDir(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close(); _ = dir.Close() }()
	opt := snapshotOptions{keep: 7, freeBytes: snapshotFreeBytes, prune: pruneSnapshotSets, copy: copyArchiveContext}
	if len(options) > 0 {
		if options[0].keep > 0 {
			opt.keep = options[0].keep
		}
		if options[0].freeBytes != nil {
			opt.freeBytes = options[0].freeBytes
		}
		if options[0].prune != nil {
			opt.prune = options[0].prune
		}
		if options[0].copy != nil {
			opt.copy = options[0].copy
		}
	}
	required, err := snapshotRequiredBytes(source, session)
	if err != nil {
		return nil, err
	}
	available, err := opt.freeBytes(dir)
	if err != nil {
		return nil, err
	}
	if available < required {
		return nil, errSnapshotSpace
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	prefix := "snapshot-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:]) + "-"
	var created []string
	published := false
	defer func() {
		if err != nil && !published {
			for _, name := range created {
				_ = root.Remove(name)
			}
		}
	}()
	for _, name := range []string{"messages.db", "notes.db", "whatsapp.db"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name == "whatsapp.db" && !session {
			continue
		}
		db, openErr := openArchiveDB(source, name, name == "notes.db")
		if openErr != nil {
			return nil, openErr
		}
		if db == nil {
			continue
		}
		fileName := prefix + name
		partial := fileName + ".partial"
		file, createErr := root.OpenFile(partial, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			_ = db.Close()
			return nil, createErr
		}
		created = append(created, partial)
		// On the shipped Linux image, SQLite resolves the target through the
		// pinned directory FD even if its pathname is concurrently replaced.
		target := filepath.Join(root.Name(), partial)
		if runtime.GOOS == "linux" {
			target = fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), partial)
		}
		_, vacuumErr := db.ExecContext(ctx, "VACUUM INTO ?", target)
		_ = db.Close()
		if vacuumErr != nil {
			_ = file.Close()
			return nil, vacuumErr
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, statErr
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			_ = file.Close()
			return nil, errors.New("snapshot file must enforce 0600")
		}
		hash := sha256.New()
		_, copyErr := opt.copy(ctx, hash, file)
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		if copyErr == nil {
			copyErr = file.Sync()
		}
		closeErr := file.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return nil, copyErr
		}
		files = append(files, snapshotFile{Name: fileName, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	// Publish only after every database has a flushed, verified .partial file.
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := root.Rename(file.Name+".partial", file.Name); err != nil {
			return nil, err
		}
		created = append(created, file.Name)
		if err := syncSnapshotDirectory(dir); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	published = true
	if err := opt.prune(ctx, root, dir, prefix, opt.keep); err != nil {
		return files, errors.Join(errSnapshotRetention, err)
	}
	return files, nil
}

func snapshotSetPrefix(name string) string {
	const prefixLength = len("snapshot-20060102T150405Z-") + 32 + 1
	if len(name) <= prefixLength || !strings.HasPrefix(name, "snapshot-") {
		return ""
	}
	prefix, suffix := name[:prefixLength], name[prefixLength:]
	if suffix != "messages.db" && suffix != "notes.db" && suffix != "whatsapp.db" {
		return ""
	}
	if prefix[25] != '-' || prefix[len(prefix)-1] != '-' {
		return ""
	}
	if _, err := time.Parse("20060102T150405Z", prefix[9:25]); err != nil {
		return ""
	}
	if nonce, err := hex.DecodeString(prefix[26 : len(prefix)-1]); err != nil || len(nonce) != 16 {
		return ""
	}
	return prefix
}

func pruneSnapshotSets(ctx context.Context, root *os.Root, dir *os.File, current string, keep int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	sets := map[string]time.Time{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		prefix := snapshotSetPrefix(entry.Name())
		if prefix == "" || prefix == current || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if old, ok := sets[prefix]; !ok || info.ModTime().After(old) {
			sets[prefix] = info.ModTime()
		}
	}
	prefixes := make([]string, 0, len(sets))
	for prefix := range sets {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if sets[prefixes[i]].Equal(sets[prefixes[j]]) {
			return prefixes[i] > prefixes[j]
		}
		return sets[prefixes[i]].After(sets[prefixes[j]])
	})
	for _, prefix := range prefixes[min(keep-1, len(prefixes)):] {
		for _, name := range []string{"messages.db", "notes.db", "whatsapp.db"} {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := root.Lstat(prefix + name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if err := root.Remove(prefix + name); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return syncSnapshotDirectory(dir)
}

type archiveContextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}
func copyArchiveContext(ctx context.Context, out io.Writer, in io.Reader) (int64, error) {
	return io.CopyBuffer(out, archiveContextReader{ctx, in}, make([]byte, 32<<10))
}

func removeSessionSnapshots(directory string) (int, error) {
	if directory == "" {
		return 0, nil
	}
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	root, dir, err := privateSnapshotDir(directory)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close(); _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		// Include leftovers from an interrupted snapshot: they can contain keys.
		base := strings.TrimSuffix(name, ".partial")
		if snapshotSetPrefix(base) == "" || !strings.HasSuffix(base, "-whatsapp.db") {
			continue
		}
		if !entry.Type().IsRegular() {
			return removed, errors.New("session snapshot is not a regular file")
		}
		if err := root.Remove(name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, syncSnapshotDirectory(dir)
}

func operatorBoolQuery(r *http.Request, key string) (bool, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false, errors.New("invalid query encoding")
	}
	if len(values) == 0 {
		return false, nil
	}
	if len(values) != 1 || len(values[key]) != 1 {
		return false, errors.New("unsupported query parameter")
	}
	switch values.Get(key) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New("query value must be true or false")
}

func (b *Bridge) handleSnapshot() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := operatorBoolQuery(r, "session")
		if err != nil || r.ContentLength != 0 {
			writeError(w, 400, "Snapshot accepts only session=true|false and an empty body")
			return
		}
		if session && !b.Archive.Session {
			writeError(w, 403, "Session snapshots require WHATSAPP_SNAPSHOT_SESSION=true")
			return
		}
		if b.SnapshotDir == "" {
			writeError(w, 503, "WHATSAPP_SNAPSHOT_DIR is not configured")
			return
		}
		if !b.snapshotBusy.CompareAndSwap(false, true) {
			writeError(w, 429, "A snapshot is already running")
			return
		}
		defer b.snapshotBusy.Store(false)
		start := time.Now()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(b.archiveTimeout())); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, 500, "Snapshot deadline unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), b.archiveTimeout())
		defer cancel()
		if session {
			release, admitted := b.beginArchiveSessionRead(cancel)
			if !admitted {
				writeError(w, 503, "Session snapshot unavailable during operator logout")
				return
			}
			defer release()
		}
		files, err := snapshotStores(ctx, b.StoreRoot, b.SnapshotDir, session, snapshotOptions{keep: b.Archive.Keep, freeBytes: b.snapshotSpace, prune: b.snapshotPrune})
		if err != nil && (!errors.Is(err, errSnapshotRetention) || ctx.Err() != nil) {
			b.Log.Warnf("Operator snapshot failed")
			if errors.Is(err, errSnapshotSpace) {
				writeError(w, 507, "Snapshot refused: insufficient free space for databases, WAL and safety margin")
				return
			}
			writeError(w, 500, "Snapshot failed; incomplete files were removed")
			return
		}
		result := map[string]any{"files": files}
		if errors.Is(err, errSnapshotRetention) {
			b.Log.Warnf("Operator snapshot retained; retention cleanup failed")
			result["retention_warning"] = true
		}
		b.Log.Infof("Operator snapshot: files=%d session=%t duration_ms=%d", len(files), session, time.Since(start).Milliseconds())
		writeJSON(w, 200, result)
	}
}

func snapshotCLI(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	directory := flags.String("out", "", "Private snapshot directory")
	session := flags.Bool("session", false, "Include the credential-bearing session store")
	if flags.Parse(args) != nil || flags.NArg() != 0 || strings.TrimSpace(*directory) == "" {
		return 1
	}
	source, err := openStoreRoot()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Snapshot source unavailable")
		return 1
	}
	defer func() { _ = source.Close() }()
	cfg, err := parseArchiveConfig(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := validateSnapshotLocation(*directory, storeDir(), os.Getenv("WHATSAPP_MEDIA_ROOTS")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	files, err := snapshotStores(ctx, source, *directory, *session, snapshotOptions{keep: cfg.Keep})
	if err != nil && (!errors.Is(err, errSnapshotRetention) || ctx.Err() != nil) {
		_, _ = fmt.Fprintln(os.Stderr, "Snapshot failed")
		return 1
	}
	returnCode := 0
	result := map[string]any{"files": files}
	if errors.Is(err, errSnapshotRetention) {
		bridgeLog.Warnf("Operator snapshot CLI retained; retention cleanup failed")
		result["retention_warning"] = true
	}
	bridgeLog.Infof("Operator snapshot CLI: files=%d session=%t", len(files), *session)
	if err := json.NewEncoder(out).Encode(result); err != nil {
		returnCode = 1
	}
	return returnCode
}
