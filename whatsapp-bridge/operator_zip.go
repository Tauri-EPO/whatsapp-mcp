package main

// A replayable ZIP64 STORE stream. archive/zip.Writer retains one central
// directory header per file. Instead, replay a stable store snapshot to emit
// the central directory, keeping memory independent of rows AND media count.
// All three passes must have the same aggregate digest; a cache change aborts
// the response before a valid end record can be emitted.
import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

type archiveEntry struct {
	name  string
	write func(io.Writer) (int64, error)
}
type archiveWalk func(func(archiveEntry) error) error
type zipEntryStats struct {
	name         string
	size, offset uint64
	crc          uint32
	sha          string
	count        int64
}
type countingWriter struct {
	io.Writer
	count uint64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n < 0 {
		return 0, errors.New("invalid writer byte count")
	}
	w.count += uint64(n)
	return n, err
}

func zipNumbers(w io.Writer, values ...any) error {
	for _, value := range values {
		if err := binary.Write(w, binary.LittleEndian, value); err != nil {
			return err
		}
	}
	return nil
}

func measureEntry(entry archiveEntry, out io.Writer, offset uint64) (zipEntryStats, error) {
	hash, crc := sha256.New(), crc32.NewIEEE()
	counter := &countingWriter{Writer: io.MultiWriter(out, hash, crc)}
	count, err := entry.write(counter)
	return zipEntryStats{entry.name, counter.count, offset, crc.Sum32(), hex.EncodeToString(hash.Sum(nil)), count}, err
}

func zipLocal(w io.Writer, name string) error {
	nameLength := len(name)
	if nameLength < 0 || nameLength > 65535 {
		return errors.New("archive name too long")
	}
	if err := zipNumbers(w, uint32(0x04034b50), uint16(45), uint16(0x808), uint16(0), uint16(0), uint16(33), uint32(0), uint32(0xffffffff), uint32(0xffffffff), uint16(nameLength), uint16(20)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, name); err != nil {
		return err
	}
	return zipNumbers(w, uint16(1), uint16(16), uint64(0), uint64(0))
}
func zipDescriptor(w io.Writer, stats zipEntryStats) error {
	return zipNumbers(w, uint32(0x08074b50), stats.crc, stats.size, stats.size)
}
func zipCentral(w io.Writer, stats zipEntryStats) error {
	nameLength := len(stats.name)
	if nameLength < 0 || nameLength > 65535 {
		return errors.New("archive name too long")
	}
	if err := zipNumbers(w, uint32(0x02014b50), uint16(3<<8|45), uint16(45), uint16(0x808), uint16(0), uint16(0), uint16(33), stats.crc, uint32(0xffffffff), uint32(0xffffffff), uint16(nameLength), uint16(28), uint16(0), uint16(0), uint16(0), uint32(0o100600<<16), uint32(0xffffffff)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, stats.name); err != nil {
		return err
	}
	return zipNumbers(w, uint16(1), uint16(24), stats.size, stats.size, stats.offset)
}

func zipEnd(w *countingWriter, entries, offset, size uint64) error {
	endOffset := w.count
	if err := zipNumbers(w, uint32(0x06064b50), uint64(44), uint16(45), uint16(45), uint32(0), uint32(0), entries, entries, size, offset, uint32(0x07064b50), uint32(0), endOffset, uint32(1)); err != nil {
		return err
	}
	return zipNumbers(w, uint32(0x06054b50), uint16(0), uint16(0), uint16(0xffff), uint16(0xffff), uint32(0xffffffff), uint32(0xffffffff), uint16(0))
}

func entryDigest(w io.Writer, stats zipEntryStats) error {
	_, err := fmt.Fprintf(w, "%s\x00%d\x00%s\x00%d\n", stats.name, stats.size, stats.sha, stats.count)
	return err
}

func streamArchive(out io.Writer, walk archiveWalk, manifest func(io.Writer, archiveWalk) (int64, error)) (int64, error) {
	w := &countingWriter{Writer: out}
	first := sha256.New()
	var entries uint64
	if err := walk(func(entry archiveEntry) error {
		offset := w.count
		if err := zipLocal(w, entry.name); err != nil {
			return err
		}
		stats, err := measureEntry(entry, w, offset)
		if err != nil {
			return err
		}
		if err := entryDigest(first, stats); err != nil {
			return err
		}
		entries++
		return zipDescriptor(w, stats)
	}); err != nil {
		return 0, err
	}
	manifestDigest := sha256.New()
	manifestWalk := func(emit func(archiveEntry) error) error {
		return walk(func(entry archiveEntry) error {
			original := entry
			entry.write = func(out io.Writer) (int64, error) {
				stats, err := measureEntry(original, out, 0)
				if err != nil {
					return 0, err
				}
				if err := entryDigest(manifestDigest, stats); err != nil {
					return 0, err
				}
				return stats.count, nil
			}
			return emit(entry)
		})
	}
	manifestEntry := archiveEntry{"manifest.json", func(out io.Writer) (int64, error) { return manifest(out, manifestWalk) }}
	manifestOffset := w.count
	if err := zipLocal(w, manifestEntry.name); err != nil {
		return 0, err
	}
	manifestStats, err := measureEntry(manifestEntry, w, manifestOffset)
	if err != nil {
		return 0, err
	}
	if hex.EncodeToString(first.Sum(nil)) != hex.EncodeToString(manifestDigest.Sum(nil)) {
		return 0, errors.New("archive media changed while generating manifest")
	}
	if err := zipDescriptor(w, manifestStats); err != nil {
		return 0, err
	}
	centralOffset := w.count
	second := sha256.New()
	var offset uint64
	if err := walk(func(entry archiveEntry) error {
		stats, err := measureEntry(entry, io.Discard, offset)
		if err != nil {
			return err
		}
		if err := entryDigest(second, stats); err != nil {
			return err
		}
		if err := zipCentral(w, stats); err != nil {
			return err
		}
		offset += uint64(30+len(stats.name)+20+24) + stats.size
		return nil
	}); err != nil {
		return 0, err
	}
	if offset != manifestOffset || hex.EncodeToString(first.Sum(nil)) != hex.EncodeToString(second.Sum(nil)) {
		return 0, errors.New("archive media changed while streaming")
	}
	if err := zipCentral(w, manifestStats); err != nil {
		return 0, err
	}
	if err := zipEnd(w, entries+1, centralOffset, w.count-centralOffset); err != nil {
		return 0, err
	}
	return int64(entries), nil
}
