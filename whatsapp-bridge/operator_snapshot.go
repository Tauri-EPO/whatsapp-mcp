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
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String() + "?" + sqliteReadOnlyOptions
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

func snapshotStores(ctx context.Context, source *os.Root, directory string, session bool) (files []snapshotFile, err error) {
	root, dir, err := privateSnapshotDir(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close(); _ = dir.Close() }()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	prefix := "snapshot-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:]) + "-"
	var created []string
	defer func() {
		if err != nil {
			for _, name := range created {
				_ = root.Remove(name)
			}
		}
	}()
	for _, name := range []string{"messages.db", "notes.db", "whatsapp.db"} {
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
		file, createErr := root.OpenFile(fileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			_ = db.Close()
			return nil, createErr
		}
		created = append(created, fileName)
		// On the shipped Linux image, SQLite resolves the target through the
		// pinned directory FD even if its pathname is concurrently replaced.
		target := filepath.Join(root.Name(), fileName)
		if runtime.GOOS == "linux" {
			target = fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), fileName)
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
		_, copyErr := io.Copy(hash, file)
		_ = file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		files = append(files, snapshotFile{Name: fileName, Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	return files, nil
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
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, 500, "Snapshot deadline unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		if session {
			release, admitted := b.beginArchiveSessionRead(cancel)
			if !admitted {
				writeError(w, 503, "Session snapshot unavailable during operator logout")
				return
			}
			defer release()
		}
		files, err := snapshotStores(ctx, b.StoreRoot, b.SnapshotDir, session)
		if err != nil {
			b.Log.Warnf("Operator snapshot failed")
			writeError(w, 500, "Snapshot failed; no complete snapshot was retained")
			return
		}
		b.Log.Infof("Operator snapshot: files=%d session=%t duration_ms=%d", len(files), session, time.Since(start).Milliseconds())
		writeJSON(w, 200, map[string]any{"files": files})
	}
}

func snapshotCLI(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	flags.SetOutput(out)
	directory := flags.String("out", "", "Private snapshot directory")
	session := flags.Bool("session", false, "Include the credential-bearing session store")
	if flags.Parse(args) != nil || flags.NArg() != 0 || strings.TrimSpace(*directory) == "" {
		return 1
	}
	source, err := openStoreRoot()
	if err != nil {
		_, _ = fmt.Fprintln(out, "Snapshot source unavailable")
		return 1
	}
	defer func() { _ = source.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	files, err := snapshotStores(ctx, source, *directory, *session)
	if err != nil {
		_, _ = fmt.Fprintln(out, "Snapshot failed")
		return 1
	}
	returnCode := 0
	bridgeLog.Infof("Operator snapshot CLI: files=%d session=%t", len(files), *session)
	if err := json.NewEncoder(out).Encode(map[string]any{"files": files}); err != nil {
		returnCode = 1
	}
	return returnCode
}
