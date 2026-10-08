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

// Replace a snapshot only with complete incoming credentials or when the old
// row has no media fields yet. Partial replays must not combine different keys
// and hashes. The named flag comes from the download's canonical predicate.
const replaceMediaSQL = `(:complete_media OR (
	COALESCE(messages.url, '') = '' AND COALESCE(messages.direct_path, '') = ''
	AND COALESCE(length(messages.media_key), 0) = 0
	AND COALESCE(length(messages.file_sha256), 0) = 0
	AND COALESCE(length(messages.file_enc_sha256), 0) = 0
	AND COALESCE(messages.file_length, 0) = 0))`

// A text write does not turn a reaction/poll pointer into a text row: blank
// media_type/filename keep their old values. Such a conversion needs an UPDATE.
const insertMessageSQL = `INSERT INTO messages
		(id, chat_jid, sender, sender_server, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id, direct_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = excluded.sender,
			-- Keep a namespace the row already has only while the user part it
			-- describes stays the same; a caller with a different bare sender
			-- and no namespace leaves it unknown rather than mislabelled.
			sender_server = CASE
				WHEN excluded.sender_server IS NOT NULL THEN excluded.sender_server
				WHEN excluded.sender = messages.sender THEN messages.sender_server
			END,
			-- Incomplete media stubs carry no replacement caption. Reaction
			-- removal still writes empty content; complete media may do so too.
			content = CASE WHEN NOT :complete_media AND excluded.content = '' AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.content ELSE excluded.content END,
			-- The timestamp also names the cached file. Move it with the
			-- snapshot when a row can accept its first partial credentials.
			timestamp = CASE WHEN NOT ` + replaceMediaSQL + ` AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.timestamp ELSE excluded.timestamp END,
			is_from_me = excluded.is_from_me,
			media_type = CASE WHEN ` + replaceMediaSQL + ` THEN COALESCE(NULLIF(excluded.media_type, ''), messages.media_type) ELSE messages.media_type END,
			filename = CASE WHEN ` + replaceMediaSQL + ` THEN COALESCE(NULLIF(excluded.filename, ''), messages.filename) ELSE messages.filename END,
			url = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.url ELSE messages.url END,
			direct_path = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.direct_path ELSE messages.direct_path END,
			media_key = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.media_key ELSE messages.media_key END,
			file_sha256 = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.file_sha256 ELSE messages.file_sha256 END,
			file_enc_sha256 = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.file_enc_sha256 ELSE messages.file_enc_sha256 END,
			file_length = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.file_length ELSE messages.file_length END,
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
	quotedMessageId string, directPath ...string) error {
	if content == "" && mediaType == "" {
		return nil
	}
	_, err := b.stmt.Exec(messageArgs(id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url,
		mediaKey, fileSHA256, fileEncSHA256, fileLength, quotedMessageId, directPath...)...)
	return err
}

// MarkViewOnce is MessageStore.MarkViewOnce inside the batch.
func (b *messageBatch) MarkViewOnce(messageID, chatJID string) error {
	return markViewOnceWith(b.tx, messageID, chatJID)
}

// SetMentions is MessageStore.SetMentions inside the batch.
func (b *messageBatch) SetMentions(messageID, chatJID, mentions string) error {
	return setMentionsWith(b.tx, messageID, chatJID, mentions)
}

// StorePoll is MessageStore.StorePoll inside the batch.
func (b *messageBatch) StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error {
	return storePollWith(b.tx, messageID, chatJID, p, createdAt)
}

// messageArgs builds the bound parameters for insertMessageSQL. An empty
// quoted_message_id is stored as NULL so the COALESCE in the upsert keeps a
// previously stored ID; the sender is split into its user part and its
// namespace the same way (splitSenderJID, sender_namespace.go), so a caller
// that only has the bare user part does not erase a namespace already stored.
// The timestamp goes in through dbTime (store_time.go) so every row carries
// the same UTC spelling. The optional direct path is inserted atomically with
// the credentials; an omitted/empty path becomes NULL for URL-only snapshots.
// On a complete write, omitting the optional path therefore clears the stored
// direct_path. Incomplete writes keep the previous snapshot, including its path.
func messageArgs(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
	quotedMessageId string, directPath ...string) []any {
	var qmid any
	if quotedMessageId != "" {
		qmid = quotedMessageId
	}
	senderUser, senderServer := splitSenderJID(sender)
	var path any
	var pathText string
	if len(directPath) > 0 && directPath[0] != "" {
		pathText = directPath[0]
		path = pathText
	}
	return []any{id, chatJID, senderUser, senderServer, content, dbTime(timestamp), isFromMe, mediaType, filename, url,
		mediaKey, fileSHA256, fileEncSHA256, fileLength, qmid, path,
		sql.Named("complete_media", mediaComplete(url, pathText, mediaKey, fileSHA256, fileEncSHA256))}
}
