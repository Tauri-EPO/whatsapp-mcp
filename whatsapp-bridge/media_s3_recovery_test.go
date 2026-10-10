package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestMinIOPublicationRechecksQuotaAfterConcurrentDeletion(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-publication-quota-race")
	data, filler := []byte("0123456789"), []byte("abcdefghij")
	old := s3TestRow(t, b, "QUOTAOLD", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, old, data)
	b.MediaQuotaBytes = uint64(len(data))
	next := s3TestRow(t, b, "QUOTAREPUBLISH", mediaTestChat, "document", data, time.Now())
	other := s3TestRow(t, b, "QUOTAFILLER", mediaTestChat, "document", filler, time.Now())
	hash := sha256Of(data)
	unlock, err := s.lockHash(b.ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(b.ctx, next, func(rel string) (int64, error) {
			return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
		})
		done <- err
	}()
	t.Cleanup(func() { release(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		waiting := s.locks[hex.EncodeToString(hash)].users == 2
		s.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publisher did not wait on the held content lock")
		}
		time.Sleep(time.Millisecond)
	}
	// Execute deletion's remote and catalog operations while holding its real
	// content lock. Another hash fills the freed budget before this one unlocks.
	if err := s.removeObject(b.ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.db.Exec(`DELETE FROM media_cache WHERE sha256=?`, hash); err != nil {
		t.Fatal(err)
	}
	s3TestWrite(t, b, other, filler)
	release()
	err = <-done
	done <- err // Cleanup also joins the publisher on an early assertion failure.
	if !errors.Is(err, errMediaQuota) {
		t.Fatalf("republished deleted hash bypassed the filled quota: %v", err)
	}
	usage, err := s.Usage(b.ctx)
	if err != nil || usage.Bytes != int64(len(filler)) {
		t.Fatal("quota usage exceeded its ceiling", usage, err)
	}
	reader, _, err := s.Open(b.ctx, other, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(actual, filler) {
		t.Fatal("admitted object's bytes changed", err)
	}
}

func TestMinIOColdMCPReadsMeasureActualBytes(t *testing.T) {
	for _, quota := range []uint64{0, 1} {
		t.Run(map[uint64]string{0: "cached", 1: "streamed"}[quota], func(t *testing.T) {
			b, _ := minioTestBridge(t, "instances/test-cold-length-"+map[uint64]string{0: "cached", 1: "streamed"}[quota])
			data := []byte("cold S3 document bytes")
			b.MediaQuotaBytes = quota
			for _, tc := range []struct {
				id       string
				declared any
			}{{"NULL", nil}, {"ZERO", 0}, {"SMALL", 1}, {"EXACT", len(data)}} {
				for _, consumer := range []string{"TOOL", "RESOURCE"} {
					row := s3TestRow(t, b, "COLD"+tc.id+consumer, mediaTestChat, "document", data, time.Now())
					if _, err := b.Store.db.Exec(`UPDATE messages SET file_length=?,filename='sample.txt' WHERE id=? AND chat_jid=?`, tc.declared, row.ID, row.ChatJID); err != nil {
						t.Fatal(err)
					}
				}
			}
			b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
				return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
			}
			server := s3ReviewREST(t, b)
			s3RecoveryMCP(t, b, server.URL, `import asyncio,json
import main,media_remote
chat='5511999999999@s.whatsapp.net'
expected=b'cold S3 document bytes'
async def run():
    errors=[]
    for declared in ('NULL','ZERO','SMALL','EXACT'):
        try:
            result=await main.mcp.call_tool('read_media',{'chat_jid':chat,'message_id':'COLD'+declared+'TOOL'})
            assert not result.is_error, result.content[0].text
            assert result.content[0].text==expected.decode(), result.content[0].text
            assert json.loads(result.content[-1].text)['bytes']==len(expected)
        except Exception as exc: errors.append(declared+' tool: '+str(exc))
        try:
            result=await main.mcp.read_resource(media_remote.uri(chat,'COLD'+declared+'RESOURCE'))
            content=result[0].content
            if isinstance(content,str): content=content.encode()
            assert content==expected, repr(content)
        except Exception as exc: errors.append(declared+' resource: '+str(exc))
    assert not errors, '; '.join(errors)
asyncio.run(run())
print('cold tool/resource: NULL, zero, undersized and exact declarations returned exact measured bytes')`)
		})
	}
}

func TestMinIOInboundWebhookDoesNotWaitForPublication(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-inbound-webhook")
	webhook, payloads := captureWebhook(t)
	t.Setenv("WEBHOOK_URL", webhook.URL)
	b.Webhook = newWebhookSender("", true)
	b.MediaAutoDownload = true
	data := []byte("fake image bytes")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s3SecurityProxy(t, s, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false
	}, nil)
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	}
	msg := buildImageMessage(phonePN, phonePN, false, "caption")
	msg.Info.ID = "INBOUNDWEBHOOK"
	msg.Message.ImageMessage.URL = proto.String("https://example.com/media")
	msg.Message.ImageMessage.MediaKey = []byte("fake-key")
	msg.Message.ImageMessage.FileSHA256 = sha256Of(data)
	msg.Message.ImageMessage.FileEncSHA256 = sha256Of([]byte("encrypted"))
	msg.Message.ImageMessage.FileLength = proto.Uint64(uint64(len(data)))
	done := make(chan struct{})
	go func() { b.handleMessage(msg); close(done) }()
	t.Cleanup(func() { close(release); <-done; b.Shutdown(5 * time.Second) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach MinIO")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inbound webhook/event consumer waited for S3 PUT")
	}
	select {
	case payload := <-payloads:
		if payload.MessageID != msg.Info.ID || payload.MediaBase64 != "" {
			t.Fatal("wrong optional webhook media", payload)
		}
	default:
		t.Fatal("webhook was not delivered")
	}
	next := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "next message")
	next.Info.ID = "AFTERWEBHOOK"
	b.handleMessage(next)
	var count int
	if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=? AND chat_jid=?`, next.Info.ID, phonePN.String()).Scan(&count); err != nil || count != 1 {
		t.Fatal("following event was not stored", count, err)
	}
}

func TestMinIOTransientHTTPStreamStopsWithLifecycle(t *testing.T) {
	testMinIOTransientHTTPShutdown(t, false)
}

func TestMinIOTransientHTTPGraceExpiryCleansSpool(t *testing.T) {
	testMinIOTransientHTTPShutdown(t, true)
}

func testMinIOTransientHTTPShutdown(t *testing.T, httpDrain bool) {
	t.Helper()
	prefix := "instances/test-transient-shutdown"
	if httpDrain {
		prefix += "-grace"
	}
	b, _ := minioTestBridge(t, prefix)
	data := []byte("temporary quota fallback")
	row := s3TestRow(t, b, "TRANSIENTSHUTDOWN", mediaTestChat, "document", data, time.Now())
	b.MediaQuotaBytes = 1
	entered, finished := make(chan struct{}), make(chan struct{})
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		defer close(finished)
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error {
			if _, err := f.Write(data); err != nil {
				return err
			}
			close(entered)
			<-ctx.Done()
			if httpDrain {
				time.Sleep(20 * time.Millisecond)
			}
			return ctx.Err()
		})
	}
	server := s3ReviewREST(t, b)
	if httpDrain {
		b.httpServer = server.Config
	}
	req, _ := http.NewRequest("GET", server.URL+"/api/media/blob?chat_jid="+url.QueryEscape(row.ChatJID)+"&message_id="+row.ID, nil)
	req.Header.Set("Authorization", "Bearer test-bridge-token")
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(requestCtx)
	done := make(chan struct{})
	go func() {
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = response.Body.Close()
		}
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done; <-finished })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("quota fallback did not start")
	}
	if httpDrain {
		b.Shutdown(200 * time.Millisecond)
	} else {
		b.Shutdown(5 * time.Second)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Shutdown returned with request-owned CDN transfer running")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP fallback did not finish")
	}
	files, err := os.ReadDir(storeDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".media-stream-") {
			t.Fatal("plaintext fallback survived shutdown")
		}
	}
}

func TestMinIOStatusPurgeIncludesDetachedStatusObjects(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-status-orphan")
	data := []byte("detached status bytes")
	row := s3TestRow(t, b, "STATUSORPHAN", "status@broadcast", "image", data, time.Now())
	s3TestWrite(t, b, row, data)
	if _, err := b.Store.db.Exec(`DELETE FROM messages WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	server := s3ReviewREST(t, b)
	for _, dry := range []bool{true, false} {
		body, _ := json.Marshal(map[string]any{"scope": "status", "dry_run": dry, "include_orphans": true})
		code, response := mediaHTTPRequest(t, server, "POST", "/api/media/purge", string(body), "test-bridge-token")
		if code != 200 || response["orphan_files"] != float64(1) || response["purged_bytes"] != float64(len(data)) {
			t.Fatal("status orphan was skipped", dry, code, response)
		}
	}
	usage, err := s.Usage(b.ctx)
	if err != nil || usage.Bytes != 0 {
		t.Fatal("status orphan remains charged", usage, err)
	}
}

func TestMinIODeleteSQLFailureRecoversAndRefetches(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-delete-recovery")
	data := []byte("recoverable remote deletion")
	row := s3TestRow(t, b, "DELETERECOVERY", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	if _, err := b.Store.db.Exec(`CREATE TRIGGER refuse_ref_delete BEFORE DELETE ON media_cache_refs BEGIN SELECT RAISE(ABORT,'injected SQL deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	results, err := s.Delete(b.ctx, []mediaRow{row}, false)
	if err == nil || len(results) != 1 || results[0].Purged {
		t.Fatal("SQL failure was not reported", results, err)
	}
	key, _ := s.key(sha256Of(data))
	_, err = s.client.StatObject(b.ctx, s.cfg.Bucket, key, minio.StatObjectOptions{})
	if minio.ToErrorResponse(err).StatusCode != 404 {
		t.Fatal("remote DELETE did not actually succeed", err)
	}
	if _, err = b.Store.db.Exec(`DROP TRIGGER refuse_ref_delete`); err != nil {
		t.Fatal(err)
	}
	// Close/reopen SQLite as after a process restart. Reapplying the schema
	// must preserve the committed recovery journal and its source messages.
	if err := b.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	b.Store = reopened
	recovered, err := newS3MediaStorage(b.ctx, s.cfg, b.Store, b.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	recovered.bridge = b
	b.MediaStorage = recovered
	entry, err := recovered.Lookup(b.ctx, row)
	if err != nil || entry != nil {
		t.Fatal("missing object remained cached after SQL failure/restart", entry, err)
	}
	var transfers atomic.Int32
	b.mediaTransfer = func(_ context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		transfers.Add(1)
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error { _, err := f.Write(data); return err })
	}
	server := s3ReviewREST(t, b)
	body, _ := json.Marshal(DownloadMediaRequest{MessageID: row.ID, ChatJID: row.ChatJID})
	code, response := mediaHTTPRequest(t, server, "POST", "/api/download", string(body), "test-bridge-token")
	if code != 200 || response["success"] != true || transfers.Load() != 1 {
		t.Fatal("HTTP download did not refetch missing cache", code, response, transfers.Load())
	}
	f, _, err := recovered.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(actual, data) {
		t.Fatal("refetched bytes changed")
	}
}

func TestMinIOQuotaStreamPermanentMediaDoesNotStarveIngest(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-permanent-quota-stream")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real Python ingest proof requires WAMCP_TEST_MCP_PYTHON")
	}
	good := []byte("healthy cached voice note")
	at := time.Now().Add(-48 * time.Hour)
	row := s3TestRow(t, b, "HEALTHYQUOTA", mediaTestChat, "audio", good, at.Add(-time.Minute))
	s3TestWrite(t, b, row, good)
	b.MediaQuotaBytes = uint64(len(good))
	for i, id := range []string{"LOSTQUOTAA", "LOSTQUOTAB", "LOSTQUOTAC"} {
		s3TestRow(t, b, id, mediaTestChat, "audio", []byte(id), at.Add(time.Duration(3-i)*time.Second))
	}
	var retries atomic.Int32
	b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
		return 0, whatsmeow.ErrMediaDownloadFailedWith404
	}
	b.mediaRetryDownload = func(context.Context, string, string, *MediaDownloader, *os.Root, string) (int64, error) {
		retries.Add(1)
		return 0, errMediaUnavailable
	}
	server := s3ReviewREST(t, b)
	code, response := mediaHTTPRequest(t, server, "GET", "/api/media/blob?chat_jid="+url.QueryEscape(mediaTestChat)+"&message_id=LOSTQUOTAA", "", "test-bridge-token")
	if failure, ok := response["error"].(map[string]any); code != 502 || !ok || failure["code"] != "media_unavailable" {
		t.Error("quota fallback lost permanent media code", code, response)
	}
	program := `import hashlib,os
import media_notes,transcribe_worker
seen=[]
def sink(path):
    with open(path,'rb') as source: assert source.read()==b'healthy cached voice note'
    seen.append(path)
    return {'text':'synthetic transcript','language':'en','backend':'test'}
result=transcribe_worker.run_once(4,transcribe=sink,fetch=True)
assert result.transcribed==1 and result.failed==0 and not result.outage,result
assert len(seen)==1 and not os.path.exists(seen[0])
for identifier in ('LOSTQUOTAA','LOSTQUOTAB','LOSTQUOTAC'):
    notes=media_notes.get_media_notes(hashlib.sha256(identifier.encode()).hexdigest())['notes']
    assert 'media_unavailable' in notes and 'transcript_error' not in notes,notes
again=transcribe_worker.run_once(4,transcribe=sink,fetch=True)
assert again.pending==0 and not again.outage,again
print('three permanent quota-stream misses recorded; healthy audio transcribed; next round has no pending work')`
	cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Trusted interpreter and literal fake-fixture program.
	cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
	var seq int
	var dbName, dbFile string
	if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+server.URL+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("permanent quota ingest: %v\n%s", err, output)
	}
	if retries.Load() != 4 {
		t.Fatal("permanent notes were retried on the following round", retries.Load())
	}
	t.Log(string(output))
}

func TestMinIOCachedRefusalPreservesReadableDuplicate(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-permanent-refusal")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real Python ingest proof requires WAMCP_TEST_MCP_PYTHON")
	}
	data := []byte("readable shared voice note")
	for i, id := range []string{"REFUSED..VOICE", "READABLEVOICE"} {
		row := s3TestRow(t, b, id, mediaTestChat, "audio", data, time.Now().Add(-time.Duration(i)*time.Minute))
		if id == "READABLEVOICE" {
			s3TestWrite(t, b, row, data)
		}
	}
	// An old/corrupt catalog can contain an unsafe identity referencing valid
	// shared bytes. Refusing that row must not suppress the readable copy.
	if _, err := b.Store.db.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256) VALUES(?,?,?)`, "REFUSED..VOICE", mediaTestChat, sha256Of(data)); err != nil {
		t.Fatal(err)
	}
	server := s3ReviewREST(t, b)
	program := `import hashlib,os
import media_notes,transcribe_worker
seen=[]
def sink(path):
    with open(path,'rb') as source: assert source.read()==b'readable shared voice note'
    seen.append(path)
    return {'text':'synthetic transcript','language':'en','backend':'test'}
first=transcribe_worker.run_once(2,transcribe=sink,fetch=False)
assert not first.outage and first.failed==0,first
refusals=media_notes.fetch_media_refusals([('5511999999999@s.whatsapp.net','REFUSED..VOICE')])
assert len(refusals)==1,refusals
sha=hashlib.sha256(b'readable shared voice note').hexdigest()
assert 'media_unavailable' not in media_notes.get_media_notes(sha)['notes']
second=transcribe_worker.run_once(2,transcribe=sink,fetch=False)
assert second.transcribed==1 and not second.outage,second
assert len(seen)==1 and not os.path.exists(seen[0])
print('refused identity remembered per message; shared hash stays readable and healthy copy transcribes')`
	cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Trusted interpreter and literal fake-fixture program.
	cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
	var seq int
	var dbName, dbFile string
	if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+server.URL+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("permanent identity refusal: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestMinIOMisleadingRemoteNamesKeepCategoryMIME(t *testing.T) {
	for _, kind := range []string{"image", "audio"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := minioTestBridge(t, "instances/test-misleading-name-"+kind)
			data := []byte("ID3 fake audio")
			if kind == "image" {
				var out bytes.Buffer
				if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
					t.Fatal(err)
				}
				data = out.Bytes()
			}
			row := s3TestRow(t, b, "MISLEADINGNAME", mediaTestChat, kind, data, time.Now())
			if _, err := b.Store.db.Exec(`UPDATE messages SET filename='photo.txt' WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
				t.Fatal(err)
			}
			s3TestWrite(t, b, row, data)
			server := s3ReviewREST(t, b)
			program := `import asyncio,hashlib
import main,media_remote
chat='5511999999999@s.whatsapp.net'
kind='MEDIA_KIND'
mime={'image':'image/png','audio':'audio/mpeg'}[kind]
async def run():
    errors=[]
    try:
        result=await main.mcp.call_tool('read_media',{'chat_jid':chat,'message_id':'MISLEADINGNAME'})
        assert not result.is_error,result.content
        assert result.content[0].type==kind and result.content[0].mime_type==mime,result.content[0].type
    except Exception as exc: errors.append('tool: '+str(exc))
    try:
        result=await main.mcp.read_resource(media_remote.uri(chat,'MISLEADINGNAME'))
        assert result[0].mime_type==mime,result[0].mime_type
        assert isinstance(result[0].content,bytes) and hashlib.sha256(result[0].content).hexdigest()=='EXPECTED_SHA'
    except Exception as exc: errors.append('resource: '+str(exc))
    assert not errors,errors
asyncio.run(run())
print('misleading sender filename did not override category/sniffed remote MIME')`
			program = strings.ReplaceAll(strings.ReplaceAll(program, "MEDIA_KIND", kind), "EXPECTED_SHA", hex.EncodeToString(sha256Of(data)))
			s3RecoveryMCP(t, b, server.URL, program)
		})
	}
}

func TestMinIOS3MaintenanceSelectionsBounded(t *testing.T) {
	for _, mode := range []string{"operator", "retention", "quota"} {
		t.Run(mode, func(t *testing.T) {
			b, s := minioTestBridge(t, "instances/test-maintenance-bounded-"+mode)
			data := []byte("shared maintenance bytes")
			row := s3TestRow(t, b, "MAINTTARGET", mediaTestChat, "document", data, time.Now().Add(-48*time.Hour))
			s3TestWrite(t, b, row, data)
			other := "120363000000000001@g.us"
			if err := b.Store.StoreChat(other, "Example group", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000)
INSERT INTO messages(id,chat_jid,sender,timestamp,media_type,file_sha256) SELECT printf('MAINT%05d',x),?,'Alice',?,'document',? FROM n`, other, dbTime(time.Now()), sha256Of(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Store.db.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256) SELECT id,chat_jid,file_sha256 FROM messages WHERE chat_jid=?`, other); err != nil {
				t.Fatal(err)
			}
			operator := mediaOperatorServer(t, b)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			switch mode {
			case "operator":
				code, result := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","chat_jid":"5511999999999@s.whatsapp.net","dry_run":true}`, fakeOperatorToken)
				if code != 200 || result["files"] != float64(1) || result["freed_bytes"] != float64(0) {
					t.Fatal("scoped dry-run changed dedupe accounting", code, result)
				}
			case "retention":
				files, freed, failed := s.Sweep(b.ctx, 24*time.Hour, nil, time.Now())
				if files != 1 || freed != 0 || failed != 0 {
					t.Fatal("scoped retention changed shared-reference accounting", files, freed, failed)
				}
			case "quota":
				b.MediaQuotaBytes = uint64(len(data))
				_, release, err := b.acquireS3MediaQuota(b.ctx, 1)
				release()
				if !errors.Is(err, errMediaQuota) {
					t.Fatal("quota without eligible types did not pause", err)
				}
			}
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s: 50001 references, allocated=%d", mode, allocated)
			if allocated > 8*1024*1024 {
				t.Fatal("maintenance materialized unrelated archive references", allocated)
			}
		})
	}
}

func TestMinIOOperatorPagingPreservesDryRunDedupe(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-paged-dedupe")
	data := []byte("shared across three purge pages")
	row := s3TestRow(t, b, "PAGEDDEDUPE", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	if _, err := b.Store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<520)
INSERT INTO messages(id,chat_jid,sender,timestamp,media_type,file_sha256) SELECT printf('DEDUPE%05d',x),?,'Alice',?,'document',? FROM n`, row.ChatJID, dbTime(row.Timestamp), sha256Of(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.db.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256) SELECT id,chat_jid,file_sha256 FROM messages WHERE chat_jid=? AND id LIKE 'DEDUPE%'`, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	var removals atomic.Int32
	s3SecurityProxy(t, s, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			removals.Add(1)
		}
		return false
	}, nil)
	operator := mediaOperatorServer(t, b)
	// An unknown category is not selected by a type filter, and must still
	// protect its shared object from a dry-run promise of freed bytes.
	if _, err := b.Store.db.Exec(`UPDATE messages SET media_type=NULL WHERE id='DEDUPE00001' AND chat_jid=?`, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	code, unknown := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"document","dry_run":true}`, fakeOperatorToken)
	if code != 200 || unknown["files"] != float64(520) || unknown["freed_bytes"] != float64(0) {
		t.Fatal("unselected NULL category did not protect shared dry-run bytes", code, unknown)
	}
	if _, err := b.Store.db.Exec(`UPDATE messages SET media_type='document' WHERE id='DEDUPE00001' AND chat_jid=?`, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	code, dry := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","dry_run":true}`, fakeOperatorToken)
	if code != 200 || dry["files"] != float64(521) || dry["freed_bytes"] != float64(len(data)) || removals.Load() != 0 {
		t.Fatal("cross-page dry-run changed unique bytes", code, dry, removals.Load())
	}
	code, actual := mediaHTTPRequest(t, operator, "POST", "/operator/v1/media/purge", `{"type":"all","dry_run":false}`, fakeOperatorToken)
	usage, err := s.Usage(b.ctx)
	if code != 200 || actual["files"] != dry["files"] || actual["freed_bytes"] != dry["freed_bytes"] || actual["failed"] != float64(0) || removals.Load() != 1 || err != nil || usage.Bytes != 0 || usage.Files != 0 {
		t.Fatal("paged real purge disagreed with dry-run", code, actual, usage, removals.Load(), err)
	}
}

func TestMinIOOutboundCacheRetriesFailedJoinedDownload(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-outbound-fallback")
	data := []byte("already uploaded outbound bytes")
	row := s3TestRow(t, b, "OUTBOUNDFALLBACK", mediaTestChat, "document", data, time.Now())
	b.MediaAutoDownload = true
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var transfers atomic.Int32
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ string) (int64, error) {
		transfers.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		return 0, errors.New("fake CDN unavailable")
	}
	server := s3ReviewREST(t, b)
	t.Cleanup(func() { once.Do(func() { close(release) }); b.mediaTransfers.wait() })
	finished := make(chan int, 1)
	go func() {
		payload, _ := json.Marshal(DownloadMediaRequest{MessageID: row.ID, ChatJID: row.ChatJID})
		request, _ := http.NewRequest("POST", server.URL+"/api/download", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-bridge-token")
		response, err := server.Client().Do(request)
		if err != nil {
			finished <- 0
			return
		}
		_ = response.Body.Close()
		finished <- response.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("CDN attempt did not start")
	}
	cached := make(chan struct{})
	go func() {
		b.cacheOutboundMedia(b.ctx, sentMessage{ID: row.ID, ChatJID: row.ChatJID, Timestamp: row.Timestamp}, outboundMedia{mediaType: row.MediaType, filename: row.Filename}, data)
		close(cached)
	}()
	key, _ := filepath.Abs(storePath(chatMediaRel(row.ChatJID), mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)))
	deadline := time.After(5 * time.Second)
	for b.mediaTransfers.waiting(key) < 2 {
		select {
		case <-deadline:
			t.Fatal("outbound did not join the existing transfer")
		case <-time.After(time.Millisecond):
		}
	}
	once.Do(func() { close(release) })
	select {
	case code := <-finished:
		if code != 500 {
			t.Fatal("did not exercise failed HTTP download", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not complete")
	}
	select {
	case <-cached:
	case <-time.After(5 * time.Second):
		t.Fatal("outbound fallback did not complete")
	}
	f, _, err := s.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal("successful outbound bytes were discarded after joined CDN failure", err)
	}
	actual, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(actual, data) || transfers.Load() != 1 {
		t.Fatal("fallback changed bytes or fetched CDN again", transfers.Load())
	}
}

func TestMinIODeletionJournalReconciliation(t *testing.T) {
	for _, mode := range []string{"present", "sweep-missing", "republish-missing"} {
		t.Run(mode, func(t *testing.T) {
			b, s := minioTestBridge(t, "instances/test-journal-"+mode)
			data := []byte("journal reconciliation evidence")
			row := s3TestRow(t, b, "JOURNALRECOVERY", mediaTestChat, "document", data, time.Now())
			s3TestWrite(t, b, row, data)
			if _, err := b.Store.db.Exec(`INSERT INTO media_cache_deletions(sha256) VALUES(?)`, sha256Of(data)); err != nil {
				t.Fatal(err)
			}
			if mode == "present" {
				dry := context.WithValue(b.ctx, mediaReadOnlyKey{}, true)
				entry, err := s.Lookup(dry, row)
				var count int
				if err != nil || entry == nil {
					t.Fatal("dry-run lost existing object", entry, err)
				}
				if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM media_cache_deletions`).Scan(&count); err != nil || count != 1 {
					t.Fatal("dry-run mutated deletion journal", count, err)
				}
				entry, err = s.Lookup(b.ctx, row)
				if err != nil || entry == nil {
					t.Fatal("crash before remote DELETE lost references", entry, err)
				}
			} else {
				key, _ := s.key(sha256Of(data))
				if err := s.client.RemoveObject(b.ctx, s.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
					t.Fatal(err)
				}
				if mode == "sweep-missing" {
					files, freed, failed := s.Sweep(b.ctx, 0, nil, time.Now())
					usage, err := s.Usage(b.ctx)
					if files != 0 || freed != int64(len(data)) || failed != 0 || err != nil || usage.Bytes != 0 || usage.Files != 0 {
						t.Fatal("bounded sweep did not recover missing charged object", files, freed, failed, usage, err)
					}
				} else {
					s3TestWrite(t, b, row, data)
				}
			}
			var pending int
			if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM media_cache_deletions`).Scan(&pending); err != nil || pending != 0 {
				t.Fatal("completed recovery retained journal", pending, err)
			}
			if mode != "sweep-missing" {
				f, _, err := s.Open(b.ctx, row, "")
				if err != nil {
					t.Fatal(err)
				}
				actual, _ := io.ReadAll(f)
				_ = f.Close()
				if !bytes.Equal(actual, data) {
					t.Fatal("journal recovery changed readable bytes")
				}
			}
		})
	}
}

func TestMinIOCatalogIdentityBatchBounds(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-batch-bounds")
	data := []byte("batch catalog evidence")
	for _, id := range []string{"BATCHA", "BATCHB"} {
		row := s3TestRow(t, b, id, mediaTestChat, "document", data, time.Now())
		s3TestWrite(t, b, row, data)
	}
	server := s3ReviewREST(t, b)
	base := "/api/media/cache?chat_jid=" + mediaTestChat
	code, body := mediaHTTPRequest(t, server, "GET", base+"&message_id=BATCHA&message_id=BATCHB&message_id=BATCHA", "", "test-bridge-token")
	items, ok := body["items"].([]any)
	if code != 200 || !ok || len(items) != 2 || body["next_cursor"] != "" {
		t.Fatal("bounded duplicate identity batch changed catalog result", code, body)
	}
	for _, suffix := range []string{"&message_id=", strings.Repeat("&message_id=BATCHA", 257)} {
		code, _ := mediaHTTPRequest(t, server, "GET", base+suffix, "", "test-bridge-token")
		if code != 400 {
			t.Fatal("invalid catalog batch accepted", code)
		}
	}
}

func TestMinIOCachedAudioStickerRepliesKeepWireContext(t *testing.T) {
	for _, kind := range []string{"audio", "sticker"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := minioTestBridge(t, "instances/test-cached-reply-"+kind)
			data := []byte("ID3fake audio")
			if kind == "sticker" {
				data = []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")
			}
			row := s3TestRow(t, b, "CACHEDREPLY", outboundReplyGroup.String(), kind, data, time.Now())
			s3TestWrite(t, b, row, data)
			if err := b.Store.StoreMessage(storedMessage{ID: "REPLYTARGET", ChatJID: row.ChatJID, Sender: mediaTestChat, Timestamp: time.Now(), Content: "stored reply preview"}); err != nil {
				t.Fatal(err)
			}
			b.Client = newTestClient(&mockLIDStore{})
			own := types.NewJID("5511999999999", types.DefaultUserServer)
			b.Client.Store.ID = &own
			b.Send, b.MediaAutoDownload = b.sendBackend(), false
			b.uploadMedia = func(_ context.Context, actual []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				if !bytes.Equal(actual, data) {
					t.Error("reply uploaded different bytes")
				}
				upload := outboundUpload()
				upload.FileSHA256, upload.FileLength = sha256Of(data), uint64(len(data))
				return upload, nil
			}
			wire := make(chan *waE2E.Message, 1)
			b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
				wire <- message
				return whatsmeow.SendResponse{ID: "CACHEDREPLYSENT", Timestamp: time.Now()}, nil
			}
			server := s3ReviewREST(t, b)
			payload, _ := json.Marshal(SendMessageRequest{Recipient: row.ChatJID, MediaPath: mediaBlobURI(row.ChatJID, row.ID), QuotedMessageID: "REPLYTARGET"})
			code, result := mediaHTTPRequest(t, server, "POST", "/api/send", string(payload), "test-bridge-token")
			if code != 200 || result["success"] != true {
				t.Fatal("cached reply send failed", code, result)
			}
			message := <-wire
			contextInfo := sharedContextInfo(message)
			if contextInfo == nil || contextInfo.GetStanzaID() != "REPLYTARGET" || contextInfo.GetParticipant() != mediaTestChat || contextInfo.GetQuotedMessage().GetConversation() != "stored reply preview" {
				t.Fatal("recipient lost reply context", contextInfo)
			}
			if kind == "audio" && message.GetAudioMessage() == nil || kind == "sticker" && message.GetStickerMessage() == nil {
				t.Fatal("cached reply changed media category")
			}
			var stored string
			if err := b.Store.db.QueryRow(`SELECT quoted_message_id FROM messages WHERE id='CACHEDREPLYSENT' AND chat_jid=?`, row.ChatJID).Scan(&stored); err != nil || stored != contextInfo.GetStanzaID() {
				t.Fatal("archive and wire disagree", stored, err)
			}
		})
	}
}

func s3RecoveryMCP(t *testing.T, b *Bridge, endpoint, program string) {
	t.Helper()
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real MCP proof requires WAMCP_TEST_MCP_PYTHON")
	}
	cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Trusted interpreter and literal fake-fixture program.
	cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
	var seq int
	var name, dbFile string
	if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbFile); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+endpoint+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real MCP/HTTP/MinIO proof: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestMinIOAudioCoverageTracksRemoteCacheAndPurge(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-audio-coverage")
	data := []byte("cached voice-note evidence")
	row := s3TestRow(t, b, "COVERAGEAUDIO", mediaTestChat, "audio", data, time.Now())
	s3TestWrite(t, b, row, data)
	server := s3ReviewREST(t, b)
	s3RecoveryMCP(t, b, server.URL, `import asyncio
import main,whatsapp
chat='5511999999999@s.whatsapp.net'
async def run():
    listed=await main.mcp.call_tool('list_media',{'chat_jid':chat})
    assert not listed.is_error and listed.structured_content['items'][0]['cached']
    result=await main.mcp.call_tool('coverage',{'chat_jid':chat})
    assert not result.is_error,result.content
    audio=result.structured_content['audio']
    assert audio['cached']==1 and audio['backlog_cached']==1,audio
    whatsapp._bridge_json(whatsapp._bridge_request('POST','/media/purge',json={'items':[{'chat_jid':chat,'message_id':'COVERAGEAUDIO'}],'dry_run':False}))
    result=await main.mcp.call_tool('coverage',{'chat_jid':chat})
    audio=result.structured_content['audio']
    assert audio['cached']==0 and audio['backlog_cached']==0 and audio['backlog']==1,audio
asyncio.run(run())
print('remote audio coverage matched list_media and tracked actual purge')`)
}

func TestMinIOMCPPaginationUsesBoundedIdentityBatches(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-page-batch")
	data := []byte("shared pagination bytes")
	row := s3TestRow(t, b, "PAGETARGET", mediaTestChat, "document", data, time.Now())
	s3TestWrite(t, b, row, data)
	if _, err := b.Store.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000)
INSERT INTO messages(id,chat_jid,sender,timestamp,media_type,file_sha256) SELECT printf('PAGE%05d',x),?,'Alice',?,'document',? FROM n`, row.ChatJID, dbTime(row.Timestamp), sha256Of(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.db.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256) SELECT id,chat_jid,file_sha256 FROM messages WHERE chat_jid=? AND id LIKE 'PAGE%' AND id!='PAGETARGET'`, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	var requests, unbounded atomic.Int32
	server := s3ReviewRESTWrapped(t, b, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/media/cache" {
				requests.Add(1)
				ids := r.URL.Query()["message_id"]
				if len(ids) == 0 || len(ids) > 2 {
					unbounded.Add(1)
				}
			}
			handler.ServeHTTP(w, r)
		})
	})
	s3RecoveryMCP(t, b, server.URL, `import asyncio
import main
async def run():
    args={'chat_jid':'5511999999999@s.whatsapp.net','limit':1}
    first=await main.mcp.call_tool('list_media',args)
    assert not first.is_error,first.content
    page=first.structured_content
    assert len(page['items'])==1 and page['items'][0]['cached'] and page['next_cursor'],page
    second=await main.mcp.call_tool('list_media',{**args,'cursor':page['next_cursor']})
    assert not second.is_error,second.content
    assert second.structured_content['items'][0]['message_id']!=page['items'][0]['message_id']
    assert second.structured_content['items'][0]['cached']
asyncio.run(run())
print('two actual MCP one-row pages returned distinct cached rows')`)
	if requests.Load() != 2 || unbounded.Load() != 0 {
		t.Fatal("one-row pages enumerated the whole chat", requests.Load(), unbounded.Load())
	}
}
