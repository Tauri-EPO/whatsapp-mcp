package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func mediaOperatorServer(t *testing.T, b *Bridge) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{mediaUsage: b.handleOperatorMediaUsage, mediaPurge: b.handleOperatorMediaPurge, settings: b.handleRuntimeSettings}, b.Log))
	t.Cleanup(server.Close)
	return server
}
func mediaHTTPRequest(t *testing.T, server *httptest.Server, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		t.Fatalf("invalid response %d: %s", resp.StatusCode, data)
	}
	return resp.StatusCode, result
}
func seedOperatorMedia(t *testing.T, b *Bridge, id, chat, kind string, age time.Duration, size int) string {
	t.Helper()
	if size < 0 {
		t.Fatal("negative fixture size")
		return ""
	}
	stamp := time.Now().Add(-age).UTC().Truncate(time.Second)
	if err := b.Store.StoreChat(chat, "Alice", stamp); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreMessage(storedMessage{ID: id, ChatJID: chat, Sender: purgeChat, Timestamp: stamp, MediaType: kind, URL: "https://example.invalid/media", MediaKey: []byte("key"), FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc"), FileLength: uint64(size)}); err != nil {
		t.Fatal(err)
	}
	name := mediaFileName(kind, stamp, id, "")
	rel := chatMediaRel(chat)
	if err := b.StoreRoot.MkdirAll(rel, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaPathComponents(rel, name); err != nil {
		return ""
	}
	if _, err := writeMediaFile(b.StoreRoot, rel+"/"+name, func(file *os.File) error { _, err := file.Write(make([]byte, size)); return err }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(storeDir(), rel, name)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorMediaHTTPUsagePurgeAndDenyMatrix(t *testing.T) {
	b := newSettingsBridge(t)
	server := mediaOperatorServer(t, b)
	video := seedOperatorMedia(t, b, "VIDEO", purgeChat, "video", 100*24*time.Hour, 30)
	image := seedOperatorMedia(t, b, "IMAGE", purgeGroup, "image", time.Hour, 10)
	status := seedOperatorMedia(t, b, "STATUS", "status@broadcast", "video", 100*24*time.Hour, 5)
	b.Policy = parseChatPolicy(purgeGroup)
	b.ReadOnly.enabled = true // Operator has archive authority regardless of data-plane restrictions.
	code, usage := mediaHTTPRequest(t, server, "GET", "/operator/v1/media/usage?limit=1", "", fakeOperatorToken)
	if code != 200 || usage["bytes"] != float64(45) {
		t.Fatalf("usage %d %+v", code, usage)
	}
	types := usage["by_type"].(map[string]any)
	var sum float64
	for _, value := range types {
		sum += value.(float64)
	}
	chats := usage["by_chat"].([]any)
	if sum != 45 || types["video"] != float64(30) || types["status"] != float64(5) || len(chats) != 1 || chats[0].(map[string]any)["chat_jid"] != purgeChat {
		t.Fatal(usage)
	}
	for _, path := range []string{"/operator/v1/media/usage", "/operator/v1/media/purge"} {
		method := "GET"
		if strings.HasSuffix(path, "purge") {
			method = "POST"
		}
		for _, token := range []string{"", purgeToken} {
			if code, _ := mediaHTTPRequest(t, server, method, path, `{"type":"all","dry_run":false}`, token); code != 401 {
				t.Fatalf("wrong token %d", code)
			}
		}
		for _, tc := range []struct{ host, origin string }{{"evil.example.test", ""}, {"", "https://evil.example.test"}} {
			req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(`{"type":"all","dry_run":false}`))
			req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
			req.Header.Set("Origin", tc.origin)
			if tc.host != "" {
				req.Host = tc.host
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatal(resp.StatusCode)
			}
		}
	}
	for _, bad := range []string{`{}`, `{"type":"unknown"}`, `{"type":"video","older_than_days":-1}`, `{"type":"all","chat_jid":"../x@g.us"}`, `{"type":"all","include_orphans":true}`} {
		if code, _ := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", bad, fakeOperatorToken); code != 400 {
			t.Fatalf("bad request %d %s", code, bad)
		}
	}
	_, preview := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"video","older_than_days":90}`, fakeOperatorToken)
	if preview["freed_bytes"] != float64(30) || !fileExists(video) {
		t.Fatal(preview)
	}
	_, real := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"video","older_than_days":90,"dry_run":false}`, fakeOperatorToken)
	if real["freed_bytes"] != preview["freed_bytes"] || fileExists(video) || !fileExists(image) || !fileExists(status) {
		t.Fatal(real)
	}
	_, all := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"all","dry_run":false}`, fakeOperatorToken)
	if all["files"] != float64(2) || fileExists(image) || fileExists(status) {
		t.Fatal(all)
	}
	var rows int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}

func TestStatusPurgePagesOrphansPolicyCLIAndOnce(t *testing.T) {
	b := newSettingsBridge(t)
	b.Policy = parseChatPolicy(purgeChat)
	b.RESTAllowedHosts = "127.0.0.1"
	server := httptest.NewServer(b.newRESTMux(8080, purgeToken))
	t.Cleanup(server.Close)
	conversation := seedOperatorMedia(t, b, "KEEP", purgeChat, "image", 48*time.Hour, 7)
	for i := range 620 {
		seedOperatorMedia(t, b, fmt.Sprintf("STATUS%04d", i), "status@broadcast", "image", 48*time.Hour, 1)
	}
	// A large uncached tail passes the test's row ceiling, but status scope
	// internally pages past it instead of returning a misleading empty batch.
	stamp := time.Now().UTC().Truncate(time.Second)
	for i := range 720 {
		if err := b.Store.StoreMessage(storedMessage{ID: fmt.Sprintf("UNCACHED%04d", i), ChatJID: "status@broadcast", Sender: purgeChat, Timestamp: stamp, MediaType: "image"}); err != nil {
			t.Fatal(err)
		}
	}
	b.PurgeScanLimit = 256
	orphan := "image_20260101_000000_ORPHAN.jpg"
	if _, err := writeMediaFile(b.StoreRoot, "status@broadcast/"+orphan, func(file *os.File) error { _, err := file.Write([]byte("abc")); return err }); err != nil {
		t.Fatal(err)
	}
	code, preview := mediaHTTPRequest(t, server, "POST", "/api/media/purge", `{"scope":"status","dry_run":true}`, purgeToken)
	if code != 200 || preview["purged_files"] != float64(620) || preview["orphan_files"] != float64(1) || preview["orphan_bytes"] != float64(3) {
		t.Fatalf("%d %+v", code, preview)
	}
	var out bytes.Buffer
	if exit := purgeStatusCLI([]string{"--dry-run"}, &out, io.Discard); exit != 0 {
		t.Fatalf("CLI exit %d", exit)
	}
	var cli operatorMediaResult
	if err := json.Unmarshal(out.Bytes(), &cli); err != nil || cli.Files != 620 || cli.FreedBytes != 620 || cli.OrphanFiles != 1 {
		t.Fatalf("CLI %+v err %v", cli, err)
	}
	_, real := mediaHTTPRequest(t, server, "POST", "/api/media/purge", `{"scope":"status","dry_run":false}`, purgeToken)
	if real["purged_bytes"] != preview["purged_bytes"] || !fileExists(conversation) || !fileExists(filepath.Join(storeDir(), "status@broadcast", orphan)) {
		t.Fatal(real)
	}
	_, all := mediaHTTPRequest(t, server, "POST", "/api/media/purge", `{"scope":"status","dry_run":false,"include_orphans":true}`, purgeToken)
	if all["purged_bytes"] != float64(3) || fileExists(filepath.Join(storeDir(), "status@broadcast", orphan)) {
		t.Fatal(all)
	}
	seedOperatorMedia(t, b, "ONCE", "status@broadcast", "audio", time.Hour, 9)
	if err := b.purgeStatusOnStart(); err != nil {
		t.Fatal(err)
	}
	keep := seedOperatorMedia(t, b, "AFTER", "status@broadcast", "audio", time.Hour, 9)
	if err := b.purgeStatusOnStart(); err != nil || !fileExists(keep) {
		t.Fatal("one-shot purged again", err)
	}
	// Runtime deny and read-only still refuse the data-plane shortcut/CLI.
	operator := settingsServer(t, b)
	settingsRequest(t, operator, "PATCH", `{"tools.deny":["purge_media"]}`)
	if code, _ := mediaHTTPRequest(t, server, "POST", "/api/media/purge", `{"scope":"status","dry_run":false}`, purgeToken); code != 403 || !fileExists(keep) {
		t.Fatal("tool deny bypassed")
	}
	if exit := purgeStatusCLI([]string{}, io.Discard, io.Discard); exit != 1 {
		t.Fatal("CLI tool deny bypassed")
	}
	settingsRequest(t, operator, "PATCH", `{"tools.deny":null}`)
	b.ReadOnly.enabled = true
	readOnlyServer := httptest.NewServer(b.newRESTMux(8080, purgeToken))
	defer readOnlyServer.Close()
	if code, _ := mediaHTTPRequest(t, readOnlyServer, "POST", "/api/media/purge", `{"scope":"status","dry_run":false}`, purgeToken); code != 403 {
		t.Fatal("read-only bypassed")
	}
}

func TestStatusRuntimeArrivalsStoreRowsAndActualBytes(t *testing.T) {
	b := newSettingsBridge(t)
	b.MediaAutoDownload = true
	server := settingsServer(t, b)
	b.autoDownloads = newMediaJobQueue(b.ctx, 0, 20, b.runAutoDownload)
	writes := make(chan string, 3)
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		n, err := writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, file whatsmeow.File) error { _, err := file.Write([]byte("bytes")); return err })
		if err == nil {
			writes <- rel
		}
		return n, err
	}
	arrive := func(kind, id string) {
		msg := buildImageMessage(types.StatusBroadcastJID, phonePN, false, "")
		msg.Info.ID = id
		url := proto.String("https://example.invalid/media")
		length := proto.Uint64(5)
		switch kind {
		case "image":
			msg.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: url, MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: length}}
		case "video":
			msg.Message = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: url, MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: length}}
		case "audio":
			msg.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{URL: url, MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: length}}
		}
		b.handleMessage(msg)
	}
	for _, kind := range []string{"image", "video", "audio"} {
		arrive(kind, "DEFAULT"+kind)
	}
	if b.autoDownloads.queued() != 0 {
		t.Fatal("default cached status")
	}
	_, media, files := storeUsage(b.StoreRoot)
	if media != 0 || files != 0 {
		t.Fatal("default wrote bytes")
	}
	settingsRequest(t, server, "PATCH", `{"media.autodownload_status":true}`)
	b.autoDownloads = newMediaJobQueue(b.ctx, 1, 20, b.runAutoDownload)
	for _, kind := range []string{"image", "video", "audio"} {
		arrive(kind, "ENABLED"+kind)
	}
	for range 3 {
		select {
		case rel := <-writes:
			found, err := findCachedMedia(b.StoreRoot, "status@broadcast", []string{filepath.Base(rel)})
			if err != nil || found == nil || found.info.Size() != 5 {
				t.Fatal("arrival bytes missing", err)
			}
			found.Close()
		case <-time.After(5 * time.Second):
			t.Fatal("arrival cache did not finish")
		}
	}
	var rows int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid='status@broadcast'").Scan(&rows); err != nil || rows != 6 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}

func TestMediaRuntimeDeployCeilingsAndInvalidValues(t *testing.T) {
	b := newSettingsBridge(t)
	var err error
	b.RuntimeDefaults, err = runtimeDefaults(func(key string) string {
		return map[string]string{mediaQuotaEnv: "100", mediaEvictTypesEnv: "video", mediaEvictTargetEnv: "80"}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	server := settingsServer(t, b)
	for _, quota := range []string{"200", "0"} {
		code, snapshot := settingsRequest(t, server, "PATCH", `{"media.quota_bytes":`+quota+`,"media.quota_evict_types":["video","image"],"media.quota_evict_target_percent":95}`)
		if code != 200 || snapshot.Settings["media.quota_bytes"].Value != float64(100) || fmt.Sprint(snapshot.Settings["media.quota_evict_types"].Value) != "[video]" || snapshot.Settings["media.quota_evict_target_percent"].Value != float64(80) {
			t.Fatalf("ceiling %+v", snapshot)
		}
	}
	for _, bad := range []string{`{"media.autodownload_status":1}`, `{"media.quota_bytes":-1}`, `{"media.quota_bytes":1.1}`, `{"media.quota_evict_types":["video,audio"]}`, `{"media.quota_evict_types":[""]}`, `{"media.quota_evict_target_percent":100}`} {
		if code, _ := settingsRequest(t, server, "PATCH", bad); code != 400 {
			t.Fatalf("invalid %s %d", bad, code)
		}
	}
}

func TestOperatorMediaRefusesSymlinksAndCraftedRows(t *testing.T) {
	b := newSettingsBridge(t)
	server := mediaOperatorServer(t, b)
	victim := seedOperatorMedia(t, b, "SAFE", purgeChat, "image", time.Hour, 19)
	seedOperatorMedia(t, b, "../SAFE", purgeGroup, "image", time.Hour, 5)
	linkChat := "status@broadcast"
	collision, err := b.Store.MediaRow("SAFE", purgeChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreChat(linkChat, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreMessage(storedMessage{ID: "SAFE", ChatJID: linkChat, Sender: purgeChat, Timestamp: collision.Timestamp, MediaType: "image"}); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, chatMediaRel(purgeChat), filepath.Join(storeDir(), linkChat))
	code, result := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"status","dry_run":false,"include_orphans":true}`, fakeOperatorToken)
	if code != 200 || result["files"] != float64(0) || !fileExists(victim) {
		t.Fatal(result)
	}
	_, result = mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"all","chat_jid":"`+purgeGroup+`","dry_run":false}`, fakeOperatorToken)
	if result["failed"] != float64(1) || !fileExists(victim) {
		t.Fatal(result)
	}
	// Link under a generated cache name is ignored for usage and removal.
	symlinkOrSkip(t, victim, filepath.Join(storeDir(), chatMediaRel(purgeGroup), "image_20260101_000000_LINK.jpg"))
	_, usage := mediaHTTPRequest(t, server, "GET", "/operator/v1/media/usage", "", fakeOperatorToken)
	if usage["bytes"] != float64(19) {
		t.Fatal(usage)
	}
}

func TestStatusRuntimeToggleConsumerRestartAndCeiling(t *testing.T) {
	for _, env := range []string{"", "false", "true"} {
		t.Run("env="+env, func(t *testing.T) {
			b := newSettingsBridge(t)
			b.MediaAutoDownload = true
			var err error
			b.RuntimeDefaults, err = runtimeDefaults(func(key string) string {
				if key == mediaAutoDownloadStatusEnv {
					return env
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			server := settingsServer(t, b)
			stamp := time.Now().UTC().Truncate(time.Second)
			cache := func(id string) {
				b.cacheOutboundMedia(t.Context(), sentMessage{ID: id, ChatJID: "status@broadcast", Timestamp: stamp}, outboundMedia{mediaType: "image"}, []byte("actual cache bytes"))
			}
			cache("BEFORE")

			code, _ := settingsRequest(t, server, "PATCH", `{"media.autodownload_status":true}`)
			if code != 200 {
				t.Fatal(code)
			}
			cache("AFTER")
			found, err := findCachedMedia(b.StoreRoot, "status@broadcast", []string{mediaFileName("image", stamp, "AFTER", "")})
			if (found != nil) != (env != "false") || err != nil {
				t.Fatalf("cache=%v err=%v", found != nil, err)
			}
			if found != nil {
				found.Close()
			}
			// Reopening the real archive sees the persisted setting.
			reopened, err := NewMessageStore()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			snapshot, err := readRuntimeSettings(t.Context(), reopened.db, b.RuntimeDefaults, b.warnSavedSetting)
			if err != nil || snapshot.Settings[statusMediaSetting].Value != (env != "false") {
				t.Fatalf("restart %+v err=%v", snapshot, err)
			}
			settingsRequest(t, server, "PATCH", `{"media.autodownload_status":false}`)
			cache("OFF")
			if found, _ := findCachedMedia(b.StoreRoot, "status@broadcast", []string{mediaFileName("image", stamp, "OFF", "")}); found != nil {
				found.Close()
				t.Fatal("disabled cache wrote bytes")
			}
			if !b.skipsStatusMedia(types.StatusBroadcastJID) {
				t.Fatal("incoming status consumer ignored runtime disable")
			}
		})
	}
}

func TestStatusRetentionAndMetricsScope(t *testing.T) {
	b := newSettingsBridge(t)
	conversation := seedOperatorMedia(t, b, "CHAT", purgeChat, "image", 48*time.Hour, 17)
	status := seedOperatorMedia(t, b, "STATUS", "status@broadcast", "image", 48*time.Hour, 11)
	metrics := b.renderMetrics()
	if !strings.Contains(metrics, `whatsapp_bridge_media_cached_bytes{scope="status"} 11`) || !strings.Contains(metrics, `whatsapp_bridge_media_cached_bytes{scope="chats"} 17`) || !strings.Contains(metrics, "whatsapp_bridge_media_bytes 28") {
		t.Fatal(metrics)
	}
	if b.healthStatus()["media_status_share"] != float64(11)/28 {
		t.Fatal(b.healthStatus())
	}
	day := 24 * time.Hour
	removed, freed, failed := sweepMediaWithStatus(b.StoreRoot, 0, &day, time.Now())
	if removed != 1 || freed != 11 || failed != 0 || !fileExists(conversation) || fileExists(status) {
		t.Fatalf("sweep %d %d %d", removed, freed, failed)
	}
}

func TestLocalQuotaOldestRuntimeTypesAndPause(t *testing.T) {
	b := newSettingsBridge(t)
	server := settingsServer(t, b)
	b.MediaAutoDownload = true
	old := seedOperatorMedia(t, b, "OLD", purgeChat, "video", 96*time.Hour, 30)
	newer := seedOperatorMedia(t, b, "NEW", purgeChat, "video", 48*time.Hour, 30)
	image := seedOperatorMedia(t, b, "IMAGE", purgeGroup, "image", 24*time.Hour, 40)
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":100}`)
	if _, release, err := b.acquireMediaQuota(t.Context(), 1); !errors.Is(err, errMediaQuota) {
		release()
		t.Fatal("off eviction must pause", err)
	}
	settingsRequest(t, server, "PATCH", `{"media.quota_evict_types":["video"],"media.quota_evict_target_percent":60}`)
	ctx, release, err := b.acquireMediaQuota(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if fileExists(old) || fileExists(newer) || !fileExists(image) || mediaLimit(ctx) != 1 {
		t.Fatal("wrong eviction set or low water")
	}
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":30}`)
	if _, release, err := b.acquireMediaQuota(t.Context(), 1); !errors.Is(err, errMediaQuota) {
		release()
		t.Fatal("unlisted files must pause", err)
	}
	if !fileExists(image) || !strings.Contains(b.renderMetrics(), `whatsapp_bridge_media_evicted_bytes_total{type="video"} 60`) {
		t.Fatal("unlisted file lost or eviction metric wrong")
	}
}

func TestLocalQuotaPausesWhenEligibleFilesCannotReachTarget(t *testing.T) {
	b := newSettingsBridge(t)
	server := mediaOperatorServer(t, b)
	video := seedOperatorMedia(t, b, "ELIGIBLE", purgeChat, "video", 96*time.Hour, 10)
	image := seedOperatorMedia(t, b, "KEPT", purgeGroup, "image", 24*time.Hour, 95)
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":100,"media.quota_evict_types":["video"],"media.quota_evict_target_percent":90}`)
	_, release, err := b.acquireMediaQuota(t.Context(), 1)
	release()
	if !errors.Is(err, errMediaQuota) || fileExists(video) || !fileExists(image) {
		t.Fatalf("eligible bytes exhausted above low-water target: err=%v", err)
	}
	code, usage := mediaHTTPRequest(t, server, "GET", "/operator/v1/media/usage", "", fakeOperatorToken)
	if code != 200 || usage["caching_paused"] != true || usage["bytes"] != float64(95) {
		t.Fatal("usage hid the low-water pause", usage)
	}
	_, release, err = b.acquireMediaQuota(t.Context(), 1)
	release()
	if !errors.Is(err, errMediaQuota) {
		t.Fatal("a retry resumed above the low-water target", err)
	}
	settingsRequest(t, server, "PATCH", `{"media.quota_evict_target_percent":99}`)
	_, usage = mediaHTTPRequest(t, server, "GET", "/operator/v1/media/usage", "", fakeOperatorToken)
	if usage["caching_paused"] != false {
		t.Fatal("usage ignored the reachable runtime target", usage)
	}
	_, release, err = b.acquireMediaQuota(t.Context(), 1)
	release()
	if err != nil || !fileExists(image) {
		t.Fatal("new reachable runtime target did not resume caching", err)
	}
}

func TestLocalQuotaPauseClearsAfterOperatorPurge(t *testing.T) {
	b := newSettingsBridge(t)
	server := mediaOperatorServer(t, b)
	seedOperatorMedia(t, b, "ELIGIBLE", purgeChat, "video", 96*time.Hour, 10)
	seedOperatorMedia(t, b, "KEPT", purgeGroup, "image", 24*time.Hour, 95)
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":100,"media.quota_evict_types":["video"],"media.quota_evict_target_percent":90}`)
	_, release, err := b.acquireMediaQuota(t.Context(), 1)
	release()
	if !errors.Is(err, errMediaQuota) {
		t.Fatal(err)
	}
	code, result := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", `{"type":"all","dry_run":false}`, fakeOperatorToken)
	if code != 200 || result["freed_bytes"] != float64(95) {
		t.Fatal(code, result)
	}
	code, usage := mediaHTTPRequest(t, server, "GET", "/operator/v1/media/usage", "", fakeOperatorToken)
	if code != 200 || usage["bytes"] != float64(0) || usage["caching_paused"] != false {
		t.Fatal("pause remained after operator cleanup", code, usage)
	}
}

func TestLocalQuotaLeaseWaitHonorsContext(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 100
	release, err := b.lockMediaQuota(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	defer once.Do(release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, unlock, err := b.acquireMediaQuota(ctx, 1)
		unlock()
		done <- err
	}()
	<-ctx.Done()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		once.Do(release)
		<-done
		t.Fatal("quota waiter retained lease after its deadline")
	}
}

func TestLocalQuotaExpiredAutomaticTransferReleasesClientGate(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 100
	seedOperatorMedia(t, b, "INCOMING", purgeChat, "image", time.Minute, 1)
	row, err := b.Store.MediaRow("INCOMING", purgeChat)
	if err != nil {
		t.Fatal(err)
	}
	if res := purgeOne(b.StoreRoot, row, false); !res.Purged {
		t.Fatal(res)
	}
	networkCalls := make(chan struct{}, 1)
	b.mediaTransfer = func(context.Context, whatsmeow.DownloadableMessage, string) (int64, error) {
		networkCalls <- struct{}{}
		return 0, errors.New("expired quota waiter must never reach the network")
	}
	release, err := b.lockMediaQuota(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { release(); b.mediaTransfers.wait() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := b.DownloadMedia(withAutomaticCache(ctx), "INCOMING", purgeChat)
		done <- err
	}()
	// Observe the real detached transfer holding its client reader gate before
	// the deadline; otherwise a scheduling miss would make this proof vacuous.
	for b.clientGate.TryLock() {
		b.clientGate.Unlock()
		if ctx.Err() != nil {
			t.Fatal("transfer never acquired its client reader gate")
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { b.mediaTransfers.wait(); close(drained) }()
	select {
	case <-drained:
		select {
		case <-networkCalls:
			t.Fatal("expired quota waiter reached the network")
		default:
		}
		if !b.clientGate.TryLock() {
			t.Fatal("completed transfer retained its client reader gate")
		}
		b.clientGate.Unlock()
	case <-time.After(time.Second):
		t.Fatal("expired automatic transfer stayed behind an unrelated quota lease")
	}
}

func TestMediaQuotaLeaseDoesNotBlockHealth(t *testing.T) {
	b := newSettingsBridge(t)
	operator := mediaOperatorServer(t, b)
	settingsRequest(t, operator, "PATCH", `{"media.quota_bytes":10}`)
	b.RESTAllowedHosts = "127.0.0.1"
	rest := httptest.NewServer(b.newRESTMux(8080, purgeToken))
	t.Cleanup(rest.Close)
	_, release, err := b.acquireMediaQuota(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release() // Hold the real transfer lease through all HTTP requests.
	for _, request := range []struct {
		server      *httptest.Server
		path, token string
		status      int
	}{{rest, "/api/health", purgeToken, 200}, {rest, "/api/ready", purgeToken, 503}, {operator, "/operator/v1/media/usage", fakeOperatorToken, 200}} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", request.server.URL+request.path, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+request.token)
		resp, err := request.server.Client().Do(req)
		if err != nil {
			t.Errorf("%s blocked by transfer lease: %v", request.path, err)
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode != request.status {
				t.Errorf("%s returned %d", request.path, resp.StatusCode)
			}
		}
		cancel()
	}
}

func TestLocalQuotaOutboundConcurrentPublication(t *testing.T) {
	b := newSettingsBridge(t)
	server := settingsServer(t, b)
	b.MediaAutoDownload = true
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":10}`)
	stamp := time.Now().UTC()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.cacheOutboundMedia(context.Background(), sentMessage{ID: fmt.Sprintf("OUT%d", i), ChatJID: purgeChat, Timestamp: stamp}, outboundMedia{mediaType: "image"}, []byte("123456"))
		}()
	}
	wg.Wait()
	_, media, files := storeUsage(b.StoreRoot)
	if media != 6 || files != 1 {
		t.Fatalf("quota overrun bytes=%d files=%d", media, files)
	}
}

func TestLocalQuotaChecksActualAutomaticBytesAndKeepsManualDownload(t *testing.T) {
	b := newSettingsBridge(t)
	server := settingsServer(t, b)
	seedOperatorMedia(t, b, "KEPT", purgeChat, "image", time.Hour, 6)
	seedOperatorMedia(t, b, "INCOMING", purgeChat, "video", time.Minute, 3)
	row, err := b.Store.MediaRow("INCOMING", purgeChat)
	if err != nil {
		t.Fatal(err)
	}
	if result := purgeOne(b.StoreRoot, row, false); !result.Purged {
		t.Fatal(result)
	}
	settingsRequest(t, server, "PATCH", `{"media.quota_bytes":10}`)
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, file whatsmeow.File) error {
			_, err := file.Write([]byte("1234567"))
			return err
		})
	}
	b.runAutoDownload(t.Context(), mediaJob{messageID: "INCOMING", chatJID: purgeChat, mediaType: "video"})
	_, used, files := storeUsage(b.StoreRoot)
	if used != 6 || files != 1 || b.metrics.mediaQuotaRefusals.Load() != 1 {
		t.Fatalf("automatic budget: %d bytes %d files refusals=%d", used, files, b.metrics.mediaQuotaRefusals.Load())
	}
	name := mediaFileName(row.MediaType, row.Timestamp, row.ID, row.Filename)
	if _, err := b.StoreRoot.Lstat(chatMediaRel(purgeChat) + "/" + name + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("part survived", err)
	}
	if ok, _, _, _, err := b.DownloadMedia(t.Context(), "INCOMING", purgeChat); !ok || err != nil {
		t.Fatal("manual fetch blocked", err)
	}
	_, used, files = storeUsage(b.StoreRoot)
	if used != 13 || files != 2 {
		t.Fatalf("manual behavior %d bytes %d files", used, files)
	}
}

func TestStatusCLIRefusesHeldLockAndDoesNotChangeRows(t *testing.T) {
	b := newSettingsBridge(t)
	file := seedOperatorMedia(t, b, "STATUS", "status@broadcast", "image", time.Hour, 4)
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		t.Fatal(err)
	}
	exit := purgeStatusCLI([]string{"--dry-run"}, io.Discard, io.Discard)
	lock.Release()
	if exit != 1 || !fileExists(file) {
		t.Fatal("held store lock ignored")
	}
	var before string
	if err := b.Store.db.QueryRow("SELECT group_concat(name) FROM sqlite_schema ORDER BY name").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if exit := purgeStatusCLI([]string{"--dry-run"}, io.Discard, io.Discard); exit != 0 {
		t.Fatal(exit)
	}
	var after string
	if err := b.Store.db.QueryRow("SELECT group_concat(name) FROM sqlite_schema ORDER BY name").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || !fileExists(file) {
		t.Fatal("dry run changed store")
	}
}

func TestLocalQuotaSlowNetworkDoesNotBlockInboundImage(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 10
	b.MediaAutoDownload = true
	b.Webhook = newWebhookSender("", true)
	b.Webhook.url = "http://127.0.0.1:1"
	queueCtx, queueCancel := context.WithCancel(b.ctx)
	b.autoDownloads = newMediaJobQueue(queueCtx, 1, 20, b.runAutoDownload)
	defer func() { queueCancel(); b.autoDownloads.wait() }()
	seedOperatorMedia(t, b, "SLOW", purgeChat, "video", time.Minute, 8)
	row, err := b.Store.MediaRow("SLOW", purgeChat)
	if err != nil {
		t.Fatal(err)
	}
	if res := purgeOne(b.StoreRoot, row, false); !res.Purged {
		t.Fatal(res)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(finish) }); b.mediaTransfers.wait() }()
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		close(entered)
		<-finish
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, file whatsmeow.File) error {
			_, err := file.Write([]byte("12345678"))
			return err
		})
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := b.DownloadMedia(withAutomaticCache(t.Context()), "SLOW", purgeChat)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no real transfer admission")
	}
	msg := buildImageMessage(phonePN, phonePN, false, "caption")
	msg.Info.ID = "NEXT"
	msg.Message.ImageMessage.FileLength = proto.Uint64(3)
	msg.Message.ImageMessage.URL = proto.String("https://example.invalid/media")
	msg.Message.ImageMessage.MediaKey = []byte("key")
	msg.Message.ImageMessage.FileSHA256 = []byte("sha")
	msg.Message.ImageMessage.FileEncSHA256 = []byte("enc")
	handled := make(chan struct{})
	go func() { b.handleMessage(msg); close(handled) }()
	select {
	case <-handled:
	case <-time.After(500 * time.Millisecond):
		once.Do(func() { close(finish) })
		<-handled
		t.Fatal("unrelated transfer blocked inbound processing")
	}
	if b.autoDownloads.queued() != 0 {
		t.Fatal("paused image was requeued")
	}
	if b.metrics.mediaQuotaRefusals.Load() != 1 {
		t.Fatal("no pause observed")
	}
	once.Do(func() { close(finish) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, used, files := storeUsage(b.StoreRoot)
	if used != 8 || files != 1 {
		t.Fatalf("reservation publication bytes=%d files=%d", used, files)
	}
}

func TestLocalQuotaConcurrentNetworkReservations(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 10
	for _, id := range []string{"FIRST", "SECOND", "THIRD"} {
		seedOperatorMedia(t, b, id, purgeChat, "video", time.Minute, 4)
		row, err := b.Store.MediaRow(id, purgeChat)
		if err != nil {
			t.Fatal(err)
		}
		if res := purgeOne(b.StoreRoot, row, false); !res.Purged {
			t.Fatal(res)
		}
	}
	entered := make(chan string, 3)
	finish := make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(finish) }); b.mediaTransfers.wait() }()
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		entered <- rel
		<-finish
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, file whatsmeow.File) error { _, err := file.Write([]byte("1234")); return err })
	}
	results := make(chan error, 2)
	for _, id := range []string{"FIRST", "SECOND"} {
		go func() {
			_, _, _, _, err := b.DownloadMedia(withAutomaticCache(t.Context()), id, purgeChat)
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("network transfer retained accounting lease")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, _, _, err := b.DownloadMedia(withAutomaticCache(ctx), "THIRD", purgeChat); !errors.Is(err, errMediaQuota) {
		t.Fatal("third transfer exceeded quota", err)
	}
	once.Do(func() { close(finish) })
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	b.mediaTransfers.wait()
	_, used, files := storeUsage(b.StoreRoot)
	if used != 8 || files != 2 {
		t.Fatalf("overrun %d %d", used, files)
	}
	_, release, err := b.acquireMediaQuota(t.Context(), 2)
	if err != nil {
		t.Fatal("reservation leaked", err)
	}
	release()
}

func TestOperatorPurgeStreamsBeyondWriteTimeout(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			b := newSettingsBridge(t)
			cached := seedOperatorMedia(t, b, "STREAM", purgeChat, "image", time.Hour, 4)
			b.Store.db.SetMaxOpenConns(1)
			conn, err := b.Store.db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			defer once.Do(func() { _ = conn.Close() })
			server := httptest.NewUnstartedServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{mediaPurge: b.handleOperatorMediaPurge}, b.Log))
			server.Config.WriteTimeout = 15 * time.Second
			server.Start()
			defer server.Close()
			go func() { time.Sleep(16 * time.Second); once.Do(func() { _ = conn.Close() }) }()
			code, result := mediaHTTPRequest(t, server, "POST", "/operator/v1/media/purge", fmt.Sprintf(`{"type":"all","dry_run":%t}`, dry), fakeOperatorToken)
			if code != 200 || result["files"] != float64(1) || result["freed_bytes"] != float64(4) || len(result["progress"].([]any)) < 4 || fileExists(cached) != dry {
				t.Fatal(code, result)
			}
		})
	}
}

func TestStatusCLIDiagnosticsAndAudit(t *testing.T) {
	b := newSettingsBridge(t)
	seedOperatorMedia(t, b, "AUDIT", "status@broadcast", "image", time.Hour, 4)
	var out, diagnostics bytes.Buffer
	lock, err := acquireInstanceLock(instanceLockPath())
	if err != nil {
		t.Fatal(err)
	}
	exit := purgeStatusCLI(nil, &out, &diagnostics)
	lock.Release()
	if exit != 1 || diagnostics.Len() == 0 || out.Len() != 0 {
		t.Fatal("silent lock refusal")
	}
	for _, tc := range []struct{ key, value, reason string }{{readOnlyEnv, "true", "read-only"}, {denyToolsEnv, "purge_media", "disabled"}} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			out.Reset()
			diagnostics.Reset()
			if purgeStatusCLI(nil, &out, &diagnostics) != 1 || !strings.Contains(diagnostics.String(), tc.reason) {
				t.Fatal(diagnostics.String())
			}
		})
	}
	out.Reset()
	diagnostics.Reset()
	if purgeStatusCLI(nil, &out, &diagnostics) != 0 || !strings.Contains(diagnostics.String(), "INFO") || !strings.Contains(diagnostics.String(), "freed_bytes=4") {
		t.Fatal(diagnostics.String())
	}
	t.Setenv(storeDirEnv, t.TempDir())
	out.Reset()
	diagnostics.Reset()
	if purgeStatusCLI(nil, &out, &diagnostics) != 1 || diagnostics.Len() == 0 {
		t.Fatal("silent missing DB")
	}
}

func TestStatusOnlyRetentionLogUsesStatusAge(t *testing.T) {
	b := newSettingsBridge(t)
	cached := seedOperatorMedia(t, b, "EXPIRED", "status@broadcast", "image", 96*time.Hour, 4)
	age := 24 * time.Hour
	b.StatusRetention = &age
	b.MediaRetention = 0
	var out bytes.Buffer
	b.Log = newTextWriter("Bridge", "INFO", &out, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.ctx = ctx
	b.runMediaRetention()
	if fileExists(cached) || !strings.Contains(out.String(), "status_age=24h0m0s") {
		t.Fatal(out.String())
	}
}

func TestOperatorPurgeKeepsCraftedPartLeaf(t *testing.T) {
	b := newSettingsBridge(t)
	stamp := time.Now().UTC().Truncate(time.Second)
	if err := b.Store.StoreChat(purgeChat, "", stamp); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.StoreMessage(storedMessage{ID: "FLIGHT.part", ChatJID: purgeChat, Sender: purgeChat, Timestamp: stamp, MediaType: "document"}); err != nil {
		t.Fatal(err)
	}
	leaf := mediaFileName("document", stamp, "FLIGHT", "") + ".part"
	if err := b.StoreRoot.MkdirAll(chatMediaRel(purgeChat), storeDirMode); err != nil {
		t.Fatal(err)
	}
	f, err := b.StoreRoot.Create(chatMediaRel(purgeChat) + "/" + leaf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("in flight"))
	_ = f.Close()
	no := false
	result, err := b.purgeOperatorMedia(t.Context(), operatorMediaPurge{Type: "all", DryRun: &no})
	if err != nil || result.Files != 0 {
		t.Fatal("crafted row deleted transfer temp", result, err)
	}
	if _, err := b.StoreRoot.Lstat(chatMediaRel(purgeChat) + "/" + leaf); err != nil {
		t.Fatal(err)
	}
}

func TestLocalQuotaSynchronousAccountingContentionFallsBack(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 10
	b.MediaAutoDownload = true
	webhook, _ := captureWebhook(t)
	b.Webhook = newWebhookSender("", true)
	b.Webhook.url = webhook.URL
	entered := make(chan struct{}, 1)
	b.mediaTransfer = func(ctx context.Context, _ whatsmeow.DownloadableMessage, rel string) (int64, error) {
		entered <- struct{}{}
		return writeMediaDownload(ctx, b.StoreRoot, rel, func(_ context.Context, file whatsmeow.File) error { _, err := file.Write([]byte("123")); return err })
	}
	release, err := b.lockMediaQuota(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	defer once.Do(release)
	msg := buildImageMessage(phonePN, phonePN, false, "caption")
	msg.Info.ID = "CONTENDED"
	msg.Message.ImageMessage = &waE2E.ImageMessage{Caption: proto.String("caption"), URL: proto.String("https://example.invalid/media"), FileLength: proto.Uint64(3), MediaKey: []byte("key"), FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc")}
	done := make(chan struct{})
	go func() { b.handleMessage(msg); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		once.Do(release)
		<-done
		t.Fatal("synchronous image waited on accounting")
	}
	if b.metrics.mediaQuotaRefusals.Load() != 0 {
		t.Fatal("contention misclassified as quota pause")
	}
	once.Do(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background fallback not consumed")
	}
	b.mediaTransfers.wait()
	_, used, files := storeUsage(b.StoreRoot)
	if used != 3 || files != 1 {
		t.Fatal(used, files)
	}
}

func TestLocalQuotaPauseLogIsRateLimited(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.MediaQuotaBytes = 1
	seedOperatorMedia(t, b, "FULL", purgeChat, "image", time.Hour, 1)
	var out bytes.Buffer
	b.Log = newTextWriter("Bridge", "INFO", &out, false)
	for range 3 {
		_, release, err := b.acquireMediaQuota(t.Context(), 1)
		release()
		if !errors.Is(err, errMediaQuota) {
			t.Fatal(err)
		}
	}
	if b.metrics.mediaQuotaRefusals.Load() != 3 || strings.Count(out.String(), "Automatic media caching paused") != 1 {
		t.Fatal(out.String())
	}
}

func TestCachedMediaWalkCancellationStopsBetweenFiles(t *testing.T) {
	b := newSettingsBridge(t)
	for i := range 600 {
		seedOperatorMedia(t, b, fmt.Sprintf("WALK%04d", i), purgeChat, "image", time.Hour, 1)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := 0
	err := eachCachedMediaContext(ctx, b.StoreRoot, func(_ string, _ *cachedMedia) { seen++; cancel() })
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatal("walk continued after cancellation", seen, err)
	}
}

func TestStatusRESTStreamsBeyondClientReadTimeout(t *testing.T) {
	b := newSettingsBridge(t)
	cached := seedOperatorMedia(t, b, "RESTSTREAM", "status@broadcast", "image", time.Hour, 4)
	b.Store.db.SetMaxOpenConns(1)
	conn, err := b.Store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	defer once.Do(func() { _ = conn.Close() })
	b.RuntimeDefaults = nil
	b.RESTAllowedHosts = "127.0.0.1"
	server := httptest.NewServer(b.newRESTMux(8080, purgeToken))
	defer server.Close()
	go func() { time.Sleep(31 * time.Second); once.Do(func() { _ = conn.Close() }) }()
	code, result := mediaHTTPRequest(t, server, "POST", "/api/media/purge", `{"scope":"status","dry_run":false}`, purgeToken)
	if code != 200 || result["success"] != true || result["purged_bytes"] != float64(4) || len(result["progress"].([]any)) < 7 || fileExists(cached) {
		t.Fatal(code, result)
	}
}

// Explicitly invoked by the durable cross-language verification script; ordinary
// Go gates skip the helper. Its REST handler, SQLite rows and cache are real.
func TestStatusRESTStreamingMCPHelper(t *testing.T) {
	if os.Getenv("WAMCP_TEST_STATUS_STREAM_MCP") != "1" {
		return
	}
	b := newSettingsBridge(t)
	b.RuntimeDefaults = nil
	b.RESTAllowedHosts = "127.0.0.1"
	cached := seedOperatorMedia(t, b, "MCPSTREAM", "status@broadcast", "image", time.Hour, 4)
	b.Store.db.SetMaxOpenConns(1)
	mux := b.newRESTMux(8080, purgeToken)
	done := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := b.Store.db.Conn(r.Context())
		if err != nil {
			writeError(w, 503, "Test database unavailable")
			return
		}
		timer := time.AfterFunc(31*time.Second, func() { _ = conn.Close() })
		defer timer.Stop()
		defer func() { _ = conn.Close() }()
		mux.ServeHTTP(w, r)
		done <- struct{}{}
	}))
	defer server.Close()
	fmt.Printf("WAMCP_STREAM_URL=%s\n", server.URL)
	for range 2 {
		select {
		case <-done:
		case <-time.After(40 * time.Second):
			t.Fatal("MCP request did not finish")
		}
	}
	if fileExists(cached) {
		t.Fatal("real MCP purge retained the file")
	}
	var rows int
	if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='MCPSTREAM'").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("MCP purge changed rows", rows, err)
	}
}
