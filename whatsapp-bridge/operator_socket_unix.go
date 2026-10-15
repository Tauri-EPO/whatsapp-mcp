//go:build !windows

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func validateOperatorSocket(path, store, media string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || len(path) > 100 {
		return errors.New("WHATSAPP_OPERATOR_SOCKET requires an absolute path of at most 100 bytes")
	}
	if err := validateSnapshotLocation(path, store, media); err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), "WHATSAPP_SNAPSHOT_DIR", "WHATSAPP_OPERATOR_SOCKET"))
	}
	return validateOperatorSocketPath(path)
}

func validateOperatorSocketPath(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("WHATSAPP_OPERATOR_SOCKET cannot inspect path")
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("WHATSAPP_OPERATOR_SOCKET refuses symlinks")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0002 != 0 {
		return errors.New("WHATSAPP_OPERATOR_SOCKET parent must exist and not be world-writable")
	}
	return nil
}

func listenOperatorSocket(path string) (net.Listener, error) {
	return listenOperatorSocketOwned(path, os.Geteuid())
}

func listenOperatorSocketOwned(path string, uid int) (net.Listener, error) {
	// Recheck immediately before binding; do not delete a live socket or a
	// foreign inode. A group-shared directory is an explicit local trust boundary.
	if err := validateOperatorSocketPath(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(owner.Uid) != uid || info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("WHATSAPP_OPERATOR_SOCKET refuses an existing non-owned socket or file")
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("WHATSAPP_OPERATOR_SOCKET is already listening")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, errors.New("WHATSAPP_OPERATOR_SOCKET cannot prove stale socket")
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) {
			return nil, errors.New("WHATSAPP_OPERATOR_SOCKET changed before removing stale socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("WHATSAPP_OPERATOR_SOCKET cannot remove stale socket")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("WHATSAPP_OPERATOR_SOCKET cannot inspect socket")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("WHATSAPP_OPERATOR_SOCKET cannot bind")
	}
	listener.SetUnlinkOnClose(false)
	owned, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	guarded := &ownedOperatorSocket{UnixListener: listener, path: path, owned: owned}
	if err := os.Chmod(path, 0660); err != nil { //nolint:gosec // Unix socket group access is the explicit local proxy boundary.
		_ = guarded.Close()
		return nil, err
	}
	return guarded, nil
}

type ownedOperatorSocket struct {
	*net.UnixListener
	path  string
	owned os.FileInfo
	once  sync.Once
	err   error
}

func (l *ownedOperatorSocket) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.owned, current) {
			l.err = errors.Join(l.err, os.Remove(l.path))
		}
	})
	return l.err
}
