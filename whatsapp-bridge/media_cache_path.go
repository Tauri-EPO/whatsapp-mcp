package main

// One rule for where a cached media file is (issue #490).
//
// A cached file is store/<chat directory>/<file name>: two plain path
// components (checkMediaPathComponents), a directory that is a directory and a
// file that is a regular file, reached through the store root with nothing
// followed on the way, not even a symlink that stays inside the store. The
// download's cache lookup, the purge and the webhook's read all come through
// the helpers below, so they cannot disagree about what "cached" means. Before
// this file the download looked with Lstat and the purge with Stat, which
// follows a link that stays inside the store, and the purge checked the
// directory name with a narrower condition of its own.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"time"
)

var (
	errChatDirNotReal  = errors.New("chat directory is not a real directory inside the store")
	errMediaNotRegular = errors.New("cached media is not a regular file")
)

// requireChatMediaDir returns the chat directory as Lstat sees it, or an error
// when it is anything but a directory: a symlink, also one to another
// directory of the store, is refused. A directory that does not exist yet is
// reported as such (fs.ErrNotExist), so a caller can tell "nothing cached" from
// "something is in the way".
func requireChatMediaDir(root *os.Root, chatDir string) (os.FileInfo, error) {
	info, err := root.Lstat(chatDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: %v", errChatDirNotReal, err)
	case !info.IsDir():
		return nil, errChatDirNotReal
	}
	return info, nil
}

// openChatMediaDir opens the chat directory itself. What it returns is a handle
// on the directory requireChatMediaDir saw: the two are compared, so a
// directory swapped for a link between the check and the open is refused, and
// every later step names a file inside that handle instead of resolving
// store/<chat directory>/<file name> again by name.
func openChatMediaDir(root *os.Root, chatDir string) (*os.Root, error) {
	if root == nil {
		return nil, errors.New("store directory unavailable")
	}
	seen, err := requireChatMediaDir(root, chatDir)
	if err != nil {
		return nil, err
	}
	dir, err := root.OpenRoot(chatDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errChatDirNotReal, err)
	}
	if pinned, err := dir.Stat("."); err != nil || !os.SameFile(seen, pinned) {
		_ = dir.Close()
		return nil, errors.New("chat directory changed while it was being opened")
	}
	return dir, nil
}

// statCachedMedia is the regular file name in an opened chat directory, without
// following a link: a name that is a symlink, wherever it points, is not a
// cached file.
func statCachedMedia(dir *os.Root, name string) (os.FileInfo, error) {
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errMediaNotRegular
	}
	return info, nil
}

// findCachedMedia returns the first of names that is cached in chatDir, with
// its info. Nothing cached is ("", nil, nil). When no name is cached and
// something other than absence stood in the way (a name or the directory is a
// link, a component is not a plain one), that comes back as the error, so the
// caller can say "the store refused this path" instead of "not cached".
func findCachedMedia(root *os.Root, chatDir string, names []string) (string, os.FileInfo, error) {
	for _, name := range names {
		if err := checkMediaPathComponents(chatDir, name); err != nil {
			return "", nil, err
		}
	}
	dir, err := openChatMediaDir(root, chatDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = dir.Close() }()
	var refused error
	for _, name := range names {
		info, err := statCachedMedia(dir, name)
		switch {
		case err == nil:
			return name, info, nil
		case !errors.Is(err, fs.ErrNotExist):
			refused = err
		}
	}
	return "", nil, refused
}

// cachedMediaPath returns the store-relative path of the existing cached file
// for a row (current name first, then the legacy one) or "" when nothing is
// cached. A name someone replaced with a symlink is never served as the media:
// the download fetches the file again and its rename replaces the link.
func cachedMediaPath(root *os.Root, chatDir, mediaType string, timestamp time.Time, messageID, originalName string) string {
	name, _, _ := findCachedMedia(root, chatDir, mediaFileNames(mediaType, timestamp, messageID, originalName))
	if name == "" {
		return ""
	}
	return path.Join(chatDir, name)
}
