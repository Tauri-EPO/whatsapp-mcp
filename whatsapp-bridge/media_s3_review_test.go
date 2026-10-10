package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func s3ReviewREST(t *testing.T, b *Bridge) *httptest.Server {
	return s3ReviewRESTWrapped(t, b, nil)
}

func s3ReviewRESTWrapped(t *testing.T, b *Bridge, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	b.Connected = func() bool { return true }
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = b.newRESTMux(server.Listener.Addr().(*net.TCPAddr).Port, "test-bridge-token")
	if wrap != nil {
		server.Config.Handler = wrap(server.Config.Handler)
	}
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func TestMinIOLargeImageMCPReadAndResource(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-large-image")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real Python MCP proof requires WAMCP_TEST_MCP_PYTHON")
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	// Padding after the JPEG end marker is valid; the plaintext is exactly
	// 3 MiB, above the binary block ceiling but below the image ceiling.
	data := append(encoded.Bytes(), make([]byte, 3*1024*1024-encoded.Len())...)
	row := s3TestRow(t, b, "BIGJPEG", mediaTestChat, "image", data, time.Now())
	row.Filename = "photo.jpg"
	if _, err := b.Store.db.Exec(`UPDATE messages SET filename=? WHERE id=? AND chat_jid=?`, row.Filename, row.ID, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	s3TestWrite(t, b, row, data)
	var lookups atomic.Int32
	server := s3ReviewRESTWrapped(t, b, func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/media/cache" && r.URL.Query().Get("message_id") == "BIGJPEG" {
				lookups.Add(1)
			}
			handler.ServeHTTP(w, r)
		})
	})
	program := `import asyncio, hashlib, json, os
import main, media_remote
chat='5511999999999@s.whatsapp.net'
async def run():
    errors=[]
    try:
        result=await main.mcp.call_tool('read_media', {'chat_jid':chat,'message_id':'BIGJPEG'})
        assert not result.is_error, result.content[0].text
        assert result.content[0].type=='image' and result.content[0].mime_type=='image/jpeg'
        assert json.loads(result.content[-1].text)['original_bytes']==3145728
        listed=await main.mcp.call_tool('list_media', {'chat_jid':chat})
        assert not listed.is_error
        assert next(item for item in listed.structured_content['items'] if item['message_id']=='BIGJPEG').get('resource_link')
    except Exception as exc: errors.append('tool: '+str(exc))
    try:
        resources=await main.mcp.read_resource(media_remote.uri(chat,'BIGJPEG'))
        assert resources[0].mime_type=='image/jpeg'
        assert len(resources[0].content)==3145728
        assert hashlib.sha256(resources[0].content).hexdigest()==os.environ['EXPECTED_SHA']
    except Exception as exc: errors.append('resource: '+str(exc))
    assert not errors, '; '.join(errors)
asyncio.run(run())
print('3 MiB JPEG: MCP image block and exact resource bytes through HTTP/MinIO passed')`
	cmd := exec.CommandContext(b.ctx, python, "-c", program) //nolint:gosec // Trusted interpreter and literal fake-fixture program.
	cmd.Dir = filepath.Join("..", "whatsapp-mcp-server")
	var seq int
	var dbName, dbFile string
	if err := b.Store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "WHATSAPP_MEDIA_BACKEND=s3", "WHATSAPP_API_URL="+server.URL+"/api", "WHATSAPP_BRIDGE_TOKEN=test-bridge-token", "WHATSAPP_DB_PATH="+dbFile, "WHATSAPP_WRAP_UNTRUSTED=false", "EXPECTED_SHA="+hex.EncodeToString(sha256Of(data)))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("large image MCP proof: %v\n%s", err, output)
	}
	t.Log(string(output))
	if lookups.Load() != 3 {
		t.Fatal("tool read, one-row list and resource must each issue one identity lookup", lookups.Load())
	}
}

func TestMinIOCachedXLSXMCPTextExtraction(t *testing.T) {
	b, _ := minioTestBridge(t, "instances/test-xlsx-reader")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real Python MCP proof requires WAMCP_TEST_MCP_PYTHON")
	}
	fixture := filepath.Join(t.TempDir(), "fixture.xlsx")
	//nolint:gosec // Trusted test interpreter and literal public fake fixture.
	create := exec.CommandContext(b.ctx, python, "-c", `import sys
from openpyxl import Workbook
workbook=Workbook();workbook.active['A1']='spreadsheet evidence';workbook.save(sys.argv[1]);workbook.close()`, fixture)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	data, err := os.ReadFile(fixture) //nolint:gosec // Test-owned scratch workbook.
	if err != nil {
		t.Fatal(err)
	}
	row := s3TestRow(t, b, "XLSXREAD", mediaTestChat, "document", data, time.Now())
	if _, err := b.Store.db.Exec(`UPDATE messages SET filename='fixture.xlsx' WHERE id=? AND chat_jid=?`, row.ID, row.ChatJID); err != nil {
		t.Fatal(err)
	}
	row.Filename = "fixture.xlsx"
	s3TestWrite(t, b, row, data)
	server := s3ReviewREST(t, b)
	program := `import asyncio,json
import main
async def run():
    result=await main.mcp.call_tool('read_media',{'chat_jid':'5511999999999@s.whatsapp.net','message_id':'XLSXREAD','as_text':True})
    assert not result.is_error, result.content[0].text
    assert 'spreadsheet evidence' in json.dumps([block.text for block in result.content])
asyncio.run(run())
print('XLSX text extracted through actual MCP/HTTP/MinIO')`
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
		t.Fatalf("XLSX MCP: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestMinIOIngestSkipsOneUnavailableObject(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-ingest-remote-error")
	python := os.Getenv("WAMCP_TEST_MCP_PYTHON")
	if python == "" {
		t.Skip("real Python ingest proof requires WAMCP_TEST_MCP_PYTHON")
	}
	bad := []byte("temporarily unavailable audio")
	good := []byte("healthy cached audio")
	for _, tc := range []struct {
		id   string
		data []byte
		at   time.Time
	}{{"BADREMOTE", bad, time.Now().Add(time.Second)}, {"GOODREMOTE", good, time.Now()}} {
		row := s3TestRow(t, b, tc.id, mediaTestChat, "audio", tc.data, tc.at)
		s3TestWrite(t, b, row, tc.data)
	}
	key, _ := s.key(sha256Of(bad))
	s3SecurityProxy(t, s, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, key) {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>wamcp-test-secret-sentinel</Message></Error>`)
			return true
		}
		return false
	}, nil)
	server := s3ReviewREST(t, b)
	program := `import os
import transcribe_worker
seen=[]
def sink(path):
    with open(path,'rb') as source: data=source.read()
    assert data==b'healthy cached audio'
    seen.append(path)
    return {'text':'synthetic transcript','language':'en','backend':'test'}
result=transcribe_worker.run_once(2,transcribe=sink,fetch=False)
assert result.transcribed==1 and result.failed==0 and not result.outage,result
assert len(seen)==1 and not os.path.exists(seen[0])
print('one remote object failure did not starve healthy ingest; private spool removed')`
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
		t.Fatalf("remote ingest: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestMinIOOutboundCacheJoinsConcurrentDownload(t *testing.T) {
	b, s := minioTestBridge(t, "instances/test-outbound-flight")
	data := []byte("shared outbound and download bytes")
	row := s3TestRow(t, b, "SHAREDSEND", mediaTestChat, "document", data, time.Now())
	b.MediaAutoDownload = true
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaFile(b.StoreRoot, rel, func(f *os.File) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := f.Write(data)
			return err
		})
	}
	server := s3ReviewREST(t, b)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); b.mediaTransfers.wait() })
	download := make(chan *http.Response, 1)
	downloadErr := make(chan error, 1)
	go func() {
		payload, _ := json.Marshal(DownloadMediaRequest{MessageID: row.ID, ChatJID: row.ChatJID})
		request, _ := http.NewRequest("POST", server.URL+"/api/download", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-bridge-token")
		response, err := server.Client().Do(request)
		if err != nil {
			downloadErr <- err
			return
		}
		download <- response
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not enter the real part writer")
	}
	cached := make(chan struct{})
	go func() {
		b.cacheOutboundMedia(b.ctx, sentMessage{ID: row.ID, ChatJID: row.ChatJID, Timestamp: row.Timestamp}, outboundMedia{mediaType: row.MediaType, filename: row.Filename}, data)
		close(cached)
	}()
	key, err := filepath.Abs(storePath(chatMediaRel(row.ChatJID), mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	waiting := true
	for waiting && b.mediaTransfers.waiting(key) < 2 {
		select {
		case <-cached:
			waiting = false
		case <-deadline:
			t.Fatal("outbound writer neither joined nor finished")
		case <-time.After(time.Millisecond):
		}
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case response := <-download:
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("concurrent download failed: status=%d body=%s", response.StatusCode, payload)
		}
	case err := <-downloadErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("download did not finish")
	}
	select {
	case <-cached:
	case <-time.After(5 * time.Second):
		t.Fatal("outbound caching did not finish")
	}
	f, _, err := s.Open(b.ctx, row, "")
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := io.ReadAll(f)
	_ = f.Close()
	usage, err := s.Usage(b.ctx)
	if err != nil || !bytes.Equal(actual, data) || usage.Files != 1 || usage.Bytes != int64(len(data)) {
		t.Fatal("shared publication lost bytes or charged twice", usage, err)
	}
}

func TestMinIOCachedSendPreservesPresentation(t *testing.T) {
	for _, kind := range []string{"image", "document"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := minioTestBridge(t, "instances/test-send-presentation-"+kind)
			data := []byte("%PDF-1.4\nfixture")
			mime := "application/pdf"
			if kind == "image" {
				var encoded bytes.Buffer
				if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
					t.Fatal(err)
				}
				data, mime = encoded.Bytes(), "image/png"
			}
			row := s3TestRow(t, b, "PRESENTATION", "120363000000000001@g.us", kind, data, time.Now())
			name, title := "report.pdf", "Report"
			row.Filename = name
			if kind == "image" {
				row.Filename = "photo.jpg"
			}
			presentation := &mediaPresentation{Hash: hex.EncodeToString(sha256Of(data)), MIME: mime}
			if kind == "document" {
				presentation.Name, presentation.Title = &name, &title
			}
			if _, err := b.Store.db.Exec(`UPDATE messages SET filename=?, media_presentation=? WHERE id=? AND chat_jid=?`, row.Filename, presentation.column(), row.ID, row.ChatJID); err != nil {
				t.Fatal(err)
			}
			s3TestWrite(t, b, row, data)
			server := s3ReviewREST(t, b)
			b.Client = newTestClient(&mockLIDStore{})
			own := types.NewJID("5511999999999", types.DefaultUserServer)
			b.Client.Store.ID = &own
			b.Send, b.MediaAutoDownload = b.sendBackend(), false
			b.uploadMedia = func(_ context.Context, actual []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				if !bytes.Equal(actual, data) {
					t.Error("cached send uploaded different bytes")
				}
				upload := outboundUpload()
				upload.FileSHA256, upload.FileLength = sha256Of(data), uint64(len(data))
				return upload, nil
			}
			wire := make(chan *waE2E.Message, 1)
			b.sendMessage = func(_ context.Context, _ types.JID, message *waE2E.Message) (whatsmeow.SendResponse, error) {
				wire <- message
				return whatsmeow.SendResponse{ID: "PRESENTATIONSENT", Timestamp: time.Now()}, nil
			}
			code, result := mediaHTTPRequest(t, server, "POST", "/api/send", `{"recipient":"120363000000000001@g.us","media_path":"`+mediaBlobURI(row.ChatJID, row.ID)+`","message":"caption"}`, "test-bridge-token")
			if code != 200 || result["success"] != true {
				t.Fatal("cached HTTP send failed", code, result)
			}
			message := <-wire
			if kind == "image" {
				if got := message.GetImageMessage().GetMimetype(); got != mime {
					t.Fatalf("recipient MIME=%q want=%q", got, mime)
				}
			} else if doc := message.GetDocumentMessage(); doc.GetMimetype() != mime || doc.GetFileName() != name || doc.GetTitle() != title {
				t.Fatalf("recipient document MIME=%q name=%q title=%q", doc.GetMimetype(), doc.GetFileName(), doc.GetTitle())
			}
			var storedName string
			if err := b.Store.db.QueryRow(`SELECT filename FROM messages WHERE id=? AND chat_jid=?`, "PRESENTATIONSENT", row.ChatJID).Scan(&storedName); err != nil {
				t.Fatal(err)
			}
			if storedName != row.Filename {
				t.Fatalf("archive filename=%q want source filename=%q", storedName, row.Filename)
			}
		})
	}
}
