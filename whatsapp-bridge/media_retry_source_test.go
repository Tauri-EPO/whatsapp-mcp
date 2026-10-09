package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestOutboundMediaRetryUsesSDKResponseIdentities(t *testing.T) {
	group := types.NewJID("120363000000000001", types.GroupServer)
	for _, mode := range []string{"group-LID", "group-LID-device", "sdk-upgraded-DM", "fallback-chat", "fallback-sender", "fallback-both"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			// A missing LID backend lets the fake SDK upgrade the PN destination
			// itself, after the bridge's recipient resolution has completed.
			b := testBridge(t, newTestClientWithSelf(nil, selfPhone), ms, testLogger())
			b.Connected = func() bool { return true }
			b.IsOnWhatsApp = func(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
				return []types.IsOnWhatsAppResponse{{IsIn: true, JID: phonePN}}, nil
			}
			b.Send = b.sendBackend()
			b.MediaAutoDownload = false
			requested, responseChat, responseSender := group, group, selfLID
			if mode == "group-LID-device" {
				responseSender.Device = 7
			}
			if mode == "sdk-upgraded-DM" {
				requested, responseChat = phonePN, phoneLID
			}
			if mode == "fallback-chat" || mode == "fallback-both" {
				responseChat = types.EmptyJID
			}
			if mode == "fallback-sender" || mode == "fallback-both" {
				responseSender = types.EmptyJID
			}
			file := filepath.Join(t.TempDir(), "sample.pdf")
			b.MediaRoots = []string{filepath.Dir(file)}
			if err := os.WriteFile(file, []byte("fake SDK response media"), 0o600); err != nil {
				t.Fatal(err)
			}
			b.uploadMedia = func(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
				return outboundUpload(), nil
			}
			stamp := time.Unix(1772359200, 0)
			b.sendMessage = func(_ context.Context, to types.JID, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
				if to != requested {
					t.Fatalf("bridge resolved destination=%v, want pre-SDK=%v", to, requested)
				}
				return whatsmeow.SendResponse{ID: "SDK-WIRE-SEND", Timestamp: stamp, Chat: responseChat, Sender: responseSender}, nil
			}
			body, _ := json.Marshal(SendMessageRequest{Recipient: requested.String(), Message: "caption", MediaPath: file})
			rr := httptest.NewRecorder()
			b.newRESTMux(8080, sendRecipientToken).ServeHTTP(rr, seamRequest(http.MethodPost, "/api/send", string(body), sendRecipientToken))
			var sent SendMessageResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &sent); err != nil || rr.Code != http.StatusOK || !sent.Success || sent.ChatJID != requested.String() {
				t.Fatalf("send=%d response=%+v err=%v", rr.Code, sent, err)
			}
			wantChat, wantSender := responseChat, responseSender
			if wantChat.IsEmpty() {
				wantChat = requested
			}
			if wantSender.IsEmpty() {
				wantSender = selfPhone
			}
			wantChat, wantSender = wantChat.ToNonAD(), wantSender.ToNonAD()
			check := func() {
				t.Helper()
				info, err := ms.mediaRetryInfo(context.Background(), sent.MessageID, sent.ChatJID)
				if err != nil || info.Chat != wantChat || info.Sender != wantSender || !info.IsFromMe || info.IsGroup != (wantChat.Server == types.GroupServer) {
					t.Fatalf("outbound receipt=%+v err=%v wantChat=%v wantSender=%v", info, err, wantChat, wantSender)
				}
			}
			check()
			// Sparse archive replay must not replace the SDK's delivered identities.
			if err := ms.StoreMessage(storedMessage{ID: sent.MessageID, ChatJID: sent.ChatJID, Sender: selfPhone.String(), Content: "caption", Timestamp: stamp, IsFromMe: true}); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestHistoryMediaRetryUsesPinnedSDKSenderPrecedence(t *testing.T) {
	for _, mode := range []string{"own-group-original-LID", "own-group-original-PN", "own-LID-DM", "group-participant", "group-key-participant", "DM-chat-author"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			lids := &mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN, selfLID: selfPhone}}
			client := newTestClientWithSelf(lids, selfPhone)
			client.Store.LID = selfLID
			b := testBridge(t, client, ms, testLogger())
			b.MediaAutoDownload = false
			wireChat, archiveChat := types.NewJID("120363000000000001", types.GroupServer), types.NewJID("120363000000000001", types.GroupServer)
			if mode == "own-LID-DM" || mode == "DM-chat-author" {
				wireChat, archiveChat = phoneLID, phonePN
			}
			fixture := largeHistoryFixture(1)
			conversation := fixture.Data.Conversations[0]
			conversation.ID = proto.String(wireChat.String())
			row := conversation.Messages[0].Message
			row.Key.ID = proto.String("HISTORY-WIRE-SELF")
			row.Key.FromMe = proto.Bool(strings.HasPrefix(mode, "own-"))
			row.Participant = proto.String(phoneLID.String())
			row.Key.Participant = proto.String(selfLID.String())
			switch mode {
			case "own-group-original-LID":
				row.OriginalSelfAuthorUserJIDString = proto.String(selfLID.String())
			case "own-group-original-PN":
				row.OriginalSelfAuthorUserJIDString = proto.String(selfPhone.String())
			case "group-key-participant":
				row.Participant, row.Key.Participant = nil, proto.String(phoneLID.String())
			case "DM-chat-author":
				row.Participant = proto.String(selfLID.String())
			}
			image := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{DirectPath: proto.String("/fake-media"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e")}}
			row.Message = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: image}, MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: []byte("fake history secret")}}
			// The pinned parser defines the wire sender; give it a local protobuf
			// copy because its envelope unwrapping can mutate context fields.
			expected, err := client.ParseWebMessage(wireChat, proto.Clone(row).(*waWeb.WebMessageInfo))
			if err != nil {
				t.Fatal(err)
			}
			before := proto.Clone(fixture.Data)
			b.handleHistorySync(fixture)
			if !proto.Equal(before, fixture.Data) {
				t.Fatal("wire sender preparation mutated SDK history payload")
			}
			info, err := ms.mediaRetryInfo(context.Background(), row.Key.GetID(), archiveChat.String())
			if err != nil || info.Chat != expected.Info.Chat || info.Sender != expected.Info.Sender.ToNonAD() || info.IsFromMe != expected.Info.IsFromMe || info.IsGroup != expected.Info.IsGroup {
				t.Fatalf("history receipt=%+v err=%v pinnedSDK=%+v", info, err, expected.Info.MessageSource)
			}
			if expected.Info.IsFromMe {
				sender, server := querySenderServer(t, ms, row.Key.GetID(), archiveChat.String())
				if sender != selfPhone.User || server != types.DefaultUserServer {
					t.Fatalf("wire identity changed normalized archive author: %s/%s", sender, server)
				}
			}
			if err := ms.StoreMessage(storedMessage{ID: row.Key.GetID(), ChatJID: archiveChat.String(), Sender: selfPhone.String(), Content: "sparse replay", Timestamp: time.Now(), IsFromMe: expected.Info.IsFromMe}); err != nil {
				t.Fatal(err)
			}
			info, err = ms.mediaRetryInfo(context.Background(), row.Key.GetID(), archiveChat.String())
			if err != nil || info.Sender != expected.Info.Sender.ToNonAD() || info.Chat != wireChat {
				t.Fatalf("replay changed history receipt: %+v err=%v", info, err)
			}
		})
	}
}

func TestMediaRetryRetainsDeliveryJIDsLiveHistoryAndLegacy(t *testing.T) {
	for _, mode := range []string{"live-DM", "live-group", "history-DM", "history-group", "legacy-LID"} {
		t.Run(mode, func(t *testing.T) {
			ms, _ := lockedProductionStore(t)
			lids := &mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}}
			b := testBridge(t, newTestClient(lids), ms, testLogger())
			b.MediaAutoDownload = false
			wireChat, archiveChat := phoneLID, phonePN
			if mode == "live-group" || mode == "history-group" {
				wireChat = types.NewJID("120363000000000001", types.GroupServer)
				archiveChat = wireChat
			}
			image := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{DirectPath: proto.String("/fake-media"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e")}}
			switch mode {
			case "live-DM", "live-group":
				event := buildTextMessage(wireChat, phoneLID, phonePN, types.EmptyJID, false, "")
				event.Info.ID, event.Message = "WIRE-SOURCE", image
				b.handleEvent(event, nil)
			case "history-DM", "history-group":
				fixture := largeHistoryFixture(1)
				conv := fixture.Data.Conversations[0]
				conv.ID = proto.String(wireChat.String())
				row := conv.Messages[0].Message
				row.Participant = proto.String(phoneLID.String())
				row.Key.ID = proto.String("WIRE-SOURCE")
				row.Message = image
				b.handleHistorySync(fixture)
			default:
				archiveChat = wireChat
				if err := ms.StoreChat(archiveChat.String(), "Alice", time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := ms.StoreMessage(storedMessage{ID: "WIRE-SOURCE", ChatJID: archiveChat.String(), Sender: phoneLID.String(), Content: "", Timestamp: time.Now(), IsFromMe: false, MediaType: "image", Filename: "", URL: "", MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: 0, QuotedMessageID: ""}); err != nil {
					t.Fatal(err)
				}
			}
			info, err := ms.mediaRetryInfo(context.Background(), "WIRE-SOURCE", archiveChat.String())
			if err != nil || info.Chat != wireChat || info.Sender != phoneLID || info.IsFromMe || info.IsGroup != (wireChat.Server == types.GroupServer) {
				t.Fatalf("receipt shape=%+v err=%v", info, err)
			}
			// A sparse replay using the normalized identity must retain the
			// wire source, rather than replacing it with archive addressing.
			if err := ms.StoreMessage(storedMessage{ID: "WIRE-SOURCE", ChatJID: archiveChat.String(), Sender: phonePN.String(), Content: "caption", Timestamp: time.Now(), IsFromMe: false, MediaType: "image", Filename: "", URL: "", MediaKey: nil, FileSHA256: nil, FileEncSHA256: nil, FileLength: 0, QuotedMessageID: ""}); err != nil {
				t.Fatal(err)
			}
			info, err = ms.mediaRetryInfo(context.Background(), "WIRE-SOURCE", archiveChat.String())
			if mode != "legacy-LID" && (err != nil || info.Chat != wireChat || info.Sender != phoneLID) {
				t.Fatal("sparse replay erased receipt identities")
			}
		})
	}
}

func TestMediaRetryGroupWithoutHistoryAuthorLearnsDeliverySender(t *testing.T) {
	ms, _ := lockedProductionStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{phoneLID: phonePN}}), ms, testLogger())
	b.MediaAutoDownload = false
	chat := types.NewJID("120363000000000001", types.GroupServer)
	history := largeHistoryFixture(1)
	conversation := history.Data.Conversations[0]
	conversation.ID = proto.String(chat.String())
	row := conversation.Messages[0].Message
	row.Participant, row.Key.Participant = nil, nil
	row.Key.ID = proto.String("AUTHOR-REPAIR")
	row.Key.FromMe = proto.Bool(false)
	row.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{DirectPath: proto.String("/fake-media"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e")}}
	b.handleHistorySync(history)
	var saved string
	if err := ms.db.QueryRow("SELECT COALESCE(media_retry_sender,'') FROM messages WHERE id='AUTHOR-REPAIR'").Scan(&saved); err != nil || saved != "" {
		t.Fatalf("unattributed history invented receipt participant %q: %v", saved, err)
	}
	if info, err := ms.mediaRetryInfo(context.Background(), "AUTHOR-REPAIR", chat.String()); err == nil {
		t.Fatalf("unattributed group produced a retry participant: %+v", info)
	}
	event := buildTextMessage(chat, phoneLID, phonePN, types.EmptyJID, false, "")
	event.Info.ID, event.Message = "AUTHOR-REPAIR", row.Message
	b.handleEvent(event, nil)
	info, err := ms.mediaRetryInfo(context.Background(), "AUTHOR-REPAIR", chat.String())
	if err != nil || info.Chat != chat || info.Sender != phoneLID || !info.IsGroup {
		t.Fatalf("attributed delivery failed to repair receipt: %+v err=%v", info, err)
	}
	sender, server := querySenderServer(t, ms, "AUTHOR-REPAIR", chat.String())
	if sender != phonePN.User || server != types.DefaultUserServer {
		t.Fatalf("fallback archive author not repaired: %s/%s", sender, server)
	}
}

func TestMediaRetryUnauthenticatedAbsenceStaysRetryable(t *testing.T) {
	_, err := mediaRetryDirectPath(&events.MediaRetry{Error: &events.MediaRetryError{Code: 2}}, []byte("fake key"))
	if !errors.Is(err, whatsmeow.ErrMediaNotAvailableOnPhone) || errors.Is(err, errMediaUnavailable) {
		t.Fatalf("error-node classification=%v", err)
	}
}
