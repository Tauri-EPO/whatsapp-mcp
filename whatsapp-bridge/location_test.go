package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func locationRowSnapshot(t *testing.T, ms *MessageStore, id, chat string) string {
	t.Helper()
	rows, err := ms.db.Query("SELECT * FROM messages WHERE id=? AND chat_jid=?", id, chat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil || !rows.Next() {
		t.Fatalf("missing row: %v", err)
	}
	values, pointers := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(values)
}

func livePosition(sequence int64, latitude float64) *waE2E.Message {
	return &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
		SequenceNumber: proto.Int64(sequence), DegreesLatitude: proto.Float64(latitude), DegreesLongitude: proto.Float64(0.5), Caption: proto.String("original"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("QUOTE"), MentionedJID: []string{phonePN.String()}},
	}}
}

func TestLiveLocationAuthorCollisions(t *testing.T) {
	for _, path := range []string{"live", "persist", "batch"} {
		for _, kind := range []string{"live", "static", "text", "image", "reverse-static", "reverse-text", "reverse-reaction", "reverse-poll-vote"} {
			for _, author := range []string{"sender", "namespace", "from-me", "unknown-namespace"} {
				for _, sequence := range []int64{0, 3} {
					t.Run(fmt.Sprintf("%s/%s/%s/seq%d", path, kind, author, sequence), func(t *testing.T) {
						t.Setenv(storeDirEnv, t.TempDir())
						ms, err := NewMessageStore()
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = ms.Close() })
						chat := types.NewJID("120363000000000001", types.GroupServer)
						stamp := time.Unix(1772359200, 0)
						if err := ms.StoreChat(chat.String(), "group", stamp); err != nil {
							t.Fatal(err)
						}
						original := livePosition(0, 0.25)
						switch kind {
						case "static":
							original = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(0.25), DegreesLongitude: proto.Float64(0.5)}}
						case "text":
							original = &waE2E.Message{Conversation: proto.String("preserve me")}
						case "image":
							original = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("preserve image"), URL: proto.String("https://example.test/image"), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(9)}}
						}
						if err := persistMessage(ms, "KNOWN", chat.String(), phonePN.String(), stamp, false, extractMessage(original, stamp, "KNOWN"), true, testLogger()); err != nil {
							t.Fatal(err)
						}
						if strings.HasPrefix(kind, "reverse-") && sequence > 0 {
							if err := persistMessage(ms, "KNOWN", chat.String(), phonePN.String(), stamp, false, extractMessage(livePosition(sequence, 0.4), stamp, "KNOWN"), true, testLogger()); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := ms.db.Exec("UPDATE messages SET deleted_at=?, view_once=1, target_message_id='TARGET' WHERE id='KNOWN'", dbTime(stamp)); err != nil {
							t.Fatal(err)
						}
						if author == "unknown-namespace" {
							if _, err := ms.db.Exec("UPDATE messages SET sender_server=NULL WHERE id='KNOWN'"); err != nil {
								t.Fatal(err)
							}
						}
						before := locationRowSnapshot(t, ms, "KNOWN", chat.String())
						b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
						b.MediaAutoDownload, b.MediaMaxBytes = true, 0
						// No worker drains this queue: even transient submissions are observed.
						b.autoDownloads = newMediaJobQueue(b.ctx, 0, 8, func(context.Context, mediaJob) { t.Error("unexpected worker") })
						srv, posts, _ := contentKindsWebhook(t)
						b.Webhook = newWebhookSender("", true)
						b.Webhook.url = srv.URL
						sender, fromMe := phonePN, false
						switch author {
						case "sender":
							sender = types.NewJID("5511888888888", types.DefaultUserServer)
						case "namespace":
							sender = types.NewJID(phonePN.User, types.HiddenUserServer)
						case "from-me":
							fromMe = true
						}
						incoming := livePosition(sequence, 0.9)
						// A hostile mixed envelope would exercise automatic media effects
						// if it escaped the collision gate.
						incoming.ImageMessage = &waE2E.ImageMessage{Caption: proto.String("incoming caption"), URL: proto.String("https://example.test/new"), MediaKey: []byte{4}, FileSHA256: []byte{5}, FileEncSHA256: []byte{6}, FileLength: proto.Uint64(9)}
						switch kind {
						case "reverse-static":
							incoming = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(0.9), DegreesLongitude: proto.Float64(0.5)}}
						case "reverse-text":
							incoming = &waE2E.Message{Conversation: proto.String("claim the author's key")}
						case "reverse-reaction":
							incoming = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("TARGET")}, Text: proto.String("👍")}}
						case "reverse-poll-vote":
							incoming = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("TARGET")}}}
							b.PollVoteDecrypt = func(context.Context, *events.Message) ([][]byte, error) { return nil, nil }
						}
						feed := func(id string) {
							if path == "live" {
								event := buildTextMessage(chat, sender, types.EmptyJID, types.EmptyJID, fromMe, "")
								event.Info.ID, event.Info.Timestamp, event.Message = id, stamp.Add(time.Minute), incoming
								b.handleMessage(event)
							} else {
								write := func(w messageWriter) error {
									if kind == "reverse-poll-vote" && incoming.GetPollUpdateMessage() != nil {
										return ms.storePollVoteMessage(id, chat.String(), sender.String(), pollVoteContent(nil), stamp.Add(time.Minute), fromMe, "TARGET", testLogger())
									}
									if kind == "reverse-reaction" && incoming.GetReactionMessage() != nil {
										return persistMessage(w, id, chat.String(), sender.String(), stamp.Add(time.Minute), fromMe, extractedMessage{content: "👍", mediaType: "reaction", filename: "TARGET"}, false, testLogger())
									}
									return persistMessage(w, id, chat.String(), sender.String(), stamp.Add(time.Minute), fromMe, extractMessage(incoming, stamp, id), true, testLogger())
								}
								var err error
								if path == "batch" && incoming.GetPollUpdateMessage() == nil {
									err = ms.Batch(func(w *messageBatch) error { return write(w) })
								} else {
									err = write(ms)
								}
								if err != nil {
									t.Fatal(err)
								}
							}
						}
						feed("KNOWN")
						if after := locationRowSnapshot(t, ms, "KNOWN", chat.String()); after != before {
							t.Fatalf("collision replaced row:\nbefore %s\nafter  %s", before, after)
						}
						var activity time.Time
						if err := ms.db.QueryRow("SELECT last_message_time FROM chats WHERE jid=?", chat.String()).Scan(&activity); err != nil || !activity.Equal(stamp) {
							t.Fatalf("collision became activity: %v %v", activity, err)
						}
						if posts.Load() != 0 || b.autoDownloads.queued() != 0 || b.metrics.messagesStored.Load() != 0 {
							t.Fatalf("collision emitted effects: posts=%d jobs=%d stored=%d", posts.Load(), b.autoDownloads.queued(), b.metrics.messagesStored.Load())
						}
						if strings.HasPrefix(kind, "reverse-") {
							// A refused text/static write must not establish an author
							// that can subsequently take over the live position.
							first := incoming
							incoming = livePosition(sequence+1, 0.95)
							feed("KNOWN")
							if after := locationRowSnapshot(t, ms, "KNOWN", chat.String()); after != before {
								t.Fatal("collision established an author for a later position")
							}
							if posts.Load() != 0 || b.autoDownloads.queued() != 0 || b.metrics.messagesStored.Load() != 0 {
								t.Fatal("later collision emitted effects")
							}
							incoming = first
						}
						feed("DISTINCT")
						locationRowSnapshot(t, ms, "DISTINCT", chat.String())
						wantJobs := 1
						if strings.HasPrefix(kind, "reverse-") {
							wantJobs = 0
						}
						wantPosts, wantStored := int32(1), int64(1)
						if kind == "reverse-reaction" || kind == "reverse-poll-vote" {
							wantStored = 0 // pointer rows have their existing separate event path
						}
						if kind == "reverse-poll-vote" {
							wantPosts = 0 // votes do not emit message webhooks
						}
						if path == "live" && (posts.Load() != wantPosts || b.autoDownloads.queued() != wantJobs || b.metrics.messagesStored.Load() != wantStored) {
							t.Fatalf("distinct-key control did not emit effects: posts=%d jobs=%d stored=%d", posts.Load(), b.autoDownloads.queued(), b.metrics.messagesStored.Load())
						}
					})
				}
			}
		}
	}
}

func TestLocationUpsertCompatibility(t *testing.T) {
	for _, kind := range []string{"legacy-text", "same-author-text", "same-author-static"} {
		t.Run(kind, func(t *testing.T) {
			ms := newTestMessageStore(t)
			stamp := time.Unix(1772359200, 0)
			chat := phonePN.String()
			if err := ms.EnsureChat(chat, "Alice"); err != nil {
				t.Fatal(err)
			}
			if err := persistMessage(ms, "COMPAT", chat, phonePN.String(), stamp, false, extractMessage(livePosition(0, 0.25), stamp, "COMPAT"), true, testLogger()); err != nil {
				t.Fatal(err)
			}
			if kind == "same-author-static" {
				if err := persistMessage(ms, "COMPAT", chat, phonePN.String(), stamp, false, extractMessage(livePosition(3, 0.4), stamp, "COMPAT"), true, testLogger()); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "legacy-text" {
				if _, err := ms.db.Exec("UPDATE messages SET media_type=NULL, location=NULL WHERE id='COMPAT'"); err != nil {
					t.Fatal(err)
				}
			}
			incoming := &waE2E.Message{Conversation: proto.String("replacement")}
			if kind == "same-author-static" {
				incoming = &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(0.75), DegreesLongitude: proto.Float64(0.5)}}
			}
			ex := extractMessage(incoming, stamp, "COMPAT")
			consumed, err := persistMessageResult(ms, "COMPAT", chat, phonePN.String(), stamp, false, ex, true, testLogger())
			if err != nil || consumed {
				t.Fatalf("compatible upsert refused: consumed=%v err=%v", consumed, err)
			}
			var content, sender, server string
			var own bool
			if err := ms.db.QueryRow("SELECT content,sender,sender_server,is_from_me FROM messages WHERE id='COMPAT'").Scan(&content, &sender, &server, &own); err != nil {
				t.Fatal(err)
			}
			if content != ex.content || sender != phonePN.User || server != types.DefaultUserServer || own {
				t.Fatalf("compatible upsert lost: %q %s %s %v", content, sender, server, own)
			}
			if kind == "same-author-static" {
				var raw string
				if err := ms.db.QueryRow("SELECT location FROM messages WHERE id='COMPAT'").Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if raw != ex.location.column().(string) {
					t.Fatalf("static location inherited the live position: got=%s want=%s", raw, ex.location.column())
				}
			}
		})
	}
}

func TestLiveLocationConcurrentAuthorClaim(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ms.Close() }()
	chat, stamp := phonePN.String(), time.Unix(1772359200, 0)
	if err := ms.EnsureChat(chat, "Alice"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 12 {
		wg.Go(func() {
			<-start
			sender, latitude := phonePN.String(), 0.25
			if i%2 != 0 {
				sender, latitude = phonePN.User+"@lid", 0.75
			}
			if err := persistMessage(ms, "RACE", chat, sender, stamp, false, extractMessage(livePosition(1, latitude), stamp, "RACE"), true, testLogger()); err != nil {
				t.Errorf("concurrent persist: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	var server string
	var latitude float64
	if err := ms.db.QueryRow("SELECT sender_server, json_extract(location,'$.latitude') FROM messages WHERE id='RACE'").Scan(&server, &latitude); err != nil {
		t.Fatal(err)
	}
	want := 0.25
	if server == types.HiddenUserServer {
		want = 0.75
	}
	if latitude != want {
		t.Fatalf("position crossed winning author namespace: %s %v", server, latitude)
	}
	before := locationRowSnapshot(t, ms, "RACE", chat)
	loser := types.HiddenUserServer
	if server == loser {
		loser = types.DefaultUserServer
	}
	if err := persistMessage(ms, "RACE", chat, phonePN.User+"@"+loser, stamp.Add(time.Minute), false, extractMessage(livePosition(99, 0.9), stamp, "RACE"), true, testLogger()); err != nil {
		t.Fatal(err)
	}
	if after := locationRowSnapshot(t, ms, "RACE", chat); after != before {
		t.Fatal("losing author overwrote winner")
	}
	if err := persistMessage(ms, "RACE", chat, phonePN.User+"@"+server, stamp.Add(time.Minute), false, extractMessage(livePosition(2, 0.4), stamp, "RACE"), true, testLogger()); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT json_extract(location,'$.latitude') FROM messages WHERE id='RACE'").Scan(&latitude); err != nil || latitude != 0.4 {
		t.Fatalf("winning author cannot advance: %v %v", latitude, err)
	}
}

func TestLiveLocationBusyFailureWithholdsEffects(t *testing.T) {
	for _, sequence := range []int64{0, 3} {
		t.Run(fmt.Sprintf("seq%d", sequence), func(t *testing.T) {
			store := newLockedStore(t)
			ms := store.ms
			stamp := time.Unix(1772359200, 0)
			if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
				t.Fatal(err)
			}
			if err := persistMessage(ms, "BUSYLOCATION", phonePN.String(), phonePN.String(), stamp, false, extractMessage(livePosition(0, 0.25), stamp, "BUSYLOCATION"), true, testLogger()); err != nil {
				t.Fatal(err)
			}
			before := locationRowSnapshot(t, ms, "BUSYLOCATION", phonePN.String())
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			b.MediaAutoDownload, b.MediaMaxBytes = true, 0
			b.autoDownloads = newMediaJobQueue(b.ctx, 0, 8, func(context.Context, mediaJob) { t.Error("unexpected worker") })
			srv, posts, _ := contentKindsWebhook(t)
			b.Webhook = newWebhookSender("", true)
			b.Webhook.url = srv.URL
			waits := 0
			b.storeRetryWait = func(time.Duration) bool { waits++; return true }
			incoming := livePosition(sequence, 0.9)
			incoming.ImageMessage = &waE2E.ImageMessage{Caption: proto.String("incoming caption"), URL: proto.String("https://example.test/new"), MediaKey: []byte{4}, FileSHA256: []byte{5}, FileEncSHA256: []byte{6}, FileLength: proto.Uint64(9)}
			event := buildTextMessage(phonePN, types.NewJID("5511888888888", types.DefaultUserServer), types.EmptyJID, types.EmptyJID, false, "")
			event.Info.ID, event.Info.Timestamp, event.Message = "BUSYLOCATION", stamp.Add(time.Minute), incoming
			store.lock(t)
			b.handleMessage(event)
			store.unlock(t)
			if after := locationRowSnapshot(t, ms, "BUSYLOCATION", phonePN.String()); after != before {
				t.Fatal("failed ownership check changed row")
			}
			if waits != len(b.StoreRetryDelays) || b.metrics.storeFailures.Load() != 1 {
				t.Fatalf("bounded retry accounting changed: waits=%d failures=%d", waits, b.metrics.storeFailures.Load())
			}
			if posts.Load() != 0 || b.autoDownloads.queued() != 0 || b.metrics.messagesStored.Load() != 0 {
				t.Fatalf("failed ownership check emitted effects: posts=%d jobs=%d stored=%d", posts.Load(), b.autoDownloads.queued(), b.metrics.messagesStored.Load())
			}
		})
	}
}

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
		// Startup migrates the author separately from the chat key (main.go).
		if err := ms.MigrateLegacyLIDSendersToPhones(path, testLogger()); err != nil {
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
	if matched, err := ms.UpdateLiveLocation("LOCATION", "222@s.whatsapp.net", "222@s.whatsapp.net", false, &messageLocation{Live: true, Latitude: proto.Float64(0.75), Longitude: proto.Float64(0.5), Sequence: proto.Int64(1)}); err != nil || !matched {
		t.Fatalf("migrated live key no longer updates: %v %v", matched, err)
	}
	var lat float64
	if err := ms.db.QueryRow("SELECT json_extract(location,'$.latitude') FROM messages WHERE id='LOCATION'").Scan(&lat); err != nil || lat != 0.75 {
		t.Fatalf("position=%v err=%v", lat, err)
	}
}
