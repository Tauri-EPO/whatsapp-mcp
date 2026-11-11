package main

import (
	"context"
	"database/sql"
	"time"
)

const (
	pendingReceiptTTL       = 2 * time.Minute
	pendingReceiptGlobalCap = 4096
	pendingReceiptChatCap   = 1024
	pendingReceiptReaderCap = 256
	pendingReceiptKeyBytes  = 1024
)

// No foreign key: a receipt must not manufacture an original or archive chat.
// Arrival/expiry use the local arrival clock, never the peer's timestamp.
const pendingReceiptsSchema = `CREATE TABLE IF NOT EXISTS pending_receipts (
	chat_jid TEXT NOT NULL, message_id TEXT NOT NULL,
	recipient TEXT NOT NULL, recipient_server TEXT NOT NULL,
	type TEXT NOT NULL, at TIMESTAMP NOT NULL,
	arrived_ms INTEGER NOT NULL, expires_ms INTEGER NOT NULL,
	PRIMARY KEY(chat_jid,message_id,recipient,recipient_server,type)
);
CREATE INDEX IF NOT EXISTS idx_pending_receipts_expiry ON pending_receipts(expires_ms);
CREATE INDEX IF NOT EXISTS idx_pending_receipts_target ON pending_receipts(message_id,chat_jid);
CREATE INDEX IF NOT EXISTS idx_pending_receipts_arrival ON pending_receipts(arrived_ms);
CREATE INDEX IF NOT EXISTS idx_pending_receipts_chat_arrival ON pending_receipts(chat_jid,arrived_ms);
CREATE TABLE IF NOT EXISTS receipt_nonoriginals (
 chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, expires_ms INTEGER NOT NULL,
 PRIMARY KEY(chat_jid,message_id)
);
CREATE INDEX IF NOT EXISTS idx_receipt_nonoriginals_expiry ON receipt_nonoriginals(expires_ms);`

// Startup reconciles verified pending twins with already archived originals too.
// Collisions retain latest evidence but the earliest arrival/expiry.
func migratePendingReceiptChat(ex sqlExecer, lid, phone string, now time.Time) error {
	if _, err := ex.Exec(`INSERT INTO pending_receipts
	 (chat_jid,message_id,recipient,recipient_server,type,at,arrived_ms,expires_ms)
	 SELECT :phone,p.message_id,p.recipient,p.recipient_server,p.type,p.at,p.arrived_ms,p.expires_ms
	 FROM pending_receipts p WHERE p.chat_jid=:lid
	 AND length(CAST(:phone AS BLOB))+length(CAST(p.message_id AS BLOB))+
	 length(CAST(p.recipient AS BLOB))+length(CAST(p.recipient_server AS BLOB))+length(CAST(p.type AS BLOB))<=:max_bytes
	 AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.chat_jid=:phone AND m.id=p.message_id AND m.is_from_me IS NOT 1)
	 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type) DO UPDATE SET
	 at=MAX(pending_receipts.at,excluded.at),arrived_ms=MIN(pending_receipts.arrived_ms,excluded.arrived_ms),
	 expires_ms=MIN(pending_receipts.expires_ms,excluded.expires_ms)`,
		sql.Named("phone", phone), sql.Named("lid", lid), sql.Named("max_bytes", pendingReceiptKeyBytes)); err != nil {
		return err
	}
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE chat_jid=?`, lid); err != nil {
		return err
	}
	if _, err := ex.Exec(`INSERT INTO message_receipts (chat_jid,message_id,recipient,recipient_server,type,at)
 SELECT p.chat_jid,p.message_id,p.recipient,p.recipient_server,p.type,p.at
 FROM pending_receipts p JOIN messages m ON m.chat_jid=p.chat_jid AND m.id=p.message_id
 WHERE p.chat_jid=? AND p.expires_ms>? AND m.is_from_me=1 AND COALESCE(m.media_type,'') NOT IN ('reaction','poll_vote')
 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type) DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`, phone, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE chat_jid=? AND EXISTS
 (SELECT 1 FROM messages m WHERE m.chat_jid=pending_receipts.chat_jid AND m.id=pending_receipts.message_id)`, phone); err != nil {
		return err
	}
	return capPendingReceipts(ex, phone)
}

// Two minutes covers ordinary event/write scheduling and the three SQLite busy
// attempts (~16 s). Receipt events carry no stanza type, so unknown edits,
// revokes and unsupported messages must not occupy a day-long pending buffer.
func prunePendingReceipts(ex sqlExecer, now time.Time) error {
	// The arrival guard also shortens rows created by a pre-upgrade longer TTL.
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE expires_ms<=? OR arrived_ms<=?`, now.UnixMilli(), now.Add(-pendingReceiptTTL).UnixMilli()); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM receipt_nonoriginals WHERE expires_ms<=?`, now.UnixMilli())
	return err
}

// Only call after insertion/rekeying. Reader identities remain typed (a phone
// and its LID each have 256 slots); reader/chat scans touch this chat only.
func capPendingReceipts(ex sqlExecer, chat string) error {
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM (
 SELECT rowid,ROW_NUMBER() OVER (PARTITION BY recipient,recipient_server ORDER BY arrived_ms DESC,rowid DESC) n
 FROM pending_receipts WHERE chat_jid=?) WHERE n>?)`, chat, pendingReceiptReaderCap); err != nil {
		return err
	}
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM pending_receipts
 WHERE chat_jid=? ORDER BY arrived_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, chat, pendingReceiptChatCap); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM pending_receipts
 ORDER BY arrived_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, pendingReceiptGlobalCap)
	return err
}

// A known non-original stanza never authorizes an archived-message receipt.
// Its bounded negative cache lasts only as long as the unknown-receipt buffer.
func excludePeerReceiptStanza(ex sqlExecer, chat, id string, now time.Time) error {
	if id == "" || len(chat)+len(id) > pendingReceiptKeyBytes {
		return nil
	}
	if _, err := ex.Exec(`INSERT INTO receipt_nonoriginals VALUES(?,?,?) ON CONFLICT DO NOTHING`, chat, id, now.Add(pendingReceiptTTL).UnixMilli()); err != nil {
		return err
	}
	if _, err := ex.Exec(`DELETE FROM pending_receipts WHERE chat_jid=? AND message_id=?`, chat, id); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM receipt_nonoriginals WHERE rowid IN (SELECT rowid FROM receipt_nonoriginals ORDER BY expires_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, pendingReceiptGlobalCap)
	return err
}

func (b *Bridge) excludeNonOriginalReceipts(chat, id string, own bool) {
	if !own || b.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	alias, err := b.pendingMetadataAlt(ctx, chat)
	if err != nil && ctx.Err() != nil {
		return
	}
	err = b.Store.BatchContext(ctx, func(batch *messageBatch) error {
		now := b.Store.pendingMetadataNow()
		if err := prunePendingReceipts(batch.tx, now); err != nil {
			return err
		}
		if err := excludePeerReceiptStanza(batch.tx, chat, id, now); err != nil {
			return err
		}
		if alias != "" {
			return excludePeerReceiptStanza(batch.tx, alias, id, now)
		}
		return nil
	})
	if err != nil {
		b.Log.Warnf("Could not exclude non-original receipt metadata: %v", err)
	}
}

// Run only after the canonical original write, inside that same transaction.
// Both ownership and chat are checked on the actual stored row. Even an inbound
// original consumes (discards) its pending rows, so they cannot later resurface.
func consumePendingReceipts(ex sqlExecer, message storedMessage, now time.Time) error {
	if _, err := ex.Exec(`INSERT INTO message_receipts
	 (chat_jid,message_id,recipient,recipient_server,type,at)
	 SELECT :chat,p.message_id,p.recipient,p.recipient_server,p.type,p.at
	 FROM pending_receipts p JOIN messages m ON m.chat_jid=:chat AND m.id=p.message_id
	 WHERE (p.chat_jid=:chat OR (:alias<>'' AND p.chat_jid=:alias))
	 AND p.message_id=:id AND p.expires_ms>:now AND m.is_from_me=1
 AND COALESCE(m.media_type,'') NOT IN ('reaction','poll_vote')
	 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
	 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`, sql.Named("chat", message.ChatJID),
		sql.Named("alias", message.PendingChatAlias), sql.Named("id", message.ID), sql.Named("now", now.UnixMilli())); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE message_id=? AND (chat_jid=? OR (?<>'' AND chat_jid=?))`,
		message.ID, message.ChatJID, message.PendingChatAlias, message.PendingChatAlias)
	return err
}
