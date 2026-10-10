package main

import (
	"context"
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
