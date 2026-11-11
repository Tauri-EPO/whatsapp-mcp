package main

import (
	"context"
	"encoding/json"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const messageReceiptsSchema = `CREATE TABLE IF NOT EXISTS message_receipts (
 chat_jid TEXT NOT NULL, message_id TEXT NOT NULL,
 recipient TEXT NOT NULL, recipient_server TEXT NOT NULL,
 type TEXT NOT NULL, at TIMESTAMP NOT NULL,
 PRIMARY KEY(chat_jid, message_id, recipient, recipient_server, type)
);
CREATE TRIGGER IF NOT EXISTS message_receipts_original_delete
AFTER DELETE ON messages BEGIN
 DELETE FROM message_receipts
 WHERE chat_jid=OLD.chat_jid AND message_id=OLD.id;
END;`

type peerReceiptJob struct {
	receipt events.Receipt
	client  *whatsmeow.Client
}

// One lifecycle-owned worker bounds event-handler work and retained ID memory.
// Overflow drops a whole event rather than exposing a partly stored receipt.
func (b *Bridge) queuePeerReceipt(v *events.Receipt) {
	if v.IsFromMe || v.Timestamp.IsZero() || b.Store == nil || len(v.MessageIDs) == 0 {
		return
	}
	if v.Type != types.ReceiptTypeDelivered && v.Type != types.ReceiptTypeRead && v.Type != types.ReceiptTypePlayed {
		return
	}
	bytes := 0
	if len(v.MessageIDs) <= 16384 {
		for _, id := range v.MessageIDs {
			bytes += len(id)
			if bytes > 1<<20 {
				break
			}
		}
	}
	if len(v.MessageIDs) > 16384 || bytes > 1<<20 {
		b.Log.Warnf("Peer receipt exceeded event budget; receipt dropped")
		return
	}
	b.peerReceiptMu.Lock()
	defer b.peerReceiptMu.Unlock()
	if b.peerReceiptStopped || b.ctx.Err() != nil {
		return
	}
	if b.peerReceiptQueue == nil {
		b.peerReceiptQueue = make(chan peerReceiptJob, 64)
		b.peerReceiptDone = make(chan struct{})
		b.peerReceiptWait.Add(1)
		go b.peerReceiptLoop()
	}
	copy := *v
	copy.MessageIDs = append([]types.MessageID(nil), v.MessageIDs...)
	select {
	case b.peerReceiptQueue <- peerReceiptJob{receipt: copy, client: b.currentClient()}:
	default:
		b.Log.Warnf("Peer receipt queue full; receipt dropped")
	}
}

func (b *Bridge) peerReceiptLoop() {
	defer b.peerReceiptWait.Done()
	defer close(b.peerReceiptDone)
	for {
		select {
		case <-b.ctx.Done():
			return
		case job := <-b.peerReceiptQueue:
			if b.ctx.Err() != nil {
				return
			}
			if job.client == b.currentClient() {
				b.writePeerReceipts(&job.receipt, job.client)
			}
			if b.peerReceiptProcessed != nil {
				b.peerReceiptProcessed()
			}
		}
	}
}

// Receipts from peers acknowledge our messages; IsFromMe describes the receipt
// sender, not the message. The SQL checks ownership using the full message key.
func (b *Bridge) storePeerReceipts(v *events.Receipt) {
	b.writePeerReceipts(v, b.currentClient())
}

func (b *Bridge) writePeerReceipts(v *events.Receipt, client *whatsmeow.Client) {
	if v.IsFromMe || v.Timestamp.IsZero() || b.Store == nil {
		return
	}
	kind := string(v.Type)
	switch v.Type {
	case types.ReceiptTypeDelivered:
		kind = "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
	default:
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	peer := resolveUserJIDContext(ctx, client, v.Sender, v.SenderAlt).ToNonAD()
	if peer.User == "" || (peer.Server != types.DefaultUserServer && peer.Server != types.HiddenUserServer) {
		return
	}
	chat := v.Chat.ToNonAD()
	if chat == v.Sender.ToNonAD() || chat == v.SenderAlt.ToNonAD() {
		chat = peer
	} else {
		chat = resolveLIDChatContext(ctx, client, v.Chat, v.SenderAlt, v.RecipientAlt, false).ToNonAD()
	}
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		b.Log.Errorf("Could not begin message receipt event: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	ex := &contextTransaction{Tx: tx, ctx: ctx}
	now := b.Store.pendingEditNow()
	if err := prunePendingReceipts(ex, now); err != nil {
		b.Log.Errorf("Could not expire pending message receipts: %v", err)
		return
	}
	// Bounded statements, one transaction for all IDs from this event.
	for start := 0; start < len(v.MessageIDs); start += 256 {
		ids, err := json.Marshal(v.MessageIDs[start:min(start+256, len(v.MessageIDs))])
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO message_receipts
			 (chat_jid,message_id,recipient,recipient_server,type,at)
			 SELECT m.chat_jid,m.id,?,?,?,? FROM messages m
			 JOIN json_each(?) batch ON batch.value=m.id
			 WHERE m.chat_jid=? AND m.is_from_me=1
			 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
			 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`,
				peer.User, peer.Server, kind, dbTime(v.Timestamp), string(ids), chat.String())
		}
		if err == nil {
			// Absence is distinct from an inbound original. Never queue receipts
			// for a known inbound message, and never insert one row at a time.
			_, err = tx.ExecContext(ctx, `INSERT INTO pending_receipts
			 (chat_jid,message_id,recipient,recipient_server,type,at,arrived_ms,expires_ms)
			 SELECT ?,batch.value,?,?,?,?,?,? FROM json_each(?) batch
			 WHERE batch.type='text' AND length(batch.value)>0
			 AND length(CAST(batch.value AS BLOB))+?<=?
			 AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.chat_jid=? AND m.id=batch.value)
			 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
			 DO UPDATE SET at=MAX(pending_receipts.at,excluded.at)`,
				chat.String(), peer.User, peer.Server, kind, dbTime(v.Timestamp), now.UnixMilli(), now.Add(pendingReceiptTTL).UnixMilli(),
				string(ids), len(chat.String())+len(peer.User)+len(peer.Server)+len(kind), pendingReceiptKeyBytes, chat.String())
		}
		if err != nil {
			b.Log.Errorf("Could not store message receipt batch: %v", err)
			return
		}
	}
	if err := capPendingReceipts(ex); err != nil {
		b.Log.Errorf("Could not bound pending message receipts: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		b.Log.Errorf("Could not commit message receipt event: %v", err)
	}
}
