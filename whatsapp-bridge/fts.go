package main

// Full-text index over current/pre-edit text and original document filenames.
//
// The MCP server's list_messages(query=...) used a substring scan
// (instr()), which cannot fold accents ("orcamento" never finds "orçamento"),
// has no notion of words ("ana" matches "semana") and reads every row. An
// FTS5 index with the unicode61 tokenizer and remove_diacritics fixes all
// three.
//
// The bridge owns the messages.db schema, so it also owns the index: it is an
// external-content FTS5 table kept in sync by triggers. Making the bridge the
// owner (rather than letting the Python side create triggers, as an earlier
// upstream attempt did) matters for one reason: a trigger that references an
// fts5 table makes every INSERT into messages fail on a SQLite build without
// the module. The bridge therefore checks FTS5 availability at startup and,
// when it is missing, *drops* any index and triggers a previous build may
// have created, so writes can never break. The MCP server checks for the
// table and falls back to the substring scan when it is absent.
//
// modernc.org/sqlite ships FTS5, so every build has it; the runtime check
// stays as a guard for a future driver or build that does not.

import (
	"database/sql"
	"fmt"
	"strings"
)

const messagesFTSTable = "messages_fts"
const documentFilenameFTSMigration = "messages_fts_document_filename_v2"
const originalContentFTSMigration = "messages_fts_original_content_v3"
const ftsBackfillChunk = 512

// ftsSchema creates the index and its sync triggers. The FTS table is an
// external-content table over a messages view (content stays in one place; the index
// only stores tokens). Triggers use the "delete" command form required by
// external-content tables; a plain DELETE on the fts table would corrupt it.
const ftsViewSchema = `
	CREATE VIEW IF NOT EXISTS messages_fts_content AS
		SELECT rowid, content, original_content,
			CASE WHEN media_type = 'document' AND NOT (
				substr(filename, 1, 9) = 'document_'
				AND substr(filename, 10, 15) GLOB '[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]'
				AND substr(filename, 25) = CASE WHEN coalesce(id, '') = '' THEN '' ELSE '_' || id END
			) THEN filename ELSE '' END AS filename
		FROM messages;
`

const ftsSchema = `
	CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		filename,
		original_content,
		content='messages_fts_content',
		content_rowid='rowid',
		tokenize='unicode61 remove_diacritics 2'
	);
	CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, content, filename, original_content)
			SELECT rowid, content, filename, original_content FROM messages_fts_content WHERE rowid = new.rowid;
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content, filename, original_content)
			VALUES ('delete', old.rowid, old.content, CASE WHEN old.media_type = 'document' AND NOT (
				substr(old.filename, 1, 9) = 'document_'
				AND substr(old.filename, 10, 15) GLOB '[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]'
				AND substr(old.filename, 25) = CASE WHEN coalesce(old.id, '') = '' THEN '' ELSE '_' || old.id END
			) THEN old.filename ELSE '' END, old.original_content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE OF id, content, original_content, filename, media_type ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content, filename, original_content)
			VALUES ('delete', old.rowid, old.content, CASE WHEN old.media_type = 'document' AND NOT (
				substr(old.filename, 1, 9) = 'document_'
				AND substr(old.filename, 10, 15) GLOB '[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]'
				AND substr(old.filename, 25) = CASE WHEN coalesce(old.id, '') = '' THEN '' ELSE '_' || old.id END
			) THEN old.filename ELSE '' END, old.original_content);
		INSERT INTO messages_fts(rowid, content, filename, original_content)
			SELECT rowid, content, filename, original_content FROM messages_fts_content WHERE rowid = new.rowid;
	END;
`

// ftsTeardown removes everything ftsSchema creates. Order matters: triggers
// first, so a half-removed state never leaves a trigger pointing at nothing.
const ftsIndexTeardown = `
	DROP TRIGGER IF EXISTS messages_fts_ai;
	DROP TRIGGER IF EXISTS messages_fts_ad;
	DROP TRIGGER IF EXISTS messages_fts_au;
	DROP TABLE IF EXISTS messages_fts;
`
const ftsTeardown = ftsIndexTeardown + `
	DROP TABLE IF EXISTS messages_fts_build;
	DROP VIEW IF EXISTS messages_fts_content;
`

// Startup owns the archive before event writers start. Populate a private index
// in bounded autocommit chunks; readers keep the old index until publication.
func buildMessagesFTS(db *sql.DB, afterChunk func(int) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS messages_fts_build;
		DROP VIEW IF EXISTS messages_fts_content;` + ftsViewSchema + `
		CREATE VIRTUAL TABLE messages_fts_build USING fts5(
			content, filename, original_content, content='messages_fts_content',
			content_rowid='rowid', tokenize='unicode61 remove_diacritics 2');`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var last int64
	first := true
	for {
		query, args := "SELECT rowid FROM messages ORDER BY rowid LIMIT ?", []any{ftsBackfillChunk}
		if !first {
			query, args = "SELECT rowid FROM messages WHERE rowid > ? ORDER BY rowid LIMIT ?", []any{last, ftsBackfillChunk}
		}
		rows, err := db.Query(query, args...)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, ftsBackfillChunk)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close() // Release the cursor before acquiring SQLite's writer.
		if err != nil || len(ids) == 0 {
			return err
		}
		last, first = ids[len(ids)-1], false
		if _, err := db.Exec(`INSERT INTO messages_fts_build(rowid,content,filename,original_content)
			SELECT rowid,content,filename,original_content FROM messages_fts_content
			WHERE rowid >= ? AND rowid <= ? ORDER BY rowid`, ids[0], last); err != nil {
			return err
		}
		if afterChunk != nil {
			if err := afterChunk(len(ids)); err != nil {
				return err
			}
		}
	}
}

// sqliteHasFTS5 reports whether the connected SQLite build exposes the fts5
// module.
func sqliteHasFTS5(db *sql.DB) bool {
	var enabled int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_compile_options WHERE compile_options = 'ENABLE_FTS5'`).Scan(&enabled)
	return err == nil && enabled > 0
}

// messagesFTSExists reports whether the index table is present in the schema.
func messagesFTSExists(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, messagesFTSTable).Scan(&n)
	return n > 0, err
}

// ensureMessagesFTS brings the index to the state this build supports.
// Returns true when the index is active. A freshly created index is
// rebuilt from existing rows so history imported before this version is
// searchable too; on a large store that is a one-off cost of a few seconds.
func ensureMessagesFTS(db *sql.DB) (bool, error) {
	return ensureMessagesFTSCapability(db, sqliteHasFTS5(db))
}

func ensureMessagesFTSCapability(db *sql.DB, available bool) (bool, error) {
	return ensureMessagesFTSMigration(db, available, func(tx *sql.Tx) error {
		// An empty archive needs no staged tokens. Avoid ALTER TABLE's full
		// schema rewrite when creating a new store (also common in tests).
		var populated bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM messages LIMIT 1)").Scan(&populated); err != nil {
			return err
		}
		publish := ftsIndexTeardown + `ALTER TABLE messages_fts_build RENAME TO messages_fts;` + ftsSchema
		if !populated {
			publish = ftsTeardown + ftsViewSchema + ftsSchema
		}
		if _, err := tx.Exec(publish); err != nil {
			return err
		}
		// This replacement also covers the older filename migration.
		_, err := tx.Exec("INSERT OR IGNORE INTO schema_migrations(name) VALUES (?)", documentFilenameFTSMigration)
		return err
	})
}

func ensureMessagesFTSMigration(db *sql.DB, available bool, migrate func(*sql.Tx) error) (bool, error) {
	existed, err := messagesFTSExists(db)
	if err != nil {
		return false, fmt.Errorf("inspect fts schema: %w", err)
	}
	if !available {
		if _, err := db.Exec(ftsTeardown); err != nil {
			return false, fmt.Errorf("remove fts index from a build without FTS5: %w", err)
		}
		return false, nil
	}
	fail := func(err error) (bool, error) {
		// Rollback can restore an older index and its triggers. Always remove
		// them before returning, so a failed optional index cannot break writes.
		if _, cleanupErr := db.Exec(ftsTeardown); cleanupErr != nil {
			return false, fmt.Errorf("migrate message search index: %w (teardown failed: %v)", err, cleanupErr)
		}
		if strings.Contains(err.Error(), "no such module") {
			return false, nil
		}
		return false, fmt.Errorf("migrate message search index: %w", err)
	}
	applied, err := migrationApplied(db, originalContentFTSMigration)
	if err != nil {
		return fail(err)
	}
	if applied && existed {
		if _, err := db.Exec(ftsViewSchema + ftsSchema); err != nil {
			return fail(err)
		}
		return true, nil
	}
	if applied {
		// Capability-off startup removed the index, not the archive. Rebuild
		// even though an earlier incarnation completed the named migration.
		if _, err := db.Exec("DELETE FROM schema_migrations WHERE name=?", originalContentFTSMigration); err != nil {
			return fail(err)
		}
	}
	if err := buildMessagesFTS(db, nil); err != nil {
		return fail(err)
	}
	// Publish only the complete replacement and its marker, in one transaction.
	if _, err := applyNamedMigration(db, originalContentFTSMigration, migrate); err != nil {
		return fail(err)
	}
	return true, nil
}
