package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestGroupSettingsEventState(t *testing.T) {
	for _, setting := range []string{"announce", "locked", "topic"} {
		t.Run(setting, func(t *testing.T) {
			store := newTestMessageStore(t)
			if err := ensureMessageStoreSchema(store.db); err != nil {
				t.Fatal(err)
			}
			b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
			jid := types.NewJID("120363000000000001", types.GroupServer)
			at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.FixedZone("test", -3*3600))
			event := &events.GroupInfo{JID: jid, Timestamp: at}
			store.groupInfo = func(context.Context, types.JID) (*types.GroupInfo, error) {
				info := &types.GroupInfo{}
				if event.MembershipApprovalMode != nil {
					info.GroupMembershipApprovalMode = *event.MembershipApprovalMode
				}
				return info, nil
			}
			var expected any = int64(1)
			switch setting {
			case "announce":
				event.Announce = &types.GroupAnnounce{IsAnnounce: true}
			case "locked":
				event.Locked = &types.GroupLocked{IsLocked: true}
			case "join_approval_required":
				event.MembershipApprovalMode = &types.GroupMembershipApprovalMode{IsJoinApprovalRequired: true}
			case "topic":
				event.Topic = &types.GroupTopic{Topic: "Description"}
				expected = "Description"
			}
			b.handleEvent(event, nil)
			var value any
			var stamp string
			query := fmt.Sprintf("SELECT group_%s, CAST(group_%s_updated_at AS TEXT) FROM chats WHERE jid=?", setting, setting) //nolint:gosec // setting is a fixed table-test key; JID is a bound parameter.
			if err := store.db.QueryRow(query, jid.String()).Scan(&value, &stamp); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(value) != fmt.Sprint(expected) || stamp != dbTime(at) {
				t.Fatalf("value=%v stamp=%s", value, stamp)
			}
			// Off/deleted is a known state, not NULL. Older replay must not undo it.
			event.Timestamp = at.Add(time.Hour)
			if event.Announce != nil {
				event.Announce.IsAnnounce = false
			}
			if event.Locked != nil {
				event.Locked.IsLocked = false
			}
			if event.MembershipApprovalMode != nil {
				event.MembershipApprovalMode.IsJoinApprovalRequired = false
			}
			if event.Topic != nil {
				event.Topic.TopicDeleted = true
			}
			b.handleEvent(event, nil)
			event.Timestamp = at
			if event.Announce != nil {
				event.Announce.IsAnnounce = true
			}
			if event.Locked != nil {
				event.Locked.IsLocked = true
			}
			if event.MembershipApprovalMode != nil {
				event.MembershipApprovalMode.IsJoinApprovalRequired = true
			}
			if event.Topic != nil {
				event.Topic.TopicDeleted = false
			}
			b.handleEvent(event, nil)
			if err := store.db.QueryRow(query, jid.String()).Scan(&value, &stamp); err != nil {
				t.Fatal(err)
			}
			expected = int64(0)
			if setting == "topic" {
				expected = ""
			}
			if fmt.Sprint(value) != fmt.Sprint(expected) || stamp != dbTime(at.Add(time.Hour)) {
				t.Fatalf("replay value=%v stamp=%s", value, stamp)
			}
			if err := ensureMessageStoreSchema(store.db); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGroupSettingsExcludedStoresNothing(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.Policy = parseChatPolicy("120363000000000002@g.us")
	jid := types.NewJID("120363000000000001", types.GroupServer)
	b.handleEvent(&events.GroupInfo{JID: jid, Timestamp: time.Now(), Announce: &types.GroupAnnounce{IsAnnounce: true}, Join: []types.JID{types.NewJID("100000000001", types.DefaultUserServer)}}, nil)
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	// Existing rename/timer behavior must survive the new settings gate.
	if err := store.StoreChat(jid.String(), "Old name", time.Now()); err != nil {
		t.Fatal(err)
	}
	b.handleEvent(&events.GroupInfo{JID: jid, Timestamp: time.Now(), Announce: &types.GroupAnnounce{IsAnnounce: true}, Name: &types.GroupName{Name: "Renamed"}, Ephemeral: &types.GroupEphemeral{IsEphemeral: true, DisappearingTimer: 3600}}, nil)
	var name string
	var expiration int
	var setting any
	if err := store.db.QueryRow("SELECT name,ephemeral_expiration,group_announce FROM chats WHERE jid=?", jid.String()).Scan(&name, &expiration, &setting); err != nil || name != "Renamed" || expiration != 3600 || setting != nil {
		t.Fatalf("name=%s expiration=%d setting=%v err=%v", name, expiration, setting, err)
	}

}

func stickerFixture(animated bool) []byte {
	data := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), make([]byte, 10)...)
	binary.LittleEndian.PutUint32(data[4:8], 22) // Fixed 30-byte fixture minus the eight-byte RIFF header.
	if animated {
		data[20] = 2
	}
	return data
}

func TestRESTStickerWireAndArchive(t *testing.T) {
	for _, kind := range []string{"static", "animated", "photo", "invalid", "caption", "denied"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
			b.Send = b.sendBackend()
			data := stickerFixture(kind == "animated")
			if kind == "invalid" {
				data = []byte("JPEG renamed to webp")
			}
			path := filepath.Join(t.TempDir(), "sample.webp")
			b.MediaRoots = []string{filepath.Dir(path)}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			uploads, sends := 0, 0
			up := outboundUpload()
			sum := sha256.Sum256(data)
			up.FileSHA256 = sum[:]
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				uploads++
				return up, nil
			}
			b.sendMessage = sendWithPermissions(func(ctx context.Context, jid types.JID, msg *waE2E.Message, extras ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				sends++
				if len(extras) != 1 || !extras[0].CheckPermissions {
					t.Fatal("missing permission check")
				}
				if kind == "denied" {
					return whatsmeow.SendResponse{}, fmt.Errorf("wrapped: %w", whatsmeow.ErrNoPermissionInGroup)
				}
				if kind == "photo" {
					if msg.ImageMessage == nil || msg.StickerMessage != nil {
						t.Fatal("WebP default changed")
					}
				} else if msg.StickerMessage == nil || msg.StickerMessage.GetIsAnimated() != (kind == "animated") {
					t.Fatalf("sticker=%v", msg)
				}
				return whatsmeow.SendResponse{ID: "STICKER1", Timestamp: time.Now()}, nil
			})
			req := SendMessageRequest{Recipient: testGroupJID, MediaPath: path, AsSticker: kind != "photo"}
			if kind == "caption" {
				req.Message = "caption"
			}
			payload, _ := json.Marshal(req)
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/send", string(payload), sendRecipientToken))
			if kind == "invalid" || kind == "caption" || kind == "denied" {
				var response apiError
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				code := "invalid_argument"
				want := 400
				if kind == "denied" {
					code = groupPermissionCode
					want = 403
				}
				if response.Error.Code != code || rec.Code != want {
					t.Fatalf("response=%s", rec.Body)
				}
				if kind != "denied" && (uploads != 0 || sends != 0) {
					t.Fatalf("uploads=%d sends=%d", uploads, sends)
				}
				var count int
				if err := b.Store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 0 {
					t.Fatalf("count=%d err=%v", count, err)
				}
				return
			}
			if rec.Code != 200 || uploads != 1 || sends != 1 {
				t.Fatalf("status=%d %s", rec.Code, rec.Body)
			}
			var mediaType, content, presentation string
			if err := b.Store.db.QueryRow("SELECT media_type,content,media_presentation FROM messages WHERE id='STICKER1'").Scan(&mediaType, &content, &presentation); err != nil {
				t.Fatal(err)
			}
			expected := "sticker"
			if kind == "photo" {
				expected = "image"
			}
			if mediaType != expected || content != "" {
				t.Fatalf("stored=%s %q", mediaType, content)
			}
			if kind != "photo" {
				source, found, err := b.Store.messageContentLookup("STICKER1", testGroupJID)
				if err != nil || !found {
					t.Fatalf("source=%v err=%v", source, err)
				}
				msg, caption, err := buildForwardMedia(context.WithValue(context.Background(), forwardSourceKey{}, &source), whatsmeow.MediaImage, "image/webp", path, data, up, "", outboundQuote{}, nil)
				if err != nil || msg.GetStickerMessage() == nil || msg.GetStickerMessage().GetIsAnimated() != (kind == "animated") || caption != "" {
					t.Fatalf("forward=%v caption=%s err=%v presentation=%s", msg, caption, err, presentation)
				}
			}
		})
	}
}

func TestWebPAnimationIgnoresPayloadTextAndBounds(t *testing.T) {
	data := stickerFixture(false)
	data = append(data, []byte("ANIM")...)
	if webPAnimated(data) {
		t.Fatal("substring is not animation")
	}
	for n := 0; n < len(data); n++ {
		_ = webPAnimated(data[:n])
	}
	binary.LittleEndian.PutUint32(data[16:20], ^uint32(0))
	if webPAnimated(data) {
		t.Fatal("truncated chunk")
	}
}

func TestRESTStickerPoliciesBeforeUpload(t *testing.T) {
	for _, policy := range []string{"allow_list", "read_only", "tools", "roots", "budget"} {
		t.Run(policy, func(t *testing.T) {
			t.Setenv(storeDirEnv, t.TempDir())
			b, _, _ := sendRecipientBridge(t, &mockLIDStore{}, registeredWithoutNinthDigit())
			b.Send = b.sendBackend()
			path := filepath.Join(t.TempDir(), "sample.webp")
			if err := os.WriteFile(path, stickerFixture(false), 0600); err != nil {
				t.Fatal(err)
			}
			b.MediaRoots = []string{filepath.Dir(path)}
			status := 403
			switch policy {
			case "allow_list":
				b.Policy = parseChatPolicy("120363000000000002@g.us")
			case "read_only":
				b.ReadOnly, _ = parseReadOnly("true")
			case "tools":
				b.Tools, _ = newToolPolicy("list_messages", "")
			case "roots":
				b.MediaRoots = []string{t.TempDir()}
			case "budget":
				b.RuntimeDefaults, _ = runtimeDefaults(func(string) string { return "" })
				limitPatch(t, b, `{"send.rate_per_day":1}`)
				b.sendMessage = func(context.Context, types.JID, *waE2E.Message) (whatsmeow.SendResponse, error) {
					return whatsmeow.SendResponse{ID: "BUDGET1", Timestamp: time.Now()}, nil
				}
				if rec := limitSend(t, b, testGroupJID, false); rec.Code != 200 {
					t.Fatalf("first budget send: %s", rec.Body)
				}
				status = 429
			}
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				t.Fatal("policy bypassed")
				return whatsmeow.UploadResponse{}, nil
			}
			body, _ := json.Marshal(SendMessageRequest{Recipient: testGroupJID, MediaPath: path, AsSticker: true})
			rec := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rec, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
			if rec.Code != status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
}

func TestGroupSettingsMigrationLegacy(t *testing.T) {
	store := newTestMessageStore(t)
	fields := []string{"announce", "locked", "join_approval_required", "topic"}
	for _, field := range fields {
		for _, suffix := range []string{"", "_updated_at"} {
			if _, err := store.db.Exec("ALTER TABLE chats DROP COLUMN group_" + field + suffix); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.StoreChat(testGroupJID, "Old group", time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ensureMessageStoreSchema(store.db); err != nil {
			t.Fatal(err)
		}
	}
	var value any
	var name string
	if err := store.db.QueryRow("SELECT name,group_announce FROM chats WHERE jid=?", testGroupJID).Scan(&name, &value); err != nil || name != "Old group" || value != nil {
		t.Fatalf("name=%s value=%v err=%v", name, value, err)
	}
}

func TestJoinApprovalSDKOffNotification(t *testing.T) {
	store := newTestMessageStore(t)
	// Each :memory: connection is a separate database. Polling the async
	// writer must use the same database, as the production file-backed pool does.
	store.db.SetMaxOpenConns(1)
	if err := ensureMessageStoreSchema(store.db); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(&mockLIDStore{})
	b := testBridge(t, client, store, testLogger())
	jid := types.NewJID("120363000000000001", types.GroupServer)
	node := &waBinary.Node{Tag: "notification", Attrs: waBinary.Attrs{"from": jid, "t": "1791988800"}, Content: []waBinary.Node{{Tag: "membership_approval_mode", Content: []waBinary.Node{{Tag: "group_join", Attrs: waBinary.Attrs{"state": "off"}}}}}}
	event, _, err := client.DangerousInternals().ParseGroupChange(node) //nolint:staticcheck // Exercise the pinned SDK's real decoder for this regression; no production use.
	if err != nil {
		t.Fatal(err)
	}
	if event.MembershipApprovalMode == nil || !event.MembershipApprovalMode.IsJoinApprovalRequired {
		t.Fatal("SDK decoder changed; reassess workaround")
	}
	store.groupInfo = func(ctx context.Context, group types.JID) (*types.GroupInfo, error) {
		if group != jid {
			t.Error("wrong group")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded metadata refresh")
		}
		return &types.GroupInfo{JID: jid, GroupMembershipApprovalMode: types.GroupMembershipApprovalMode{IsJoinApprovalRequired: false}}, nil
	}
	b.handleEvent(event, nil)
	waitGroupSetting(t, store, "join_approval_required", int64(0))
}

func waitGroupSetting(t *testing.T, store *MessageStore, field string, want any) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var value any
		err := store.db.QueryRow("SELECT group_"+field+" FROM chats WHERE jid=?", testGroupJID).Scan(&value)
		if err == nil && fmt.Sprint(value) == fmt.Sprint(want) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("setting %s=%v err=%v, want %v", field, value, err, want)
		case <-tick.C:
		}
	}
}
