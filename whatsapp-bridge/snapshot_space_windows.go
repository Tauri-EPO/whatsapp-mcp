package main

import (
	"golang.org/x/sys/windows"
	"os"
)

func snapshotFreeBytes(dir *os.File) (uint64, error) {
	name, err := windows.UTF16PtrFromString(dir.Name())
	if err != nil {
		return 0, err
	}
	var available uint64
	err = windows.GetDiskFreeSpaceEx(name, &available, nil, nil)
	return available, err
}

// Windows cannot fsync a directory handle opened by os.Open. NTFS publishes
// the rename atomically; the file itself is flushed before publication.
func syncSnapshotDirectory(_ *os.File) error { return nil }
