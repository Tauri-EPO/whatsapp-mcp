package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func minioTestBridge(t *testing.T, prefix string) (*Bridge, *s3MediaStorage) {
	t.Helper()
	endpoint := os.Getenv("WAMCP_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("local MinIO integration: set WAMCP_TEST_MINIO_ENDPOINT")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := minio.DefaultTransport(u.Scheme == "https")
	if err != nil {
		t.Fatal(err)
	}
	transport.Proxy = nil
	client, err := minio.New(u.Host, &minio.Options{Secure: u.Scheme == "https", Creds: credentials.NewStaticV4("wamcp-test-access", "wamcp-test-secret-sentinel", ""), Region: "us-east-1", BucketLookup: minio.BucketLookupPath, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "wamcp-media-test"
	if ok, err := client.BucketExists(context.Background(), bucket); err != nil {
		t.Fatal(err)
	} else if !ok {
		if err := client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	storeInScratch(t)
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b := testBridge(t, nil, store, installRecordingLogger(t))
	cfg := mediaBackendConfig{Backend: "s3", Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, Prefix: prefix + "/", AccessKey: "wamcp-test-access", SecretKey: "wamcp-test-secret-sentinel", PathStyle: true} //nolint:gosec // Public fake credentials for the isolated MinIO test container.
	s, err := newS3MediaStorage(b.ctx, cfg, b.Store, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	b.MediaStorage = s
	s.bridge = b
	t.Cleanup(func() { _ = s.Close() })
	t.Cleanup(func() {
		log := b.Log.(*recordingLogger).String()
		for _, value := range []string{cfg.SecretKey, cfg.AccessKey, cfg.Bucket, strings.TrimSuffix(cfg.Prefix, "/")} {
			if strings.Contains(log, value) {
				t.Error("S3 logger disclosed private backend configuration")
			}
		}
	})
	t.Cleanup(func() {
		for object := range client.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: cfg.Prefix, Recursive: true}) {
			if object.Err == nil {
				_ = client.RemoveObject(context.Background(), bucket, object.Key, minio.RemoveObjectOptions{})
			}
		}
	})
	return b, s
}

func s3TestRow(t *testing.T, b *Bridge, id, chat, kind string, data []byte, at time.Time) mediaRow {
	t.Helper()
	if err := b.Store.StoreChat(chat, "Alice", at); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRoot.MkdirAll(chatMediaRel(chat), storeDirMode); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreMessage(storedMessage{ID: id, ChatJID: chat, Sender: "5511999999999", Timestamp: at, MediaType: kind, Filename: "sample.txt", URL: "https://example.com/media", MediaKey: []byte("fake-key"), FileSHA256: sha256Of(data), FileEncSHA256: sha256Of([]byte("encrypted")), FileLength: uint64(len(data))}); err != nil {
		t.Fatal(err)
	}
	row, err := b.Store.MediaRow(id, chat)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func s3TestWrite(t *testing.T, b *Bridge, row mediaRow, data []byte) {
	t.Helper()
	_, err := b.mediaStorage().Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMinIOMediaDedupeIsolationAndReaders(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-a")
	data := []byte("shared object text")
	a := s3TestRow(t, b, "S3A", mediaTestChat, "document", data, time.Now().Add(-48*time.Hour))
	c := s3TestRow(t, b, "S3B", "120363000000000001@g.us", "document", data, time.Now())
	s3TestWrite(t, b, a, data)
	s3TestWrite(t, b, c, data)
	usage, err := s.Usage(b.ctx)
	if err != nil || usage.Files != 1 || usage.Bytes != int64(len(data)) {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	objects := 0
	for object := range s.client.ListObjects(b.ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.cfg.Prefix, Recursive: true}) {
		if object.Err != nil {
			t.Fatal(object.Err)
		}
		objects++
	}
	if objects != 1 {
		t.Fatalf("objects=%d", objects)
	}
	operator := mediaOperatorServer(t, b)
	code, dry := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","chat_jid":"5511999999999@s.whatsapp.net","dry_run":true}`, fakeOperatorToken)
	if code != 200 || dry["files"] != float64(1) || dry["freed_bytes"] != float64(0) {
		t.Fatal("shared dry", code, dry)
	}
	code, real := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","chat_jid":"5511999999999@s.whatsapp.net","dry_run":false}`, fakeOperatorToken)
	if code != 200 || real["files"] != dry["files"] || real["freed_bytes"] != dry["freed_bytes"] {
		t.Fatal("shared real", code, real)
	}
	if strings.Count(b.Log.(*recordingLogger).String(), "Operator media purge:") != 1 {
		t.Fatal("purge audit missing or duplicated")
	}
	f, size, err := s.Open(b.ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := io.ReadAll(f)
	_ = f.Close()
	if size != int64(len(data)) || !bytes.Equal(actual, data) {
		t.Fatal("kept chat lost its bytes")
	}
	b.Connected = func() bool { return true }
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = b.newRESTMux(server.Listener.Addr().(*net.TCPAddr).Port, "test-bridge-token")
	server.Start()
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/media/blob?chat_jid="+url.QueryEscape(c.ChatJID)+"&message_id="+c.ID, nil)
	request.Header.Set("Authorization", "Bearer test-bridge-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || !bytes.Equal(payload, data) {
		t.Fatalf("HTTP read status=%d body=%q", response.StatusCode, payload)
	}
	request.Header.Del("Authorization")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("anonymous status=%d", response.StatusCode)
	}
	if python := os.Getenv("WAMCP_TEST_MCP_PYTHON"); python != "" {
		// Real Python consumers, real SQLite, real bearer HTTP, real MinIO.
		program := `import asyncio, json, os
import main, media_read, media_inventory, media_remote, media_resource, whatsapp
chat='120363000000000001@g.us'
blocks=media_read.read_media(chat,'S3B')
assert blocks[0].text == 'shared object text', blocks
assert media_inventory.media_stats(chat)['total']['cached_bytes']==18
assert media_inventory.list_media_page(chat_jid=chat).items[0]['cached']
assert media_resource.read_media_resource(chat,'S3B').content=='shared object text'
status=json.dumps(whatsapp.bridge_status())
assert not any(secret in status for secret in ('wamcp-test-access','wamcp-test-secret-sentinel','blobs/sha256','wamcp-media-test'))
async def through_mcp():
    result = await main.mcp.call_tool('read_media', {'chat_jid':chat,'message_id':'S3B'})
    assert not result.is_error and result.content[0].text == 'shared object text', result
    listed = await main.mcp.call_tool('list_media', {'chat_jid':chat})
    assert not listed.is_error and listed.structured_content['items'][0]['cached'], listed
    resource = await main.mcp.read_resource(media_remote.uri(chat,'S3B'))
    assert resource[0].content == 'shared object text', resource
asyncio.run(through_mcp())
print('MCP dispatcher -> bridge HTTP -> MinIO read/list/resource: passed')
print('MCP HTTP read/list/stats/resource: passed')`
		cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Test runner selects its trusted Python interpreter; the program is a literal fixture.
		cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
		dbFile := filepath.Join(b.StoreRoot.Name(), "messages.db")
		var seq int
		var dbName string
		if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile); err != nil {
			t.Fatal(err)
		}
		cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+server.URL+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("MCP integration: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
	b.Client = newTestClient(&mockLIDStore{})
	own := types.NewJID("5511999999999", types.DefaultUserServer)
	b.Client.Store.ID = &own
	b.Send = b.sendBackend()
	b.MediaAutoDownload = false
	var uploads, sends int
	b.uploadMedia = func(_ context.Context, actual []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		uploads++
		if !bytes.Equal(actual, data) {
			t.Fatal("cached send did not read MinIO bytes")
		}
		upload := outboundUpload()
		upload.FileSHA256 = sha256Of(data)
		upload.FileLength = uint64(len(data))
		return upload, nil
	}
	b.sendMessage = func(_ context.Context, to types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
		sends++
		if to.String() != c.ChatJID || message.GetDocumentMessage() == nil {
			t.Fatal("cached sender wire message", to, message)
		}
		return whatsmeow.SendResponse{ID: "CACHEDSEND", Timestamp: time.Now()}, nil
	}
	sendBody, _ := json.Marshal(SendMessageRequest{Recipient: c.ChatJID, MediaPath: mediaBlobURI(c.ChatJID, c.ID), Message: "caption"})
	sendRequest, _ := http.NewRequest("POST", server.URL+"/api/send", bytes.NewReader(sendBody))
	sendRequest.Header.Set("Authorization", "Bearer test-bridge-token")
	response, err = server.Client().Do(sendRequest)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || uploads != 1 || sends != 1 {
		t.Fatalf("cached HTTP send status=%d uploads=%d sends=%d body=%s", response.StatusCode, uploads, sends, payload)
	}
	_, webhookBytes := b.webhookMedia(c.ChatJID, "unused", sha256Of(data))
	if !bytes.Equal(webhookBytes, data) {
		t.Fatal("webhook did not read S3 bytes")
	}
	healthJSON, _ := json.Marshal(b.healthStatus())
	if bytes.Contains(healthJSON, []byte(s.cfg.SecretKey)) || bytes.Contains(healthJSON, []byte(s.cfg.AccessKey)) || strings.Contains(b.Log.(*recordingLogger).String(), s.cfg.SecretKey) {
		t.Fatal("S3 credentials disclosed")
	}
	// A second instance shares the bucket with its own prefix and database.
	other, os3 := minioTestBridge(t, "instances/test-b")
	o := s3TestRow(t, other, "S3C", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, other, o, data)
	results, err := s.Delete(b.ctx, []mediaRow{c}, false)
	if err != nil || results[0].Bytes != int64(len(data)) {
		t.Fatalf("last reference=%+v %v", results, err)
	}
	f, _, err = os3.Open(other.ctx, o, "")
	if err != nil {
		t.Fatal("other prefix lost object", err)
	}
	_ = f.Close()
}

func TestMinIOHashQuotaRetentionAndMigration(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-maintenance")
	data := []byte("1234567890")
	a := s3TestRow(t, b, "MIGRATEA", mediaTestChat, "video", data, time.Now().Add(-72*time.Hour))
	c := s3TestRow(t, b, "MIGRATEB", "120363000000000001@g.us", "video", data, time.Now())
	local := localMediaStorage{root: b.StoreRoot}
	for _, row := range []mediaRow{a, c} {
		_, err := local.Write(b.ctx, row, func(rel string) (int64, error) {
			return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []mediaRow{a, c} {
		f, _, err := local.Open(b.ctx, row, "")
		if err != nil {
			t.Fatal("source open", row.ID, err)
		}
		payload, _ := io.ReadAll(f)
		_ = f.Close()
		t.Logf("source %s bytes %q", row.ID, payload)
	}
	dry, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, s, "s3", true, true, 2)
	if err != nil || dry.Files != 2 || dry.Bytes != 20 || dry.DedupeSavedBytes != 10 {
		t.Fatalf("dry=%+v %v", dry, err)
	}
	usage, _ := s.Usage(b.ctx)
	if usage.Files != 0 {
		t.Fatal("dry wrote objects")
	}
	real, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, s, "s3", false, true, 2)
	if err != nil || real.Files != 2 || real.Failed != 0 {
		t.Fatalf("migration=%+v %v", real, err)
	}
	if entry, _ := local.Lookup(b.ctx, a); entry != nil {
		t.Fatal("source retained after verified migration")
	}
	removed, freed, failed := s.Sweep(b.ctx, 48*time.Hour, nil, time.Now())
	if removed != 1 || freed != 0 || failed != 0 {
		t.Fatalf("retention %d %d %d", removed, freed, failed)
	}
	b.MediaQuotaBytes = 10
	bad := s3TestRow(t, b, "BADHASH", mediaTestChat, "image", []byte("expected"), time.Now())
	_, err = s.Write(b.ctx, bad, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write([]byte("wrong")); return err })
	})
	if !errors.Is(err, errMediaHash) {
		t.Fatalf("hash mismatch=%v", err)
	}
	if !strings.Contains(b.Log.(*recordingLogger).String(), "SHA256 mismatch") {
		t.Fatal("hash refusal was not logged")
	}
	newRow := s3TestRow(t, b, "QUOTA", mediaTestChat, "image", []byte("new file"), time.Now())
	_, err = s.Write(b.ctx, newRow, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write([]byte("new file")); return err })
	})
	if !errors.Is(err, errMediaQuota) {
		t.Fatalf("quota=%v", err)
	}
	b.Connected = func() bool { return true }
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write([]byte("new file")); return err })
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/media/blob?chat_jid="+url.QueryEscape(mediaTestChat)+"&message_id=QUOTA", nil)
	b.handleMediaBlob(w, r)
	if w.Code != 200 || w.Body.String() != "new file" || w.Header().Get("X-Media-Cached") != "false" {
		t.Fatalf("quota stream=%d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if entry, _ := s.Lookup(b.ctx, newRow); entry != nil {
		t.Fatal("quota stream cached object")
	}
	b.MediaQuotaBytes = 0
	back, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, s, "local", false, true, 2)
	if err != nil || back.Files != 1 || back.Failed != 0 {
		t.Fatalf("reverse=%+v %v", back, err)
	}
	entry, _ := local.Lookup(b.ctx, c)
	if entry == nil {
		t.Fatal("reverse migration lost file")
	}
	usage, _ = s.Usage(b.ctx)
	if usage.Files != 0 {
		t.Fatalf("reverse left objects=%+v", usage)
	}
}

func TestMinIOPythonQuotaUnknownLengthAndAudioIngest(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-python-quota")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("set WAMCP_TEST_MCP_PYTHON for real MCP consumers")
	}
	data := []byte("new file")
	full := s3TestRow(t, b, "FULLCACHE", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, full, data)
	audioData := []byte("cached audio bytes")
	audio := s3TestRow(t, b, "INGESTAUDIO", mediaTestChat, "audio", audioData, time.Now())
	s3TestWrite(t, b, audio, audioData)
	newRow := s3TestRow(t, b, "UNKNOWNSIZE", mediaTestChat, "document", []byte("quota read bytes"), time.Now())
	if _, err := b.Store.db.Exec(`UPDATE messages SET file_length=NULL WHERE id=? AND chat_jid=?`, newRow.ID, newRow.ChatJID); err != nil {
		t.Fatal(err)
	}
	b.MediaQuotaBytes = uint64(len(data) + len(audioData))
	b.Connected = func() bool { return true }
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write([]byte("quota read bytes")); return err })
	}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = b.newRESTMux(server.Listener.Addr().(*net.TCPAddr).Port, "test-bridge-token")
	server.Start()
	defer server.Close()
	program := `import os,json
import media_read,media_inventory,transcribe_worker
chat='5511999999999@s.whatsapp.net'
blocks=media_read.read_media(chat,'UNKNOWNSIZE')
assert blocks[0].text=='quota read bytes',blocks
assert json.loads(blocks[-1].text)['bytes']==16
assert not any(item['message_id']=='UNKNOWNSIZE' and item['cached'] for item in media_inventory.list_media_page(chat_jid=chat).items)
seen=[]
def sink(path):
    assert open(path,'rb').read()==b'cached audio bytes'
    seen.append(path)
    return {'text':'synthetic transcript','language':'en','backend':'test'}
result=transcribe_worker.run_once(1,transcribe=sink,fetch=False)
assert result.transcribed==1,result
assert seen and not os.path.exists(seen[0])
print('MCP quota unknown-length read + audio ingest spool/cleanup: passed')`
	cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Test runner selects its trusted Python interpreter; the program is a literal fixture.
	cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
	var seq int
	var dbName, dbFile string
	if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+server.URL+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("MCP quota/ingest: %v\n%s", err, output)
	}
	t.Log(string(output))
	if entry, _ := s.Lookup(b.ctx, newRow); entry != nil {
		t.Fatal("quota read retained S3 object")
	}
}

func TestS3ConfigSecretsAndDenyPaths(t *testing.T) {
	values := map[string]string{"WHATSAPP_MEDIA_BACKEND": "s3", "WHATSAPP_MEDIA_S3_ENDPOINT": "http://localhost:9000", "WHATSAPP_MEDIA_S3_BUCKET": "fake-bucket", "WHATSAPP_MEDIA_S3_PREFIX": "instances/test/", "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID": "sentinel-access", "WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY": "sentinel-secret"}
	getenv := func(k string) string { return values[k] }
	if _, err := parseMediaBackend(getenv); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"", "../escape", "a//b", "a/../b", "a\\b", "a/%2e"} {
		values["WHATSAPP_MEDIA_S3_PREFIX"] = prefix
		if _, err := parseMediaBackend(getenv); err == nil {
			t.Fatalf("accepted prefix %q", prefix)
		}
	}
	values["WHATSAPP_MEDIA_S3_PREFIX"] = "instances/test/"
	values["WHATSAPP_MEDIA_S3_ENDPOINT"] = "http://sentinel-access:sentinel-secret@localhost:9000"
	_, err := parseMediaBackend(getenv)
	if err == nil || strings.Contains(err.Error(), "sentinel-") {
		t.Fatalf("secret leaked: %v", err)
	}
	for _, value := range []string{"whatsapp://media/a/b/c", "whatsapp://media/a/../b", "whatsapp://media/a/%2e%2e", "s3://fake/key"} {
		if _, _, err := parseMediaURI(value); err == nil {
			t.Fatalf("accepted URI %q", value)
		}
	}
}

func TestMinIOMigrationResumeAfterRemoteFailure(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-resume")
	data := map[string][]byte{"RESUMEA": []byte("completed bytes"), "RESUMEB": []byte("retained bytes")}
	var rows []mediaRow
	local := localMediaStorage{root: b.StoreRoot}
	for _, id := range []string{"RESUMEA", "RESUMEB"} {
		row := s3TestRow(t, b, id, mediaTestChat, "document", data[id], time.Now())
		rows = append(rows, row)
		if _, err := local.Write(b.ctx, row, func(rel string) (int64, error) {
			return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data[id]); return err })
		}); err != nil {
			t.Fatal(err)
		}
	}
	blocked, _ := s.key(sha256Of(data["RESUMEB"]))
	target, _ := url.Parse(s.cfg.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxy.Transport = transport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, blocked) {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := s.cfg
	cfg.Endpoint = server.URL
	interrupted, err := newS3MediaStorage(b.ctx, cfg, b.Store, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	result, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, interrupted, "s3", false, true, 1)
	if err != nil || result.Files != 1 || result.Failed != 1 {
		t.Fatal("interrupted migration", result, err)
	}
	if entry, _ := local.Lookup(b.ctx, rows[0]); entry != nil {
		t.Fatal("completed source not removed")
	}
	if entry, _ := local.Lookup(b.ctx, rows[1]); entry == nil {
		t.Fatal("failed source removed")
	}
	resume, err := migrateMedia(b.ctx, b.Store, b.StoreRoot, s, "s3", false, true, 2)
	if err != nil || resume.Files != 1 || resume.Failed != 0 {
		t.Fatal("resume", resume, err)
	}
	for _, row := range rows {
		reader, _, err := s.Open(b.ctx, row, "")
		if err != nil {
			t.Fatal(err)
		}
		actual, _ := io.ReadAll(reader)
		_ = reader.Close()
		if !bytes.Equal(actual, data[row.ID]) {
			t.Fatal("resumed bytes", row.ID)
		}
	}
}

func TestMinIOPublicationRejectsReplacedSnapshot(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			b, s := minioTestBridge(t, "instances/test-snapshot-"+fmt.Sprint(missing))
			old, newData := []byte("old bytes"), []byte("new snapshot bytes")
			row := s3TestRow(t, b, "SNAPSHOT", mediaTestChat, "document", old, time.Now())
			if missing {
				if _, err := b.Store.db.Exec(`UPDATE messages SET file_sha256=NULL WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
					t.Fatal(err)
				}
			}
			key, _ := s.key(sha256Of(old))
			target, _ := url.Parse(s.cfg.Endpoint)
			proxy := httputil.NewSingleHostReverseProxy(target)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			proxy.Transport = transport
			var replaced atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, key) && replaced.CompareAndSwap(false, true) {
					if err := b.Store.StoreMessage(storedMessage{ID: row.ID, ChatJID: row.ChatJID, Sender: "5511999999999", Timestamp: row.Timestamp, MediaType: row.MediaType, Filename: row.Filename, URL: "https://example.com/new-media", MediaKey: []byte("new-key"), FileSHA256: sha256Of(newData), FileEncSHA256: sha256Of([]byte("new-encrypted")), FileLength: uint64(len(newData))}); err != nil {
						t.Error(err)
						w.WriteHeader(500)
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
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(old); return err })
			})
			if !errors.Is(err, errMediaSnapshot) || !replaced.Load() {
				t.Fatal("stale publication accepted", err)
			}
			var hash []byte
			if err := b.Store.db.QueryRow(`SELECT file_sha256 FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID).Scan(&hash); err != nil || !bytes.Equal(hash, sha256Of(newData)) {
				t.Fatal("replacement snapshot overwritten", err)
			}
			if entry, _ := s.Lookup(b.ctx, row); entry != nil {
				t.Fatal("old bytes marked cached")
			}
			for object := range s.client.ListObjects(b.ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.cfg.Prefix, Recursive: true}) {
				if object.Err != nil {
					t.Fatal(object.Err)
				}
				t.Fatal("rejected upload survived cleanup")
			}
		})
	}
}

func TestMinIOLazyQuotaRuntimeOperatorAndWarnings(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-runtime")
	var err error
	b.RuntimeDefaults, err = runtimeDefaults(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	b.Connected = func() bool { return true }
	operator := mediaOperatorServer(t, b)
	if code, _ := settingsRequest(t, operator, "PATCH", `{"media.autodownload_types":["image","audio","document","sticker"]}`); code != 200 {
		t.Fatal(code)
	}
	lazyData := []byte("lazydata")
	lazy := s3TestRow(t, b, "LAZYVIDEO", mediaTestChat, "video", lazyData, time.Now().Add(-time.Hour))
	var transfers atomic.Int64
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		transfers.Add(1)
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(lazyData); return err })
	}
	b.runAutoDownload(b.ctx, mediaJob{messageID: lazy.ID, chatJID: lazy.ChatJID, mediaType: lazy.MediaType})
	if transfers.Load() != 0 {
		t.Fatal("lazy video reached CDN")
	}
	if row, err := b.Store.MediaRow(lazy.ID, lazy.ChatJID); err != nil || row.ID != lazy.ID {
		t.Fatal("lazy video lost its archive row", err)
	}
	if entry, _ := s.Lookup(b.ctx, lazy); entry != nil {
		t.Fatal("lazy video cached automatically")
	}
	ok, _, _, uri, err := b.DownloadMedia(b.ctx, lazy.ID, lazy.ChatJID)
	if !ok || err != nil || uri != mediaBlobURI(lazy.ChatJID, lazy.ID) || transfers.Load() != 1 {
		t.Fatalf("explicit download %t %s %v", ok, uri, err)
	}
	for _, item := range []struct {
		id, kind string
		size     int
	}{{"OLDVIDEO", "video", 20}, {"KEPTIMAGE", "image", 20}, {"KEPTDOCUMENT", "document", 40}} {
		data := bytes.Repeat([]byte(item.id[:1]), item.size)
		row := s3TestRow(t, b, item.id, mediaTestChat, item.kind, data, time.Now().Add(-2*time.Hour))
		s3TestWrite(t, b, row, data)
	}
	sharedData := bytes.Repeat([]byte("O"), 20)
	shared := s3TestRow(t, b, "KEEPSSHARED", "120363000000000001@g.us", "document", sharedData, time.Now())
	s3TestWrite(t, b, shared, sharedData)
	if code, _ := settingsRequest(t, operator, "PATCH", `{"media.quota_bytes":100}`); code != 200 {
		t.Fatal(code)
	}
	events := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		if json.NewDecoder(r.Body).Decode(&event) != nil {
			t.Error("bad quota event")
		}
		events <- event["state"].(string)
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	b.Webhook = &webhookSender{client: receiver.Client(), url: receiver.URL, enabled: true, failures: &b.metrics.webhookFailures}
	b.ForwardConnection = true
	health := b.healthStatus()
	if health["media_quota_warning"] != true || health["media_bytes"] != int64(88) || health["media_backend"] != "s3" {
		t.Fatal(health)
	}
	if state := <-events; state != "warning" {
		t.Fatal(state)
	}
	_ = b.healthStatus()
	select {
	case state := <-events:
		t.Fatal("duplicate quota event", state)
	default:
	}
	code, usage := mediaHTTPRequest(t, operator, "GET", "/operator/v1/media/usage?limit=1", "", fakeOperatorToken)
	var sum float64
	for _, value := range usage["by_type"].(map[string]any) {
		sum += value.(float64)
	}
	if code != 200 || sum != 88 || len(usage["by_chat"].([]any)) != 1 {
		t.Fatal(code, usage)
	}
	code, _ = mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"dry_run":true}`, fakeOperatorToken)
	if code != 400 {
		t.Fatal("purge accepted missing type", code)
	}
	code, _ = mediaHTTPRequest(t, operator, "GET", "/operator/v1/media/usage", "", "wrong-token")
	if code != 401 {
		t.Fatal("operator auth", code)
	}
	more := []byte("twelve-bytes")
	row := s3TestRow(t, b, "FULLIMAGE", mediaTestChat, "image", more, time.Now())
	s3TestWrite(t, b, row, more)
	health = b.healthStatus()
	if health["media_caching_paused"] != true {
		t.Fatal(health)
	}
	if state := <-events; state != "full" {
		t.Fatal(state)
	}
	newData := bytes.Repeat([]byte("N"), 20)
	newRow := s3TestRow(t, b, "FULLREFUSAL", mediaTestChat, "image", newData, time.Now())
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		transfers.Add(1)
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(newData); return err })
	}
	b.runAutoDownload(b.ctx, mediaJob{messageID: newRow.ID, chatJID: newRow.ChatJID, mediaType: newRow.MediaType})
	if transfers.Load() != 1 {
		t.Fatal("full quota automatic fetch reached CDN")
	}
	w := httptest.NewRecorder()
	b.handleMediaBlob(w, httptest.NewRequest("GET", "/api/media/blob?chat_jid="+url.QueryEscape(newRow.ChatJID)+"&message_id="+newRow.ID, nil))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), newData) || w.Header().Get("X-Media-Cached") != "false" || w.Header().Get("X-Media-Reason") != "quota" || transfers.Load() != 2 {
		t.Fatalf("stream %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	entries, _ := fs.ReadDir(b.StoreRoot.FS(), ".")
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".media-stream-") {
			t.Fatal("stream spool survived")
		}
	}
	if code, _ := settingsRequest(t, operator, "PATCH", `{"media.quota_evict_types":["video"],"media.quota_evict_target_percent":60}`); code != 200 {
		t.Fatal(code)
	}
	_, release, err := b.acquireMediaQuota(b.ctx, 10)
	release()
	if !errors.Is(err, errMediaQuota) {
		t.Fatal("unlisted types evicted", err)
	}
	if f, _, err := s.Open(b.ctx, shared, ""); err != nil {
		t.Fatal("eviction deleted object held by unlisted type", err)
	} else {
		_ = f.Close()
	}
	for _, id := range []string{"KEPTIMAGE", "KEPTDOCUMENT", "FULLIMAGE"} {
		row, _ := b.Store.MediaRow(id, mediaTestChat)
		if entry, _ := s.Lookup(b.ctx, row); entry == nil {
			t.Fatal("unlisted type lost", id)
		}
	}
	if code, _ := settingsRequest(t, operator, "PATCH", `{"media.quota_evict_types":["video","document"],"media.quota_evict_target_percent":60}`); code != 200 {
		t.Fatal(code)
	}
	_, release, err = b.acquireMediaQuota(b.ctx, 10)
	release()
	if err != nil {
		t.Fatal("eligible eviction refused", err)
	}
	usageNow, _ := s.Usage(b.ctx)
	if usageNow.Bytes != 52 {
		t.Fatal("eviction bytes", usageNow)
	}
	metrics := httptest.NewRecorder()
	b.handleMetrics()(metrics, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{`whatsapp_bridge_media_quota_bytes 100`, `backend="s3"`, `whatsapp_bridge_media_evicted_bytes_total`} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Fatal("missing metric", want)
		}
	}
	if !strings.Contains(b.Log.(*recordingLogger).String(), "Media quota full") {
		t.Fatal("quota log missing")
	}
	unlock, err := b.lockMediaQuota(b.ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := b.metrics.mediaQuotaRefusals.Load()
	tryCtx, cancel := context.WithTimeout(context.WithValue(withAutomaticCache(b.ctx), quotaTryKey{}, true), time.Second)
	_, _, _, _, err = b.DownloadMedia(tryCtx, newRow.ID, newRow.ChatJID)
	cancel()
	unlock()
	if !errors.Is(err, errMediaQuotaBusy) || b.metrics.mediaQuotaRefusals.Load() != before || transfers.Load() != 2 {
		t.Fatal("busy quota lease misclassified or reached CDN", err)
	}
}

func TestMinIOPartialPurgeAndCorruptObject(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-partial")
	aData, cData := []byte("first media"), []byte("second media")
	a := s3TestRow(t, b, "PARTIALA", mediaTestChat, "document", aData, time.Now())
	c := s3TestRow(t, b, "PARTIALB", mediaTestChat, "document", cData, time.Now())
	s3TestWrite(t, b, a, aData)
	s3TestWrite(t, b, c, cData)
	key, _ := s.key(sha256Of(aData))
	target, _ := url.Parse(s.cfg.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
	proxyTransport.Proxy = nil
	proxy.Transport = proxyTransport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, key) {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>wamcp-test-secret-sentinel</Message></Error>`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	transport, _ := minio.DefaultTransport(false)
	transport.Proxy = nil
	client, err := minio.New(u.Host, &minio.Options{Secure: false, Creds: credentials.NewStaticV4(s.cfg.AccessKey, s.cfg.SecretKey, ""), Region: s.cfg.Region, BucketLookup: minio.BucketLookupPath, Transport: transport, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	original := s.client
	s.client = client
	results, err := s.Delete(b.ctx, []mediaRow{a, c}, false)
	if err != nil || len(results) != 2 || results[0].Purged || results[0].Bytes != 0 || !results[1].Purged || results[1].Bytes != int64(len(cData)) {
		t.Fatalf("partial purge=%+v %v", results, err)
	}
	encoded, _ := json.Marshal(results)
	if bytes.Contains(encoded, []byte("sentinel")) {
		t.Fatal("SDK secret leaked")
	}
	s.client = original
	usage, _ := s.Usage(b.ctx)
	if usage.Files != 1 || usage.Bytes != int64(len(aData)) {
		t.Fatal("failed removal no longer charged", usage)
	}
	operator := mediaOperatorServer(t, b)
	code, preview := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","include_orphans":true,"dry_run":true}`, fakeOperatorToken)
	if code != 200 || preview["orphan_files"] != float64(0) || preview["freed_bytes"] != float64(len(aData)) {
		t.Fatal("retained reference preview", code, preview)
	}
	code, removed := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","include_orphans":true,"dry_run":false}`, fakeOperatorToken)
	if code != 200 || removed["freed_bytes"] != preview["freed_bytes"] {
		t.Fatal("orphan cleanup", code, removed)
	}
	s3TestWrite(t, b, a, aData)
	s.client = client
	if items, err := s.Delete(b.ctx, []mediaRow{a}, false); err != nil || items[0].Purged {
		t.Fatal("retry setup", items, err)
	}
	s.client = original
	items, err := s.Delete(b.ctx, []mediaRow{a}, false)
	if err != nil || !items[0].Purged || items[0].Bytes != int64(len(aData)) {
		t.Fatal("retry failed", items, err)
	}
	s3TestWrite(t, b, c, cData)
	key, _ = s.key(sha256Of(cData))
	tampered := bytes.Repeat([]byte("X"), len(cData))
	if _, err := s.client.PutObject(b.ctx, s.cfg.Bucket, key, bytes.NewReader(tampered), int64(len(tampered)), minio.PutObjectOptions{DisableMultipart: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(b.ctx, c, ""); !errors.Is(err, errMediaHash) {
		t.Fatal("corrupt object released bytes", err)
	}
	_, webhook := b.webhookMedia(c.ChatJID, "unused", sha256Of(cData))
	if len(webhook) != 0 {
		t.Fatal("webhook released corrupt bytes")
	}
}

func TestS3ConfigMigrationRefusesCatalogSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "catalog")
	if err := os.WriteFile(outside, []byte("untouched sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "messages.db")); err != nil {
		t.Skip("symlinks unavailable")
	}
	for key, value := range map[string]string{ //nolint:gosec // Public fake credentials; no server is reached by this refusal fixture.
		"WHATSAPP_STORE_DIR": root, "WHATSAPP_MEDIA_S3_BUCKET": "wamcp-media-test",
		"WHATSAPP_MEDIA_S3_PREFIX": "instances/test-catalog", "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID": "wamcp-test-access",
		"WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY": "wamcp-test-secret-sentinel",
	} {
		t.Setenv(key, value)
	}
	for _, args := range [][]string{{"--to", "s3"}, {"--to", "s3", "--dry-run"}} {
		var out, diagnostic bytes.Buffer
		if code := migrateMediaCLI(args, &out, &diagnostic); code != 1 || !strings.Contains(diagnostic.String(), "existing regular file") {
			t.Fatal("catalog symlink accepted", code, &diagnostic)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "untouched sentinel" { //nolint:gosec // Test-owned scratch path, checked for unchanged bytes after refusal.
		t.Fatal("symlink target changed", err)
	}
}

func TestS3ConfigQuotaWarningRoundsUp(t *testing.T) {
	b := testBridge(t, nil, newTestMessageStore(t), installRecordingLogger(t))
	for _, tc := range []struct {
		quota uint64
		used  int64
		want  bool
	}{{1, 0, false}, {1, 1, true}, {3, 2, false}, {3, 3, true}, {101, 80, false}, {101, 81, true}, {0, 100, false}} {
		if got := b.observeMediaQuota(tc.quota, tc.used); got != tc.want {
			t.Fatalf("quota=%d used=%d warning=%t want=%t", tc.quota, tc.used, got, tc.want)
		}
	}
}

func TestMinIOMigrationCLIReadOnlyResumeAndSecrets(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-cli")
	data := []byte("migration bytes")
	row := s3TestRow(t, b, "CLIMEDIA", mediaTestChat, "document", data, time.Now())
	local := localMediaStorage{root: b.StoreRoot}
	_, err := local.Write(b.ctx, row, func(rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"WHATSAPP_MEDIA_S3_ENDPOINT": s.cfg.Endpoint, "WHATSAPP_MEDIA_S3_REGION": s.cfg.Region, "WHATSAPP_MEDIA_S3_BUCKET": s.cfg.Bucket, "WHATSAPP_MEDIA_S3_PREFIX": s.cfg.Prefix, "WHATSAPP_MEDIA_S3_ACCESS_KEY_ID": s.cfg.AccessKey, "WHATSAPP_MEDIA_S3_SECRET_ACCESS_KEY": s.cfg.SecretKey, "WHATSAPP_MEDIA_S3_FORCE_PATH_STYLE": "true"} {
		t.Setenv(key, value)
	}
	if err := b.StoreRoot.WriteFile(chatMediaRel(row.ChatJID)+"/unmapped.txt", []byte("unmapped"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(b.StoreRoot.Name(), chatMediaRel(row.ChatJID), "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	run := func(args ...string) mediaMigrationResult {
		t.Helper()
		out.Reset()
		diagnostic.Reset()
		code := migrateMediaCLI(args, &out, &diagnostic)
		if code != 0 {
			t.Fatalf("CLI exit=%d %s %s", code, &out, &diagnostic)
		}
		var result mediaMigrationResult
		if json.Unmarshal(out.Bytes(), &result) != nil {
			t.Fatal(&out)
		}
		return result
	}
	dry := run("--to", "s3", "--dry-run", "--concurrency", "2")
	if dry.Files != 1 || dry.SkipReasons["unmapped_file"] != 1 || dry.SkipReasons["unsafe_symlink"] != 1 {
		t.Fatal(dry)
	}
	usage, _ := s.Usage(b.ctx)
	if usage.Files != 0 {
		t.Fatal("dry-run changed catalog")
	}
	real := run("--to", "s3", "--concurrency", "2")
	if real.Files != 1 || real.Failed != 0 {
		t.Fatal(real)
	}
	resume := run("--to", "s3", "--delete-source")
	if resume.Files != 0 || resume.SkipReasons["already_migrated"] != 1 {
		t.Fatal(resume)
	}
	if entry, _ := local.Lookup(b.ctx, row); entry != nil {
		t.Fatal("resume did not delete verified source")
	}
	back := run("--to", "local", "--delete-source")
	if back.Files != 1 {
		t.Fatal(back)
	}
	f, _, err := local.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(actual, data) {
		t.Fatal("roundtrip corrupted bytes")
	}
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostic.Reset()
	code := migrateMediaCLI([]string{"--to", "s3", "--dry-run"}, &out, &diagnostic)
	lock.Release()
	if code != 1 || !strings.Contains(diagnostic.String(), "Stop the bridge") {
		t.Fatal("live bridge lock not enforced", code, &diagnostic)
	}
	t.Setenv("WHATSAPP_MEDIA_BACKEND", "s3")
	statusData := []byte("status")
	status := s3TestRow(t, b, "S3STATUS", "status@broadcast", "image", statusData, time.Now())
	s3TestWrite(t, b, status, statusData)
	out.Reset()
	diagnostic.Reset()
	code = purgeStatusCLI([]string{"--dry-run"}, &out, &diagnostic)
	var statusDry operatorMediaResult
	if code != 0 || json.Unmarshal(out.Bytes(), &statusDry) != nil || statusDry.FreedBytes != 6 || statusDry.Files != 1 {
		t.Fatal("S3 status CLI dry", code, &out, &diagnostic)
	}
	out.Reset()
	diagnostic.Reset()
	code = purgeStatusCLI(nil, &out, &diagnostic)
	var statusReal operatorMediaResult
	if code != 0 || json.Unmarshal(out.Bytes(), &statusReal) != nil || statusReal.FreedBytes != statusDry.FreedBytes || statusReal.Files != 1 {
		t.Fatal("S3 status CLI real", code, &out, &diagnostic)
	}
	if row, err := b.Store.MediaRow(status.ID, status.ChatJID); err != nil || row.ID != status.ID {
		t.Fatal("status purge removed row", err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret-file-sentinel\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{mediaS3SecretFileEnv: secret}
	credential, err := mediaSecret(func(k string) string { return values[k] }, mediaS3SecretEnv, mediaS3SecretFileEnv)
	if err != nil || credential != "secret-file-sentinel" {
		t.Fatal("private credential file", err)
	}
	if err := os.Chmod(secret, 0644); err != nil { //nolint:gosec // Deliberately permissive mode proves the credential-file refusal.
		t.Fatal(err)
	}
	_, err = mediaSecret(func(k string) string { return values[k] }, mediaS3SecretEnv, mediaS3SecretFileEnv)
	if err == nil || strings.Contains(err.Error(), "secret-file-sentinel") {
		t.Fatal("credential permission check", err)
	}
	badCfg := s.cfg
	badCfg.SecretKey = "wrong-secret-sentinel"
	_, err = newS3MediaStorage(b.ctx, badCfg, b.Store, b.StoreRoot)
	if err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatal("probe did not sanitize authentication failure", err)
	}
}
