package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func holdS3Publications(t *testing.T, b *Bridge, s *s3MediaStorage, rows []mediaRow, data [][]byte, writeContexts ...context.Context) {
	t.Helper()
	started, release := make(chan struct{}, len(rows)), make(chan struct{})
	var once sync.Once
	s3SecurityProxy(t, s, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false
	}, nil)
	done := make(chan error, len(rows))
	writeCtx := b.ctx
	if len(writeContexts) != 0 {
		writeCtx = writeContexts[0]
	}
	for i, row := range rows {
		go func() {
			_, err := s.Write(writeCtx, row, func(rel string) (int64, error) {
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data[i]); return err })
			})
			done <- err
		}()
	}
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		for range rows {
			if err := <-done; err != nil {
				t.Error("released publication failed", err)
			}
		}
	})
	for range rows {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("publication did not reach held PUT")
		}
	}
}

func TestMinIOTransientReadBudgetStopsBeforeDiskLimit(t *testing.T) {
	for _, declared := range []int{0, 1} {
		t.Run(strconv.Itoa(declared), func(t *testing.T) {
			b, _ := minioTestBridge(t, "instances/test-transient-budget-"+strconv.Itoa(declared))
			data := bytes.Repeat([]byte("x"), 1024)
			row := s3TestRow(t, b, "BOUNDEDTRANSIENT", mediaTestChat, "document", data, time.Now())
			if _, err := b.Store.db.Exec(`UPDATE messages SET file_length=? WHERE id=? AND chat_jid=?`, declared, row.ID, row.ChatJID); err != nil {
				t.Fatal(err)
			}
			b.MediaQuotaBytes = 1
			var peak atomic.Int64
			b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
				return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, f whatsmeow.File) error {
					for start := 0; start < len(data); start += 16 {
						if _, err := f.Write(data[start : start+16]); err != nil {
							return err
						}
						info, err := f.Stat()
						if err != nil {
							return err
						}
						peak.Store(info.Size())
					}
					return nil
				})
			}
			server := s3ReviewREST(t, b)
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+row.ChatJID+"&message_id="+row.ID+"&max_bytes=64", nil)
			req.Header.Set("Authorization", "Bearer test-bridge-token")
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusRequestEntityTooLarge || peak.Load() > 90 {
				t.Fatal("transient download exceeded preallocated plaintext/wire budget", response.StatusCode, peak.Load(), string(body))
			}
		})
	}
}

func TestMinIOQuotaMeasuredUploadDropsUnusedReservation(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-measured")
	oldData, data := bytes.Repeat([]byte("a"), 20), bytes.Repeat([]byte("b"), 30)
	old := s3TestRow(t, b, "MEASUREDOLD", mediaTestChat, "document", oldData, time.Now())
	s3TestWrite(t, b, old, oldData)
	b.MediaQuotaBytes, b.MediaEvictTypes = 100, []string{"document"}
	row := s3TestRow(t, b, "MEASUREDPUT", mediaTestChat, "document", data, time.Now())
	if _, err := b.Store.db.Exec(`UPDATE messages SET file_sha256=NULL,file_length=NULL WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	ctx, release, err := b.acquireS3WriteBudget(b.ctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	holdS3Publications(t, b, s, []mediaRow{row}, [][]byte{data}, ctx)
	_, finish, err := b.acquireS3MediaQuota(b.ctx, 25)
	if err != nil {
		t.Fatal("measured 30 byte PUT should release unused reservation", err)
	}
	defer finish()
	if entry, err := s.Lookup(b.ctx, old); err != nil || entry == nil {
		t.Fatal("measured upload caused unnecessary eviction", err)
	}
}

func TestMinIOQuotaCountsHeldUploadOnce(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-quota")
	oldData, uploading := bytes.Repeat([]byte("a"), 20), bytes.Repeat([]byte("b"), 30)
	old := s3TestRow(t, b, "CAPACITYOLD", mediaTestChat, "document", oldData, time.Now())
	s3TestWrite(t, b, old, oldData)
	b.MediaQuotaBytes, b.MediaEvictTypes = 100, []string{"document"}
	row := s3TestRow(t, b, "CAPACITYPUT", mediaTestChat, "document", uploading, time.Now())
	holdS3Publications(t, b, s, []mediaRow{row}, [][]byte{uploading})
	usage, err := s.Usage(b.ctx)
	if err != nil || usage.Bytes != 50 {
		t.Fatal("catalog and durable intent must charge 50 bytes", usage, err)
	}
	_, release, err := b.acquireS3MediaQuota(b.ctx, 25)
	if err != nil {
		t.Fatal("75 bytes should fit quota 100", err)
	}
	defer release()
	entry, err := s.Lookup(b.ctx, old)
	if err != nil || entry == nil {
		t.Fatal("25 byte admission evicted the 20 byte object despite total 75", entry, err)
	}
}

func TestMinIOReadProceedsWithAllWritesBusy(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-read-slots")
	data := []byte("cached readable bytes")
	row := s3TestRow(t, b, "CAPACITYREAD", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	rows, writes := []mediaRow{}, [][]byte{}
	for i, id := range []string{"BUSYWRITEA", "BUSYWRITEB", "BUSYWRITEC", "BUSYWRITED"} {
		payload := bytes.Repeat([]byte{byte(i + 1)}, 30)
		rows = append(rows, s3TestRow(t, b, id, mediaTestChat, "document", payload, time.Now()))
		writes = append(writes, payload)
	}
	holdS3Publications(t, b, s, rows, writes)
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	f, _, err := s.Open(ctx, row, "")
	if err != nil {
		t.Fatal("read blocked behind four held writes", err)
	}
	actual, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("read returned wrong bytes", err)
	}
}

func TestMinIORangesReuseOneVerifiedGet(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-range-reuse")
	data := bytes.Repeat([]byte("verified bytes"), 200000)
	row := s3TestRow(t, b, "CAPACITYRANGES", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	key, _ := s.key(sha256Of(data))
	var gets atomic.Int32
	s3SecurityProxy(t, s, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, key) {
			gets.Add(1)
		}
		return false
	}, nil)
	assembled := []byte{}
	for offset := int64(0); offset < int64(len(data)); offset += 524288 {
		query := "/api/media/blob?chat_jid=" + row.ChatJID + "&message_id=" + row.ID + "&offset=" + strconv.FormatInt(offset, 10) + "&length=524288"
		request := httptest.NewRequest(http.MethodGet, query, nil)
		response := httptest.NewRecorder()
		b.handleMediaBlob(response, request)
		if response.Code != http.StatusOK {
			t.Fatal("range failed", response.Code, response.Body.String())
		}
		assembled = append(assembled, response.Body.Bytes()...)
	}
	if !bytes.Equal(assembled, data) || gets.Load() != 1 {
		t.Fatal("range reads must reconstruct bytes with exactly one GetObject", gets.Load())
	}
}

func TestMinIOVerifiedSpoolExpiresAndCloses(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-ttl")
	if s.spools.idleTTL != 2*time.Minute {
		t.Fatal("unexpected production idle TTL")
	}
	s.spools.idleTTL = 40 * time.Millisecond
	data := []byte("verified spool expiry bytes")
	row := s3TestRow(t, b, "CAPACITYTTL", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	f, _, err := s.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	reader := f.(*s3VerifiedReader)
	rel := reader.entry.rel
	if info, err := b.StoreRoot.Lstat(rel); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("spool permissions", err)
	}
	_ = f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := b.StoreRoot.Lstat(rel); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle TTL did not remove spool")
		}
		time.Sleep(time.Millisecond)
	}
	f, _, err = s.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	reader = f.(*s3VerifiedReader)
	rel = reader.entry.rel
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StoreRoot.Lstat(rel); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shutdown left active spool", err)
	}
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal("shutdown did not close reader", err)
	}
	_ = f.Close()
}

func TestMinIOVerifiedSpoolCapacityReturns503(t *testing.T) {
	for _, limit := range []string{"bytes", "readers", "objects"} {
		t.Run(limit, func(t *testing.T) {
			b, s := minioTestBridge(t, "instances/test-capacity-"+limit)
			data := bytes.Repeat([]byte("a"), 20)
			row := s3TestRow(t, b, "CAPACITYPIN", mediaTestChat, "document", data, time.Now())
			otherData := bytes.Repeat([]byte("b"), 20)
			other := s3TestRow(t, b, "CAPACITYNEXT", mediaTestChat, "document", otherData, time.Now())
			s3TestWrite(t, b, row, data)
			s3TestWrite(t, b, other, otherData)
			if limit == "bytes" {
				s.spools.maxBytes = 20
			}
			if limit == "objects" {
				s.spools.maxEntries = 1
			}
			readers := []io.ReadCloser{}
			defer func() {
				for _, reader := range readers {
					_ = reader.Close()
				}
			}()
			count := 1
			if limit == "readers" {
				count = 4
			}
			for range count {
				reader, _, err := s.Open(b.ctx, row, "")
				if err != nil {
					t.Fatal(err)
				}
				readers = append(readers, reader)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/media/blob?chat_jid="+other.ChatJID+"&message_id="+other.ID, nil)
			b.handleMediaBlob(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatal("full capacity must return 503", response.Code, response.Body.String())
			}
			s.spools.mu.Lock()
			allocated := s.spools.bytes
			s.spools.mu.Unlock()
			if allocated != 20 {
				t.Fatal("refused request allocated another spool", allocated)
			}
		})
	}
}

type heldBlobResponse struct {
	http.ResponseWriter
	started chan struct{}
	release <-chan struct{}
}

func (w heldBlobResponse) Write(data []byte) (int, error) {
	w.started <- struct{}{}
	<-w.release
	return w.ResponseWriter.Write(data)
}

func TestMinIOConcurrentBlobResponsesReturn503(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-capacity-http-responses")
	data := []byte("verified HTTP body bytes")
	row := s3TestRow(t, b, "HELDHTTPBODY", mediaTestChat, "document", data, time.Now())
	other := s3TestRow(t, b, "FIFTHHTTPBODY", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	s3TestWrite(t, b, other, data)
	started, release := make(chan struct{}, 4), make(chan struct{})
	server := s3ReviewRESTWrapped(t, b, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/media/blob" && r.URL.Query().Get("message_id") == row.ID {
				w = heldBlobResponse{ResponseWriter: w, started: started, release: release}
			}
			handler.ServeHTTP(w, r)
		})
	})
	done := make(chan error, 4)
	t.Cleanup(func() {
		close(release)
		for range 4 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
	for range 4 {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+row.ChatJID+"&message_id="+row.ID, nil)
			req.Header.Set("Authorization", "Bearer test-bridge-token")
			response, err := server.Client().Do(req)
			if err == nil {
				actual, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				err = readErr
				if !bytes.Equal(actual, data) {
					err = errors.New("held HTTP body changed")
				}
			}
			done <- err
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("HTTP body did not reach held writer")
		}
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+other.ChatJID+"&message_id="+other.ID, nil)
	req.Header.Set("Authorization", "Bearer test-bridge-token")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("fifth real HTTP response must be 503", response.StatusCode)
	}
	s.spools.mu.Lock()
	allocated := s.spools.bytes
	s.spools.mu.Unlock()
	if allocated != int64(len(data)) {
		t.Fatal("concurrent HTTP readers duplicated verified spool bytes", allocated)
	}
}

func TestMinIOTransientHashMismatchReleasesNoBytes(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-transient-integrity")
	data := []byte("expected verified bytes")
	row := s3TestRow(t, b, "TRANSIENTINTEGRITY", mediaTestChat, "document", data, time.Now())
	b.MediaQuotaBytes = 1
	wrong := bytes.Repeat([]byte("z"), len(data))
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, f whatsmeow.File) error { _, err := f.Write(wrong); return err })
	}
	server := s3ReviewREST(t, b)
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+row.ChatJID+"&message_id="+row.ID+"&offset=0&length=4", nil)
	req.Header.Set("Authorization", "Bearer test-bridge-token")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway || bytes.Contains(body, wrong) {
		t.Fatal("unverified transient bytes reached caller", response.StatusCode, string(body))
	}
	s.spools.mu.Lock()
	allocated := s.spools.bytes
	s.spools.mu.Unlock()
	if allocated != 0 {
		t.Fatal("failed verification retained plaintext spool", allocated)
	}
}

func TestMinIOVerifiedCachedSpoolPublishesNewReference(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-spool-shared-reference")
	data := []byte("shared verified cached bytes")
	first := s3TestRow(t, b, "SPOOLFIRST", mediaTestChat, "document", data, time.Now())
	second := s3TestRow(t, b, "SPOOLSECOND", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, first, data)
	reader, _, err := s.Open(b.ctx, first, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, f whatsmeow.File) error { _, err := f.Write(data); return err })
	}
	server := s3ReviewREST(t, b)
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+second.ChatJID+"&message_id="+second.ID, nil)
	req.Header.Set("Authorization", "Bearer test-bridge-token")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	var refs int
	err = b.Store.db.QueryRow(`SELECT COUNT(*) FROM media_cache_refs WHERE id=? AND chat_jid=?`, second.ID, second.ChatJID).Scan(&refs)
	if response.StatusCode != 200 || response.Header.Get("X-Media-Cached") != "true" || err != nil || refs != 1 || !bytes.Equal(actual, data) {
		t.Fatal("verified spool bypassed publication of the new reference", response.StatusCode, refs, err)
	}
}
