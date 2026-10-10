package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func s3SecurityProxy(t *testing.T, s *s3MediaStorage, filter func(http.ResponseWriter, *http.Request) bool, modify func(*http.Response) error) {
	t.Helper()
	target, _ := url.Parse(s.cfg.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy.Transport = transport
	proxy.ModifyResponse = modify
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filter != nil && filter(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	original := s.client
	u, _ := url.Parse(server.URL)
	sdkTransport, _ := minio.DefaultTransport(false)
	sdkTransport.Proxy = nil
	client, err := minio.New(u.Host, &minio.Options{Secure: false, Creds: credentials.NewStaticV4(s.cfg.AccessKey, s.cfg.SecretKey, ""), Region: s.cfg.Region, BucketLookup: minio.BucketLookupPath, Transport: sdkTransport, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.client = client
	t.Cleanup(func() { s.client = original; server.Close() })
}

func TestMinIOSlowPutDoesNotBlockOtherReadsOrWebhook(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-transfer-isolation")
	fast := []byte("fast cached bytes")
	slow := []byte("slow uploading bytes")
	a := s3TestRow(t, b, "FASTREAD", mediaTestChat, "document", fast, time.Now())
	s3TestWrite(t, b, a, fast)
	c := s3TestRow(t, b, "SLOWPUT", mediaTestChat, "document", slow, time.Now())
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	key, _ := s.key(sha256Of(slow))
	s3SecurityProxy(t, s, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, key) {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return false
		}
		return false
	}, nil)
	finished := make(chan error, 1)
	go func() {
		_, err := s.Write(b.ctx, c, func(rel string) (int64, error) {
			return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(slow); return err })
		})
		finished <- err
	}()
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		if err := <-finished; err != nil {
			t.Error("released PUT failed", err)
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT did not reach proxy")
	}
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	f, _, err := s.Open(ctx, a, "")
	if err != nil {
		t.Fatal("unrelated GET blocked behind PUT", err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(got, fast) {
		t.Fatal("read lost bytes")
	}
	begun := time.Now()
	_, got = b.webhookMedia(c.ChatJID, "unused", sha256Of(slow))
	if len(got) != 0 || time.Since(begun) > 300*time.Millisecond {
		t.Fatal("synchronous webhook waited on pending transfer")
	}
	// Also hold an already cached hash; the optional webhook must time out on
	// the same-hash lock rather than stall the event consumer.
	unlock, err := s.lockHash(b.ctx, sha256Of(fast))
	if err != nil {
		t.Fatal(err)
	}
	begun = time.Now()
	_, got = b.webhookMedia(a.ChatJID, "unused", sha256Of(fast))
	unlock()
	if len(got) != 0 || time.Since(begun) > 300*time.Millisecond {
		t.Fatal("webhook blocked on same-hash transfer")
	}
}

func TestMinIOPutRejectsSameLengthTamperedVerification(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-put-integrity")
	data := []byte("verified plaintext")
	row := s3TestRow(t, b, "TAMPERPUT", mediaTestChat, "document", data, time.Now())
	key, _ := s.key(sha256Of(data))
	s3SecurityProxy(t, s, nil, func(response *http.Response) error {
		if response.Request.Method == http.MethodGet && strings.HasSuffix(response.Request.URL.Path, key) {
			_ = response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("X"), len(data))))
			response.ContentLength = int64(len(data))
			response.Header.Del("X-Amz-Checksum-Crc64nvme")
		}
		return nil
	})
	_, err := s.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if !errors.Is(err, errMediaHash) {
		t.Fatal("same-length remote corruption was accepted", err)
	}
	entry, _ := s.Lookup(b.ctx, row)
	if entry != nil {
		t.Fatal("tampered PUT published a reference")
	}
}

func TestMinIOFailedPutPreservesUnmigratedLocalCopy(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-preserved-source")
	data := []byte("unmigrated exact local bytes")
	row := s3TestRow(t, b, "KEEPSOURCE", mediaTestChat, "document", data, time.Now())
	local := localMediaStorage{root: b.StoreRoot}
	_, err := local.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
	s3SecurityProxy(t, s, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>wamcp-test-secret-sentinel</Message></Error>`)
			return true
		}
		return false
	}, nil)
	_, err = s.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err == nil {
		t.Fatal("failed PUT succeeded")
	}
	f, _, err := local.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal("failed PUT destroyed unmigrated source", err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("local source changed")
	}
	dir, _ := b.StoreRoot.Open(".")
	defer func() { _ = dir.Close() }()
	entries, _ := dir.ReadDir(-1)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".media-stage-") {
			t.Fatal("failed PUT retained plaintext stage")
		}
	}
}

func TestMinIOLIDMigrationPreservesCacheReference(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-lid-cache")
	data := []byte("LID cached bytes")
	row := s3TestRow(t, b, "LIDCACHE", "111@lid", "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	dbPath := filepath.Join(t.TempDir(), "whatsapp.db")
	session, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if _, err := session.Exec(`CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT NOT NULL); INSERT INTO whatsmeow_lid_map VALUES ('111','222')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.Store.MigrateLegacyLIDChatsToPhoneJIDs(dbPath, b.Log); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := b.Store.MediaRow(row.ID, "222@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := s.Open(b.ctx, moved, "")
	if err != nil {
		t.Fatal("LID migration lost cache", err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("migrated bytes changed")
	}
	freed, failed := s.sweepUnreferenced(b.ctx)
	if freed != 0 || failed != 0 {
		t.Fatal("retention removed migrated object", freed, failed)
	}
	f, _, err = s.Open(b.ctx, moved, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestMinIOStartupCleansSpoolsAndMigrationAdoptsPendingObject(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-crash-resume")
	data := []byte("surviving migration source")
	row := s3TestRow(t, b, "RESUMEPENDING", mediaTestChat, "document", data, time.Now())
	local := localMediaStorage{root: b.StoreRoot}
	_, err := local.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := s.key(sha256Of(data))
	if _, err := s.client.PutObject(b.ctx, s.cfg.Bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{DisableMultipart: true}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".media-stage-crash", ".media-stage-crash.part", ".media-stream-crash", ".media-stream-crash.part", ".media-verified-crash"} {
		f, err := b.StoreRoot.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte("plaintext"))
		_ = f.Close()
	}
	resumed, err := newS3MediaStorage(b.ctx, s.cfg, b.Store, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	resumed.bridge = b
	b.MediaStorage = resumed
	for _, rel := range []string{".media-stage-crash", ".media-stage-crash.part", ".media-stream-crash", ".media-stream-crash.part", ".media-verified-crash"} {
		if _, err := b.StoreRoot.Lstat(rel); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("startup retained stale plaintext", err)
		}
	}
	var puts atomic.Int32
	s3SecurityProxy(t, resumed, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		return false
	}, nil)
	result, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, resumed, "s3", false, true, 1)
	if err != nil || result.Files != 1 || result.Failed != 0 || puts.Load() != 0 {
		t.Fatal("pending object not adopted", result, puts.Load(), err)
	}
	if entry, _ := local.Lookup(b.ctx, row); entry != nil {
		t.Fatal("verified migration retained source")
	}
	f, _, err := resumed.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("adopted bytes changed")
	}
}

func TestMinIOLookupRefusesComponentsAndKeysIgnoreLongIDs(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-key-identity")
	data := []byte("fixed object hash")
	row := s3TestRow(t, b, strings.Repeat("LONGMESSAGE", 1000), mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	key, _ := s.key(sha256Of(data))
	if strings.Contains(key, row.ID) || len(key) > len(s.cfg.Prefix)+90 {
		t.Fatal("object key used message ID")
	}
	// Observe the actual remote object name, rather than only checking key().
	found := 0
	for object := range s.client.ListObjects(b.ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.cfg.Prefix, Recursive: true}) {
		if object.Err != nil {
			t.Fatal(object.Err)
		}
		if object.Key != key {
			t.Fatal("remote key contains source identity")
		}
		found++
	}
	if found != 1 {
		t.Fatal("unexpected object count", found)
	}
	for _, kind := range []string{"chat", "id", "type"} {
		bad := row
		switch kind {
		case "chat":
			bad.ChatJID = "../outside"
		case "id":
			bad.ID = "../outside"
		case "type":
			bad.MediaType = "../document"
		}
		if entry, err := s.Lookup(b.ctx, bad); err == nil || entry != nil {
			t.Fatal("crafted component accepted", kind)
		}
	}
}

func TestS3ConfigPlaintextWarningClassification(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		warn     bool
	}{{"http://example.com:9000", true}, {"https://example.com:9000", false}, {"http://127.0.0.1:9000", false}, {"http://[::1]:9000", false}, {"http://localhost:9000", false}} {
		cfg := mediaBackendConfig{Backend: "s3", Endpoint: tc.endpoint}
		if cfg.plaintextRemote() != tc.warn {
			t.Fatal("incorrect HTTP warning classification")
		}
	}
}

func TestMinIODryRunCLIsPreservePlaintextSpools(t *testing.T) {
	for _, command := range []string{"migrate", "purge"} {
		t.Run(command, func(t *testing.T) {
			b, s := minioTestBridge(t, "instances/test-dry-spools-"+command)
			for key, value := range map[string]string{"WHATSAPP_MEDIA_BACKEND": "s3", "WHATSAPP_MEDIA_S3_ENDPOINT": s.cfg.Endpoint, "WHATSAPP_MEDIA_S3_REGION": s.cfg.Region, "WHATSAPP_MEDIA_S3_BUCKET": s.cfg.Bucket, "WHATSAPP_MEDIA_S3_PREFIX": s.cfg.Prefix, "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID": s.cfg.AccessKey, "WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY": s.cfg.SecretKey, "WHATSAPP_MEDIA_S3_FORCE_PATH_STYLE": "true"} {
				t.Setenv(key, value)
			}
			for _, name := range []string{".media-stage-dry", ".media-stream-dry", ".media-verified-dry"} {
				if err := b.StoreRoot.WriteFile(name, []byte("unchanged dry-run evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out, diagnostic bytes.Buffer
			code := 0
			if command == "migrate" {
				code = migrateMediaCLI([]string{"--to", "s3", "--dry-run"}, &out, &diagnostic)
			} else {
				code = purgeStatusCLI([]string{"--dry-run"}, &out, &diagnostic)
			}
			if code != 0 {
				t.Fatal("dry-run CLI failed", code, &diagnostic)
			}
			for _, name := range []string{".media-stage-dry", ".media-stream-dry", ".media-verified-dry"} {
				got, err := b.StoreRoot.ReadFile(name)
				if err != nil || string(got) != "unchanged dry-run evidence" {
					t.Fatal("dry-run removed plaintext spool", command, name, err)
				}
			}
		})
	}
}

type interruptedLocalPurge struct{ localMediaStorage }

func (s interruptedLocalPurge) Delete(ctx context.Context, rows []mediaRow, dry bool) ([]PurgeResult, error) {
	// Exercise the real local remover with cancellation between selected rows.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	first, err := s.localMediaStorage.Delete(ctx, rows[:1], dry)
	if err != nil {
		return first, err
	}
	cancel()
	rest, err := s.localMediaStorage.Delete(ctx, rows[1:], dry)
	return append(first, rest...), err
}

func TestMediaPurgeIncompleteLocalBatchReturns503(t *testing.T) {
	b, files := purgeFixture(t)
	b.MediaStorage = interruptedLocalPurge{localMediaStorage{root: b.StoreRoot}}
	code, _ := purgeCall(t, b, `{"dry_run":false,"older_than_days":1}`)
	if code != 503 {
		t.Fatal("partial local batch reported success", code)
	}
	kept, removed := 0, 0
	for _, file := range files {
		if fileExists(file) {
			kept++
		} else {
			removed++
		}
	}
	if kept != 1 || removed != 1 {
		t.Fatal("did not exercise a real partial removal", kept, removed)
	}
}

func TestMinIOCatalogIdentityLookupDoesNotPage(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-catalog-lookup")
	data := []byte("one shared catalog object")
	row := s3TestRow(t, b, "LOOKUPTARGET", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	// Ten thousand other references in the same chat must not be enumerated
	// when the caller asks about one exact identity.
	if _, err := b.Store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO messages(id,chat_jid,sender,timestamp,media_type,file_sha256) SELECT printf('CATALOG%05d',x),?,'Alice',?,'document',? FROM n`, row.ChatJID, dbTime(row.Timestamp), sha256Of(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.db.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256) SELECT id,chat_jid,file_sha256 FROM messages WHERE chat_jid=? AND id LIKE 'CATALOG%'`, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/media/cache?chat_jid="+url.QueryEscape(row.ChatJID)+"&message_id="+row.ID, nil)
	response := httptest.NewRecorder()
	b.handleMediaCache(response, request)
	var body struct {
		Items []struct {
			MessageID string `json:"message_id"`
		}
		Next string `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(body.Items) != 1 || body.Items[0].MessageID != row.ID || body.Next != "" {
		t.Fatal("identity query scanned paginated chat", response.Code, len(body.Items), body.Next)
	}
}
