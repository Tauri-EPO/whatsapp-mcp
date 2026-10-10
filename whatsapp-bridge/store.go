package main

// SQLite message store: schema, migrations and every (*MessageStore) method
// that does not belong to a feature file (fts.go, polls.go, view_once.go ...).
// messages.db is owned by the bridge; whatsapp.db is whatsmeow's and only
// read here for contact/LID resolution.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db   *sql.DB
	waDB *sql.DB // whatsmeow's DB for contact name resolution fallback

	names     *chatNameCache   // resolved chat names + failed group lookups (chat_names.go)
	groupInfo groupInfoLookup  // live group metadata fetch; nil = no network
	fts       bool             // messages_fts active (fts.go)
	editNow   func() time.Time // per-store arrival clock for pending edit expiry
}

type ChatEphemeralSettings struct {
	Expiration       uint32
	SettingTimestamp int64
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), storeDirMode); err != nil {
		return nil, fmt.Errorf("failed to create store directory %q: %v", storeDir(), err)
	}

	// Open SQLite database for messages
	// WAL lets the MCP server read messages.db while the bridge writes (a
	// history-sync burst used to make readers hit SQLITE_BUSY), and the busy
	// timeout makes both sides wait instead of failing on a short lock.
	privateDatabase(messagesDBPath())
	db, err := sql.Open("sqlite", sqliteURI(messagesDBPath(), messagesWriterOptions))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}
	boundPool(db, messagesPoolConns)

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP,
			ephemeral_expiration INTEGER NOT NULL DEFAULT 0,
			ephemeral_setting_timestamp INTEGER NOT NULL DEFAULT 0
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			deleted_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE TABLE IF NOT EXISTS calls (
			call_id TEXT,
			chat_jid TEXT,
			from_jid TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			call_type TEXT,
			is_group BOOLEAN,
			result TEXT,
			duration_sec INTEGER,
			ended_at TIMESTAMP,
			reason TEXT,
			PRIMARY KEY (call_id, chat_jid)
		);

		CREATE INDEX IF NOT EXISTS idx_calls_chat ON calls(chat_jid);
		CREATE INDEX IF NOT EXISTS idx_calls_timestamp ON calls(timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_chat_timestamp ON messages(chat_jid, timestamp);
		-- The MCP server filters/sorts on these without a chat (list_messages
		-- across chats, get_last_interaction, get_contact_chats, list_chats);
		-- idx_messages_chat_jid was a redundant prefix of the composite index.
		CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender, timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_media_cursor ON messages(timestamp, id, chat_jid) WHERE media_type IN ('image','video','audio','document','sticker');
		CREATE INDEX IF NOT EXISTS idx_chats_last_message ON chats(last_message_time);
		-- Media inventory and notes (MCP server) group and look up rows by content
		-- hash; text rows carry NULL, so a partial index stays small.
		CREATE INDEX IF NOT EXISTS idx_messages_file_sha256 ON messages(file_sha256) WHERE file_sha256 IS NOT NULL;
		DROP INDEX IF EXISTS idx_messages_chat_jid;
	`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Open whatsmeow's database read-only for contact name resolution fallback.
	// Missing DBs are expected on first run and should not create a new file.
	waDB, err := openWhatsmeowContactsDB(whatsmeowDBPath())
	if err != nil {
		bridgeLog.Warnf("could not open whatsmeow database for contact resolution: %v", err)
	}

	if err := ensureMessageStoreSchema(db); err != nil {
		_ = db.Close()
		if waDB != nil {
			_ = waDB.Close()
		}
		return nil, err
	}

	// Full-text index (see fts.go). Never fatal: search degrades to the
	// substring scan when the index is unavailable.
	ftsOn, ftsErr := ensureMessagesFTS(db)
	switch {
	case ftsErr != nil:
		bridgeLog.Warnf("full-text search index unavailable: %v", ftsErr)
	case ftsOn:
		bridgeLog.Infof("Full-text search index (FTS5) active for messages.content")
	default:
		bridgeLog.Infof("SQLite built without FTS5: message search uses the substring scan")
	}

	return &MessageStore{db: db, waDB: waDB, names: newChatNameCache(), fts: ftsOn}, nil
}

func openWhatsmeowContactsDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	db, err := sql.Open("sqlite", sqliteURI(path, sqliteReadOnlyOptions))
	if err != nil {
		return nil, err
	}
	boundPool(db, contactsPoolConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureMessageStoreSchema(db *sql.DB) error {
	if _, err := db.Exec(pendingEditsSchema); err != nil {
		return fmt.Errorf("failed to ensure pending edits: %w", err)
	}
	if err := prunePendingEdits(db, time.Now()); err != nil {
		return err
	}
	if err := ensureColumn(db, "chats", "ephemeral_expiration", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("failed to ensure chats.ephemeral_expiration column: %w", err)
	}
	if err := ensureColumn(db, "chats", "ephemeral_setting_timestamp", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("failed to ensure chats.ephemeral_setting_timestamp column: %w", err)
	}
	if err := ensureColumn(db, "chats", "last_read_time", "TIMESTAMP"); err != nil {
		return fmt.Errorf("failed to ensure chats.last_read_time column: %w", err)
	}
	if err := ensureColumn(db, "messages", "deleted_at", "TIMESTAMP"); err != nil {
		return fmt.Errorf("failed to ensure messages.deleted_at column: %w", err)
	}
	// Only revoked messages carry deleted_at, so this partial index holds a
	// handful of rows; it keeps the startup timestamp probe (store_time.go) off
	// the messages table. It lives here rather than with the other indexes
	// because the column above may have just been added.
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_messages_deleted_at ON messages(deleted_at) WHERE deleted_at IS NOT NULL`,
	); err != nil {
		return fmt.Errorf("failed to ensure idx_messages_deleted_at: %w", err)
	}
	// target_message_id: the message a reaction or poll vote refers to. Older
	// rows kept that ID in `filename`; the migration below copies it over once
	// and is a no-op afterwards (WHERE target_message_id IS NULL).
	if err := ensureColumn(db, "messages", "target_message_id", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.target_message_id column: %w", err)
	}
	if _, err := db.Exec(`UPDATE messages SET target_message_id = filename
		WHERE media_type IN ('reaction', 'poll_vote') AND target_message_id IS NULL AND filename IS NOT NULL AND filename != ''`); err != nil {
		return fmt.Errorf("failed to migrate target_message_id: %w", err)
	}
	if err := ensureColumn(db, "messages", "view_once", "BOOLEAN NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("failed to ensure messages.view_once column: %w", err)
	}
	if _, err := db.Exec(pollsSchema); err != nil {
		return fmt.Errorf("failed to ensure poll tables: %w", err)
	}
	if err := ensureColumn(db, "messages", "quoted_message_id", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.quoted_message_id column: %w", err)
	}
	// mentions: the users a message addressed, backfilled from the text for
	// rows stored before the column existed (mentions.go).
	if err := ensureColumn(db, "messages", "mentions", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.mentions column: %w", err)
	}
	// direct_path: the media's own direct path, which is what a download asks
	// the CDN for. NULL on rows an older bridge wrote and on messages that
	// carried none; those keep the path cut out of `url` (media.go, issue #452).
	if err := ensureColumn(db, "messages", "direct_path", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.direct_path column: %w", err)
	}
	// Recipient-visible metadata is captured for new media. Adding the nullable
	// column is constant-time; old rows keep their documented legacy fallback.
	if err := ensureColumn(db, "messages", "media_presentation", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.media_presentation: %w", err)
	}
	if err := ensureColumn(db, "messages", "location", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.location: %w", err)
	}
	for column, spec := range map[string]string{
		"message_edit_timestamp": "INTEGER NOT NULL DEFAULT 0",
		"read_receipt_sent":      "INTEGER NOT NULL DEFAULT 0",
		"media_retry_chat":       "TEXT",
		"media_retry_sender":     "TEXT",
	} {
		if err := ensureColumn(db, "messages", column, spec); err != nil {
			return fmt.Errorf("failed to ensure messages.%s: %w", column, err)
		}
	}
	// sender_server: the namespace messages.sender lives in ("s.whatsapp.net"
	// or "lid"), NULL when it is unknown — rows an older bridge wrote, and
	// senders that are not user JIDs at all (sender_namespace.go).
	if err := ensureColumn(db, "messages", "sender_server", "TEXT"); err != nil {
		return fmt.Errorf("failed to ensure messages.sender_server column: %w", err)
	}
	// The backfill and its startup probe only ever look at the unclassified
	// rows, so a partial index bounds both by how many of those are left
	// instead of by the size of the table.
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_messages_sender_server_null
		 ON messages(sender) WHERE sender_server IS NULL`,
	); err != nil {
		return fmt.Errorf("failed to ensure idx_messages_sender_server_null: %w", err)
	}
	if _, err := db.Exec(groupMembersSchema); err != nil {
		return fmt.Errorf("failed to ensure group_members table: %w", err)
	}
	if _, err := applyNamedMigration(db, "labels_schema_v1", func(tx *sql.Tx) error {
		_, err := tx.Exec(labelsSchema)
		return err
	}); err != nil {
		return fmt.Errorf("failed to ensure label tables: %w", err)
	}
	if _, err := applyNamedMigration(db, "labels_metadata_v2", func(tx *sql.Tx) error {
		return ensureLabelMetadata(tx)
	}); err != nil {
		return fmt.Errorf("failed to ensure label metadata: %w", err)
	}
	if _, err := applyNamedMigration(db, "runtime_settings_v1", func(tx *sql.Tx) error {
		_, err := tx.Exec(runtimeSettingsSchema)
		return err
	}); err != nil {
		return fmt.Errorf("runtime settings schema: %w", err)
	}
	if _, err := applyNamedMigration(db, "send_limits_v1", func(tx *sql.Tx) error {
		_, err := tx.Exec(sendLimitsSchema)
		return err
	}); err != nil {
		return fmt.Errorf("send limits schema: %w", err)
	}
	if _, err := applyNamedMigration(db, "operator_logout_v1", func(tx *sql.Tx) error {
		_, err := tx.Exec(operatorLogoutSchema)
		return err
	}); err != nil {
		return fmt.Errorf("operator logout schema: %w", err)
	}
	if _, err := applyNamedMigration(db, "operator_logout_cleanup_v2", func(tx *sql.Tx) error {
		return ensureColumn(tx, "operator_state", "local_session_wiped", "INTEGER NOT NULL DEFAULT 0")
	}); err != nil {
		return fmt.Errorf("operator cleanup schema: %w", err)
	}
	if _, err := applyNamedMigration(db, "media_cache_v1", func(tx *sql.Tx) error {
		_, err := tx.Exec(mediaCacheSchema)
		return err
	}); err != nil {
		return fmt.Errorf("media cache schema: %w", err)
	}
	// Run data rewrites after their tables and columns exist. Each owns an
	// independent schema_migrations marker; legacy user_version is untouched.
	if err := migrateCanonicalTimestamps(db); err != nil {
		return err
	}
	if err := migrateUndeclaredMediaLengths(db); err != nil {
		return fmt.Errorf("migrate media lengths: %w", err)
	}
	if err := migrateMentionsBackfill(db); err != nil {
		return err
	}
	return nil
}

type schemaWriter interface {
	sqlExecer
	Query(query string, args ...any) (*sql.Rows, error)
}

func ensureColumn(db schemaWriter, tableName, columnName, columnSpec string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return err
	}

	exists := false
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == columnName {
			exists = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	// Close before ALTER: SQLite holds a read lock while rows are open,
	// which would make the schema change fail with "database is locked".
	if err := rows.Close(); err != nil {
		return err
	}
	if exists {
		return nil
	}

	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, columnSpec))
	return err
}

// beginMessageMigration pins ATTACH and DETACH to the same pooled connection.
// An alias must never remain when BEGIN IMMEDIATE later reserves its writers.
func (store *MessageStore) beginMessageMigration(alias string) (*sql.Tx, func(), error) {
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	finish := func() {
		_ = tx.Rollback()
		if alias != "" {
			if _, err := conn.ExecContext(ctx, "DETACH DATABASE "+alias); err != nil {
				// Discard a connection whose attachment could not be removed.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				bridgeLog.Warnf("Discarded migration connection after detach failure: %v", err)
			}
		}
		_ = conn.Close()
	}
	return tx, finish, nil
}

// MigrateLegacyLIDChatsToPhoneJIDs rewrites message/chat rows stored under
// legacy @lid chat JIDs into phone-based @s.whatsapp.net chat JIDs using the
// whatsmeow LID map in whatsapp.db.
func (store *MessageStore) MigrateLegacyLIDChatsToPhoneJIDs(whatsappDBPath string, logger waLog.Logger) error {
	if _, err := os.Stat(whatsappDBPath); err != nil {
		if os.IsNotExist(err) {
			logger.Infof("Skipping LID chat migration: %s not found", whatsappDBPath)
			return nil
		}
		return fmt.Errorf("failed to stat WhatsApp DB %s: %w", whatsappDBPath, err)
	}

	if err := store.migratePendingEditChats(context.Background(), whatsappDBPath); err != nil {
		return fmt.Errorf("migrate pending edit chats: %w", err)
	}
	alias := fmt.Sprintf("wa_mig_%d", time.Now().UnixNano())
	tx, finish, err := store.beginMessageMigration(alias)
	if err != nil {
		return fmt.Errorf("failed to start LID chat migration transaction: %w", err)
	}
	defer finish()

	escapedPath := strings.ReplaceAll(whatsappDBPath, "'", "''")
	if _, err := tx.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS %s;", escapedPath, alias)); err != nil {
		return fmt.Errorf("failed to attach WhatsApp DB for LID chat migration: %w", err)
	}

	var lidMapTableExists int
	if err := tx.QueryRow(fmt.Sprintf(
		"SELECT COUNT(1) FROM %s.sqlite_master WHERE type='table' AND name='whatsmeow_lid_map';",
		alias,
	)).Scan(&lidMapTableExists); err != nil {
		return fmt.Errorf("failed to inspect WhatsApp DB schema for LID migration: %w", err)
	}
	if lidMapTableExists == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit no-op LID chat migration: %w", err)
		}
		logger.Infof("Skipping LID chat migration: whatsmeow_lid_map table not found")
		return nil
	}

	if _, err := tx.Exec(fmt.Sprintf(`
		CREATE TEMP TABLE tmp_lid_to_phone AS
		SELECT DISTINCT
			lm.lid || '@lid' AS lid_jid,
			lm.pn || '@s.whatsapp.net' AS phone_jid
		FROM %s.whatsmeow_lid_map lm
		WHERE lm.lid != '' AND lm.pn != ''
		  AND (
		  	EXISTS (SELECT 1 FROM chats c WHERE c.jid = lm.lid || '@lid')
		  	OR EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = lm.lid || '@lid')
		  );
	`, alias)); err != nil {
		return fmt.Errorf("failed to build temporary LID mapping table: %w", err)
	}

	var mappedChats int
	if err := tx.QueryRow("SELECT COUNT(*) FROM tmp_lid_to_phone;").Scan(&mappedChats); err != nil {
		return fmt.Errorf("failed to count mapped LID chats: %w", err)
	}

	if mappedChats == 0 {
		if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_to_phone;"); err != nil {
			return fmt.Errorf("failed to clean temporary LID mapping table: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit no-op LID chat migration: %w", err)
		}
		logger.Infof("LID chat migration: nothing to migrate")
		return nil
	}

	if _, err := tx.Exec(`
		CREATE TEMP TABLE tmp_lid_chat_candidates AS
		SELECT
			m.phone_jid AS phone_jid,
			m.lid_jid AS lid_jid,
			NULLIF(TRIM(c.name), '') AS source_name,
			COALESCE(
				c.last_message_time,
				(
					SELECT MAX(msg.timestamp)
					FROM messages msg
					WHERE msg.chat_jid = m.lid_jid
				)
			) AS source_last_message_time,
			c.last_read_time AS source_last_read_time
		FROM tmp_lid_to_phone m
		LEFT JOIN chats c ON c.jid = m.lid_jid;
	`); err != nil {
		return fmt.Errorf("failed to build temporary chat candidate table: %w", err)
	}

	if _, err := tx.Exec(`
		CREATE TEMP TABLE tmp_lid_chat_meta AS
		SELECT
			c.phone_jid AS phone_jid,
			COALESCE(
				(
					SELECT c2.source_name
					FROM tmp_lid_chat_candidates c2
					WHERE c2.phone_jid = c.phone_jid
						AND c2.source_name IS NOT NULL
					ORDER BY
						CASE WHEN c2.source_last_message_time IS NULL THEN 1 ELSE 0 END,
						c2.source_last_message_time DESC,
						c2.lid_jid ASC
					LIMIT 1
				),
				substr(c.phone_jid, 1, instr(c.phone_jid, '@') - 1)
			) AS source_name,
			MAX(c.source_last_message_time) AS source_last_message_time,
			MAX(c.source_last_read_time) AS source_last_read_time
		FROM tmp_lid_chat_candidates c
		GROUP BY c.phone_jid;
	`); err != nil {
		return fmt.Errorf("failed to build temporary chat metadata table: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO chats (jid, name, last_message_time, last_read_time)
		SELECT phone_jid, source_name, source_last_message_time, source_last_read_time
		FROM tmp_lid_chat_meta;
	`); err != nil {
		return fmt.Errorf("failed to upsert destination chat rows: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE chats
		SET
			name = CASE
				WHEN (name IS NULL OR TRIM(name) = '') THEN (
					SELECT m.source_name
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				)
				ELSE name
			END,
			last_message_time = CASE
				WHEN (
					SELECT m.source_last_message_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				) IS NULL THEN last_message_time
				WHEN last_message_time IS NULL THEN (
					SELECT m.source_last_message_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				)
				WHEN (
					SELECT m.source_last_message_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				) > last_message_time THEN (
					SELECT m.source_last_message_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				)
				ELSE last_message_time
			END,
			last_read_time = CASE
				WHEN (
					SELECT m.source_last_read_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				) IS NULL THEN last_read_time
				WHEN last_read_time IS NULL THEN (
					SELECT m.source_last_read_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				)
				WHEN (
					SELECT m.source_last_read_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				) > last_read_time THEN (
					SELECT m.source_last_read_time
					FROM tmp_lid_chat_meta m
					WHERE m.phone_jid = chats.jid
				)
				ELSE last_read_time
			END
		WHERE jid IN (SELECT phone_jid FROM tmp_lid_chat_meta);
	`); err != nil {
		return fmt.Errorf("failed to merge destination chat metadata: %w", err)
	}

	insertResult, err := tx.Exec(`
		INSERT OR IGNORE INTO messages (
			id, chat_jid, sender, sender_server, content, timestamp, is_from_me,
			media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, direct_path, media_presentation, location,
			quoted_message_id, mentions, deleted_at, view_once, target_message_id, message_edit_timestamp, media_retry_chat, media_retry_sender, read_receipt_sent
		)
		SELECT
			msg.id,
			m.phone_jid,
			msg.sender,
			msg.sender_server,
			msg.content,
			msg.timestamp,
			msg.is_from_me,
			msg.media_type,
			msg.filename,
			msg.url,
			msg.media_key,
			msg.file_sha256,
			msg.file_enc_sha256,
			msg.file_length,
			msg.direct_path,
			msg.media_presentation,
			msg.location,
			msg.quoted_message_id,
			msg.mentions,
			msg.deleted_at,
			msg.view_once,
			msg.target_message_id,
			msg.message_edit_timestamp,
			msg.media_retry_chat,
			msg.media_retry_sender,
			msg.read_receipt_sent
		FROM messages msg
		JOIN tmp_lid_to_phone m ON m.lid_jid = msg.chat_jid;
	`)
	if err != nil {
		return fmt.Errorf("failed to copy legacy LID messages into phone chats: %w", err)
	}

	insertedMessages, _ := insertResult.RowsAffected()
	// A PN row wins metadata collisions, but either source's acknowledged
	// receipt must survive so a partial refusal does not resend that receipt.
	if _, err := tx.Exec(`
		UPDATE messages SET read_receipt_sent = 1
		WHERE chat_jid IN (SELECT phone_jid FROM tmp_lid_to_phone)
		AND EXISTS (
			SELECT 1 FROM messages source
			JOIN tmp_lid_to_phone m ON m.lid_jid = source.chat_jid
			WHERE m.phone_jid = messages.chat_jid AND source.id = messages.id
			AND source.read_receipt_sent = 1
		);
	`); err != nil {
		return fmt.Errorf("failed to merge migrated receipt progress: %w", err)
	}

	// Preserve cache references before deleting source rows (the FK cascades).
	// A colliding destination message may receive a ref only for its own hash.
	if _, err := tx.Exec(`INSERT INTO media_cache_refs(id,chat_jid,sha256)
		SELECT r.id,map.phone_jid,r.sha256 FROM media_cache_refs r
		JOIN tmp_lid_to_phone map ON map.lid_jid=r.chat_jid
		JOIN messages dest ON dest.id=r.id AND dest.chat_jid=map.phone_jid
		WHERE dest.file_sha256=r.sha256
		ON CONFLICT(id,chat_jid) DO NOTHING`); err != nil {
		return fmt.Errorf("failed to migrate media cache references: %w", err)
	}
	deleteMessagesResult, err := tx.Exec(`
		DELETE FROM messages
		WHERE chat_jid IN (SELECT lid_jid FROM tmp_lid_to_phone);
	`)
	if err != nil {
		return fmt.Errorf("failed to delete migrated LID messages: %w", err)
	}
	deletedMessages, _ := deleteMessagesResult.RowsAffected()

	deleteChatsResult, err := tx.Exec(`
		DELETE FROM chats
		WHERE jid IN (SELECT lid_jid FROM tmp_lid_to_phone);
	`)
	if err != nil {
		return fmt.Errorf("failed to delete migrated LID chats: %w", err)
	}
	deletedChats, _ := deleteChatsResult.RowsAffected()

	if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_to_phone;"); err != nil {
		return fmt.Errorf("failed to clean temporary LID mapping table: %w", err)
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_chat_meta;"); err != nil {
		return fmt.Errorf("failed to clean temporary chat metadata table: %w", err)
	}
	if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_chat_candidates;"); err != nil {
		return fmt.Errorf("failed to clean temporary chat candidate table: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit LID chat migration: %w", err)
	}

	logger.Infof(
		"LID chat migration complete: mapped_chats=%d inserted_messages=%d deleted_lid_messages=%d deleted_lid_chats=%d",
		mappedChats,
		insertedMessages,
		deletedMessages,
		deletedChats,
	)
	return nil
}

// ResetSelfNamedChats renames the chats an older bridge named after our own
// number (issue #448) back to the user part of their JID, the placeholder the
// normal resolution (chat_names.go) improves when the next message arrives and
// the MCP server already reads through to the phone book. The chat with
// ourselves keeps its name and groups are never looked at. One statement over
// the chats table, no message row touched, and a second run matches nothing,
// so it is safe on every startup. It returns how many chats it renamed.
func (store *MessageStore) ResetSelfNamedChats(self selfUsers) (int64, error) {
	if self.phone == "" {
		return 0, nil // not paired yet: no chat can be named after us
	}
	lid := self.lid
	if lid == "" {
		lid = self.phone // a session with no LID: the same value fills both binds
	}
	res, err := store.db.Exec(
		`UPDATE chats SET name = substr(jid, 1, instr(jid, '@') - 1)
		 WHERE name IN (?, ?)
		   AND jid NOT LIKE '%@g.us'
		   AND instr(jid, '@') > 1
		   AND substr(jid, 1, instr(jid, '@') - 1) NOT IN (?, ?)`,
		self.phone, lid, self.phone, lid,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to reset chats named after our own number: %w", err)
	}
	return res.RowsAffected()
}

// MigrateLegacyLIDSendersToPhones rewrites the `sender` column for any
// message whose stored value is a LID user-part for which whatsmeow has a
// known phone-number mapping. This is the row-level analogue of the
// chat-JID migration above and is required because earlier builds resolved
// the chat JID but stored the raw LID user-part as the sender, leaving
// the database internally inconsistent (chat = phone, sender = LID).
//
// The migration is idempotent: a second run finds no remaining LID-shaped
// senders to rewrite. It is safe to run on every startup.
func (store *MessageStore) MigrateLegacyLIDSendersToPhones(whatsappDBPath string, logger waLog.Logger) error {
	if _, err := os.Stat(whatsappDBPath); err != nil {
		if os.IsNotExist(err) {
			logger.Infof("Skipping LID sender migration: %s not found", whatsappDBPath)
			return nil
		}
		return fmt.Errorf("failed to stat WhatsApp DB %s: %w", whatsappDBPath, err)
	}

	alias := fmt.Sprintf("wa_sender_mig_%d", time.Now().UnixNano())
	tx, finish, err := store.beginMessageMigration(alias)
	if err != nil {
		return fmt.Errorf("failed to start LID sender migration transaction: %w", err)
	}
	defer finish()

	escapedPath := strings.ReplaceAll(whatsappDBPath, "'", "''")
	if _, err := tx.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS %s;", escapedPath, alias)); err != nil {
		return fmt.Errorf("failed to attach WhatsApp DB for LID sender migration: %w", err)
	}

	var lidMapTableExists int
	if err := tx.QueryRow(fmt.Sprintf(
		"SELECT COUNT(1) FROM %s.sqlite_master WHERE type='table' AND name='whatsmeow_lid_map';",
		alias,
	)).Scan(&lidMapTableExists); err != nil {
		return fmt.Errorf("failed to inspect WhatsApp DB schema for LID sender migration: %w", err)
	}
	if lidMapTableExists == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit no-op LID sender migration: %w", err)
		}
		logger.Infof("Skipping LID sender migration: whatsmeow_lid_map table not found")
		return nil
	}

	// The sender column stores just the user-part (no @server suffix), so we
	// match directly against whatsmeow_lid_map.lid. We pre-build a temp table
	// scoped to senders that actually appear in our messages, both to avoid
	// scanning the full LID map per row and to give us an accurate row count.
	if _, err := tx.Exec(fmt.Sprintf(`
		CREATE TEMP TABLE tmp_lid_sender_map AS
		SELECT DISTINCT lm.lid AS lid_user, lm.pn AS phone_user
		FROM %s.whatsmeow_lid_map lm
		WHERE lm.lid != '' AND lm.pn != ''
		  AND EXISTS (SELECT 1 FROM messages m WHERE m.sender = lm.lid);
	`, alias)); err != nil {
		return fmt.Errorf("failed to build temporary LID sender mapping table: %w", err)
	}

	var mappedSenders int
	if err := tx.QueryRow("SELECT COUNT(*) FROM tmp_lid_sender_map;").Scan(&mappedSenders); err != nil {
		return fmt.Errorf("failed to count mapped LID senders: %w", err)
	}

	if mappedSenders == 0 {
		if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_sender_map;"); err != nil {
			return fmt.Errorf("failed to clean temporary LID sender mapping table: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit no-op LID sender migration: %w", err)
		}
		logger.Infof("LID sender migration: nothing to migrate")
		return nil
	}

	// The namespace moves with the user part: a row whose LID we can now name
	// is a phone-number row (sender_namespace.go). It also bounds the rewrite,
	// which used to match on the digits alone: a row already recorded as a
	// phone number is not a LID that happens to spell the same number.
	updateResult, err := tx.Exec(`
		UPDATE messages
		SET sender = (
			SELECT phone_user FROM tmp_lid_sender_map WHERE lid_user = messages.sender
		),
		sender_server = 's.whatsapp.net'
		WHERE sender IN (SELECT lid_user FROM tmp_lid_sender_map)
		  AND (sender_server IS NULL OR sender_server = 'lid');
	`)
	if err != nil {
		return fmt.Errorf("failed to rewrite legacy LID senders: %w", err)
	}
	updatedRows, _ := updateResult.RowsAffected()

	if _, err := tx.Exec("DROP TABLE IF EXISTS tmp_lid_sender_map;"); err != nil {
		return fmt.Errorf("failed to clean temporary LID sender mapping table: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit LID sender migration: %w", err)
	}

	logger.Infof(
		"LID sender migration complete: mapped_senders=%d updated_messages=%d",
		mappedSenders,
		updatedRows,
	)
	return nil
}

// Close the database connections
func (store *MessageStore) Close() error {
	var waErr error
	if store.waDB != nil {
		waErr = store.waDB.Close()
	}
	if err := store.db.Close(); err != nil {
		return err
	}
	return waErr
}

// Store a chat in the database. An empty `name` preserves any existing
// resolved contact/group name on the row — outbound-message persistence
// doesn't have a friendly name available at send time and must not clobber
// names set by inbound handling or history sync. last_message_time is
// merged monotonically so out-of-order delivery (history sync, backfill)
// can't move it backwards.
//
// A zero lastMessageTime binds NULL, which the merge reads as "no news":
// the row is created or renamed and its time is left alone (EnsureChat).
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	return storeChatWith(store.db, jid, name, lastMessageTime)
}

func storeChatWith(ex sqlExecer, jid, name string, lastMessageTime time.Time) error {
	var seen any
	if !lastMessageTime.IsZero() {
		seen = dbTime(lastMessageTime)
	}
	_, err := ex.Exec(
		`INSERT INTO chats (jid, name, last_message_time)
		VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name = '' THEN chats.name ELSE excluded.name END,
			last_message_time = CASE
				WHEN chats.last_message_time IS NULL THEN excluded.last_message_time
				WHEN excluded.last_message_time IS NULL THEN chats.last_message_time
				WHEN excluded.last_message_time > chats.last_message_time THEN excluded.last_message_time
				ELSE chats.last_message_time
			END`,
		jid, name, seen,
	)
	return err
}

// UpdateChatEphemeralSettings records the chat's disappearing-message timer.
// Writes are gated on settingTimestamp so that low-information events don't
// clobber authoritative ones:
//
//   - settingTimestamp == 0: skip entirely. Sparse history-sync chunks and
//     plain (non-ephemeral) messages deliver records with no ephemeral fields,
//     and we must not interpret that absence as "the user turned it off".
//   - settingTimestamp older than the stored one: skip. Out-of-order delivery
//     (replays, late history-sync chunks, old messages flowing in) would
//     otherwise downgrade newer state to older state.
func (store *MessageStore) UpdateChatEphemeralSettings(jid string, expiration uint32, settingTimestamp int64) error {
	if settingTimestamp == 0 {
		return nil
	}
	// INSERT only the ephemeral columns; leave name/last_message_time NULL
	// so a `GroupInfo` event firing before any StoreChat call doesn't
	// fabricate placeholder metadata (raw JID as name, year-0001 timestamp)
	// that would leak into list_chats output.
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, ephemeral_expiration, ephemeral_setting_timestamp)
		VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			ephemeral_expiration = excluded.ephemeral_expiration,
			ephemeral_setting_timestamp = excluded.ephemeral_setting_timestamp
		WHERE excluded.ephemeral_setting_timestamp >= chats.ephemeral_setting_timestamp`,
		jid, expiration, settingTimestamp,
	)
	return err
}

// MarkChatRead records that we read the chat up to readAt. The marker merges
// monotonically — out-of-order receipts and history-sync backfill can never
// move it backwards and un-read a chat. Like UpdateChatEphemeralSettings it
// inserts only its own column, leaving name/last_message_time NULL so a receipt
// arriving before any StoreChat call doesn't fabricate placeholder metadata.
func (store *MessageStore) MarkChatRead(jid string, readAt time.Time) error {
	return markChatReadWith(store.db, jid, readAt)
}

func markChatReadWith(ex sqlExecer, jid string, readAt time.Time) error {
	_, err := ex.Exec(
		`INSERT INTO chats (jid, last_read_time)
		VALUES (?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			last_read_time = CASE
				WHEN chats.last_read_time IS NULL THEN excluded.last_read_time
				WHEN excluded.last_read_time IS NULL THEN chats.last_read_time
				WHEN excluded.last_read_time > chats.last_read_time THEN excluded.last_read_time
				ELSE chats.last_read_time
			END`,
		jid, dbTime(readAt),
	)
	return err
}

func (store *MessageStore) GetChatEphemeralSettings(jid string) (ChatEphemeralSettings, error) {
	var settings ChatEphemeralSettings
	err := store.db.QueryRow(
		"SELECT ephemeral_expiration, ephemeral_setting_timestamp FROM chats WHERE jid = ?",
		jid,
	).Scan(&settings.Expiration, &settings.SettingTimestamp)
	if err != nil {
		return ChatEphemeralSettings{}, err
	}
	return settings, nil
}

// GetMessageIsFromMe resolves the origin of a stored message for a quoted
// reply. The boolean pointer distinguishes a known false value from a quote
// that is absent from the local store.
func (store *MessageStore) GetMessageIsFromMe(id, chatJID string) (*bool, error) {
	var isFromMe bool
	err := store.db.QueryRow(
		"SELECT is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&isFromMe)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &isFromMe, nil
}

// bareSenderUser normalizes a phone/LID or full JID to the bare user part
// stored in messages.sender.
func bareSenderUser(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i]
	}
	return s
}

// ValidateInboundMarkRead checks that every message ID exists in chatJID, is
// inbound, and belongs to the expected sender before we send a read receipt.
// senderHint may be bare or a full JID; when empty (DM), the chat user is used.
func (store *MessageStore) ValidateInboundMarkRead(chatJID, senderHint string, ids []string) error {
	expected := bareSenderUser(senderHint)
	if expected == "" {
		if jid, err := types.ParseJID(chatJID); err == nil {
			expected = jid.User
		}
	}
	if expected == "" {
		return fmt.Errorf("could not determine expected sender for chat %q", chatJID)
	}

	for _, id := range ids {
		var sender string
		var isFromMe bool
		err := store.db.QueryRow(
			`SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?`,
			id, chatJID,
		).Scan(&sender, &isFromMe)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("message %q not found in chat %q", id, chatJID)
		}
		if err != nil {
			return err
		}
		if isFromMe {
			return fmt.Errorf("message %q is outbound; only inbound messages can be marked read", id)
		}
		if bareSenderUser(sender) != expected {
			return fmt.Errorf("message %q sender %q does not match %q", id, sender, expected)
		}
	}
	return nil
}

// MaxMessageTimestamp returns the latest stored timestamp among the given
// message IDs in chatJID. ok is false when none of the IDs are present.
func (store *MessageStore) MaxMessageTimestamp(chatJID string, ids []string) (time.Time, bool, error) {
	return maxMessageTimestampWith(store.db, chatJID, ids)
}

type sqlRowQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

func maxMessageTimestampWith(ex sqlRowQuerier, chatJID string, ids []string) (time.Time, bool, error) {
	if len(ids) == 0 {
		return time.Time{}, false, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, 1+len(ids))
	args = append(args, chatJID)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	var raw any
	err := ex.QueryRow(
		`SELECT MAX(timestamp) FROM messages WHERE chat_jid = ? AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	).Scan(&raw)
	if err != nil {
		return time.Time{}, false, err
	}
	if raw == nil {
		return time.Time{}, false, nil
	}
	ts := anchorTime(raw)
	if ts.IsZero() {
		return time.Time{}, false, fmt.Errorf("unparseable message timestamp %v", raw)
	}
	return ts, true, nil
}

// unreadInboundMessage is one row selected for a whole-chat read receipt.
type unreadInboundMessage struct {
	ID        string
	Sender    string // bare user part, as stored in messages.sender
	Timestamp time.Time
}

// UnreadInboundMessages returns the inbound, not-deleted messages of chatJID
// that the local read marker (chats.last_read_time) has not covered yet and
// that are no newer than upTo, oldest first. limit caps the rows returned
// (0 = no limit).
//
// The bounds are string comparisons against the TIMESTAMP column, which is
// chronological because every value is written in the canonical UTC spelling
// (store_time.go) — that is also what keeps idx_messages_chat_timestamp usable
// here. A row whose timestamp no layout can parse is skipped rather than
// acknowledged with a made-up read time.
//
// Reaction and poll-vote rows are included on purpose: they advance
// chats.last_message_time like any other row (events.go stores them as
// messages), so a chat whose newest row is a reaction would keep reporting
// itself unread if the marker could not reach it.
func (store *MessageStore) UnreadInboundMessages(chatJID string, upTo time.Time, limit int) ([]unreadInboundMessage, error) {
	var marker sql.NullString
	err := store.db.QueryRow(
		`SELECT CAST(last_read_time AS TEXT) FROM chats WHERE jid = ?`, chatJID,
	).Scan(&marker)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	query := `SELECT id, sender, timestamp FROM messages
		WHERE chat_jid = ? AND is_from_me = 0 AND read_receipt_sent = 0 AND deleted_at IS NULL AND timestamp <= ?`
	args := []any{chatJID, dbTime(upTo)}
	if strings.TrimSpace(marker.String) != "" {
		query += ` AND timestamp > ?`
		args = append(args, marker.String)
	}
	// id breaks ties so a LIMIT always cuts the same page in the same place.
	query += ` ORDER BY timestamp, id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pending []unreadInboundMessage
	for rows.Next() {
		var id string
		var sender sql.NullString
		var rawTS any
		if err := rows.Scan(&id, &sender, &rawTS); err != nil {
			return nil, err
		}
		ts := anchorTime(rawTS)
		if ts.IsZero() {
			bridgeLog.Warnf("skipping message %s in %s: unparseable timestamp %v", id, chatJID, rawTS)
			continue
		}
		pending = append(pending, unreadInboundMessage{
			ID:        id,
			Sender:    bareSenderUser(sender.String),
			Timestamp: ts,
		})
	}
	return pending, rows.Err()
}

// AcknowledgedReceiptPrefix finds saved receipt progress above the current
// marker, stopping before the first receipt still pending in this range.
func (store *MessageStore) AcknowledgedReceiptPrefix(chatJID string, upTo time.Time) (time.Time, error) {
	var raw any
	err := store.db.QueryRow(`
		WITH marker AS (SELECT COALESCE(last_read_time, '') AS at FROM chats WHERE jid=?)
		SELECT MAX(timestamp) FROM messages
		WHERE chat_jid=? AND is_from_me=0 AND deleted_at IS NULL AND read_receipt_sent=1
		AND timestamp > COALESCE((SELECT at FROM marker), '') AND timestamp <= ?
		AND timestamp < COALESCE((
			SELECT MIN(timestamp) FROM messages
			WHERE chat_jid=? AND is_from_me=0 AND deleted_at IS NULL AND read_receipt_sent=0
			AND timestamp > COALESCE((SELECT at FROM marker), '') AND timestamp <= ?
		), '9999-12-31 23:59:59+00:00')`, chatJID, chatJID, dbTime(upTo), chatJID, dbTime(upTo)).Scan(&raw)
	return anchorTime(raw), err
}

// StoreMessage stores one message. sender may be the full resolved JID —
// which is what every bridge write path passes, so the row records which
// namespace the sender lives in — or the bare user part, which leaves
// messages.sender_server unset (splitSenderJID, sender_namespace.go).
func (store *MessageStore) StoreMessage(message storedMessage) error {
	return store.Batch(func(batch *messageBatch) error { return batch.StoreMessage(message) })
}

// MarkMessageDeleted records a "delete for everyone" event by stamping
// deleted_at on the target row. Content is preserved on purpose — the
// local DB is an archive, and the value is in knowing the message was
// retracted, not in erasing what was said.
//
// First-revoke-wins: once deleted_at is set, a later REVOKE does not
// overwrite it. Calling this for a message that does not exist (e.g.
// the bridge missed the original) is a silent no-op, not an error.
func (store *MessageStore) MarkMessageDeleted(messageID, chatJID string, deletedAt time.Time) error {
	_, err := store.db.Exec(
		`UPDATE messages SET deleted_at = ?
		 WHERE id = ? AND chat_jid = ? AND deleted_at IS NULL`,
		dbTime(deletedAt), messageID, chatJID,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Call storage methods.
//
// WhatsApp calls arrive as a sequence of events: Offer/OfferNotice → Accept →
// Terminate (or Reject → Terminate). We model each call as a single row keyed
// by (call_id, chat_jid), upserted as events arrive. The `result` column
// tracks the call's final state as the event sequence plays out.
//
// State machine:
//   Offer/OfferNotice → result = "in_progress"
//   Accept            → result = "answered"
//   Reject            → result = "rejected"
//   Terminate         → if result == "in_progress" → "missed"
//                       if result == "answered"    → "ended"
//                       otherwise preserve existing (rejected stays rejected)

// StoreCallOffer inserts a new call row when an offer event arrives. Uses
// INSERT OR IGNORE so duplicate offer events (rare but possible) don't clobber
// a call already in a later lifecycle state.
func (store *MessageStore) StoreCallOffer(callID, chatJID, fromJID string, timestamp time.Time, isFromMe bool, callType string, isGroup bool) error {
	_, err := store.db.Exec(
		`INSERT OR IGNORE INTO calls
		 (call_id, chat_jid, from_jid, timestamp, is_from_me, call_type, is_group, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'in_progress')`,
		callID, chatJID, fromJID, dbTime(timestamp), isFromMe, callType, isGroup,
	)
	return err
}

// MarkCallAnswered records that the offer was accepted.
func (store *MessageStore) MarkCallAnswered(callID, chatJID string) error {
	_, err := store.db.Exec(
		`UPDATE calls SET result = 'answered'
		 WHERE call_id = ? AND chat_jid = ? AND result = 'in_progress'`,
		callID, chatJID,
	)
	return err
}

// MarkCallRejected records that the call was explicitly rejected.
func (store *MessageStore) MarkCallRejected(callID, chatJID string) error {
	_, err := store.db.Exec(
		`UPDATE calls SET result = 'rejected'
		 WHERE call_id = ? AND chat_jid = ? AND result = 'in_progress'`,
		callID, chatJID,
	)
	return err
}

// MarkCallTerminated records the end of a call, computing duration from the
// offer timestamp. Infers final result when the call was still in_progress
// (meaning no accept was seen → the call was missed).
func (store *MessageStore) MarkCallTerminated(callID, chatJID, reason string, endedAt time.Time) error {
	// ROUND before CAST: julianday() arithmetic produces a float and CAST truncates
	// toward zero, so a 90-second call would otherwise record as 89. julianday()
	// reads the canonical spelling and the driver's "…-03:00" one, so the duration
	// is right for any row the migration has seen; a row it had to skip (Go's
	// "… -0300 -03" form, which SQLite's date functions cannot read) yields NULL.
	ended := dbTime(endedAt)
	_, err := store.db.Exec(
		`UPDATE calls SET
			ended_at = ?,
			duration_sec = CAST(ROUND((julianday(?) - julianday(timestamp)) * 86400) AS INTEGER),
			reason = ?,
			result = CASE result
				WHEN 'in_progress' THEN 'missed'
				WHEN 'answered'    THEN 'ended'
				ELSE result
			END
		 WHERE call_id = ? AND chat_jid = ?`,
		ended, ended, reason, callID, chatJID,
	)
	return err
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		// last_message_time can be NULL — UpdateChatEphemeralSettings can
		// create a chat row from a GroupInfo / ephemeral-setting event
		// before any message has landed for that chat.
		var lastMessageTime sql.NullTime
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		if lastMessageTime.Valid {
			chats[jid] = lastMessageTime.Time
		} else {
			chats[jid] = time.Time{}
		}
	}

	return chats, nil
}

// SetTargetMessageID records which message a reaction or poll vote refers to.
// `filename` still carries the same value for one release so older readers
// keep working; new readers use target_message_id.
func (store *MessageStore) SetTargetMessageID(id, chatJID, target string) error {
	return setTargetMessageIDWith(store.db, id, chatJID, target)
}

func setTargetMessageIDWith(ex sqlExecer, id, chatJID, target string) error {
	_, err := ex.Exec(`UPDATE messages SET target_message_id = ? WHERE id = ? AND chat_jid = ?`, target, id, chatJID)
	return err
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID string, refreshed *MediaDownloader) error {
	url, mediaKey := refreshed.URL, refreshed.MediaKey
	fileSHA256, fileEncSHA256, fileLength := refreshed.FileSHA256, refreshed.FileEncSHA256, refreshed.FileLength
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, direct_path = NULLIF(?, ''), media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = CASE WHEN ? = 0 AND file_sha256 = ? THEN file_length ELSE NULLIF(?, 0) END, media_presentation = CASE WHEN file_sha256 = ? THEN media_presentation END WHERE id = ? AND chat_jid = ?",
		url, refreshed.DirectPath, mediaKey, fileSHA256, fileEncSHA256, fileLength, fileSHA256, fileLength, fileSHA256, id, chatJID,
	)
	return err
}

// SetDirectPath records the direct path of a stored message's media. An empty
// path clears the column, so the download falls back to the one in `url`.
func (store *MessageStore) SetDirectPath(messageID, chatJID, directPath string) error {
	return setDirectPathWith(store.db, messageID, chatJID, directPath)
}

func setDirectPathWith(ex sqlExecer, messageID, chatJID, directPath string) error {
	_, err := ex.Exec(`UPDATE messages SET direct_path = NULLIF(?, '') WHERE id = ? AND chat_jid = ?`, directPath, messageID, chatJID)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength sql.NullInt64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	length, lengthErr := mediaLengthValue(fileLength)
	if err == nil {
		err = lengthErr
	}
	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, length, err
}
