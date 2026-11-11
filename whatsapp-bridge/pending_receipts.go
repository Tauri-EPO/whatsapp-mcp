package main

import "time"

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
CREATE INDEX IF NOT EXISTS idx_pending_receipts_arrival ON pending_receipts(arrived_ms);`

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
	 SELECT p.chat_jid,p.message_id,p.recipient,p.recipient_server,p.type,p.at
	 FROM pending_receipts p JOIN messages m ON m.chat_jid=p.chat_jid AND m.id=p.message_id
	 WHERE p.chat_jid=? AND p.message_id=? AND p.expires_ms>? AND m.is_from_me=1
	 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
	 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`, message.ChatJID, message.ID, now.UnixMilli()); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM pending_receipts WHERE chat_jid=? AND message_id=?`, message.ChatJID, message.ID)
	return err
}
