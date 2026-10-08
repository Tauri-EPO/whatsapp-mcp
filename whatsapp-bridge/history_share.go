package main

import (
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	historyShareCompressedLimit = 16 << 20
	historyShareInflatedLimit   = 64 << 20
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
	if kind != "bundle" || !download || b.ctx.Err() != nil {
		return
	}
	if !historyShareGroupOrigin(chat) {
		b.Log.Warnf("Shared history import failed: originating group scope refused")
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, historyShareTimeout)
	defer cancel()
	// Callers wait in their existing event path; no extra goroutines or queues.
	// Nested bundles are recognised above with download=false, so cannot deadlock.
	b.historyShareInit.Do(func() { b.historyShareGate = make(chan struct{}, 1) })
	select {
	case b.historyShareGate <- struct{}{}:
		defer func() { <-b.historyShareGate }()
	case <-ctx.Done():
		return
	}
	data, err := b.decodeHistoryShare(ctx, msg.GetMessageHistoryBundle(), historyShareCompressedLimit, historyShareInflatedLimit)
	if err != nil {
		// SDK errors can contain CDN paths; neither errors nor payloads are logged.
		b.Log.Warnf("Shared history import failed: download, validation or decoding refused")
		return
	}
	if !historyShareMatchesGroup(data, chat) {
		b.Log.Warnf("Shared history import failed: conversation scope refused")
		return
	}
	if ctx.Err() == nil {
		b.handleHistorySyncWithShares(&events.HistorySync{Data: data}, false)
	}
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
		err = b.Client.DownloadToFile(boundedCtx, notif, bounded)
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
	data := &waHistorySync.HistorySync{}
	if err := (proto.UnmarshalOptions{RecursionLimit: 100}).Unmarshal(plain, data); err != nil {
		return nil, err
	}
	return data, nil
}
