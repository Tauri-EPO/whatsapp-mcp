package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestLiveLocationKeyPolicy(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "history"}[history], func(t *testing.T) {
			ms := newTestMessageStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			b.MediaAutoDownload = false
			srv, posts, _ := contentKindsWebhook(t)
			b.Webhook = newWebhookSender("", true)
			b.Webhook.url = srv.URL
			timestamp := time.Unix(1772359200, 0)
			feed := func(id string, chat types.JID, sequence *int64, latitude float64) {
				m := &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
					DegreesLatitude: proto.Float64(latitude), DegreesLongitude: proto.Float64(0.5),
					Caption: proto.String("Meet — here"), SequenceNumber: sequence,
				}}
				if history {
					b.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
						Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat.String()),
							Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
								Key:              &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(false)},
								MessageTimestamp: proto.Uint64(uint64(timestamp.Unix())), Message: m, //nolint:gosec // fixed positive fake epoch, advanced by minutes only
							}}},
						}},
					}})
				} else {
					event := buildTextMessage(chat, phonePN, types.EmptyJID, types.EmptyJID, false, "")
					event.Info.ID, event.Info.Timestamp, event.Message = id, timestamp, m
					b.handleMessage(event)
				}
				timestamp = timestamp.Add(time.Minute)
			}
			feed("SHARE", phonePN, nil, 0.25)
			var firstContent, firstTime string
			if err := ms.db.QueryRow("SELECT content, timestamp FROM messages WHERE id='SHARE' AND chat_jid=?", phonePN.String()).Scan(&firstContent, &firstTime); err != nil {
				t.Fatal(err)
			}
			feed("SHARE", phonePN, proto.Int64(2), 0.75)
			feed("SHARE", phonePN, proto.Int64(1), 0.4) // stale replay
			var activity time.Time
			if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid=?", phonePN.String()).Scan(&activity); err != nil || activity.UTC().Format(time.RFC3339) != firstTime {
				t.Fatalf("position became new conversation activity: %v err=%v", activity, err)
			}
			feed("OTHER", phonePN, proto.Int64(3), 0.8) // never guess an unrelated key
			feed("SHARE", types.NewJID("120363000000000001", types.GroupServer), proto.Int64(4), 0.9)
			var content, stamp, raw string
			if err := ms.db.QueryRow("SELECT content, timestamp, location FROM messages WHERE id='SHARE' AND chat_jid=?", phonePN.String()).Scan(&content, &stamp, &raw); err != nil {
				t.Fatal(err)
			}
			var p messageLocation
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				t.Fatal(err)
			}
			if content != firstContent || stamp != firstTime || p.Latitude == nil || *p.Latitude != 0.75 || p.Sequence == nil || *p.Sequence != 2 || p.Comment != "Meet — here" {
				t.Fatalf("update changed the first row or lost latest position: content=%q timestamp=%q location=%s", content, stamp, raw)
			}
			var count int
			if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 3 {
				t.Fatalf("distinct-key samples lost: count=%d err=%v", count, err)
			}
			wantPosts := int32(3)
			if history {
				wantPosts = 0
			}
			if posts.Load() != wantPosts {
				t.Fatalf("position updates produced webhooks: %d want %d", posts.Load(), wantPosts)
			}
			if ok, _, _, _, err := b.downloadMedia(context.Background(), "SHARE", phonePN.String()); ok || err == nil {
				t.Fatal("location must not be downloaded as a file")
			}
		})
	}
}

func TestLocationNativeFieldsAndSparseValues(t *testing.T) {
	for _, m := range []*waE2E.Message{
		{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Park — East"), Address: proto.String("Street — 1"), DegreesLatitude: proto.Float64(math.NaN()), DegreesLongitude: proto.Float64(0.5)}},
		{LiveLocationMessage: &waE2E.LiveLocationMessage{SpeedInMps: proto.Float32(float32(math.Inf(1)))}},
	} {
		e := extractMessage(m, time.Now(), "SPARSE")
		if e.empty() || e.mediaType != "location" || e.location.column() == nil {
			t.Fatalf("sparse native envelope lost: %+v", e)
		}
		if e.location.Latitude != nil || e.location.Longitude != nil || e.location.Speed != nil {
			t.Fatalf("invalid numbers retained: %+v", e.location)
		}
	}
}

func TestLocationNewestFirstHistoryAndMigration(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1772359200, 0)
	b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
	b.MediaAutoDownload = false
	fixture := largeHistoryFixture(2)
	for i, sequence := range []int64{3, 0} {
		lat, offset := 0.8, int64(60)
		if sequence == 0 {
			lat, offset = 0.25, 0
		}
		m := &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{SequenceNumber: proto.Int64(sequence), DegreesLatitude: proto.Float64(lat), DegreesLongitude: proto.Float64(0.5), Caption: proto.String("initial")}}
		fixture.Data.Conversations[0].Messages[i].Message = &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String("CANONICAL"), RemoteJID: proto.String(phonePN.String()), FromMe: proto.Bool(false)},
			MessageTimestamp: proto.Uint64(uint64(stamp.Unix() + offset)), Message: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: m}}, //nolint:gosec // fixed positive fake epoch plus 0 or 60 seconds
		}
	}
	b.handleHistorySync(fixture)
	var activity time.Time
	var latestLatitude float64
	if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid=?", phonePN.String()).Scan(&activity); err != nil || !activity.Equal(stamp) {
		t.Fatalf("newest-first update advanced chat activity: %v %v", activity, err)
	}
	if err := ms.db.QueryRow("SELECT json_extract(location,'$.latitude') FROM messages WHERE id='CANONICAL'").Scan(&latestLatitude); err != nil || latestLatitude != 0.8 {
		t.Fatalf("canonical wrapped history lost latest position: %v %v", latestLatitude, err)
	}
	for _, sample := range []struct {
		seq     int64
		lat     float64
		caption string
	}{{3, 0.8, "later"}, {0, 0.25, "initial"}} {
		e := extractMessage(&waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
			SequenceNumber: proto.Int64(sample.seq), DegreesLatitude: proto.Float64(sample.lat), DegreesLongitude: proto.Float64(0.5), Caption: proto.String(sample.caption),
		}}, stamp, "HISTORY")
		if err := ms.Batch(func(w *messageBatch) error {
			return persistMessage(w, "HISTORY", phonePN.String(), phonePN.String(), stamp, false, e, true, testLogger())
		}); err != nil {
			t.Fatal(err)
		}
	}
	var lat float64
	var content string
	if err := ms.db.QueryRow("SELECT content, json_extract(location,'$.latitude') FROM messages WHERE id='HISTORY'").Scan(&content, &lat); err != nil || lat != 0.8 || content != "📍 Live location (0.250000, 0.500000) — initial" {
		t.Fatalf("newest-first replay lost metadata/position: %q %v %v", content, lat, err)
	}
	if err := ms.StoreMessage("OLD", phonePN.String(), phonePN.String(), "📍 Old — text", stamp, false, "", "", "", nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec("ALTER TABLE messages DROP COLUMN location"); err != nil {
		t.Fatal(err)
	}
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	ms, err = NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := ms.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id='OLD' AND content='📍 Old — text' AND COALESCE(media_type,'')='' AND location IS NULL").Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("migration guessed or erased legacy text: %d %v", retained, err)
	}
}

func TestForwardLocationRefusedBeforeRemoteEffects(t *testing.T) {
	ms := newTestMessageStore(t)
	if err := ms.EnsureChat(phonePN.String(), "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("LOCATION", phonePN.String(), phonePN.String(), "📍 place", time.Now(), false, "location", "", "", nil, nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	deps := forwardDeps{lookup: ms.messageContentLookup,
		resolveRecipient: func(context.Context, http.ResponseWriter, string) (string, bool) {
			t.Fatal("location resolved remotely")
			return "", false
		},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/forward", strings.NewReader(`{"chat_jid":"`+phonePN.String()+`","message_id":"LOCATION","to_chat_jid":"`+phonePN.String()+`"}`))
	handleForwardMessage(deps, chatPolicy{})(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot forward a location") {
		t.Fatalf("forward response=%d %s", w.Code, w.Body.String())
	}
}

func TestLocationLIDMigrationPreservesFieldsAndUpdates(t *testing.T) {
	ms := newTestMessageStore(t)
	stamp := time.Unix(1772359200, 0)
	if err := ms.EnsureChat("111@lid", "Alice"); err != nil {
		t.Fatal(err)
	}
	m := &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{DegreesLatitude: proto.Float64(0.25), DegreesLongitude: proto.Float64(0.5), Caption: proto.String("Park — East"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("Q1"), MentionedJID: []string{phonePN.String()}},
	}}
	if err := persistMessage(ms, "LOCATION", "111@lid", "111@lid", stamp, false, extractMessage(m, stamp, "LOCATION"), true, testLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.db.Exec("UPDATE messages SET deleted_at=?, view_once=1, target_message_id='TARGET' WHERE id='LOCATION'", dbTime(stamp)); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := ms.db.QueryRow("SELECT location FROM messages WHERE id='LOCATION'").Scan(&original); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	waDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waDB.Close() }()
	if _, err := waDB.Exec("CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT NOT NULL); INSERT INTO whatsmeow_lid_map VALUES('111','222')"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ms.MigrateLegacyLIDChatsToPhoneJIDs(path, testLogger()); err != nil {
			t.Fatal(err)
		}
	}
	var raw, quote, mentions, target string
	var deleted sql.NullTime
	var viewOnce bool
	if err := ms.db.QueryRow("SELECT location, quoted_message_id, mentions, deleted_at, view_once, target_message_id FROM messages WHERE id='LOCATION' AND chat_jid='222@s.whatsapp.net'").Scan(&raw, &quote, &mentions, &deleted, &viewOnce, &target); err != nil {
		t.Fatal(err)
	}
	if raw != original || quote != "Q1" || mentions != phonePN.User || !deleted.Valid || !deleted.Time.Equal(stamp) || !viewOnce || target != "TARGET" {
		t.Fatalf("migration dropped metadata: %q %q %q %v %v %q", raw, quote, mentions, deleted, viewOnce, target)
	}
	if matched, err := ms.UpdateLiveLocation("LOCATION", "222@s.whatsapp.net", &messageLocation{Live: true, Latitude: proto.Float64(0.75), Longitude: proto.Float64(0.5), Sequence: proto.Int64(1)}); err != nil || !matched {
		t.Fatalf("migrated live key no longer updates: %v %v", matched, err)
	}
	var lat float64
	if err := ms.db.QueryRow("SELECT json_extract(location,'$.latitude') FROM messages WHERE id='LOCATION'").Scan(&lat); err != nil || lat != 0.75 {
		t.Fatalf("position=%v err=%v", lat, err)
	}
}
