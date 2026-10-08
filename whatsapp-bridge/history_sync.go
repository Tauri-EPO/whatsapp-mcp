package main

// History sync: messages the phone replays at pair time or on demand
// (history_ondemand.go). Mirrors handleMessage but works on
// waWeb.WebMessageInfo rows instead of live events.

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Keep history write locks bounded; the next sync safely upserts partial imports.
const historyBatchMessages = 500

func (b *Bridge) handleHistorySync(historySync *events.HistorySync) {
	b.handleHistorySyncWithShares(historySync, true, false)
}

// A decoded share uses the canonical importer without following nested bundles.
func (b *Bridge) handleHistorySyncWithShares(historySync *events.HistorySync, downloadShares, preserveExisting bool) {
	b.handleHistorySyncWithSharesContext(b.ctx, historySync, downloadShares, preserveExisting)
}

func (b *Bridge) handleHistorySyncWithSharesContext(ctx context.Context, historySync *events.HistorySync, downloadShares, preserveExisting bool) {
	stopping := func() bool {
		return (preserveExisting && ctx.Err() != nil) || b.historyStopping()
	}
	retry := b.retryBusy
	if preserveExisting {
		retry = func(write func() error) error {
			return retryBusyWithWait(func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return write()
			}, b.StoreRetryDelays, func(delay time.Duration) bool {
				if ctx.Err() != nil {
					return false
				}
				if b.storeRetryWait != nil {
					return b.storeRetryWait(delay) && ctx.Err() == nil
				}
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return false
				case <-timer.C:
					return true
				}
			})
		}
	}
	// Peer imports can introduce rows, never replace any existing archive row.
	// Check existence under the canonical IMMEDIATE transaction on every replay.
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

	// Recognise once outside retries. Queue bundles only after this phone sync's
	// own rows have completed; imported peer history never follows nested shares.
	b.recogniseHistoryShares(historySync.Data, false)
	if downloadShares {
		defer b.queueHistoryShares(historySync.Data)
	}
	writeBatch := messageStore.Batch
	if b.historyBatchWriter != nil {
		writeBatch = b.historyBatchWriter
	}
	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		if stopping() {
			return
		}
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
			timestamp = b.historyLocationActivityTime(messages, chatJID, timestamp)

			if err := retry(func() error {
				if preserveExisting {
					return messageStore.EnsureChat(chatJID, name)
				}
				return messageStore.StoreChat(chatJID, name, timestamp)
			}); err != nil {
				if stopping() {
					return
				}
				b.noteHistoryLoss(messages, timestamp, chatJID, err)
				continue
			}
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

			// The synchronous callback captures the current chunk; keep extraction
			// unchanged and retry an entire transaction rather than individual rows.
			chunk := messages
			storedInBatch := 0
			var chunkPeerRows map[string]struct{}
			storeChunk := func(batch *messageBatch) error {
				for _, msg := range chunk {
					if preserveExisting && ctx.Err() != nil {
						return ctx.Err()
					}
					if msg == nil || msg.Message == nil {
						continue
					}
					if msg.Message.Message.GetPollUpdateMessage() != nil {
						continue
					}

					histMsgID := ""
					if msg.Message != nil && msg.Message.Key != nil && msg.Message.Key.ID != nil {
						histMsgID = *msg.Message.Key.ID
					}
					// Same extraction as the live path (persist.go): view-once
					// unwrap, text, media, poll. Works on a local view; the
					// whatsmeow payload is not mutated.
					ex := extractMessage(msg.Message.Message, timestamp, histMsgID)
					content, mediaType, filename := ex.content, ex.mediaType, ex.filename

					// Log the message content for debugging
					logger.Debugf("Message content: %v, Media Type: %v", content, mediaType)

					// Skip messages with no content and no media
					if ex.empty() {
						continue
					}

					if preserveExisting {
						exists, err := batch.historyRowExists(histMsgID, chatJID)
						if err != nil {
							return err
						}
						if exists {
							continue
						}
					}

					resolvedSender, isFromMe := b.historySender(msg.Message, jid, preserveExisting)
					sender := resolvedSender.User
					// The row records which namespace that user part belongs
					// to, so a LID the store cannot map is not read back as a
					// phone number (#375).
					storedSenderJID := storedSender(resolvedSender)

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
					if !preserveExisting && ex.location != nil && ex.location.Live {
						storedSenderJID, err = b.liveLocationSender(ctx, batch.tx, msgID, chatJID, storedSenderJID, isFromMe)
						if err != nil {
							return err
						}
					}

					// quoted_message_id is not persisted: history sync does not
					// carry a usable ContextInfo.
					err = persistMessage(batch, msgID, chatJID, storedSenderJID, msgTimestamp, isFromMe, ex, false, logger)
					if err == nil {
						err = batch.failure
					}
					if err != nil {
						return err
					} else {
						storedInBatch++
						if preserveExisting {
							chunkPeerRows[histMsgID] = struct{}{}
						}
						// Per-message echo stays at DEBUG: user content out of INFO,
						// and two lines per row would swamp a full sync.
						if mediaType != "" {
							logger.Debugf("Stored message: [%s] %s -> %s: [%s: %s] %s",
								msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
						} else {
							logger.Debugf("Stored message: [%s] %s -> %s: %s",
								msgTimestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
						}
					}
				}
				if preserveExisting && len(chunkPeerRows) > 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
					return batch.storePeerHistoryActivity(chatJID, chunkPeerRows)
				}
				return nil
			}
			storedInChat := 0
			processed := len(messages)
			commitChunk := func() error {
				return retry(func() error {
					storedInBatch = 0
					chunkPeerRows = make(map[string]struct{})
					return writeBatch(storeChunk)
				})
			}
			countCommitted := func() {
				syncedCount += storedInBatch
				storedInChat += storedInBatch
				b.metrics.historyMessages.Add(int64(storedInBatch))

			}
		chunks:
			for start := 0; start < len(messages); start += historyBatchMessages {
				if stopping() {
					return
				}
				chunk = messages[start:min(start+historyBatchMessages, len(messages))]
				batchErr := commitChunk()
				if batchErr == nil {
					countCommitted()
					continue
				}
				if stopping() {
					return
				}
				if isBusyError(batchErr) {
					// Preserve the newest committed prefix for request_history's anchor.
					processed = start
					b.noteHistoryLoss(messages[start:], timestamp, chatJID, batchErr)
					break
				}
				// A bad row must not cost its good neighbours. Each replay remains
				// atomic with its auxiliary writes and owns one retry budget.
				for index, msg := range chunk {
					if stopping() {
						return
					}
					chunk = []*waHistorySync.HistorySyncMsg{msg}
					rowErr := commitChunk()
					if rowErr == nil {
						countCommitted()
						continue
					}
					if stopping() {
						return
					}
					if isBusyError(rowErr) {
						processed = start + index
						b.noteHistoryLoss(messages[processed:], timestamp, chatJID, rowErr)
						break chunks
					}
					b.noteHistoryLoss(chunk, timestamp, chatJID, rowErr)
				}
			}
			logger.Infof("History sync: %s stored %d of %d messages", chatJID, storedInChat, len(messages))
			for _, msg := range messages[:processed] {
				// A peer knows a group's poll secret and cannot authenticate another
				// voter's identity. Only the account's own phone history imports votes.
				if !preserveExisting && msg != nil && msg.Message != nil && msg.Message.Message.GetPollUpdateMessage() != nil {
					pendingVotes = append(pendingVotes, msg.Message)
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

	b.Log.Infof("History sync complete. Stored %d messages.", syncedCount)
}

// Used only after a chunk fails: count actual storable rows, excluding sparse
// envelopes and poll updates, so rollback losses include the unattempted tail.
func (b *Bridge) noteHistoryLoss(messages []*waHistorySync.HistorySyncMsg, fallback time.Time, chat string, err error) {
	count := 0
	first, last := "", ""
	for _, msg := range messages {
		if msg == nil || msg.Message == nil || msg.Message.GetMessageTimestamp() == 0 || msg.Message.Message.GetPollUpdateMessage() != nil {
			continue
		}
		if ex := extractMessage(msg.Message.Message, fallback, msg.Message.GetKey().GetID()); !ex.empty() {
			last = msg.Message.GetKey().GetID()
			if count == 0 {
				first = last
			}
			count++
		}
	}
	b.metrics.storeFailures.Add(int64(count))
	b.Log.Errorf("Failed to store %d history messages in %s (first ID %s, last ID %s): %v", count, chat, first, last, err)
}

func (b *Bridge) historyStopping() bool {
	if b.ctx != nil && b.ctx.Err() != nil {
		b.Log.Infof("History sync stopped during shutdown")
		return true
	}
	return false
}

// Peer attribution was resolved by the context-bound adapter; the account's
// own phone keeps its canonical participant / own-account / LID precedence.
func (b *Bridge) historySender(info *waWeb.WebMessageInfo, chat types.JID, peer bool) (types.JID, bool) {
	if info.Key == nil {
		return chat, false
	}
	own := info.Key.GetFromMe()
	var raw types.JID
	switch {
	case own && b.Client.Store.ID != nil:
		raw = b.Client.Store.ID.ToNonAD()
	case info.GetParticipant() != "" || info.Key.GetParticipant() != "":
		participant := info.GetParticipant()
		if participant == "" {
			participant = info.Key.GetParticipant()
		}
		if parsed, err := types.ParseJID(participant); err == nil {
			raw = parsed
		} else {
			raw = types.JID{User: participant}
		}
	default:
		raw = chat
	}
	if peer {
		return raw, own
	}
	var alt types.JID
	if own && b.Client.Store.ID != nil {
		alt = b.Client.Store.ID.ToNonAD()
	}
	return resolveUserJID(b.Client, raw, alt), own
}
