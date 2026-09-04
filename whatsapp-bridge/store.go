package main

// SQLite message store: schema, migrations and every (*MessageStore) method
// that does not belong to a feature file (fts.go, polls.go, view_once.go ...).
// messages.db is owned by the bridge; whatsapp.db is whatsmeow's and only
// read here for contact/LID resolution.

import (
	"database/sql"
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

	names     *chatNameCache  // resolved chat names + failed group lookups (chat_names.go)
	groupInfo groupInfoLookup // live group metadata fetch; nil = no network
	fts       bool            // messages_fts active (fts.go)
}

type ChatEphemeralSettings struct {
	Expiration       uint32
	SettingTimestamp int64
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(storeDir(), 0o750); err != nil {
		return nil, fmt.Errorf("failed to create store directory %q: %v", storeDir(), err)
	}

	// Open SQLite database for messages
	// WAL lets the MCP server read messages.db while the bridge writes (a
	// history-sync burst used to make readers hit SQLITE_BUSY), and the busy
	// timeout makes both sides wait instead of failing on a short lock.
	db, err := sql.Open("sqlite3", sqliteURI(messagesDBPath(), "_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

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
		CREATE INDEX IF NOT EXISTS idx_chats_last_message ON chats(last_message_time);
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
		bridgeLog.Infof("SQLite built without FTS5: message search uses the substring scan (build with -tags sqlite_fts5 to enable)")
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

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureMessageStoreSchema(db *sql.DB) error {
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
	return nil
}

func ensureColumn(db *sql.DB, tableName, columnName, columnSpec string) error {
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

	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start LID chat migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	alias := fmt.Sprintf("wa_mig_%d", time.Now().UnixNano())
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
			id, chat_jid, sender, content, timestamp, is_from_me,
			media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length
		)
		SELECT
			msg.id,
			m.phone_jid,
			msg.sender,
			msg.content,
			msg.timestamp,
			msg.is_from_me,
			msg.media_type,
			msg.filename,
			msg.url,
			msg.media_key,
			msg.file_sha256,
			msg.file_enc_sha256,
			msg.file_length
		FROM messages msg
		JOIN tmp_lid_to_phone m ON m.lid_jid = msg.chat_jid;
	`)
	if err != nil {
		return fmt.Errorf("failed to copy legacy LID messages into phone chats: %w", err)
	}

	insertedMessages, _ := insertResult.RowsAffected()

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

	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to start LID sender migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	alias := fmt.Sprintf("wa_sender_mig_%d", time.Now().UnixNano())
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

	updateResult, err := tx.Exec(`
		UPDATE messages
		SET sender = (
			SELECT phone_user FROM tmp_lid_sender_map WHERE lid_user = messages.sender
		)
		WHERE sender IN (SELECT lid_user FROM tmp_lid_sender_map);
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
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
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
		jid, name, lastMessageTime,
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
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, last_read_time)
		VALUES (?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			last_read_time = CASE
				WHEN chats.last_read_time IS NULL THEN excluded.last_read_time
				WHEN excluded.last_read_time IS NULL THEN chats.last_read_time
				WHEN excluded.last_read_time > chats.last_read_time THEN excluded.last_read_time
				ELSE chats.last_read_time
			END`,
		jid, readAt,
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
	err := store.db.QueryRow(
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

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
	quotedMessageId string) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	// Store empty quoted_message_id as SQL NULL so the column is null for
	// plain messages (no ContextInfo). This makes the ON CONFLICT merge
	// straightforward: COALESCE prefers the new non-null value over a
	// kept null, and ignores an incoming null so it cannot clobber a
	// previously-stored ID.
	var qmid interface{}
	if quotedMessageId != "" {
		qmid = quotedMessageId
	}

	_, err := store.db.Exec(
		`INSERT INTO messages
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
			quoted_message_id = COALESCE(excluded.quoted_message_id, messages.quoted_message_id)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, qmid,
	)
	return err
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
		deletedAt, messageID, chatJID,
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
		callID, chatJID, fromJID, timestamp, isFromMe, callType, isGroup,
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
	// toward zero, so a 90-second call would otherwise record as 89.
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
		endedAt, endedAt, reason, callID, chatJID,
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
	_, err := store.db.Exec(`UPDATE messages SET target_message_id = ? WHERE id = ? AND chat_jid = ?`, target, id, chatJID)
	return err
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}
