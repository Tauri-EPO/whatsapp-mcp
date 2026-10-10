package main

import (
	"context"
	"io"
	"os"
	"path"
	"testing"
	"time"
)

func TestLocalMediaStorageLifecycle(t *testing.T) {
	storeInScratch(t)
	b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
	storage := b.mediaStorage()
	row := mediaRow{ID: "CACHE1", ChatJID: mediaTestChat, MediaType: "image", Timestamp: time.Now()}
	chat := chatMediaRel(row.ChatJID)
	if err := b.StoreRoot.MkdirAll(chat, storeDirMode); err != nil {
		t.Fatal(err)
	}
	data := "cache lifecycle bytes"
	written, err := storage.Write(context.Background(), row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error {
			_, err := io.WriteString(f, data)
			return err
		})
	})
	if err != nil || written != int64(len(data)) {
		t.Fatalf("write: %d %v", written, err)
	}
	entry, err := storage.Lookup(context.Background(), row)
	if err != nil || entry == nil || entry.Path != path.Join(chat, mediaFileName(row.MediaType, row.Timestamp, row.ID, "")) {
		t.Fatalf("lookup: %+v %v", entry, err)
	}
	f, size, err := storage.Open(context.Background(), row, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(actual) != data || size != written {
		t.Fatalf("read: %q size=%d err=%v", actual, size, err)
	}
	dry, err := storage.Delete(context.Background(), []mediaRow{row}, true)
	if err != nil || len(dry) != 1 || !dry[0].Purged || dry[0].Bytes != written {
		t.Fatalf("dry: %+v %v", dry, err)
	}
	usage, err := storage.Usage(context.Background())
	if err != nil || usage.Files != 1 || usage.Bytes != written {
		t.Fatalf("dry removed bytes: %+v %v", usage, err)
	}
	real, err := storage.Delete(context.Background(), []mediaRow{row}, false)
	if err != nil || len(real) != 1 || !real[0].Purged || real[0].Bytes != dry[0].Bytes {
		t.Fatalf("delete: %+v %v", real, err)
	}
	if found, err := storage.Lookup(context.Background(), row); err != nil || found != nil {
		t.Fatalf("still cached: %+v %v", found, err)
	}
}

func TestMediaCatalogSchemaIsIdempotentAndConstrained(t *testing.T) {
	storeInScratch(t)
	store := newTestMessageStore(t)
	if err := ensureMessageStoreSchema(store.db); err != nil {
		t.Fatal(err)
	}
	now := dbTime(time.Now())
	hash := sha256Of([]byte("catalog test"))
	if _, err := store.db.Exec(`INSERT INTO media_cache VALUES(?, 12, 'image', 's3', ?, ?)`, hash, now, now); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO media_cache VALUES(X'01', 12, 'image', 's3', 'x', 'x')`,
		`UPDATE media_cache SET bytes = -1`,
		`UPDATE media_cache SET backend = 'unknown'`,
		`INSERT INTO media_cache_refs VALUES('CACHE1', '5511999999999@s.whatsapp.net', X'01')`,
	} {
		if _, err := store.db.Exec(statement); err == nil {
			t.Fatalf("accepted invalid catalog value: %s", statement)
		}
	}
	if err := ensureMessageStoreSchema(store.db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM media_cache`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration lost object: count=%d err=%v", count, err)
	}
}
