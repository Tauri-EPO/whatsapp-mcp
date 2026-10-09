package main

import (
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	historyShareCompressedLimit = 16 << 20
	historyShareInflatedLimit   = 4 << 20
	historyShareTimeout         = 2 * time.Minute
)

// A bundle is history media, but the pinned SDK does not map its proto name.
func historyShareNotification(bundle *waE2E.MessageHistoryBundle) *waE2E.HistorySyncNotification {
	return &waE2E.HistorySyncNotification{
		DirectPath: bundle.DirectPath, MediaKey: bundle.MediaKey,
		FileSHA256: bundle.FileSHA256, FileEncSHA256: bundle.FileEncSHA256,
	}
}

func (b *Bridge) recogniseHistoryShares(data *waHistorySync.HistorySync, download bool) {
	for _, conversation := range data.GetConversations() {
		for _, row := range conversation.GetMessages() {
			if b.ctx.Err() != nil {
				return
			}
			info := row.GetMessage()
			ex := extractMessage(info.GetMessage(), time.Time{}, info.GetKey().GetID())
			b.processHistoryShare(ex.inner, conversation.GetID(), info.GetKey().GetID(), info.GetKey().GetFromMe(), download)
		}
	}
}

func (b *Bridge) handleHistoryShare(msg *waE2E.Message, chat, id string, fromMe bool) {
	b.processHistoryShare(msg, chat, id, fromMe, true)
}

func (b *Bridge) processHistoryShare(msg *waE2E.Message, chat, id string, fromMe, download bool) {
	kind, meta := sharedGroupHistory(msg)
	if kind == "" {
		return
	}
	b.metrics.groupHistoryShares.Add(1)
	b.Log.Infof("Group history %s seen in %s (message %s, from_me=%t): %s; shared history recognised",
		kind, chat, id, fromMe, describeSharedGroupHistory(meta))
	if kind != "bundle" || !download {
		return
	}
	b.queueHistoryShare(msg.GetMessageHistoryBundle(), chat)
}

// Own-phone history calls this only after its rows have been persisted. Nested
// bundles use recognition only and never reach this queue-only pass.
func (b *Bridge) queueHistoryShares(data *waHistorySync.HistorySync) {
	for _, conversation := range data.GetConversations() {
		for _, row := range conversation.GetMessages() {
			info := row.GetMessage()
			ex := extractMessage(info.GetMessage(), time.Time{}, info.GetKey().GetID())
			if bundle := ex.inner.GetMessageHistoryBundle(); bundle != nil {
				b.queueHistoryShare(bundle, conversation.GetID())
			}
		}
	}
}

func (b *Bridge) queueHistoryShare(bundle *waE2E.MessageHistoryBundle, chat string) {
	if !historyShareGroupOrigin(chat) {
		b.Log.Warnf("Shared history import failed: originating group scope refused")
		return
	}
	if bundle == nil || len(bundle.GetDirectPath()) > 2048 || !strings.HasPrefix(bundle.GetDirectPath(), "/") || len(bundle.GetMediaKey()) != 32 || len(bundle.GetFileSHA256()) != 32 || len(bundle.GetFileEncSHA256()) != 32 {
		b.Log.Warnf("Shared history import failed: incomplete or oversized credentials")
		return
	}
	b.historyShareMu.Lock()
	defer b.historyShareMu.Unlock()
	if b.historyShareStopped || b.ctx.Err() != nil {
		b.Log.Warnf("Shared history import failed: job cancelled")
		return
	}
	if b.historyShares == nil {
		b.historyShares = newHistoryShareQueue(b.ctx, b.runHistoryShare)
	}
	timeout := b.historyShareJobTimeout
	if timeout == 0 {
		timeout = historyShareTimeout
	}
	// Retain only the bounded download credentials, not the live event tree.
	credentials := &waE2E.MessageHistoryBundle{
		DirectPath:    proto.String(bundle.GetDirectPath()),
		MediaKey:      append([]byte(nil), bundle.GetMediaKey()...),
		FileSHA256:    append([]byte(nil), bundle.GetFileSHA256()...),
		FileEncSHA256: append([]byte(nil), bundle.GetFileEncSHA256()...),
	}
	if !b.historyShares.submit(historyShareJob{bundle: credentials, chat: chat, deadline: time.Now().Add(timeout)}) {
		b.Log.Warnf("Shared history import failed: queue full (one active, one waiting)")
	}
}

func (b *Bridge) runHistoryShare(ctx context.Context, job historyShareJob) {
	data, err := b.decodeHistoryShare(ctx, job.bundle, historyShareCompressedLimit, historyShareInflatedLimit)
	if err != nil {
		// SDK errors can contain CDN paths; neither errors nor payloads are logged.
		b.Log.Warnf("Shared history import failed: download, validation or decoding refused")
		return
	}
	if !historyShareMatchesGroup(data, job.chat) {
		b.Log.Warnf("Shared history import failed: conversation scope refused")
		return
	}
	messages, skipped, err := b.historyShareMessages(ctx, data)
	if err != nil {
		b.Log.Warnf("Shared history import failed: message validation, participant lookup or job cancellation")
		return
	}
	b.handleHistorySyncWithSharesContext(ctx, &events.HistorySync{Data: messages}, false, true)
	if ctx.Err() != nil {
		b.Log.Warnf("Shared history import failed: job cancelled")
		return
	}
	b.Log.Infof("Shared history import finished: skipped %d receiver-attributed rows", skipped)
}

// The sender supplies both the media key and hashes: integrity proves the
// bytes, not that a participant owns another conversation's history. Validate
// the complete bundle before any canonical import can mutate archive state.
func historyShareGroupOrigin(chat string) bool {
	jid, err := canonicalChatJID(chat, false)
	return err == nil && jid.Server == types.GroupServer && jid.String() == chat
}

func historyShareMatchesGroup(data *waHistorySync.HistorySync, chat string) bool {
	if !historyShareGroupOrigin(chat) || len(data.GetConversations()) == 0 {
		return false
	}
	for _, conversation := range data.GetConversations() {
		if conversation.GetID() != chat {
			return false
		}
		for _, row := range conversation.GetMessages() {
			remote := row.GetMessage().GetKey().GetRemoteJID()
			if remote != "" && remote != chat {
				return false
			}
		}
	}
	return true
}

// A participant's bundle is message data, not our own phone's account state.
// Keep an explicit allow-list so peer read markers, disappearing-message
// settings and future conversation metadata never enter the phone importer.
func (b *Bridge) historyShareMessages(ctx context.Context, data *waHistorySync.HistorySync) (*waHistorySync.HistorySync, int, error) {
	client := b.currentClient()
	phone, lid := clientIdentity(client)(ctx)
	if phone.Server == types.HostedServer {
		phone.Server = types.DefaultUserServer
	}
	if lid.Server == types.HostedLIDServer {
		lid.Server = types.HiddenUserServer
	}
	messages := &waHistorySync.HistorySync{SyncType: data.SyncType}
	skipped := 0
	// At most two participant keys per bounded row. Repeated senders reuse the
	// context-bound map answer instead of performing thousands of SQLite reads.
	alternates := make(map[types.JID]types.JID)
	for _, conversation := range data.GetConversations() {
		clean := &waHistorySync.Conversation{ID: conversation.ID}
		for _, row := range conversation.GetMessages() {
			if ctx.Err() != nil {
				return nil, skipped, ctx.Err()
			}
			if row == nil || row.Message == nil {
				clean.Messages = append(clean.Messages, row)
				continue
			}
			// The decoder produced an exclusively owned tree. Rewrite attribution
			// in place instead of doubling every peer-controlled proto allocation.
			info := row.Message
			// Peer seconds are unsigned. Validate before converting to int64:
			// the Python readers and fixed-width SQLite ordering stop at 9999.
			if info.GetMessageTimestamp() > 253402300799 {
				return nil, skipped, errors.New("shared history timestamp outside archive domain")
			}
			if info.Key == nil {
				info.Key = &waCommon.MessageKey{}
			}
			sender := types.EmptyJID
			own := false
			// Check both attribution fields: a conflicting alternate must not
			// smuggle an owner identity into a row. Compare full namespaces.
			for _, raw := range []string{info.GetParticipant(), info.Key.GetParticipant()} {
				candidate, err := normalizedUserJID(raw)
				if err != nil {
					continue
				}
				candidate = candidate.ToNonAD()
				switch candidate.Server {
				case types.DefaultUserServer, types.HostedServer:
					candidate.Server = types.DefaultUserServer
				case types.HiddenUserServer, types.HostedLIDServer:
					candidate.Server = types.HiddenUserServer
				default:
					continue
				}
				own = own || candidate == phone.ToNonAD() || candidate == lid.ToNonAD()
				if !own {
					alt, known := alternates[candidate]
					if !known {
						var err error
						alt, err = lookupAltJID(ctx, client, candidate)
						if err != nil {
							return nil, skipped, err
						}
						alt = alt.ToNonAD()
						switch alt.Server {
						case types.HostedServer:
							alt.Server = types.DefaultUserServer
						case types.HostedLIDServer:
							alt.Server = types.HiddenUserServer
						}
						alternates[candidate] = alt
					}
					own = !alt.IsEmpty() && (alt == phone.ToNonAD() || alt == lid.ToNonAD())
					if candidate.Server == types.HiddenUserServer && !alt.IsEmpty() {
						candidate = alt
					}
				}
				own = own || candidate == phone.ToNonAD() || candidate == lid.ToNonAD()
				if sender.IsEmpty() {
					sender = candidate
				}
			}
			if own {
				skipped++
				continue
			}
			participant := ""
			if !sender.IsEmpty() {
				participant = sender.String()
			}
			info.Participant, info.Key.Participant = proto.String(participant), proto.String(participant)
			// FromMe belongs to the exporting peer, not this receiver. Missing
			// or invalid participant metadata never establishes ownership.
			info.Key.FromMe = proto.Bool(false)
			clean.Messages = append(clean.Messages, row)
		}
		messages.Conversations = append(messages.Conversations, clean)
	}
	return messages, skipped, ctx.Err()
}

func (b *Bridge) decodeHistoryShare(ctx context.Context, bundle *waE2E.MessageHistoryBundle, compressedLimit, inflatedLimit int64) (*waHistorySync.HistorySync, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !strings.HasPrefix(bundle.GetDirectPath(), "/") || len(bundle.GetMediaKey()) != 32 || len(bundle.GetFileSHA256()) != 32 || len(bundle.GetFileEncSHA256()) != 32 {
		return nil, errors.New("incomplete shared history credentials")
	}
	file, err := os.CreateTemp("", "whatsapp-history-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	boundedCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bounded := &cappedMediaFile{file: file, limit: (compressedLimit/16+1)*16 + 10, cancel: cancel}
	notif := historyShareNotification(bundle)
	if b.historyShareDownload != nil {
		err = b.historyShareDownload(boundedCtx, notif, bounded)
	} else {
		client := b.currentClient()
		if client == nil {
			return nil, errors.New("shared history client unavailable")
		}
		err = client.DownloadToFile(boundedCtx, notif, bounded)
	}
	if bounded.exceeded {
		return nil, errors.New("shared history compressed limit exceeded")
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > compressedLimit {
		return nil, errors.New("shared history compressed limit exceeded")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return inflateHistoryShare(ctx, file, inflatedLimit)
}

type historyShareReader struct {
	ctx context.Context
	io.Reader
}

func (r historyShareReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func inflateHistoryShare(ctx context.Context, compressed io.Reader, limit int64) (*waHistorySync.HistorySync, error) {
	zr, err := zlib.NewReader(historyShareReader{ctx, compressed})
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	plain, err := io.ReadAll(io.LimitReader(historyShareReader{ctx, zr}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > limit {
		return nil, errors.New("shared history inflated limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Scan wire counts before allocating any objects in the peer's proto tree.
	if _, err := scanHistoryShareWire(ctx, plain); err != nil {
		return nil, err
	}
	data := &waHistorySync.HistorySync{}
	if err := (proto.UnmarshalOptions{RecursionLimit: 100, DiscardUnknown: true}).Unmarshal(plain, data); err != nil {
		return nil, err
	}
	return data, nil
}
