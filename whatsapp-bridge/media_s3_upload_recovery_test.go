package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

func waitS3Recovery(t *testing.T, b *Bridge, s *s3MediaStorage, remaining int64) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); b.runMediaRetention() }()
	defer func() { b.cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		usage, err := s.Usage(b.ctx)
		if err == nil && usage.Bytes == remaining {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("startup recovery with retention disabled did not reclaim remote bytes")
}

func TestMinIORecoveryRunsWithoutRetention(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-retention-disabled-recovery")
	data := []byte("replaced cached document")
	row := s3TestRow(t, b, "REPLACEDCACHE", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	newData := []byte("new snapshot")
	updated := s3TestRow(t, b, row.ID, row.ChatJID, row.MediaType, newData, time.Now())
	s3TestWrite(t, b, updated, newData)
	usage, err := s.Usage(b.ctx)
	if err != nil || usage.Bytes != int64(len(data)+len(newData)) {
		t.Fatal("detached object was not charged before recovery", usage, err)
	}
	waitS3Recovery(t, b, s, int64(len(newData)))
	key, _ := s.key(sha256Of(data))
	if _, err := s.client.StatObject(context.Background(), s.cfg.Bucket, key, minio.StatObjectOptions{}); !missingS3Object(err) {
		t.Fatal("detached remote object survived recovery", err)
	}
}

func TestMinIOFailedPublicationPersistsUploadRecovery(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-upload-intent-recovery")
	data := []byte("uploaded bytes awaiting publication")
	row := s3TestRow(t, b, "UPLOADINTENT", mediaTestChat, "document", data, time.Now())
	key, _ := s.key(sha256Of(data))
	target, _ := url.Parse(s.cfg.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy.Transport = transport
	var fail, intentBeforePut atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, key) {
			if r.Method == http.MethodPut {
				var count int
				err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM media_cache_uploads WHERE sha256=? AND bytes=?`, sha256Of(data), len(data)).Scan(&count)
				intentBeforePut.Store(err == nil && count == 1)
			}
			if fail.Load() && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := s.cfg
	cfg.Endpoint = server.URL
	pending, err := newS3MediaStorage(b.ctx, cfg, b.Store, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pending.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err == nil || !intentBeforePut.Load() {
		t.Fatal("failed publication did not persist intent before PUT", err, intentBeforePut.Load())
	}
	usage, err := pending.Usage(b.ctx)
	if err != nil || usage.Bytes != int64(len(data)) {
		t.Fatal("unpublished remote bytes were not charged", usage, err)
	}
	if _, err := s.client.StatObject(b.ctx, s.cfg.Bucket, key, minio.StatObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	b.Store = reopened
	fail.Store(false)
	recovered, err := newS3MediaStorage(b.ctx, cfg, reopened, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	b.MediaStorage = recovered
	waitS3Recovery(t, b, recovered, 0)
	if _, err := s.client.StatObject(context.Background(), s.cfg.Bucket, key, minio.StatObjectOptions{}); !missingS3Object(err) {
		t.Fatal("unpublished remote object survived restart recovery", err)
	}
}

func TestMinIORecoverySchemaUpgradesExistingCatalog(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-recovery-schema-upgrade")
	data := []byte("existing cached document")
	row := s3TestRow(t, b, "LEGACYCACHE", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	for _, statement := range []string{
		`DROP TABLE media_cache_uploads`,
		`DROP TABLE media_cache_deletions`,
		`DROP INDEX idx_messages_media_cache_cursor`,
		`DELETE FROM schema_migrations WHERE name='media_cache_recovery_v2'`,
	} {
		if _, err := b.Store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	b.Store = reopened
	if err := ensureMessageStoreSchema(reopened.db); err != nil {
		t.Fatal(err)
	}
	upgraded, err := newS3MediaStorage(b.ctx, s.cfg, reopened, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	b.MediaStorage = upgraded
	updated := []byte("published after archive upgrade")
	newRow := s3TestRow(t, b, "AFTERUPGRADE", mediaTestChat, "document", updated, time.Now())
	s3TestWrite(t, b, newRow, updated)
	for _, expected := range []struct {
		row  mediaRow
		data []byte
	}{{row, data}, {newRow, updated}} {
		reader, _, err := upgraded.Open(b.ctx, expected.row, "")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || !bytes.Equal(actual, expected.data) {
			t.Fatal("upgrade changed cached bytes", err)
		}
	}
	var objects, refs, markers, indexes int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM media_cache`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM media_cache_refs`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='media_cache_recovery_v2'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_media_cache_cursor'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if objects != 2 || refs != 2 || markers != 1 || indexes != 1 {
		t.Fatal("upgrade schema/counts", objects, refs, markers, indexes)
	}
	results, err := upgraded.Delete(b.ctx, []mediaRow{row}, false)
	if err != nil || len(results) != 1 || !results[0].Purged {
		t.Fatal("upgraded deletion journal unavailable", results, err)
	}
}
