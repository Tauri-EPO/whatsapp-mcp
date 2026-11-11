package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	"google.golang.org/protobuf/proto"
)

type editSecretStore struct {
	store.MsgSecretStore
	secret []byte
}

func (s editSecretStore) GetMessageSecret(_ context.Context, _, sender types.JID, _ types.MessageID) ([]byte, types.JID, error) {
	return s.secret, sender, nil
}

func TestSecretEditRealSDKDecryption(t *testing.T) {
	for _, fromMe := range []bool{false, true} {
		for _, badKey := range []bool{false, true} {
			t.Run(map[bool]string{false: "peer", true: "own"}[fromMe]+map[bool]string{false: "/valid", true: "/wrong-secret"}[badKey], func(t *testing.T) {
				ms, _ := lockedProductionStore(t)
				client := newTestClientWithSelf(&mockLIDStore{}, selfPhone)
				secret := bytes.Repeat([]byte{42}, 32)
				client.Store.MsgSecrets = editSecretStore{secret: secret}
				b := testBridge(t, client, ms, testLogger())
				b.bindRuntimeClient()
				stamp := time.Unix(1772359200, 0)
				sender := phonePN
				if fromMe {
					sender = selfPhone
				}
				original := buildTextMessage(phonePN, sender, types.EmptyJID, types.EmptyJID, fromMe, "oldword")
				original.Info.ID, original.Info.Timestamp = "TARGET", stamp
				b.handleEvent(original, nil)
				evt := secretEditEvent(stamp.Add(time.Minute))
				evt.Info.Sender, evt.Info.IsFromMe = sender, fromMe
				enc := evt.Message.SecretEncryptedMessage
				enc.TargetMessageKey.RemoteJID = proto.String(phonePN.String())
				enc.TargetMessageKey.FromMe = proto.Bool(fromMe)
				plaintext, err := proto.Marshal(&waE2E.Message{Conversation: proto.String("newword")})
				if err != nil {
					t.Fatal(err)
				}
				// Matches the SDK's original ID + author + editor + use-case derivation.
				key := hkdfutil.SHA256(secret, nil, []byte("TARGET"+sender.String()+sender.String()+"Message Edit"), 32)
				enc.EncIV = bytes.Repeat([]byte{7}, 12)
				enc.EncPayload, err = gcmutil.Encrypt(key, enc.EncIV, plaintext, nil)
				if err != nil {
					t.Fatal(err)
				}
				if badKey {
					client.Store.MsgSecrets = editSecretStore{secret: bytes.Repeat([]byte{1}, 32)}
				}
				b.handleEvent(evt, nil)
				var content string
				var before *string
				if err := ms.db.QueryRow("SELECT content,original_content FROM messages WHERE id='TARGET'").Scan(&content, &before); err != nil {
					t.Fatal(err)
				}
				if badKey {
					if content != "oldword" || before != nil {
						t.Fatal("wrong secret changed archive")
					}
				} else if content != "newword" || before == nil || *before != "oldword" {
					t.Fatalf("SDK edit not archived: content=%q original=%v", content, before)
				}
			})
		}
	}
}

func TestOriginalContentMigrationAndEmptyCaption(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	if _, err := ms.db.Exec("ALTER TABLE messages DROP COLUMN original_content"); err != nil {
		t.Fatal(err)
	}
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1772359200, 0)
	if err := ms.StoreChat(phonePN.String(), "Alice", stamp); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage(storedMessage{ID: "EMPTY", ChatJID: phonePN.String(), Sender: phonePN.String(), Timestamp: stamp, MediaType: "document", Filename: "report.pdf"}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"captionword", "latestword"} {
		if err := ms.UpdateMessageContent("EMPTY", phonePN.String(), text, stamp.UnixMilli()+int64(i)+1); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureMessageStoreSchema(ms.db); err != nil {
		t.Fatal(err)
	}
	var content, before string
	if err := ms.db.QueryRow("SELECT content,original_content FROM messages WHERE id='EMPTY'").Scan(&content, &before); err != nil || before != "" || content != "latestword" {
		t.Fatalf("empty original lost after edits/migration: content=%q original=%q err=%v", content, before, err)
	}
}

func secretEditEvent(stamp time.Time) *events.Message {
	evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
	evt.Info.ID, evt.Info.Timestamp = "SECRET-EDIT", stamp
	evt.Message = &waE2E.Message{SecretEncryptedMessage: &waE2E.SecretEncryptedMessage{
		SecretEncType:    waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(),
		TargetMessageKey: &waCommon.MessageKey{ID: proto.String("TARGET"), RemoteJID: proto.String(selfPhone.String())},
	}}
	return evt
}

func TestSecretEditUsesCanonicalEditPath(t *testing.T) {
	for _, shape := range []string{"text", "protocol", "wrapped-caption"} {
		for _, pending := range []bool{false, true} {
			t.Run(shape+map[bool]string{false: "/known", true: "/pending"}[pending], func(t *testing.T) {
				ms, _ := lockedProductionStore(t)
				b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
				stamp := time.Unix(1772359200, 0)
				original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
				original.Info.ID, original.Info.Timestamp = "TARGET", stamp
				if !pending {
					b.handleEvent(original, nil)
				}
				evt := secretEditEvent(stamp.Add(time.Minute))
				if shape == "text" {
					b.origTimes.remember(evt.Info.ID, evt.Info.Timestamp)
					evt.Info.Timestamp = stamp.Add(3 * time.Minute)
				}
				calls := 0
				b.SecretEditDecrypt = func(ctx context.Context, got *events.Message) (*waE2E.Message, error) {
					calls++
					if got != evt || ctx == nil {
						t.Fatal("decrypt did not receive the original event and context")
					}
					replacement := &waE2E.Message{Conversation: proto.String("newword")}
					if shape == "protocol" {
						// A different inner key must not redirect the edit.
						return incomingEdit("OTHER", "newword", stamp.Add(time.Minute).UnixMilli()), nil
					}
					if shape == "wrapped-caption" {
						replacement = &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
							DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("newword")},
						}}}
					}
					return replacement, nil
				}
				b.handleEvent(evt, nil)
				if pending {
					if pendingCount(t, ms) != 1 {
						t.Fatal("decrypted edit not deferred")
					}
					b.handleEvent(original, nil)
				}
				assertPendingArchive(t, ms, "TARGET", phonePN.String(), "newword", "", stamp.Add(time.Minute).UnixMilli())
				var before string
				var rows int
				if err := ms.db.QueryRow("SELECT original_content,(SELECT COUNT(*) FROM messages) FROM messages WHERE id='TARGET'").Scan(&before, &rows); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || before != "oldword" || rows != 1 || evt.Message.GetSecretEncryptedMessage() == nil {
					t.Fatalf("calls=%d original=%q rows=%d; event must stay encrypted", calls, before, rows)
				}
			})
		}
	}
}

func TestSecretEditRefusalsDoNotChangeTarget(t *testing.T) {
	for _, mode := range []string{"decrypt-error", "nil-payload", "unsupported", "missing-target", "wrong-sender", "wrong-own", "wrong-chat", "deleted", "stale"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			rec := installRecordingLogger(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, rec)
			stamp := time.Unix(1772359200, 0)
			original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
			original.Info.ID, original.Info.Timestamp = "TARGET", stamp
			b.handleEvent(original, nil)
			evt := secretEditEvent(stamp.Add(time.Minute))
			switch mode {
			case "unsupported":
				evt.Message.SecretEncryptedMessage.SecretEncType = waE2E.SecretEncryptedMessage_EVENT_EDIT.Enum()
			case "missing-target":
				evt.Message.SecretEncryptedMessage.TargetMessageKey = nil
			case "wrong-sender":
				evt.Info.Sender = selfPhone
			case "wrong-own":
				evt.Info.IsFromMe = true
			case "wrong-chat":
				evt.Info.Chat = types.NewJID("120363000000000001", types.GroupServer)
			case "deleted":
				if _, err := ms.db.Exec("UPDATE messages SET deleted_at=?", dbTime(stamp)); err != nil {
					t.Fatal(err)
				}
			case "stale":
				if _, err := ms.db.Exec("UPDATE messages SET message_edit_timestamp=?", stamp.Add(2*time.Minute).UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			b.SecretEditDecrypt = func(context.Context, *events.Message) (*waE2E.Message, error) {
				calls++
				if mode == "decrypt-error" {
					return nil, errors.New("privateword")
				}
				if mode == "nil-payload" {
					return nil, nil
				}
				return &waE2E.Message{Conversation: proto.String("newword")}, nil
			}
			b.handleEvent(evt, nil)
			var content string
			var before *string
			if err := ms.db.QueryRow("SELECT content,original_content FROM messages WHERE id='TARGET' AND chat_jid=?", phonePN.String()).Scan(&content, &before); err != nil {
				t.Fatal(err)
			}
			if content != "oldword" || before != nil || strings.Contains(rec.String(), "privateword") {
				t.Fatal("refused edit changed the target or leaked error data")
			}
			if (mode == "unsupported" || mode == "missing-target") && calls != 0 {
				t.Fatal("unsupported or targetless payload reached decrypt")
			}
		})
	}
}

func TestEditOriginalContentWrittenOnce(t *testing.T) {
	for _, mode := range []string{"live", "pending", "outbound"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			b := testBridge(t, newTestClient(&mockLIDStore{}), ms, testLogger())
			stamp := time.Unix(1772359200, 0)
			original := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "oldword")
			original.Info.ID, original.Info.Timestamp = "TARGET", stamp
			apply := func(text string, delta time.Duration) {
				if mode == "outbound" {
					if err := ms.UpdateMessageContent("TARGET", phonePN.String(), text, stamp.Add(delta).UnixMilli()); err != nil {
						t.Fatal(err)
					}
					return
				}
				evt := buildTextMessage(phonePN, phonePN, types.EmptyJID, types.EmptyJID, false, "")
				evt.Info.ID, evt.Message = "EDIT", incomingEdit("TARGET", text, stamp.Add(delta).UnixMilli())
				b.handleEvent(evt, nil)
			}
			if mode != "pending" {
				b.handleEvent(original, nil)
			}
			apply("firstword", time.Minute)
			if mode == "pending" {
				b.handleEvent(original, nil)
			}
			apply("latestword", 2*time.Minute)
			apply("staleword", time.Minute)
			b.handleEvent(original, nil)
			var content, before string
			if err := ms.db.QueryRow("SELECT content,original_content FROM messages WHERE id='TARGET'").Scan(&content, &before); err != nil {
				t.Fatal(err)
			}
			if content != "latestword" || before != "oldword" {
				t.Fatalf("content=%q original=%q", content, before)
			}
		})
	}
}
