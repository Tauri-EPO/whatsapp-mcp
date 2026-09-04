package main

// Batched writes for history sync.
//
// A pair-time backfill stores tens of thousands of rows. One db.Exec per row
// means one implicit transaction and one fsync per message (plus the FTS
// triggers); wrapping a conversation in a transaction with a prepared
// statement turns that into one fsync per conversation.

import (
	"database/sql"
	"fmt"
	"time"
)

// sqlExecer is satisfied by *sql.DB and *sql.Tx.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

const insertMessageSQL = `INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = excluded.sender,
			content = excluded.content,
			timestamp = excluded.timestamp,
			is_from_me = excluded.is_from_me,
			media_type = excluded.media_type,
			filename = excluded.filename,
			url = excluded.url,
			media_key = excluded.media_key,
			file_sha256 = excluded.file_sha256,
			file_enc_sha256 = excluded.file_enc_sha256,
			file_length = excluded.file_length,
			quoted_message_id = COALESCE(excluded.quoted_message_id, messages.quoted_message_id)`

// messageBatch groups message writes in one transaction. Obtain one through
// MessageStore.Batch; it is not safe for concurrent use.
type messageBatch struct {
	tx   *sql.Tx
	stmt *sql.Stmt
}

// Batch runs fn inside a transaction with a prepared message insert and
// commits when fn returns nil (rolls back otherwise).
func (store *MessageStore) Batch(fn func(b *messageBatch) error) error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	stmt, err := tx.Prepare(insertMessageSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare batch insert: %w", err)
	}
	b := &messageBatch{tx: tx, stmt: stmt}
	if err := fn(b); err != nil {
		_ = stmt.Close()
		_ = tx.Rollback()
		return err
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// StoreMessage is MessageStore.StoreMessage inside the batch.
func (b *messageBatch) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
	quotedMessageId string) error {
	if content == "" && mediaType == "" {
		return nil
	}
	_, err := b.stmt.Exec(messageArgs(id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url,
		mediaKey, fileSHA256, fileEncSHA256, fileLength, quotedMessageId)...)
	return err
}

// MarkViewOnce is MessageStore.MarkViewOnce inside the batch.
func (b *messageBatch) MarkViewOnce(messageID, chatJID string) error {
	return markViewOnceWith(b.tx, messageID, chatJID)
}

// StorePoll is MessageStore.StorePoll inside the batch.
func (b *messageBatch) StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error {
	return storePollWith(b.tx, messageID, chatJID, p, createdAt)
}

// messageArgs builds the bound parameters for insertMessageSQL. An empty
// quoted_message_id is stored as NULL so the COALESCE in the upsert keeps a
// previously stored ID.
func messageArgs(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
	quotedMessageId string) []any {
	var qmid any
	if quotedMessageId != "" {
		qmid = quotedMessageId
	}
	return []any{id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url,
		mediaKey, fileSHA256, fileEncSHA256, fileLength, qmid}
}
