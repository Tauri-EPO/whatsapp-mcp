package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newSettingsBridge(t *testing.T) *Bridge {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.Connected = func() bool { return false }
	b.RuntimeDefaults, err = runtimeDefaults(func(name string) string {
		if name == denyToolsEnv {
			return "delete_message"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func settingsRequest(t *testing.T, server *httptest.Server, method, body string) (int, runtimeSettingsSnapshot) {
	t.Helper()
	req, _ := http.NewRequest(method, server.URL+"/operator/v1/settings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fakeOperatorToken)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot runtimeSettingsSnapshot
	if response.StatusCode == 200 && json.Unmarshal(data, &snapshot) != nil {
		t.Fatal("invalid snapshot")
	}
	return response.StatusCode, snapshot
}

func settingsServer(t *testing.T, b *Bridge) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(newOperatorHandler(operatorConfig{Bind: "127.0.0.1", Port: 8090, Token: fakeOperatorToken, AllowedHosts: "127.0.0.1"}, operatorRoutes{settings: b.handleRuntimeSettings}, b.Log))
	t.Cleanup(server.Close)
	return server
}

func TestRuntimeSettingsHTTPPrecedenceAtomicValidationAndRestart(t *testing.T) {
	b := newSettingsBridge(t)
	b.ReadOnly = readOnlyPolicy{enabled: true} // Control plane remains usable.
	audit := &recordingLogger{}
	b.Log = audit
	server := settingsServer(t, b)
	status, before := settingsRequest(t, server, "GET", "")
	if status != 200 || before.Version != 0 || before.Settings["tools.deny"].Source != "env" || before.Settings["tools.allow"].Source != "default" {
		t.Fatalf("initial=%+v", before)
	}
	for _, bad := range []string{
		`{"transcription.ingest_chats":"direct","tools.allow":["unknown"]}`,
		`{"tools.allow":"send_message"}`, `{"transcription.ingest_chats":"group"}`,
		`{"tools.deny":[1]}`, `{"read_only":false}`, `null`, `{}`, `{"tools.allow":[]} {}`,
	} {
		if status, _ := settingsRequest(t, server, "PATCH", bad); status != 400 {
			t.Fatalf("invalid PATCH=%d", status)
		}
		_, current := settingsRequest(t, server, "GET", "")
		if current.Version != 0 || current.Settings["transcription.ingest_chats"].Source != "default" {
			t.Fatal("partial write on invalid PATCH")
		}
	}
	status, after := settingsRequest(t, server, "PATCH", `{"tools.allow":["send_message"],"tools.deny":[],"transcription.ingest_chats":"direct"}`)
	if status != 200 || after.Version != 1 || after.Settings["tools.allow"].Source != "runtime" || after.Settings["transcription.ingest_chats"].Value != "direct" {
		t.Fatalf("after=%+v", after)
	}
	// A fresh bridge opens the same physical database, without reusing in-memory settings.
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	fresh := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	fresh.RuntimeDefaults = b.RuntimeDefaults
	restored, err := fresh.settingsSnapshot(context.Background())
	if err != nil || restored.Version != 1 || restored.Settings["transcription.ingest_chats"].Value != "direct" {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	status, cleared := settingsRequest(t, server, "PATCH", `{"tools.allow":null,"tools.deny":null,"transcription.ingest_chats":null}`)
	if status != 200 || cleared.Version != 2 || cleared.Settings["tools.deny"].Source != "env" || cleared.Settings["transcription.ingest_chats"].Source != "default" {
		t.Fatalf("clear=%+v", cleared)
	}
	if !strings.Contains(b.renderMetrics(), "whatsapp_runtime_settings_version 2") {
		t.Fatal("gauge did not observe clear")
	}
	if !strings.Contains(audit.String(), "[INFO] Runtime setting key=tools.deny source=runtime->env version=2") || strings.Contains(audit.String(), "send_message") || strings.Contains(audit.String(), fakeOperatorToken) {
		t.Fatal("setting audit leaked values or omitted sources")
	}
	var marker int
	if err := ms.db.QueryRow("SELECT count(*) FROM schema_migrations WHERE name='runtime_settings_v1'").Scan(&marker); err != nil || marker != 1 {
		t.Fatal("missing schema marker")
	}
}

func TestRuntimeSettingsBridgePolicyChangesWithoutRebuildingMux(t *testing.T) {
	b := newSettingsBridge(t)
	server := settingsServer(t, b)
	mux := b.newRESTMux(8080, readOnlyTestToken)
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8080"+path, strings.NewReader(bodyFor(t, path)))
		req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	if request("/api/send").Code == 403 {
		t.Fatal("initial endpoint disabled")
	}
	if status, _ := settingsRequest(t, server, "PATCH", `{"tools.deny":["send_message","send_file","send_audio_message"]}`); status != 200 {
		t.Fatal(status)
	}
	if request("/api/send").Code != 403 {
		t.Fatal("denial did not reach existing mux")
	}
	if status, _ := settingsRequest(t, server, "PATCH", `{"tools.allow":["send_reaction"],"tools.deny":[]}`); status != 200 {
		t.Fatal(status)
	}
	if request("/api/send").Code != 403 || request("/api/react").Code == 403 {
		t.Fatal("allow list did not reach both endpoints")
	}
	if status, _ := settingsRequest(t, server, "PATCH", `{"tools.allow":null,"tools.deny":null}`); status != 200 {
		t.Fatal(status)
	}
	if request("/api/send").Code == 403 {
		t.Fatal("clear did not restore endpoint")
	}
	b.ReadOnly = readOnlyPolicy{enabled: true}
	mux = b.newRESTMux(8080, readOnlyTestToken)
	if status, _ := settingsRequest(t, server, "PATCH", `{"tools.allow":["send_message"]}`); status != 200 {
		t.Fatal(status)
	}
	if got := request("/api/send"); got.Code != 403 || !strings.Contains(got.Body.String(), readOnlyEnv) {
		t.Fatal("runtime allow bypassed read-only")
	}
}

func TestRuntimeSettingsRollbackOnDatabaseFailureAndConcurrentVersions(t *testing.T) {
	b := newSettingsBridge(t)
	if _, err := b.Store.db.Exec(`CREATE TRIGGER reject_setting BEFORE INSERT ON runtime_settings WHEN NEW.key='tools.deny' BEGIN SELECT RAISE(ABORT,'fake write failure'); END`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.handleRuntimeSettings(w, httptest.NewRequest("PATCH", "/operator/v1/settings", strings.NewReader(`{"tools.allow":["send_message"],"tools.deny":[]}`)))
	snapshot, err := b.settingsSnapshot(context.Background())
	if w.Code != 503 || err != nil || snapshot.Version != 0 {
		t.Fatal("failed transaction leaked an earlier key")
	}
	if _, err := b.Store.db.Exec("DROP TRIGGER reject_setting"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			b.handleRuntimeSettings(w, httptest.NewRequest("PATCH", "/operator/v1/settings", strings.NewReader(`{"tools.allow":[]}`)))
			if w.Code != 200 {
				t.Errorf("concurrent PATCH=%d", w.Code)
			}
		})
	}
	wg.Wait()
	snapshot, err = b.settingsSnapshot(context.Background())
	if err != nil || snapshot.Version != 8 {
		t.Fatalf("clock=%d err=%v", snapshot.Version, err)
	}
}
