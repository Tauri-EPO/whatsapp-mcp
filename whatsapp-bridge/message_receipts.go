package main

import (
	"context"
	"database/sql"
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
	// Reuse the verified resolution or wire alternate before another SDK read.
	// An unresolved LID above has already had its one mapping lookup.
	alias := receiptChatHint(chat, v.Chat)
	if alias == "" && (chat == v.Sender.ToNonAD() || chat == v.SenderAlt.ToNonAD()) {
		alias = receiptChatHint(chat, v.Sender)
		if alias == "" {
			alias = receiptChatHint(chat, v.SenderAlt)
		}
	}
	if alias == "" && chat.Server == types.DefaultUserServer {
		var err error
		alias, err = b.pendingMetadataAlt(ctx, chat.String())
		if err != nil && ctx.Err() != nil {
			return
		}
	}
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		b.Log.Errorf("Could not begin message receipt event: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	ex := &contextTransaction{Tx: tx, ctx: ctx}
	now := b.Store.pendingMetadataNow()
	if err := prunePendingReceipts(ex, now); err != nil {
		b.Log.Errorf("Could not expire pending message receipts: %v", err)
		return
	}
	inserted := false
	// Bounded statements, one transaction for all IDs from this event.
	for start := 0; start < len(v.MessageIDs); start += 256 {
		ids, err := json.Marshal(v.MessageIDs[start:min(start+256, len(v.MessageIDs))])
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO message_receipts
			 (chat_jid,message_id,recipient,recipient_server,type,at)
			 SELECT m.chat_jid,m.id,?,?,?,? FROM messages m
			 JOIN json_each(?) batch ON batch.value=m.id
			 WHERE (m.chat_jid=? OR (?<>'' AND m.chat_jid=? AND NOT EXISTS(
    SELECT 1 FROM messages exact WHERE exact.chat_jid=? AND exact.id=m.id)))
    AND m.is_from_me=1 AND COALESCE(m.media_type,'') NOT IN ('reaction','poll_vote')
			 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
			 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`,
				peer.User, peer.Server, kind, dbTime(v.Timestamp), string(ids), chat.String(), alias, alias, chat.String())
		}
		if err == nil {
			err = reconcilePendingReceiptIDs(ex, chat.String(), alias, string(ids), now)
		}
		if err == nil {
			var added bool
			added, err = storePendingReceiptIDs(ex, chat.String(), alias, peer, kind, v.Timestamp, now, string(ids))
			inserted = inserted || added
		}
		if err != nil {
			b.Log.Errorf("Could not store message receipt batch: %v", err)
			return
		}
	}
	if inserted {
		if err := capPendingReceipts(ex, chat.String()); err != nil {
			b.Log.Errorf("Could not bound pending message receipts: %v", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		b.Log.Errorf("Could not commit message receipt event: %v", err)
	}
}

// Hints come from whatsmeow's verified message source or our resolution of it;
// only opposite user namespaces can name a chat twin.
func receiptChatHint(chat, hint types.JID) string {
	current, valid := pendingMetadataUserJID(chat.String())
	other, otherValid := pendingMetadataUserJID(hint.String())
	if valid && otherValid && current.Server != other.Server {
		return other.String()
	}
	return ""
}

// Direct and verified-twin archived originals settle previously parked evidence
// in this event transaction. An exact inbound key cannot borrow an own twin.
func reconcilePendingReceiptIDs(ex sqlExecer, chat, alias, ids string, now time.Time) error {
	if _, err := ex.Exec(`INSERT INTO message_receipts (chat_jid,message_id,recipient,recipient_server,type,at)
 SELECT m.chat_jid,p.message_id,p.recipient,p.recipient_server,p.type,p.at
 FROM pending_receipts p JOIN messages m ON m.id=p.message_id
 WHERE (p.chat_jid=:chat OR (:alias<>'' AND p.chat_jid=:alias))
 AND (m.chat_jid=p.chat_jid OR (:alias<>'' AND m.chat_jid IN (:chat,:alias) AND NOT EXISTS(
 SELECT 1 FROM messages exact WHERE exact.chat_jid=p.chat_jid AND exact.id=p.message_id)))
 AND p.message_id IN (SELECT value FROM json_each(:ids)) AND p.expires_ms>:now
 AND m.is_from_me=1 AND COALESCE(m.media_type,'') NOT IN ('reaction','poll_vote')
 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type) DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`,
		sql.Named("chat", chat), sql.Named("alias", alias), sql.Named("ids", ids), sql.Named("now", now.UnixMilli())); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE (chat_jid=:chat OR (:alias<>'' AND chat_jid=:alias))
 AND message_id IN (SELECT value FROM json_each(:ids)) AND EXISTS(SELECT 1 FROM messages m
 WHERE m.id=pending_receipts.message_id AND (m.chat_jid=:chat OR (:alias<>'' AND m.chat_jid=:alias)))`, sql.Named("chat", chat), sql.Named("alias", alias), sql.Named("ids", ids))
	return err
}

// Distinguish a new insert from a replay/update so cap scans run only on growth.
// A known inbound or non-original stanza is never parked, including in its twin.
func storePendingReceiptIDs(ex sqlExecer, chat, alias string, peer types.JID, kind string, stamp, now time.Time, ids string) (bool, error) {
	res, err := ex.Exec(`INSERT INTO pending_receipts
 (chat_jid,message_id,recipient,recipient_server,type,at,arrived_ms,expires_ms)
 SELECT :chat,batch.value,:peer,:server,:kind,:at,:now,:expires FROM json_each(:ids) batch
 WHERE batch.type='text' AND length(batch.value)>0 AND length(CAST(batch.value AS BLOB))+:key_bytes<=:max_bytes
 AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.id=batch.value AND (m.chat_jid=:chat OR (:alias<>'' AND m.chat_jid=:alias)))
 AND NOT EXISTS(SELECT 1 FROM receipt_nonoriginals n WHERE n.message_id=batch.value AND n.expires_ms>:now AND (n.chat_jid=:chat OR (:alias<>'' AND n.chat_jid=:alias)))
 ON CONFLICT DO NOTHING`, sql.Named("chat", chat), sql.Named("alias", alias), sql.Named("peer", peer.User), sql.Named("server", peer.Server), sql.Named("kind", kind), sql.Named("at", dbTime(stamp)), sql.Named("now", now.UnixMilli()), sql.Named("expires", now.Add(pendingReceiptTTL).UnixMilli()), sql.Named("ids", ids), sql.Named("key_bytes", len(chat)+len(peer.User)+len(peer.Server)+len(kind)), sql.Named("max_bytes", pendingReceiptKeyBytes))
	if err != nil {
		return false, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	_, err = ex.Exec(`UPDATE pending_receipts SET at=MAX(at,?) WHERE chat_jid=? AND recipient=? AND recipient_server=? AND type=? AND message_id IN (SELECT value FROM json_each(?))`, dbTime(stamp), chat, peer.User, peer.Server, kind, ids)
	return count > 0, err
}
