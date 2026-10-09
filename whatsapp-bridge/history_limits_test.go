package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestHistoryPairingConfigurationAndFlagConflict(t *testing.T) {
	values := map[string]string{historySyncDaysEnv: "30", historySyncSizeEnv: "128", historySyncQuotaEnv: "256", "WHATSAPP_HISTORY_MAX_AGE_DAYS": "14", "WHATSAPP_STORE_WARN_BYTES": "4096"}
	cfg, err := parseBridgeConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	for _, paired := range []bool{false, true} {
		props := &waCompanionReg.DeviceProps{RequireFullSync: proto.Bool(false), HistorySyncConfig: &waCompanionReg.DeviceProps_HistorySyncConfig{FullSyncDaysLimit: proto.Uint32(90)}}
		cfg.History.apply(props, paired, false)
		if paired {
			if props.GetRequireFullSync() || props.HistorySyncConfig.GetFullSyncDaysLimit() != 90 {
				t.Fatal("paired device settings changed")
			}
		} else if !props.GetRequireFullSync() || props.HistorySyncConfig.GetFullSyncDaysLimit() != 30 || props.HistorySyncConfig.GetFullSyncSizeMbLimit() != 128 || props.HistorySyncConfig.GetStorageQuotaMb() != 256 {
			t.Fatal("bounded request not applied to fresh pair")
		}
	}
	if cfg.History.validateFlag(true) == nil {
		t.Fatal("env plus flag accepted")
	}
	props := &waCompanionReg.DeviceProps{}
	historyLimits{}.apply(props, false, true)
	if props.HistorySyncConfig.GetFullSyncDaysLimit() != 3650 || props.HistorySyncConfig.GetFullSyncSizeMbLimit() != 102400 || props.HistorySyncConfig.GetStorageQuotaMb() != 102400 {
		t.Fatal("full history shortcut changed")
	}
	for _, name := range []string{historySyncDaysEnv, historySyncSizeEnv, historySyncQuotaEnv, "WHATSAPP_HISTORY_MAX_AGE_DAYS", "WHATSAPP_STORE_WARN_BYTES"} {
		for _, value := range []string{"0", "-1", "oops", "18446744073709551615"} {
			if _, err := parseBridgeConfig(func(key string) string {
				if key == name {
					return value
				}
				return ""
			}); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("bad %s accepted", name)
			}
		}
	}
}

func TestHistoryAgeGuardProgressHealthAndLiveMessage(t *testing.T) {
	ms := newTestMessageStore(t)
	logger := &recordingLogger{}
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, logger)
	b.HistoryLimits.MaxAge = 7 * 24 * time.Hour
	fixture := largeHistoryFixture(1001)
	for i, row := range fixture.Data.Conversations[0].Messages {
		stamp := time.Now().Add(-time.Hour).Unix()
		if i%2 == 0 {
			stamp = time.Now().Add(-30 * 24 * time.Hour).Unix()
		}
		row.Message.MessageTimestamp = proto.Uint64(uint64(stamp)) //nolint:gosec // Synthetic current-date timestamps are positive epoch seconds.
	}
	fixture.Data.Progress = proto.Uint32(0)
	b.handleHistorySync(fixture)
	var count int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 500 || b.metrics.historyDropped.Load() != 501 || strings.Count(logger.String(), "History age guard:") != 3 {
		t.Fatalf("rows=%d dropped=%d logs=%s", count, b.metrics.historyDropped.Load(), logger.String())
	}
	if len(fixture.Data.Conversations[0].Messages) != 1001 {
		t.Fatal("SDK payload mutated")
	}
	if b.historyProgress.snapshot().State != "syncing" {
		t.Fatal("sync state not exposed")
	}
	fixture = largeHistoryFixture(1)
	fixture.Data.Conversations[0].Messages[0].Message.Key.ID = proto.String("COMPLETE")
	fixture.Data.Conversations[0].Messages[0].Message.MessageTimestamp = proto.Uint64(uint64(time.Now().Unix())) //nolint:gosec // Current test clock is after the epoch.
	fixture.Data.Progress = proto.Uint32(100)
	b.handleHistorySync(fixture)
	health := httptest.NewRecorder()
	b.handleHealth()(health, httptest.NewRequest("GET", "/api/health", nil))
	var status struct {
		History historyStatus `json:"history_sync"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.History.State != "complete" || status.History.Progress != 100 || status.History.Messages != 501 || status.History.Conversations != 2 || !strings.Contains(b.renderMetrics(), "whatsapp_bridge_history_sync_progress 100") {
		t.Fatalf("health=%s", health.Body.String())
	}
	live := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "old live message remains")
	live.Info.Timestamp = time.Now().Add(-90 * 24 * time.Hour)
	b.handleMessage(live)
	if count := queryMessageCount(ms, phonePN.String()); count != 502 {
		t.Fatalf("old live message dropped: rows=%d", count)
	}
}

func TestDatabaseUsageGaugesAndWarningCrossings(t *testing.T) {
	b, _ := archiveFixture(t, false)
	logger := &recordingLogger{}
	b.Log = logger
	b.HistoryLimits.WarnBytes = 1
	for i := 0; i < 2; i++ {
		metrics := b.renderMetrics()
		for _, name := range []string{"messages", "whatsapp"} {
			var size int64
			for _, suffix := range []string{".db", ".db-wal"} {
				info, err := b.StoreRoot.Lstat(name + suffix)
				if err == nil {
					size += info.Size()
				}
			}
			if !strings.Contains(metrics, "whatsapp_bridge_db_bytes{db=\""+name+"\"} "+fmtInt(size)) {
				t.Fatalf("database gauge mismatched: %s", name)
			}
		}
		if !strings.Contains(metrics, "whatsapp_bridge_messages_rows 1") || b.healthStatus()["store_warning"] != true {
			t.Fatal("rows/warning not exposed")
		}
	}
	if strings.Count(logger.String(), "warning threshold") != 1 {
		t.Fatal("duplicate crossing WARN")
	}
	b.HistoryLimits.WarnBytes = 1 << 62
	_ = b.healthStatus()
	b.HistoryLimits.WarnBytes = 1
	_ = b.healthStatus()
	if strings.Count(logger.String(), "warning threshold") != 2 {
		t.Fatal("second threshold crossing not logged")
	}
}

func TestStoreWarningPostsOneSafeEventPerCrossing(t *testing.T) {
	b, _ := archiveFixture(t, false)
	posts := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		posts <- payload
		w.WriteHeader(204)
	}))
	defer server.Close()
	b.Webhook = &webhookSender{client: server.Client(), url: server.URL, enabled: true, token: "test-token-0123456789abcdef", failures: &b.metrics.webhookFailures}
	b.ForwardConnection = true
	b.HistoryLimits.WarnBytes = 1
	_ = b.healthStatus()
	_ = b.healthStatus()
	b.connectionEvents.Wait()
	if len(posts) != 1 {
		t.Fatalf("crossing events=%d", len(posts))
	}
	payload := <-posts
	if len(payload) != 2 || payload["type"] != "store" || payload["state"] != "above_threshold" {
		t.Fatalf("store event=%v", payload)
	}
	b.HistoryLimits.WarnBytes = 1 << 62
	_ = b.healthStatus()
	b.HistoryLimits.WarnBytes = 1
	_ = b.healthStatus()
	b.connectionEvents.Wait()
	if len(posts) != 1 {
		t.Fatal("second crossing event missing")
	}
}

func fmtInt(value int64) string {
	var out bytes.Buffer
	_ = json.NewEncoder(&out).Encode(value)
	return strings.TrimSpace(out.String())
}

func TestExportPinsReadSnapshotWhileLiveWritesContinue(t *testing.T) {
	b, server := archiveFixture(t, false)
	tx, err := b.Store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err := tx.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES(?,'111@s.whatsapp.net','2026-10-01 00:00:00+00:00',?)", fmtInt(int64(i)), strings.Repeat("x", 4096)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	response := archiveRequest(t, server, "GET", "/operator/v1/export")
	defer func() { _ = response.Body.Close() }()
	first := make([]byte, 1)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	second := archiveRequest(t, server, "GET", "/operator/v1/export")
	_ = second.Body.Close()
	if second.StatusCode != 429 {
		t.Fatalf("live concurrent export=%d", second.StatusCode)
	}
	if _, err := b.Store.db.Exec("INSERT INTO messages(id,chat_jid,timestamp,content) VALUES('AFTER_EXPORT','111@s.whatsapp.net','2026-10-01 00:00:00+00:00','AFTER_EXPORT_SENTINEL')"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("AFTER_EXPORT_SENTINEL")) {
		t.Fatal("export included a write after its pinned snapshot")
	}
	if _, err := os.Stat(storePath("notes.db")); !os.IsNotExist(err) {
		t.Fatal("export created optional notes store")
	}
}
