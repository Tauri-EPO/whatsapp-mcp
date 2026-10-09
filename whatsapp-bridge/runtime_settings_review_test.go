package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func settingsEndpoint(t *testing.T, b *Bridge, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080"+path, strings.NewReader(bodyFor(t, path)))
	r.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(w, r)
	return w
}

func TestRuntimeToolsCannotReopenDeploymentFloor(t *testing.T) {
	b := newSettingsBridge(t)
	server := settingsServer(t, b)
	for _, body := range []string{`{"tools.deny":[]}`, `{"tools.allow":["delete_message"],"tools.deny":[]}`} {
		code, snapshot := settingsRequest(t, server, http.MethodPatch, body)
		effective, _ := json.Marshal(snapshot.Settings["tools.deny"].Value)
		if code != 200 || !strings.Contains(string(effective), "delete_message") {
			t.Fatal("GET omitted effective deploy denial")
		}
		w := settingsEndpoint(t, b, "/api/delete")
		if code != 200 || w.Code != 403 || !strings.Contains(w.Body.String(), "WHATSAPP_DENY_TOOLS (deploy)") {
			t.Fatalf("deploy floor reopened: %d %s", w.Code, w.Body.String())
		}
	}
	b.RuntimeDefaults["tools.allow"] = runtimeSetting{Value: []string{"send_reaction"}, Source: "env"}
	code, _ := settingsRequest(t, server, http.MethodPatch, `{"tools.allow":["send_message"]}`)
	if code != 200 || settingsEndpoint(t, b, "/api/send").Code != 403 || settingsEndpoint(t, b, "/api/react").Code != 403 {
		t.Fatal("runtime allow widened deploy allow")
	}
	settingsRequest(t, server, http.MethodPatch, `{"tools.allow":null}`)
	if settingsEndpoint(t, b, "/api/react").Code == 403 {
		t.Fatal("clear failed to restore deploy allow")
	}
}

func TestSavedRuntimeSettingsRecoverAndCanAlwaysClear(t *testing.T) {
	for _, key := range []string{"tools.allow", "tools.deny", "transcription.ingest_chats"} {
		t.Run(key, func(t *testing.T) {
			b := newSettingsBridge(t)
			log := &recordingLogger{}
			b.Log = log
			if _, err := b.Store.db.Exec("INSERT INTO runtime_settings VALUES (?,?,'2026-10-09',1)", key, "invalid-json"); err != nil {
				t.Fatal(err)
			}
			server := settingsServer(t, b)
			code, _ := settingsRequest(t, server, http.MethodGet, "")
			if code != 200 {
				t.Fatal("invalid saved row wedged GET")
			}
			settingsRequest(t, server, http.MethodGet, "")
			if strings.Count(log.String(), "[WARN]") != 1 || !strings.Contains(log.String(), key) || strings.Contains(log.String(), "invalid-json") {
				t.Fatal("missing/repeated/value-leaking WARN")
			}
			if key == "tools.allow" && settingsEndpoint(t, b, "/api/send").Code != 403 {
				t.Fatal("corrupt allow widened access")
			}
			code, snapshot := settingsRequest(t, server, http.MethodPatch, `{"`+key+`":null}`)
			if code != 200 || snapshot.Version != 2 {
				t.Fatal("null could not clear corrupt row")
			}
		})
	}
}

func TestSavedUnknownToolsAreDroppedAndCorruptAllowUsesExplicitFloor(t *testing.T) {
	b := newSettingsBridge(t)
	b.RuntimeDefaults["tools.allow"] = runtimeSetting{Value: []string{"send_reaction"}, Source: "env"}
	if _, err := b.Store.db.Exec(`INSERT INTO runtime_settings VALUES ('tools.deny','["future_tool","delete_message"]','2026-10-09',1),('tools.allow','invalid-json','2026-10-09',1)`); err != nil {
		t.Fatal(err)
	}
	server := settingsServer(t, b)
	code, _ := settingsRequest(t, server, http.MethodGet, "")
	if code != 200 || settingsEndpoint(t, b, "/api/react").Code == 403 || settingsEndpoint(t, b, "/api/send").Code != 403 || settingsEndpoint(t, b, "/api/delete").Code != 403 {
		t.Fatal("saved fallback widened or wedged deploy floor")
	}
}

func TestBridgeRuntimePolicyReadFailureRefusesActualMutatingEndpoint(t *testing.T) {
	b := newSettingsBridge(t)
	before := settingsEndpoint(t, b, "/api/send")
	if strings.Contains(before.Body.String(), "settings_unavailable") {
		t.Fatal("baseline unavailable")
	}
	if err := b.Store.db.Close(); err != nil {
		t.Fatal(err)
	}
	w := settingsEndpoint(t, b, "/api/send")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "settings_unavailable") {
		t.Fatalf("unreadable policy reached mutation: %d %s", w.Code, w.Body.String())
	}
}
