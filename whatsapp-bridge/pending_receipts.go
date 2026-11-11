package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const (
	pendingReceiptTTL       = 24 * time.Hour
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
CREATE INDEX IF NOT EXISTS idx_pending_receipts_arrival ON pending_receipts(arrived_ms);`

const pendingReceiptChatsSQL = `SELECT json_group_array(chat_jid) FROM (
	SELECT DISTINCT chat_jid FROM pending_receipts WHERE message_id=? AND expires_ms>? ORDER BY chat_jid)`

// Only a verified DM twin is eligible; equal IDs in groups or unrelated chats
// never suffice. The snapshot is already drained before any session lookup.
func (b *Bridge) pendingReceiptChatAlias(ctx context.Context, chat, snapshot string) (string, error) {
	current, valid := pendingEditUserJID(chat)
	if !valid {
		return "", nil
	}
	var candidates []string
	if err := json.Unmarshal([]byte(snapshot), &candidates); err != nil {
		return "", err
	}
	needsLookup := false
	for _, candidate := range candidates {
		other, valid := pendingEditUserJID(candidate)
		if valid && other.Server != current.Server {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return "", nil
	}
	alias, err := b.pendingEditAlt(ctx, chat)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil // an unverified candidate cannot block the original
	}
	for _, candidate := range candidates {
		if candidate == alias {
			return alias, nil
		}
	}
	return "", nil
}

// Startup rekeys pending-only chats without fabricating originals or promoting
// receipts. Collisions retain latest evidence but the earliest arrival/expiry.
func migratePendingReceiptChat(ex sqlExecer, lid, phone string) error {
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
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE chat_jid=?`, lid)
	return err
}

func prunePendingReceipts(ex sqlExecer, now time.Time) error {
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE expires_ms<=?`, now.UnixMilli())
	return err
}

// Called once per event, after bounded JSON batches, before committing. Keep
// newest arrivals; repeated receipts may update at but never extend retention.
func capPendingReceipts(ex sqlExecer) error {
	for _, capSQL := range []struct {
		query string
		limit int
	}{
		{`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM (
		 SELECT rowid,ROW_NUMBER() OVER (PARTITION BY chat_jid,recipient,recipient_server
		 ORDER BY arrived_ms DESC,rowid DESC) n FROM pending_receipts) WHERE n>?)`, pendingReceiptReaderCap},
		{`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM (
		 SELECT rowid,ROW_NUMBER() OVER (PARTITION BY chat_jid
		 ORDER BY arrived_ms DESC,rowid DESC) n FROM pending_receipts) WHERE n>?)`, pendingReceiptChatCap},
		{`DELETE FROM pending_receipts WHERE rowid IN (SELECT rowid FROM pending_receipts
		 ORDER BY arrived_ms DESC,rowid DESC LIMIT -1 OFFSET ?)`, pendingReceiptGlobalCap},
	} {
		if _, err := ex.Exec(capSQL.query, capSQL.limit); err != nil {
			return err
		}
	}
	return nil
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
	 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
	 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`, sql.Named("chat", message.ChatJID),
		sql.Named("alias", message.EditChatAlias), sql.Named("id", message.ID), sql.Named("now", now.UnixMilli())); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE message_id=? AND (chat_jid=? OR (?<>'' AND chat_jid=?))`,
		message.ID, message.ChatJID, message.EditChatAlias, message.EditChatAlias)
	return err
}
