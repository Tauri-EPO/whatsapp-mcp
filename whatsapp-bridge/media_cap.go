package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"

	"go.mau.fi/whatsmeow"
)

var errAutoMediaLimit = errors.New("automatic media exceeds WHATSAPP_MEDIA_MAX_BYTES")

func (b *Bridge) recordAutoSizeSkip(messageID, chatJID string) {
	b.metrics.mediaAutoSizeSkips.Add(1)
	b.Log.Infof("Skipping automatic media cache for message %s in %s: exceeds WHATSAPP_MEDIA_MAX_BYTES; download_media still fetches it", messageID, chatJID)
}

type mediaLimitKey struct{}

func withMediaLimit(ctx context.Context, limit uint64) context.Context {
	return context.WithValue(ctx, mediaLimitKey{}, limit)
}

func mediaLimit(ctx context.Context) uint64 {
	limit, _ := ctx.Value(mediaLimitKey{}).(uint64)
	return limit
}

// A capped caller may join a manual transfer or find its existing cache. Check
// that result without cancelling the manual caller or deleting its file.
func checkCachedMediaLimit(ctx context.Context, root *os.Root, relPath string) error {
	limit := mediaLimit(ctx)
	if limit == 0 {
		return nil
	}
	found, err := findCachedMedia(root, path.Dir(relPath), []string{path.Base(relPath)})
	if err != nil {
		return err
	}
	if found == nil {
		return fs.ErrNotExist
	}
	defer found.Close()
	size := found.info.Size()
	if size < 0 {
		return errors.New("invalid cached media length")
	}
	if uint64(size) > limit {
		return errAutoMediaLimit
	}
	return nil
}

// The encrypted download needs one AES padding block and ten MAC bytes beyond
// the plaintext budget. The final decrypted file is checked before publication.
func writeMediaDownload(ctx context.Context, root *os.Root, relPath string, fill func(context.Context, whatsmeow.File) error) (int64, error) {
	return writeMediaFile(root, relPath, func(file *os.File) error {
		limit := mediaLimit(ctx)
		if limit == 0 {
			return fill(ctx, file)
		}
		plain, wire := int64(math.MaxInt64), int64(math.MaxInt64)
		if limit <= uint64(math.MaxInt64) {
			plain = int64(limit)
		}
		if limit <= uint64(math.MaxInt64-26) {
			wire = int64((limit/16+1)*16 + 10)
		}
		boundedCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		bounded := &cappedMediaFile{file: file, limit: wire, cancel: cancel}
		err := fill(boundedCtx, bounded)
		if bounded.exceeded {
			return errAutoMediaLimit
		}
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Size() > plain {
			return errAutoMediaLimit
		}
		return nil
	})
}

// A named field avoids promoting writable os.File methods that could bypass
// the cap (in particular io.ReaderFrom).
type cappedMediaFile struct {
	file     *os.File
	limit    int64
	cancel   context.CancelFunc
	exceeded bool
}

func (f *cappedMediaFile) Read(p []byte) (int, error)                { return f.file.Read(p) }
func (f *cappedMediaFile) ReadAt(p []byte, off int64) (int, error)   { return f.file.ReadAt(p, off) }
func (f *cappedMediaFile) Seek(off int64, whence int) (int64, error) { return f.file.Seek(off, whence) }
func (f *cappedMediaFile) Stat() (os.FileInfo, error)                { return f.file.Stat() }

func (f *cappedMediaFile) check(off int64, count int64) error {
	if off >= 0 && (off > f.limit || count > f.limit-off) {
		f.exceeded = true
		f.cancel()
		return errAutoMediaLimit
	}
	return nil
}

func (f *cappedMediaFile) Write(p []byte) (int, error) {
	off, err := f.file.Seek(0, io.SeekCurrent)
	if err == nil {
		err = f.check(off, int64(len(p)))
	}
	if err != nil {
		return 0, err
	}
	return f.file.Write(p)
}

func (f *cappedMediaFile) WriteAt(p []byte, off int64) (int, error) {
	if err := f.check(off, int64(len(p))); err != nil {
		return 0, err
	}
	return f.file.WriteAt(p, off)
}

func (f *cappedMediaFile) Truncate(size int64) error {
	if err := f.check(0, size); err != nil {
		return err
	}
	return f.file.Truncate(size)
}

func (f *cappedMediaFile) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{f}, r)
}
