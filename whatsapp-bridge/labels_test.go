package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func labelEvent(id, name string, ts time.Time, deleted bool) *events.LabelEdit {
	return &events.LabelEdit{LabelID: id, Timestamp: ts, Action: &waSyncAction.LabelEditAction{Name: proto.String(name), Color: proto.Int32(3), Deleted: proto.Bool(deleted)}}
}

type labelSnapshotStore struct {
	store.AppStateStore
	calls atomic.Int32
	err   error
}

func (s *labelSnapshotStore) DeleteAppStateVersion(_ context.Context, name string) error {
	if name != string(appstate.WAPatchRegular) {
		return errors.New("unexpected label collection")
	}
	s.calls.Add(1)
	return s.err // Stop the real SDK fetch before any network request.
}

func TestLabelRuntimeHandoffWiring(t *testing.T) {
	first, second := newTestClient(nil), newTestClient(nil)
	old, active := &labelSnapshotStore{err: errors.New("retired label store")}, &labelSnapshotStore{err: errors.New("active label store")}
	first.Store.AppState, second.Store.AppState = old, active
	b := newBridge(first, newTestMessageStore(t), testLogger(), "", nil, bridgeSwitches{})
	defer b.Shutdown(time.Second)
	b.installClient(second, true, make(chan bool, 1))
	if !second.EmitAppStateEventsOnFullSync {
		t.Error("replacement SDK client suppresses full-sync label events")
	}
	if err := b.LabelResync(context.Background()); !errors.Is(err, active.err) || old.calls.Load() != 0 || active.calls.Load() != 1 {
		t.Fatal("resync retained retired SDK client", err, old.calls.Load(), active.calls.Load())
	}
}

func TestLabelSyncUsesRuntimePairing(t *testing.T) {
	for _, paired := range []bool{true, false} {
		t.Run(map[bool]string{true: "paired replacement", false: "unpaired replacement"}[paired], func(t *testing.T) {
			first, second := newTestClient(nil), newTestClient(nil)
			if paired {
				first.Store.ID = nil // Initial client was never paired.
			} else {
				id := types.NewJID("5511999999999", types.DefaultUserServer)
				first.Store.ID = &id // Retired client had been paired.
			}
			b := testBridge(t, first, newTestMessageStore(t), testLogger())
			b.Connected = func() bool { return true }
			b.installClient(second, paired, make(chan bool, 1))
			called := make(chan struct{})
			b.LabelResync = func(context.Context) error { close(called); return nil }
			b.startLabelSync()
			if paired {
				select {
				case <-called:
				case <-time.After(time.Second):
					t.Error("paired replacement did not schedule label sync")
				}
			} else {
				untouched := false
				b.labelSyncOnce.Do(func() { untouched = true })
				if !untouched {
					t.Error("unpaired replacement consumed label sync attempt")
				}
			}
			b.Shutdown(time.Second)
		})
	}
}

func TestLabelSyncRechecksRuntimePairingAfterWaiting(t *testing.T) {
	first := newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer))
	b := testBridge(t, first, newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	b.installClient(first, true, make(chan bool, 1))
	var calls atomic.Int32
	b.LabelResync = func(context.Context) error { calls.Add(1); return nil }
	b.appStateGate <- struct{}{}
	b.startLabelSync()
	b.installClient(newTestClient(nil), false, make(chan bool, 1))
	<-b.appStateGate
	b.labelSyncWait.Wait()
	b.Shutdown(time.Second)
	if calls.Load() != 0 {
		t.Fatal("queued resync ran after runtime became unpaired")
	}
}

func TestLabelListMetadataPersists(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, newTestClient(nil), store, testLogger())
	e := labelEvent("1", "Alice", time.Now(), false)
	e.FromFullSync = true
	e.Action.Type = waSyncAction.LabelEditAction_FAVORITES.Enum()
	e.Action.IsImmutable = proto.Bool(true)
	e.Action.PredefinedID = proto.Int32(7)
	b.handleEvent(e, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rec := httptest.NewRecorder()
	handleLabels(store, parseChatPolicy(""), nil)(rec, httptest.NewRequest(http.MethodGet, "/api/labels", nil))
	var result struct {
		Labels []map[string]any `json:"labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(result.Labels) != 1 || result.Labels[0]["type"] != float64(3) || result.Labels[0]["immutable"] != true || result.Labels[0]["predefined_id"] != float64(7) {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestImmutableLabelRefusedThroughMux(t *testing.T) {
	b := testBridge(t, newTestClient(nil), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	e := labelEvent("1", "Alice", time.Now(), false)
	e.Action.IsImmutable = proto.Bool(true)
	b.handleEvent(e, nil)
	b.SendAppState = func(context.Context, appstate.PatchInfo) error { t.Fatal("immutable label sent"); return nil }
	for _, flag := range []string{"true", "false"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/chat/label", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","label_id":"1","labeled":`+flag+`}`))
		req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		rec := httptest.NewRecorder()
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"invalid_argument"`) || !strings.Contains(rec.Body.String(), "immutable") {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}

func TestLabelsMetadataMigrationFromV1(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
		t.Fatal(err)
	}
	// A cache created by the first draft keeps its data and gains metadata.
	if _, err := store.db.Exec(`ALTER TABLE labels DROP COLUMN type;
	 ALTER TABLE labels DROP COLUMN immutable; ALTER TABLE labels DROP COLUMN predefined_id;
	 DELETE FROM schema_migrations WHERE name='labels_metadata_v2'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ensureMessageStoreSchema(store.db); err != nil {
			t.Fatal(err)
		}
	}
	var name string
	var kind int32
	var immutable bool
	var predefined any
	if err := store.db.QueryRow("SELECT name, type, immutable, predefined_id FROM labels WHERE id='1'").Scan(&name, &kind, &immutable, &predefined); err != nil || name != "Alice" || kind != 0 || immutable || predefined != nil {
		t.Fatal(name, kind, immutable, predefined, err)
	}
	if applied, err := migrationApplied(store.db, "labels_metadata_v2"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	if _, err := store.db.Exec(`UPDATE labels SET type=3, immutable=1, predefined_id=7 WHERE id='1';
	 DELETE FROM schema_migrations WHERE name='labels_metadata_v2'`); err != nil {
		t.Fatal(err)
	}
	if err := ensureMessageStoreSchema(store.db); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT type, immutable, predefined_id FROM labels WHERE id='1'").Scan(&kind, &immutable, &predefined); err != nil || kind != 3 || !immutable || predefined != int64(7) {
		t.Fatal(kind, immutable, predefined, err)
	}
}

func TestLabelsEventReplayAndPersistence(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, newTestClient(nil), store, testLogger())
	ts := time.Date(2026, 1, 1, 12, 0, 0, 100000000, time.FixedZone("test", -3*3600))
	first := labelEvent("1", "Alice", ts, false)
	first.FromFullSync = true
	b.handleEvent(first, nil)
	jid, _ := types.ParseJID(archiveTestChat)
	assoc := &events.LabelAssociationChat{LabelID: "1", JID: jid, Timestamp: ts, FromFullSync: true, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(true)}}
	b.handleEvent(assoc, nil)
	got, err := store.listLabels([]string{archiveTestChat}, false)
	if err != nil || len(got) != 1 || got[0].Name != "Alice" {
		t.Fatal(got, err)
	}
	newer := ts.Add(500 * time.Millisecond)
	b.handleEvent(labelEvent("1", "", newer, true), nil)
	b.handleEvent(first, nil) // older within the same canonical second must not resurrect
	assoc.Timestamp = newer
	assoc.FromFullSync = false
	assoc.Action.Labeled = proto.Bool(false)
	b.handleEvent(assoc, nil)
	assoc.Timestamp = ts
	assoc.Action.Labeled = proto.Bool(true)
	b.handleEvent(assoc, nil)
	got, err = store.listLabels(nil, true)
	if err != nil || len(got) != 1 || !got[0].Deleted || got[0].Name != "Alice" {
		t.Fatal(got, err)
	}
	got, err = store.listLabels([]string{archiveTestChat}, true)
	if err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	var stamp string
	var ms int64
	if err := store.db.QueryRow("SELECT CAST(updated_at AS TEXT), action_ms FROM labels WHERE id='1'").Scan(&stamp, &ms); err != nil {
		t.Fatal(err)
	}
	if stamp != dbTime(newer) || ms != newer.UnixMilli() {
		t.Fatal(stamp, ms)
	}
	if _, err := store.db.Exec("UPDATE labels SET updated_at='2026-01-01T12:00:00-03:00'; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err := ensureMessageStoreSchema(store.db); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT CAST(updated_at AS TEXT) FROM labels WHERE id='1'").Scan(&stamp); err != nil || stamp != dbTime(newer) {
		t.Fatal(stamp, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err = reopened.listLabels(nil, true)
	if err != nil || len(got) != 1 || !got[0].Deleted {
		t.Fatal(got, err)
	}
	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal(version, err)
	}
	for _, marker := range []string{"labels_schema_v1", "labels_metadata_v2", canonicalTimestampsMigration} {
		if applied, err := migrationApplied(reopened.db, marker); err != nil || !applied {
			t.Fatal(marker, applied, err)
		}
	}
}

func TestLabelMalformedChatBeforeEffects(t *testing.T) {
	for _, raw := range []string{"", "@lid", "5511999999999", "5511999999999@", "5511999999999:1@s.whatsapp.net", "5511999999999@s.whatsapp.net@lid"} {
		b := testBridge(t, nil, newTestMessageStore(t), testLogger())
		b.Connected = func() bool { t.Fatal("malformed chat reached connection check"); return false }
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			url := "http://127.0.0.1:8080/api/labels?chat_jid=" + raw
			if method == http.MethodPost {
				url = "http://127.0.0.1:8080/api/chat/label"
			}
			req := httptest.NewRequest(method, url, strings.NewReader(`{"chat_jid":"`+raw+`","label_id":"1","labeled":true}`))
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != 400 {
				t.Fatal(raw, method, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestLabelsRealHTTPAndEventCache(t *testing.T) {
	pn, _ := types.ParseJID(archiveTestChat)
	lid, _ := types.ParseJID("100000000000001@lid")
	b := testBridge(t, newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{lid: pn}, lidByPN: map[types.JID]types.JID{pn: lid}}), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	b.handleEvent(labelEvent("1", "Alice", time.Now(), false), nil)
	patches := make(chan appstate.PatchInfo, 2)
	b.SendAppState = func(_ context.Context, patch appstate.PatchInfo) error { patches <- patch; return nil }
	srv := httptest.NewServer(b.newRESTMux(8080, readOnlyTestToken))
	defer srv.Close()
	client := srv.Client()
	for _, flag := range []string{"true", "false"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/chat/label", strings.NewReader(`{"chat_jid":" 5511999999999@S.WHATSAPP.NET ","label_id":"1","labeled":`+flag+`}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		req.Host = "127.0.0.1:8080"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || body["confirmed"] != false {
			t.Fatal(body, err, resp.StatusCode)
		}
		patch := <-patches
		if patch.Type != appstate.WAPatchRegular || patch.Mutations[0].Index[2] != lid.String() {
			t.Fatal(patch)
		}
		// Phone confirmation is represented by the actual event consumer, which owns the cache.
		b.handleEvent(&events.LabelAssociationChat{JID: lid, LabelID: "1", Timestamp: time.Now(), Action: patch.Mutations[0].Value.GetLabelAssociationAction()}, nil)
		req, err = http.NewRequest(http.MethodGet, srv.URL+"/api/labels?chat_jid="+archiveTestChat, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		req.Host = "127.0.0.1:8080"
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Labels []labelRecord `json:"labels"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		want := 0
		if flag == "true" {
			want = 1
		}
		if err != nil || resp.StatusCode != 200 || len(result.Labels) != want {
			t.Fatal(flag, result, err)
		}
	}
}

func TestLabelsUnusedFullSyncEventsAreIgnored(t *testing.T) {
	log := &recordingLogger{}
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(nil), store, log)
	for _, evt := range []any{&events.AppState{}, &events.Mute{}, &events.Pin{}, &events.Archive{}, &events.Contact{}, &events.ClearChat{}, &events.DeleteChat{}, &events.Star{}, &events.DeleteForMe{}, &events.MarkChatAsRead{}, &events.PushNameSetting{}, &events.UnarchiveChatsSetting{}, &events.UserStatusMute{}, &events.LabelAssociationMessage{}, &events.AppStateSyncComplete{}, &events.AppStateSyncError{}} {
		b.handleEvent(evt, nil)
	}
	b.handleEvent(&events.LabelEdit{}, nil)
	b.handleEvent(&events.LabelAssociationChat{}, nil)
	if got := log.String(); got != "" {
		t.Fatal(got)
	}
	for _, table := range []string{"labels", "chat_labels", "messages", "chats"} {
		var n int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestLabelDeletionWinsEqualTimestampReplay(t *testing.T) {
	store := newTestMessageStore(t)
	ts := time.Now()
	for _, e := range []*events.LabelEdit{labelEvent("1", "Alice", ts, false), labelEvent("1", "", ts, true), labelEvent("1", "Alice", ts, false)} {
		if err := store.storeLabel(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.listLabels(nil, true)
	if err != nil || len(got) != 1 || !got[0].Deleted || got[0].Name != "Alice" {
		t.Fatal(got, err)
	}
}

func TestLabelGroupThroughMux(t *testing.T) {
	store := newTestMessageStore(t)
	if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, newTestClient(nil), store, testLogger())
	b.Connected = func() bool { return true }
	group := "120363000000000001@g.us"
	calls := 0
	b.SendAppState = func(_ context.Context, p appstate.PatchInfo) error {
		calls++
		if p.Mutations[0].Index[2] != group || !p.Mutations[0].Value.GetLabelAssociationAction().GetLabeled() {
			t.Fatal(p)
		}
		return nil
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/chat/label", strings.NewReader(`{"chat_jid":"`+group+`","label_id":"1","labeled":true}`))
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	rec := httptest.NewRecorder()
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
	if rec.Code != 200 || calls != 1 || !strings.Contains(rec.Body.String(), `"label_id":"1"`) {
		t.Fatal(rec.Code, calls, rec.Body.String())
	}
}

func TestLabelsLatestPermittedTwinAssociation(t *testing.T) {
	const lid = "100000000000001@lid"
	for _, requested := range []string{archiveTestChat, lid} {
		for _, sameTime := range []bool{false, true} {
			for _, restricted := range []bool{false, true} {
				store := newTestMessageStore(t)
				ts := time.Now()
				if err := store.storeLabel(labelEvent("1", "Alice", ts, false)); err != nil {
					t.Fatal(err)
				}
				if err := store.storeChatLabel(lid, &events.LabelAssociationChat{LabelID: "1", Timestamp: ts, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(true)}}); err != nil {
					t.Fatal(err)
				}
				if !sameTime {
					ts = ts.Add(time.Millisecond)
				}
				if err := store.storeChatLabel(archiveTestChat, &events.LabelAssociationChat{LabelID: "1", Timestamp: ts, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(false)}}); err != nil {
					t.Fatal(err)
				}
				policy := chatPolicy{}
				if restricted {
					policy = parseChatPolicy(requested)
				}
				twin := func(_ context.Context, jid types.JID) (types.JID, error) {
					if jid.Server == types.HiddenUserServer {
						return types.ParseJID(archiveTestChat)
					}
					return types.ParseJID(lid)
				}
				rec := httptest.NewRecorder()
				handleLabels(store, policy, twin)(rec, httptest.NewRequest(http.MethodGet, "/api/labels?chat_jid="+requested, nil))
				wantEmpty := !restricted || requested == archiveTestChat
				if rec.Code != 200 || strings.Contains(rec.Body.String(), `"labels":[]`) != wantEmpty {
					t.Fatal(requested, sameTime, restricted, rec.Code, rec.Body.String())
				}
			}
		}
	}
}

func TestLabelAssociationPreservesSourceNamespaceAndRemoval(t *testing.T) {
	pn, _ := types.ParseJID(archiveTestChat)
	lid, _ := types.ParseJID("100000000000001@lid")
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{lid: pn}, lidByPN: map[types.JID]types.JID{pn: lid}}), store, testLogger())
	b.Policy = parseChatPolicy(lid.String())
	ts := time.Now()
	if err := store.storeLabel(labelEvent("1", "Alice", ts, false)); err != nil {
		t.Fatal(err)
	}
	e := &events.LabelAssociationChat{JID: lid, LabelID: "1", Timestamp: ts, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(true)}}
	b.handleEvent(e, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/labels?chat_jid="+lid.String(), nil)
	req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
	b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"1"`) {
		t.Error(rec.Code, rec.Body.String())
	}
	e.JID = pn
	e.Action.Labeled = proto.Bool(false)
	b.handleEvent(e, nil)
	e.JID = lid
	e.Action.Labeled = proto.Bool(true)
	e.FromFullSync = true
	b.handleEvent(e, nil)
	got, err := store.listLabels([]string{pn.String(), lid.String()}, false)
	if err != nil || len(got) != 0 {
		t.Error(got, err)
	}
	e.Action.Labeled = proto.Bool(false)
	b.handleEvent(e, nil)
	e.Action.Labeled = proto.Bool(true)
	b.handleEvent(e, nil)
	got, err = store.listLabels([]string{lid.String()}, false)
	if err != nil || len(got) != 0 {
		t.Error("equal-time replay resurrected removal", got, err)
	}
}

func TestLabelSyncIsAsyncOnceAndBounded(t *testing.T) {
	client := newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer))
	b := testBridge(t, client, newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	b.LabelResyncTimeout = 30 * time.Millisecond
	entered, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b.LabelResync = func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	}
	b.handleEvent(&events.Connected{}, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
	}
	b.startLabelSync()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("unbounded sync")
	}
	b.labelSyncWait.Wait()
	b.startLabelSync()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestLabelSyncEligibilityAndShutdown(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		paired, connected, populated bool
	}{{"unpaired", false, true, false}, {"disconnected", true, false, false}, {"populated", true, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(nil)
			if tc.paired {
				client = newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer))
			}
			store := newTestMessageStore(t)
			if tc.populated {
				if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
					t.Fatal(err)
				}
			}
			b := testBridge(t, client, store, testLogger())
			b.Connected = func() bool { return tc.connected }
			b.LabelResync = func(context.Context) error { t.Error("ineligible sync"); return nil }
			b.startLabelSync()
			b.labelSyncWait.Wait()
			b.Shutdown(time.Second)
			b.startLabelSync()
		})
	}
}

func TestLabelSyncShutdownCancelsFetch(t *testing.T) {
	b := testBridge(t, newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer)), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	entered, finished := make(chan struct{}), make(chan struct{})
	b.LabelResync = func(ctx context.Context) error { close(entered); <-ctx.Done(); close(finished); return ctx.Err() }
	b.startLabelSync()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
	}
	b.Shutdown(time.Second)
	select {
	case <-finished:
	default:
		t.Fatal("shutdown did not wait for cancelled fetch")
	}
	b.startLabelSync()
}

func TestLabelSyncSerializesWithOutboundAppState(t *testing.T) {
	b := testBridge(t, newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer)), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return true }
	entered, release := make(chan struct{}), make(chan struct{})
	b.LabelResync = func(context.Context) error { close(entered); <-release; return nil }
	sends := 0
	b.SendAppState = func(context.Context, appstate.PatchInfo) error { sends++; return nil }
	b.startLabelSync()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sync did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := b.sendAppState(ctx, appstate.PatchInfo{})
	close(release)
	b.labelSyncWait.Wait()
	if !errors.Is(err, context.DeadlineExceeded) || sends != 0 {
		t.Fatal(err, sends)
	}
	if err := b.sendAppState(context.Background(), appstate.PatchInfo{}); err != nil || sends != 1 {
		t.Fatal(err, sends)
	}
}

func TestLabelSyncRechecksCacheAfterWaiting(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClientWithSelf(nil, types.NewJID("5511999999999", types.DefaultUserServer)), store, testLogger())
	b.Connected = func() bool { return true }
	entered, release, sent := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	b.SendAppState = func(context.Context, appstate.PatchInfo) error { close(entered); <-release; return nil }
	b.LabelResync = func(context.Context) error { t.Error("cache became populated while queued"); return nil }
	go func() { sent <- b.sendAppState(context.Background(), appstate.PatchInfo{}) }()
	<-entered
	b.startLabelSync()
	if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	b.labelSyncWait.Wait()
}

func TestLabelStoreFailureIsCounted(t *testing.T) {
	store := newTestMessageStore(t)
	log := &recordingLogger{}
	b := testBridge(t, newTestClient(nil), store, log)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	b.recordLabel(labelEvent("1", "Alice", time.Now(), false))
	if !strings.Contains(log.String(), "[ERROR]") || strings.Count(log.String(), "[ERROR]") != 1 {
		t.Fatal(log.String())
	}
	if b.metrics.storeFailures.Load() != 1 {
		t.Fatal("lost label write was not counted")
	}
}

func TestLabelProductionWiring(t *testing.T) {
	client := newTestClient(nil)
	b := newBridge(client, newTestMessageStore(t), testLogger(), "", nil, testSwitches())
	defer b.Shutdown(time.Second)
	if !client.EmitAppStateEventsOnFullSync || b.LabelResync == nil || b.LabelResyncTimeout <= 0 || b.SendAppState == nil || cap(b.appStateGate) != 1 {
		t.Fatal("missing production label/full-sync wiring")
	}
}

func TestLabelValidationAndCancellation(t *testing.T) {
	store := newTestMessageStore(t)
	deps := labelDeps{store: store, connected: func() bool { return true },
		resolve: func(context.Context, string) (types.JID, error) {
			t.Fatal("lookup after refusal")
			return types.EmptyJID, nil
		},
		send: func(context.Context, appstate.PatchInfo) error { t.Fatal("send after refusal"); return nil },
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{`, 400},
		{`{"chat_jid":"` + archiveTestChat + `","label_id":"1"}`, 400},
		{`{"chat_jid":"status@broadcast","label_id":"1","labeled":true}`, 400},
		{`{"chat_jid":"` + archiveTestChat + `","label_id":"1","labeled":true}`, 404},
	} {
		rec := httptest.NewRecorder()
		handleLabelChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	handleLabelChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","label_id":"1","labeled":true}`)).WithContext(ctx))
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), "nothing sent") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestLabelCachedGETFilterAndAuthentication(t *testing.T) {
	store := newTestMessageStore(t)
	if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
		t.Fatal(err)
	}
	jid, _ := types.ParseJID(archiveTestChat)
	if err := store.storeChatLabel(archiveTestChat, &events.LabelAssociationChat{JID: jid, LabelID: "1", Timestamp: time.Now(), Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(true)}}); err != nil {
		t.Fatal(err)
	}
	b := testBridge(t, nil, store, testLogger())
	b.Connected = func() bool { t.Fatal("read requires no socket"); return false }
	for _, tc := range []struct {
		path, method string
		token        bool
		status       int
		contains     string
	}{
		{"/api/labels", http.MethodGet, false, 401, ""},
		{"/api/labels", http.MethodPost, true, 405, ""},
		{"/api/labels?include_deleted=maybe", http.MethodGet, true, 400, ""},
		{"/api/labels?chat_jid=" + archiveTestChat, http.MethodGet, true, 200, `"id":"1"`},
		{"/api/labels?chat_jid=5511888888888@s.whatsapp.net", http.MethodGet, true, 200, `"labels":[]`},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:8080"+tc.path, nil)
		if tc.token {
			req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		}
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}

func TestLabelChatPatchAndFailures(t *testing.T) {
	for _, labeled := range []bool{true, false} {
		store := newTestMessageStore(t)
		if err := store.storeLabel(labelEvent("1", "Alice", time.Now(), false)); err != nil {
			t.Fatal(err)
		}
		calls := 0
		deps := labelDeps{store: store, connected: func() bool { return true }, resolve: func(context.Context, string) (types.JID, error) { return types.ParseJID("100000000000001@lid") }, send: func(ctx context.Context, p appstate.PatchInfo) error {
			calls++
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 20*time.Second {
				t.Error("unbounded request")
			}
			if p.Type != appstate.WAPatchRegular || p.Mutations[0].Index[1] != "1" || p.Mutations[0].Index[2] != "100000000000001@lid" || p.Mutations[0].Value.GetLabelAssociationAction().GetLabeled() != labeled {
				t.Fatal(p)
			}
			return nil
		}}
		flag := "false"
		if labeled {
			flag = "true"
		}
		body := `{"chat_jid":"` + archiveTestChat + `","label_id":"1","labeled":` + flag + `}`
		rec := httptest.NewRecorder()
		handleLabelChat(deps)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if rec.Code != 200 || calls != 1 || !strings.Contains(rec.Body.String(), `"confirmed":false`) {
			t.Fatal(rec.Code, calls, rec.Body.String())
		}
		for _, tc := range []struct {
			name   string
			status int
		}{{"deleted", 404}, {"unknown", 404}, {"disconnected", 503}, {"send", 502}} {
			t.Run(tc.name, func(t *testing.T) {
				local := deps
				local.connected = func() bool { return tc.name != "disconnected" }
				local.send = func(context.Context, appstate.PatchInfo) error { return errors.New("transport failed") }
				testBody := body
				if tc.name == "unknown" {
					testBody = strings.ReplaceAll(body, `"label_id":"1"`, `"label_id":"2"`)
				}
				if tc.name == "deleted" {
					if err := store.storeLabel(labelEvent("1", "Alice", time.Now().Add(time.Hour), true)); err != nil {
						t.Fatal(err)
					}
				}
				rec := httptest.NewRecorder()
				handleLabelChat(local)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testBody)))
				if rec.Code != tc.status {
					t.Fatal(rec.Code, rec.Body.String())
				}
				if tc.name == "deleted" {
					if err := store.storeLabel(labelEvent("1", "Alice", time.Now().Add(2*time.Hour), false)); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestLabelMuxGuardsAndCachedRead(t *testing.T) {
	for _, tc := range []struct {
		name, allow, deny, chats string
		readonly, token          bool
		status                   int
	}{{"token", "", "", "", false, false, 401}, {"readonly", "label_chat", "", "", true, true, 403}, {"allow", "archive_chat", "", "", false, true, 403}, {"deny", "label_chat", "label_chat", "", false, true, 403}, {"chat", "", "", "5511888888888", false, true, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge(t, nil, newTestMessageStore(t), testLogger())
			b.ReadOnly = readOnlyPolicy{enabled: tc.readonly}
			b.Tools = mustToolPolicy(t, tc.allow, tc.deny)
			b.Policy = parseChatPolicy(tc.chats)
			b.Connected = func() bool { t.Fatal("past guard"); return false }
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/chat/label", strings.NewReader(`{"chat_jid":"`+archiveTestChat+`","label_id":"1","labeled":true}`))
			if tc.token {
				req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
			}
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatal(rec.Code, rec.Body.String())
			}
		})
	}
	store := newTestMessageStore(t)
	b := testBridge(t, nil, store, testLogger())
	b.ReadOnly = readOnlyPolicy{enabled: true}
	b.Connected = func() bool { t.Fatal("cached read needs no connection"); return false }
	for _, path := range []string{"/api/labels", "/api/labels?include_deleted=1"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil)
		req.Header.Set("Authorization", "Bearer "+readOnlyTestToken)
		b.newRESTMux(8080, readOnlyTestToken).ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"labels":[]`) {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	b.Policy = parseChatPolicy("5511888888888")
	rec := httptest.NewRecorder()
	handleLabels(store, b.Policy, nil)(rec, httptest.NewRequest(http.MethodGet, "/api/labels?chat_jid="+archiveTestChat, nil))
	if rec.Code != 403 {
		t.Fatal(rec.Code)
	}
}
