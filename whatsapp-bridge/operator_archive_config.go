package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type archiveConfig struct {
	Keep    int
	Session bool
	Timeout time.Duration
}

func parseArchiveConfig(getenv func(string) string) (archiveConfig, error) {
	cfg := archiveConfig{Keep: 7, Timeout: 30 * time.Minute}
	if value := getenv("WHATSAPP_SNAPSHOT_KEEP"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 10000 {
			return cfg, errors.New("WHATSAPP_SNAPSHOT_KEEP must be between 1 and 10000")
		}
		cfg.Keep = n
	}
	if value := getenv("WHATSAPP_OPERATOR_EXPORT_TIMEOUT_MIN"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 1440 {
			return cfg, errors.New("WHATSAPP_OPERATOR_EXPORT_TIMEOUT_MIN must be between 1 and 1440")
		}
		cfg.Timeout = time.Duration(n) * time.Minute
	}
	var err error
	cfg.Session, err = parseBoolEnv("WHATSAPP_SNAPSHOT_SESSION", getenv("WHATSAPP_SNAPSHOT_SESSION"), false)
	return cfg, err
}

func (b *Bridge) archiveTimeout() time.Duration {
	if b.Archive.Timeout > 0 {
		return b.Archive.Timeout
	}
	return 30 * time.Minute
}

// Resolve existing ancestors without creating directories. A media root may
// itself be a symlink; confinement must compare its resolved destination.
func snapshotResolvedPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	current := abs
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			rel, err := filepath.Rel(current, abs)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, rel), nil
		}
		if !errors.Is(err, os.ErrNotExist) || current == filepath.Dir(current) {
			return "", err
		}
		current = filepath.Dir(current)
	}
}

func validateSnapshotLocation(directory, store, media string) error {
	if directory == "" {
		return nil
	}
	target, err := snapshotResolvedPath(directory)
	if err != nil {
		return errors.New("WHATSAPP_SNAPSHOT_DIR cannot be resolved")
	}
	roots, err := resolveMediaRootsValue(media, true)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	roots = append(roots, store, filepath.Join(home, defaultOutboxSubpath))
	for _, root := range roots {
		resolved, err := snapshotResolvedPath(root)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(resolved, target)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("WHATSAPP_SNAPSHOT_DIR must be outside store, outbox and media roots")
		}
	}
	return nil
}
