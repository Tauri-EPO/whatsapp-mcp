package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestAutomaticCacheSizeUsesCanonicalPathRule(t *testing.T) {
	for _, name := range []string{"regular oversized", "file symlink", "chat symlink"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && name != "regular oversized" {
				t.Skip("symlinks require Windows privileges")
			}
			dir := t.TempDir()
			root := storeRootAt(t, dir)
			chat := chatMediaRel(mediaTestChat)
			if err := os.MkdirAll(filepath.Join(dir, "other"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "other", "cached.bin"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if name == "chat symlink" {
				if err := os.Symlink("other", filepath.Join(dir, chat)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(dir, chat), 0o700); err != nil {
					t.Fatal(err)
				}
				if name == "file symlink" {
					if err := os.Symlink(filepath.Join("..", "other", "cached.bin"), filepath.Join(dir, chat, "cached.bin")); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(dir, chat, "cached.bin"), []byte("oversized"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := errAutoMediaLimit
			if name == "file symlink" {
				want = errMediaNotRegular
			}
			if name == "chat symlink" {
				want = errChatDirNotReal
			}
			if err := checkCachedMediaLimit(withMediaLimit(t.Context(), 2), root, chat+"/cached.bin"); !errors.Is(err, want) {
				t.Fatalf("cache size check=%v want=%v", err, want)
			}
		})
	}
}

func TestAutomaticMediaWireAndPlaintextCaps(t *testing.T) {
	for _, operation := range []string{"copy", "write-at", "truncate", "plaintext", "valid", "empty", "unlimited"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			root := storeRootAt(t, dir)
			limit := uint64(16)
			if operation == "unlimited" {
				limit = 0
			}
			var cancelled bool
			written, err := writeMediaDownload(withMediaLimit(t.Context(), limit), root, "document.bin", func(ctx context.Context, file whatsmeow.File) error {
				var err error
				switch operation {
				case "copy", "unlimited":
					// Hide bytes.Reader.WriteTo so io.Copy exercises ReaderFrom.
					_, err = io.Copy(file, struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("x"), 1024))})
				case "write-at":
					_, err = file.WriteAt([]byte("x"), 1024)
				case "truncate":
					err = file.Truncate(1024)
				case "plaintext":
					// Fits the encrypted wire allowance, exceeds plaintext limit.
					_, err = file.Write(bytes.Repeat([]byte("x"), 17))
				case "valid":
					_, err = file.Write(bytes.Repeat([]byte("x"), 42)) // AES padding + MAC
					if err == nil {
						err = file.Truncate(16)
					} // decrypt in place
				case "empty":
					_, err = file.Write(bytes.Repeat([]byte("x"), 26))
					if err == nil {
						err = file.Truncate(0)
					}
				}
				cancelled = ctx.Err() != nil
				return err
			})
			wantRefusal := operation == "copy" || operation == "write-at" || operation == "truncate" || operation == "plaintext"
			if errors.Is(err, errAutoMediaLimit) != wantRefusal {
				t.Fatalf("written=%d err=%v", written, err)
			}
			if wantRefusal {
				if _, err := os.Stat(filepath.Join(dir, "document.bin")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published oversized file: %v", err)
				}
				if operation != "plaintext" && !cancelled {
					t.Fatal("wire cap did not cancel further host retries")
				}
			} else {
				info, err := os.Stat(filepath.Join(dir, "document.bin"))
				if err != nil || info.Size() != written {
					t.Fatalf("published size=%v written=%d err=%v", info, written, err)
				}
				if operation == "empty" && written != 0 {
					t.Fatal("empty file was rejected")
				}
				if operation == "valid" && written != 16 {
					t.Fatal("padding allowance was rejected")
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "document.bin.part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temp file survived: %v", err)
			}
		})
	}
}
