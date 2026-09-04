package main

// Full-text index over messages.content.
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

// ftsSchema creates the index and its sync triggers. The FTS table is an
// external-content table over messages (content stays in one place; the index
// only stores tokens). Triggers use the "delete" command form required by
// external-content tables; a plain DELETE on the fts table would corrupt it.
const ftsSchema = `
	CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		content='messages',
		content_rowid='rowid',
		tokenize='unicode61 remove_diacritics 2'
	);
	CREATE TRIGGER IF NOT EXISTS messages_fts_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_ad AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_au AFTER UPDATE OF content ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
	END;
`

// ftsTeardown removes everything ftsSchema creates. Order matters: triggers
// first, so a half-removed state never leaves a trigger pointing at nothing.
const ftsTeardown = `
	DROP TRIGGER IF EXISTS messages_fts_ai;
	DROP TRIGGER IF EXISTS messages_fts_ad;
	DROP TRIGGER IF EXISTS messages_fts_au;
	DROP TABLE IF EXISTS messages_fts;
`

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
	existed, err := messagesFTSExists(db)
	if err != nil {
		return false, fmt.Errorf("inspect fts schema: %w", err)
	}
	if !sqliteHasFTS5(db) {
		if existed {
			if _, err := db.Exec(ftsTeardown); err != nil {
				return false, fmt.Errorf("remove fts index from a build without FTS5: %w", err)
			}
		}
		return false, nil
	}
	if _, err := db.Exec(ftsSchema); err != nil {
		// Defensive: if creation fails halfway, make sure no trigger survives.
		_, _ = db.Exec(ftsTeardown)
		if strings.Contains(err.Error(), "no such module") {
			return false, nil
		}
		return false, fmt.Errorf("create fts index: %w", err)
	}
	if !existed {
		if _, err := db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')`); err != nil {
			return false, fmt.Errorf("populate fts index: %w", err)
		}
	}
	return true, nil
}
