//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func snapshotFreeBytes(dir *os.File) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil //nolint:gosec // Filesystem block sizes and available blocks are nonnegative.
}

func syncSnapshotDirectory(dir *os.File) error { return dir.Sync() }
