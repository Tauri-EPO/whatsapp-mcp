package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func archiveFakeGroup(t *testing.T, b *Bridge) {
	t.Helper()
	if err := b.Store.StoreChat(testGroupJID, "Fake group", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSecondReviewSettingsUseLocalObservationClock(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	archiveFakeGroup(t, b)
	info := fakeGroup()
	info.Topic = "Snapshot"
	// Host is an hour ahead of the server. A subsequent real notification
	// must still replace the snapshot, even within the same stored second.
	server := time.Now().Add(-time.Hour)
	if _, err := b.storeFetchedGroupInfo(testGroupJID, info, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.applyGroupSettings(chatPolicy{}, &events.GroupInfo{JID: info.JID, Timestamp: server, Topic: &types.GroupTopic{Topic: "Event"}})
	var topic, stamp string
	if err := b.Store.db.QueryRow(`SELECT group_topic, CAST(group_topic_updated_at AS TEXT) FROM chats WHERE jid=?`, testGroupJID).Scan(&topic, &stamp); err != nil {
		t.Fatal(err)
	}
	if topic != "Event" || stamp <= dbTime(server) {
		t.Fatalf("event lost to host clock: %q at %s", topic, stamp)
	}
	// The next fetch is the last local observation, including equal seconds.
	// Its participant sweep stamp is old, but settings are observed now.
	if _, err := b.storeFetchedGroupInfo(testGroupJID, info, server); err != nil {
		t.Fatal(err)
	}
	if err := b.Store.db.QueryRow(`SELECT group_topic FROM chats WHERE jid=?`, testGroupJID).Scan(&topic); err != nil || topic != "Snapshot" {
		t.Fatalf("fetch lost: %q err=%v", topic, err)
	}
}

func TestSecondReviewSettingsDoNotCreateChat(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	info := fakeGroup()
	b.applyGroupSettings(chatPolicy{}, &events.GroupInfo{JID: info.JID, Topic: &types.GroupTopic{Topic: "Unarchived"}})
	if _, err := b.storeFetchedGroupInfo(testGroupJID, info, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.Store.db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ghost chats=%d err=%v", n, err)
	}
	if len(readMembers(t, b.Store, testGroupJID)) == 0 {
		t.Fatal("roster should still be stored")
	}
}

func TestSecondReviewSettingsFailureDoesNotBlockRoster(t *testing.T) {
	logger := &recordingLogger{}
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), logger)
	archiveFakeGroup(t, b)
	if _, err := b.Store.db.Exec(`CREATE TRIGGER refuse_settings BEFORE UPDATE OF group_topic ON chats BEGIN SELECT RAISE(FAIL,'synthetic settings failure'); END`); err != nil {
		t.Fatal(err)
	}
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) { return fakeGroup(), nil }
	if !b.refreshGroupRoster(context.Background(), testGroupJID, fakeGroup().JID, time.Now()) {
		t.Fatal("settings failure blocked roster")
	}
	if len(readMembers(t, b.Store, testGroupJID)) == 0 || b.rosterFailures.recent(testGroupJID, time.Now()) {
		t.Fatal("settings failure poisoned roster backoff")
	}
	if log := logger.String(); !strings.Contains(log, testGroupJID) || !strings.Contains(log, "synthetic settings failure") {
		t.Fatal(log)
	}
}

func TestSecondReviewSubjectDoesNotRecordOldTopic(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	archiveFakeGroup(t, b)
	if _, err := b.Store.db.Exec(`UPDATE chats SET group_topic='Current' WHERE jid=?`, testGroupJID); err != nil {
		t.Fatal(err)
	}
	info := fakeGroup()
	info.Topic = "Stale fetch"
	ops := groupOps{getInfo: func(context.Context, types.JID) (*types.GroupInfo, error) { return info, nil }, setTopic: func(context.Context, types.JID, groupTopicUpdate) error { return nil }}
	code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}, b.recordGroupRoster), fmt.Sprintf(`{"group_jid":%q,"description":"New"}`, testGroupJID))
	if code != 200 || !resp.Success {
		t.Fatalf("%d %+v", code, resp)
	}
	var topic string
	if err := b.Store.db.QueryRow(`SELECT group_topic FROM chats WHERE jid=?`, testGroupJID).Scan(&topic); err != nil || topic != "Current" {
		t.Fatalf("old topic recorded: %q %v", topic, err)
	}
}

func TestSecondReviewRefreshDisconnected(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.Connected = func() bool { return false }
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		return nil, fmt.Errorf("disconnected fetch")
	}
	b.requestGroupSettingsRefresh(fakeGroup().JID)
	// A disconnected event never initializes or queues a worker.
	if b.groupSettingsRefresh != nil {
		t.Fatal("disconnected refresh queued")
	}
}

func TestSecondReviewRefreshDeduplicatesWithoutBackoff(t *testing.T) {
	logger := &recordingLogger{}
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), logger)
	b.Connected = func() bool { return true }
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	b.Store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, fmt.Errorf("synthetic fetch failure")
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	b.requestGroupSettingsRefresh(fakeGroup().JID)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no refresh")
	}
	for range 20 {
		b.requestGroupSettingsRefresh(fakeGroup().JID)
	}
	if len(b.groupSettingsRefresh) != 1 {
		t.Fatalf("want exactly one follow-up for notifications during fetch, queued=%d", len(b.groupSettingsRefresh))
	}
	close(release)
	released = true
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("notifications during fetch were lost or duplicated: calls=%d", calls.Load())
	}
	if b.rosterFailures.recent(testGroupJID, time.Now()) {
		t.Fatal("event refresh poisoned roster backoff")
	}
	b.cancel()
}

type secondReviewMediaStorage struct {
	mediaStorage
	fail error
	data []byte
}

func (s secondReviewMediaStorage) Open(context.Context, mediaRow, string) (io.ReadCloser, int64, error) {
	if s.fail != nil {
		return nil, 0, s.fail
	}
	return io.NopCloser(bytes.NewReader(s.data)), int64(len(s.data)), nil
}

func TestSecondReviewCachedStickerPermanentErrors(t *testing.T) {
	for _, sentinel := range []error{errMediaUnavailable, errMediaRefused} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
			b.MediaStorage = secondReviewMediaStorage{fail: sentinel}
			if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,filename) VALUES ('CACHE',?,'100000000001','',?,0,'image','fake.webp')`, testGroupJID, dbTime(time.Now())); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(SendMessageRequest{Recipient: testGroupJID, MediaPath: "whatsapp://media/" + testGroupJID + "/CACHE", AsSticker: true})
			w := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(w, seamRequest("POST", "/api/send", string(body), sendRecipientToken))
			if w.Code != 500 || !strings.Contains(w.Body.String(), permanentMediaCode(sentinel)) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSecondReviewStickerSizeLimit(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprint(preview), func(t *testing.T) {
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
			p := filepath.Join(t.TempDir(), "oversized.webp")
			data := append(stickerFixture(false), make([]byte, 1<<20)...)
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			b.MediaRoots = []string{filepath.Dir(p)}
			b.Send = func(context.Context, string, string, string, string, string, string, []string) (bool, string, sentMessage) {
				t.Error("oversized sticker reached sender")
				return false, "", sentMessage{}
			}
			body, _ := json.Marshal(SendMessageRequest{Recipient: testGroupJID, MediaPath: p, AsSticker: true, DryRun: preview})
			w := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(w, seamRequest("POST", "/api/send", string(body), sendRecipientToken))
			if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_argument") || !strings.Contains(w.Body.String(), "1 MiB") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSecondReviewStickerPreviewReadsHeaderOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "preview.webp")
	data := append(stickerFixture(true), make([]byte, 500000)...)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	header, err := readStickerMedia(p, true)
	if err != nil || len(header) != 30 || !webPAnimated(header) {
		t.Fatalf("header len=%d animated=%v err=%v", len(header), webPAnimated(header), err)
	}
}

func TestSecondReviewCachePreservesForwardSource(t *testing.T) {
	b := testBridge(t, newTestClient(&mockLIDStore{}), newTestMessageStore(t), testLogger())
	b.MediaStorage = secondReviewMediaStorage{data: stickerFixture(false)}
	if _, err := b.Store.db.Exec(`INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,filename) VALUES ('CACHE',?,'100000000001','',?,0,'image','fake.webp')`, testGroupJID, dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	original := &forwardSource{mediaType: "sticker", id: "ORIGINAL"}
	ctx := context.WithValue(context.Background(), forwardSourceKey{}, original)
	resolved, path, cleanup, err := b.resolveSendMedia(ctx, "whatsapp://media/"+testGroupJID+"/CACHE")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if resolved.Value(forwardSourceKey{}) != original {
		t.Fatal("forward presentation replaced")
	}
	if resolved.Value(cachedSendSourceKey{}).(*forwardSource).id != "CACHE" {
		t.Fatal("cache source missing")
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the bridge-created test spool, never user input.
	if err != nil || !bytes.Equal(data, stickerFixture(false)) {
		t.Fatalf("materialized bytes=%x err=%v", data, err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("spool not cleaned")
	}
}
