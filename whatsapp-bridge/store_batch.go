package main

// Batched writes for history sync.
//
// A pair-time backfill stores tens of thousands of rows. One db.Exec per row
// means one implicit transaction and one fsync per message (plus the FTS
// triggers); prepared inserts in bounded history chunks amortize fsyncs
// without holding the write lock for an entire conversation.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types"
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

// CASE avoids JSON functions on corrupt stored text. Only an object with the
// field types the Go decoder accepts can be merged into a new snapshot.
const validPresentationSQL = `(CASE
	WHEN length(messages.media_presentation) > 4096 THEN 0
	WHEN NOT json_valid(messages.media_presentation) THEN 0
	ELSE json_type(messages.media_presentation) = 'object'
	AND COALESCE(json_type(messages.media_presentation, '$.sha256'), 'null') IN ('text', 'null')
	AND COALESCE(json_type(messages.media_presentation, '$.mime'), 'null') IN ('text', 'null')
	AND COALESCE(json_type(messages.media_presentation, '$.name'), 'null') IN ('text', 'null')
	AND COALESCE(json_type(messages.media_presentation, '$.title'), 'null') IN ('text', 'null')
	AND COALESCE(json_type(messages.media_presentation, '$.ptt'), 'null') IN ('true', 'false', 'null')
	AND COALESCE(json_type(messages.media_presentation, '$.animated'), 'null') IN ('true', 'false', 'null')
	AND (COALESCE(json_type(messages.media_presentation, '$.seconds'), 'null') = 'null'
		OR (json_type(messages.media_presentation, '$.seconds') = 'integer' AND json_extract(messages.media_presentation, '$.seconds') BETWEEN 0 AND 86400))
	AND (COALESCE(json_type(messages.media_presentation, '$.waveform'), 'null') = 'null'
		OR (json_type(messages.media_presentation, '$.waveform') = 'text' AND length(json_extract(messages.media_presentation, '$.waveform')) = 88
			AND substr(json_extract(messages.media_presentation, '$.waveform'), -2) = '=='
			AND json_extract(messages.media_presentation, '$.waveform') NOT GLOB '*[^A-Za-z0-9+/=]*'))
	END)`

// A text write does not turn a reaction/poll pointer into a text row: blank
// media_type/filename keep their old values. Such a conversion needs an UPDATE.
const insertMessageSQL = `INSERT INTO messages
		(id, chat_jid, sender, sender_server, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id, direct_path, media_presentation, location, media_retry_chat, media_retry_sender)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))
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
			content = CASE WHEN messages.message_edit_timestamp > 0 THEN messages.content
				WHEN NOT :complete_media AND excluded.content = '' AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.content ELSE excluded.content END,
			-- The timestamp also names the cached file. Move it with the
			-- snapshot when a row can accept its first partial credentials.
			timestamp = CASE WHEN NOT ` + replaceMediaSQL + ` AND messages.media_type IN
				('image', 'video', 'audio', 'document', 'sticker')
				THEN messages.timestamp ELSE excluded.timestamp END,
			is_from_me = excluded.is_from_me,
			media_type = CASE WHEN ` + replaceMediaSQL + ` THEN COALESCE(NULLIF(excluded.media_type, ''), messages.media_type) ELSE messages.media_type END,
			filename = CASE WHEN ` + replaceMediaSQL + ` THEN CASE
				WHEN excluded.media_type = 'document' AND :complete_media AND excluded.file_sha256 IS NOT messages.file_sha256
				THEN excluded.filename ELSE COALESCE(NULLIF(excluded.filename, ''), messages.filename) END ELSE messages.filename END,
			url = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.url ELSE messages.url END,
			direct_path = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.direct_path ELSE messages.direct_path END,
			media_retry_chat = COALESCE(messages.media_retry_chat, excluded.media_retry_chat),
			media_retry_sender = COALESCE(messages.media_retry_sender, excluded.media_retry_sender),
			media_key = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.media_key ELSE messages.media_key END,
			file_sha256 = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.file_sha256 ELSE messages.file_sha256 END,
			file_enc_sha256 = CASE WHEN ` + replaceMediaSQL + ` THEN excluded.file_enc_sha256 ELSE messages.file_enc_sha256 END,
			file_length = CASE WHEN ` + replaceMediaSQL + ` THEN CASE WHEN excluded.file_length IS NULL AND excluded.file_sha256 = messages.file_sha256 THEN messages.file_length ELSE excluded.file_length END ELSE messages.file_length END,
			-- Presentation belongs to the same snapshot. A same-file replay
			-- can add fields; missing metadata must not erase its old fields.
			media_presentation = CASE WHEN ` + replaceMediaSQL + ` THEN CASE
				WHEN excluded.file_sha256 = messages.file_sha256 AND ` + validPresentationSQL + ` THEN CASE
					WHEN excluded.media_presentation IS NULL THEN messages.media_presentation
					WHEN json_extract(messages.media_presentation, '$.sha256') = json_extract(excluded.media_presentation, '$.sha256')
					THEN json_patch(messages.media_presentation, excluded.media_presentation)
					ELSE excluded.media_presentation END
				ELSE excluded.media_presentation END ELSE messages.media_presentation END,
			quoted_message_id = COALESCE(excluded.quoted_message_id, messages.quoted_message_id),
			-- History is newest-first: an initial sample replayed after a later
			-- same-key sample supplies the original metadata, not an older position.
			location = CASE WHEN excluded.media_type = 'location' AND messages.media_type = 'location'
				AND CASE WHEN json_valid(messages.location) AND json_valid(excluded.location)
					THEN json_extract(messages.location, '$.live') = 1
					AND json_extract(excluded.location, '$.live') = 1
					AND json_extract(messages.location, '$.sequence') > COALESCE(json_extract(excluded.location, '$.sequence'), 0) ELSE 0 END
				THEN json_patch(excluded.location, json_object(
					'latitude', COALESCE(json_extract(messages.location, '$.latitude'), json_extract(excluded.location, '$.latitude')),
					'longitude', COALESCE(json_extract(messages.location, '$.longitude'), json_extract(excluded.location, '$.longitude')),
					'accuracy_meters', COALESCE(json_extract(messages.location, '$.accuracy_meters'), json_extract(excluded.location, '$.accuracy_meters')),
					'speed_mps', COALESCE(json_extract(messages.location, '$.speed_mps'), json_extract(excluded.location, '$.speed_mps')),
					'bearing_degrees', COALESCE(json_extract(messages.location, '$.bearing_degrees'), json_extract(excluded.location, '$.bearing_degrees')),
					'sequence', json_extract(messages.location, '$.sequence'),
					'time_offset_seconds', json_extract(messages.location, '$.time_offset_seconds')))
				ELSE COALESCE(excluded.location, messages.location) END`

// messageBatch groups message writes in one transaction. Obtain one through
// MessageStore.Batch; it is not safe for concurrent use.
type messageBatch struct {
	tx      *contextTransaction
	ctx     context.Context
	stmt    *sql.Stmt
	failure error // first failed write; never issue more SQL after a possible rollback
}

// Batch runs fn inside a transaction with a prepared message insert and
// commits when fn and every write succeed (rolls back otherwise).
func (store *MessageStore) Batch(fn func(b *messageBatch) error) error {
	return store.BatchContext(context.Background(), fn)
}

// BatchContext retains the normal transaction/replay contract while allowing
// peer imports to cancel connection waits and every statement on shutdown.
func (store *MessageStore) BatchContext(ctx context.Context, fn func(b *messageBatch) error) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, insertMessageSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare batch insert: %w", err)
	}
	b := &messageBatch{tx: &contextTransaction{Tx: tx, ctx: ctx}, ctx: ctx, stmt: stmt}
	err = fn(b)
	if err == nil {
		err = b.failure
	}
	if err != nil {
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

type contextTransaction struct {
	*sql.Tx
	ctx context.Context
}

func (tx *contextTransaction) Exec(query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(tx.ctx, query, args...)
}

func (tx *contextTransaction) QueryRow(query string, args ...any) *sql.Row {
	return tx.QueryRowContext(tx.ctx, query, args...)
}

type contextExecer struct {
	db  *sql.DB
	ctx context.Context
}

func (ex contextExecer) Exec(query string, args ...any) (sql.Result, error) {
	return ex.db.ExecContext(ex.ctx, query, args...)
}

// SQLite can roll back a transaction on a write error without invalidating
// the driver Tx. Remember it so ignored side-table errors cannot turn later
// statements into autocommits; Batch also refuses to commit after such an error.
func (b *messageBatch) write(fn func() error) error {
	if b.failure == nil {
		b.failure = fn()
	}
	return b.failure
}

// StoreMessage is MessageStore.StoreMessage inside the batch.
func (b *messageBatch) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength any,
	quotedMessageId string, options ...messageMediaOptions) error {
	if content == "" && mediaType == "" {
		return nil
	}
	return b.write(func() error {
		_, err := b.stmt.ExecContext(b.ctx, messageArgs(id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url,
			mediaKey, fileSHA256, fileEncSHA256, fileLength, quotedMessageId, options...)...)
		return err
	})
}

// MarkViewOnce is MessageStore.MarkViewOnce inside the batch.
func (b *messageBatch) MarkViewOnce(messageID, chatJID string) error {
	return b.write(func() error { return markViewOnceWith(b.tx, messageID, chatJID) })
}

// SetMentions is MessageStore.SetMentions inside the batch.
func (b *messageBatch) SetMentions(messageID, chatJID, mentions string) error {
	return b.write(func() error { return setMentionsWith(b.tx, messageID, chatJID, mentions) })
}

// StorePoll is MessageStore.StorePoll inside the batch.
func (b *messageBatch) StorePoll(messageID, chatJID string, p *pollCreation, createdAt time.Time) error {
	return b.write(func() error { return storePollWith(b.tx, messageID, chatJID, p, createdAt) })
}

// messageArgs builds the bound parameters for insertMessageSQL. An empty
// quoted_message_id is stored as NULL so the COALESCE in the upsert keeps a
// previously stored ID; the sender is split into its user part and its
// namespace the same way (splitSenderJID, sender_namespace.go), so a caller
// that only has the bare user part does not erase a namespace already stored.
// The timestamp goes in through dbTime (store_time.go) so every row carries
// the same UTC spelling. The optional direct path is inserted atomically with
// the credentials and recipient-visible presentation; an omitted/empty path
// becomes NULL for URL-only snapshots.
// On a complete write, omitting the optional path therefore clears the stored
// direct_path. Incomplete writes keep the previous snapshot, including its path.
func messageArgs(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength any,
	quotedMessageId string, options ...messageMediaOptions) []any {
	switch mediaType {
	case "image", "video", "audio", "document", "sticker":
		if length, ok := fileLength.(uint64); ok {
			fileLength = storedMediaLength(length)
		}
	default:
		fileLength = nil
	}
	var qmid any
	if quotedMessageId != "" {
		qmid = quotedMessageId
	}
	senderUser, senderServer := splitSenderJID(sender)
	var path any
	var pathText string
	var presentation *mediaPresentation
	var location *messageLocation
	var retryChat, retrySender string
	if len(options) > 0 {
		pathText, presentation = options[0].directPath, options[0].presentation
		location = options[0].location
		retryChat, retrySender = options[0].retryChat, options[0].retrySender
		if pathText != "" {
			path = pathText
		}
	}
	// A group JID is the history sender fallback when participant fields are
	// absent, not a verified retry participant. Leave it unset so a later
	// fully attributed delivery can populate the actual wire sender.
	if wire, err := types.ParseJID(retrySender); err != nil ||
		(wire.Server != types.DefaultUserServer && wire.Server != types.HiddenUserServer) {
		retrySender = ""
	}
	return []any{id, chatJID, senderUser, senderServer, content, dbTime(timestamp), isFromMe, mediaType, filename, url,
		mediaKey, fileSHA256, fileEncSHA256, fileLength, qmid, path, presentation.forFile(mediaType, fileSHA256).column(), location.column(), retryChat, retrySender,
		sql.Named("complete_media", mediaComplete(chatJID, url, pathText, mediaKey, fileSHA256, fileEncSHA256))}
}
