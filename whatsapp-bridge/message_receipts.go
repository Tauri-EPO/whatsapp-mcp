package main

import (
	"context"
	"encoding/json"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const messageReceiptsSchema = `CREATE TABLE IF NOT EXISTS message_receipts (
 chat_jid TEXT NOT NULL, message_id TEXT NOT NULL,
 recipient TEXT NOT NULL, recipient_server TEXT NOT NULL,
 type TEXT NOT NULL, at TIMESTAMP NOT NULL,
 PRIMARY KEY(chat_jid, message_id, recipient, recipient_server, type)
);`

// Receipts from peers acknowledge our messages; IsFromMe describes the receipt
// sender, not the message. The SQL checks ownership using the full message key.
func (b *Bridge) storePeerReceipts(v *events.Receipt) {
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
	client := b.currentClient()
	chat := resolveLIDChatContext(ctx, client, v.Chat, v.SenderAlt, v.RecipientAlt, false).ToNonAD()
	peer := resolveUserJIDContext(ctx, client, v.Sender, v.SenderAlt).ToNonAD()
	if peer.User == "" || (peer.Server != types.DefaultUserServer && peer.Server != types.HiddenUserServer) {
		return
	}
	// Bound each writer hold and SQLite's parameter count, including large receipts.
	for start := 0; start < len(v.MessageIDs); start += 256 {
		ids, err := json.Marshal(v.MessageIDs[start:min(start+256, len(v.MessageIDs))])
		if err == nil {
			_, err = b.Store.db.ExecContext(ctx, `INSERT INTO message_receipts
			 (chat_jid,message_id,recipient,recipient_server,type,at)
			 SELECT m.chat_jid,m.id,?,?,?,? FROM messages m
			 JOIN json_each(?) batch ON batch.value=m.id
			 WHERE m.chat_jid=? AND m.is_from_me=1
			 ON CONFLICT(chat_jid,message_id,recipient,recipient_server,type)
			 DO UPDATE SET at=MAX(message_receipts.at,excluded.at)`,
				peer.User, peer.Server, kind, dbTime(v.Timestamp), string(ids), chat.String())
		}
		if err != nil {
			b.Log.Errorf("Could not store message receipt batch: %v", err)
			return
		}
	}
}
