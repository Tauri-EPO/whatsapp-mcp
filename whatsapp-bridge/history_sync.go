package main

// History sync: messages the phone replays at pair time or on demand
// (history_ondemand.go). Mirrors handleMessage but works on
// waWeb.WebMessageInfo rows instead of live events.

import (
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (b *Bridge) handleHistorySync(historySync *events.HistorySync) {
	client, messageStore, logger := b.Client, b.Store, b.Log
	// Log every history sync event with its shape. Different sync types
	// carry different payloads; logging type/chunk/progress makes it easy
	// to reason about what arrived from WhatsApp when debugging.
	logger.Infof("Received history sync: type=%s chunk=%d progress=%d conversations=%d",
		historySync.Data.GetSyncType(),
		historySync.Data.GetChunkOrder(),
		historySync.Data.GetProgress(),
		len(historySync.Data.Conversations),
	)

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		rawChatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(rawChatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", rawChatJID, err)
			continue
		}

		// Resolve LID-based chats to phone-based JIDs.
		// History sync doesn't carry SenderAlt, so rely on the
		// LID store mapping populated during live message handling.
		resolved := resolveLIDChat(client, jid, types.EmptyJID, types.EmptyJID, false)
		chatJID := resolved.String()

		// Get appropriate chat name by passing the history sync conversation directly
		// History sync never fetches group metadata (see chat_names.go).
		name := GetChatName(client, messageStore, resolved, chatJID, conversation, "", false, logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			ts := latestMsg.Message.GetMessageTimestamp()
			if ts == 0 {
				continue
			}
			timestamp := time.Unix(int64(ts), 0) //nolint:gosec // WhatsApp seconds-since-epoch fit int64

			_ = messageStore.StoreChat(chatJID, name, timestamp)
			// Backfill read state only when WhatsApp explicitly reports unread
			// metadata. Sparse history-sync chunks omit UnreadCount; the
			// generated getter then returns 0 and would permanently mark the
			// chat read under the monotonic merge.
			if conversation.UnreadCount != nil &&
				conversation.GetUnreadCount() == 0 &&
				!conversation.GetMarkedAsUnread() {
				if err := messageStore.MarkChatRead(chatJID, timestamp); err != nil {
					logger.Warnf("Failed to backfill read state for %s: %v", chatJID, err)
				}
			}
			if err := messageStore.UpdateChatEphemeralSettings(
				chatJID,
				conversation.GetEphemeralExpiration(),
				conversation.GetEphemeralSettingTimestamp(),
			); err != nil {
				logger.Warnf("Failed to store history sync ephemeral settings for %s: %v", chatJID, err)
			}

			// Poll votes are decoded after the loop so the poll rows exist
			// first (history chunks are newest-first). See polls.go.
			var pendingVotes []*waWeb.WebMessageInfo

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}
				if msg.Message.Message.GetPollUpdateMessage() != nil {
					pendingVotes = append(pendingVotes, msg.Message)
					continue
				}

				// Extract text content via the shared extractor — the same
				// one the live path uses — so contact cards, media captions
				// and hydrated templates are surfaced on the history-sync
				// path too. This inline block previously handled only
				// Conversation/ExtendedText and silently dropped everything
				// else arriving through history sync.
				histViewOnce := false
				if inner, wrapped := unwrapViewOnce(msg.Message.Message); wrapped {
					msg.Message.Message = inner
					histViewOnce = true
				}
				content := extractTextContent(msg.Message.Message)
				histPoll := extractPollCreation(msg.Message.Message)
				if histPoll != nil {
					content = pollContent(histPoll)
				}

				// Extract media info - pass message timestamp + ID for unique filenames
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				histMsgID := ""
				if msg.Message != nil && msg.Message.Key != nil && msg.Message.Key.ID != nil {
					histMsgID = *msg.Message.Key.ID
				}

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message, timestamp, histMsgID)
				}
				if histPoll != nil {
					mediaType = "poll"
				}
				if histViewOnce {
					content = viewOnceContent(content, mediaType)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender. History-sync rows do not carry SenderAlt,
				// so any LID-based participant is resolved through the
				// whatsmeow LID store (populated during live message handling).
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					var rawSender types.JID
					switch {
					case isFromMe && client.Store.ID != nil:
						rawSender = client.Store.ID.ToNonAD()
					case msg.Message.GetParticipant() != "" || msg.Message.Key.GetParticipant() != "":
						// Modern history syncs carry the group sender in the top-level
						// WebMessageInfo.participant, older ones in Key.participant;
						// whatsmeow's ParseWebMessage checks them in this order too.
						// Without this every group message was attributed to the group JID.
						participant := msg.Message.GetParticipant()
						if participant == "" {
							participant = msg.Message.Key.GetParticipant()
						}
						if parsed, perr := types.ParseJID(participant); perr == nil {
							rawSender = parsed
						} else {
							rawSender = types.JID{User: participant}
						}
					default:
						rawSender = jid
					}
					var alt types.JID
					if isFromMe && client.Store.ID != nil {
						alt = client.Store.ID.ToNonAD()
					}
					sender = resolveUserJID(client, rawSender, alt).User
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				ts := msg.Message.GetMessageTimestamp()
				if ts == 0 {
					continue
				}
				msgTimestamp := time.Unix(int64(ts), 0) //nolint:gosec // WhatsApp seconds-since-epoch fit int64

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					msgTimestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
					"", // quoted_message_id: history sync does not carry ContextInfo
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					if histViewOnce {
						if verr := messageStore.MarkViewOnce(msgID, chatJID); verr != nil {
							logger.Warnf("Failed to flag view-once history message: %v", verr)
						}
					}
					if histPoll != nil {
						if perr := messageStore.StorePoll(msgID, chatJID, histPoll, msgTimestamp); perr != nil {
							logger.Warnf("Failed to store history poll: %v", perr)
						}
					}
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
			if len(pendingVotes) > 0 {
				b.historyVotes.Add(1)
				go func(chat types.JID, chatJID string, votes []*waWeb.WebMessageInfo) {
					defer b.historyVotes.Done()
					b.storeHistoryPollVotes(chat, chatJID, votes, nil)
				}(resolved, chatJID, pendingVotes)
			}
		}
	}

	bridgeLog.Infof("History sync complete. Stored %d messages.", syncedCount)
}
