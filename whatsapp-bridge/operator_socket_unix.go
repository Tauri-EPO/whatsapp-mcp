//go:build !windows

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("WHATSAPP_OPERATOR_SOCKET cannot bind")
	}
	if err := os.Chmod(path, 0660); err != nil { //nolint:gosec // Unix socket group access is the explicit local proxy boundary.
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
