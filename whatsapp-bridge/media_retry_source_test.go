package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

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
