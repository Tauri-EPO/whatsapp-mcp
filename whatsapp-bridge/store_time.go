package main

// One canonical spelling for every timestamp the bridge writes to messages.db.
//
// Until this file existed, every write bound a time.Time and let the SQLite
// driver render it. modernc.org/sqlite with `_time_format=sqlite` writes the
// time in whatever location the time.Time carries, so a row stored from a
// whatsmeow event (time.Unix -> Local) got "…-03:00" while a row stored from a
// UTC value got "…+00:00", and older stores also hold Go's time.Time.String()
// form ("… -0300 -03"). Three consequences, all bugs:
//
//   - `ORDER BY timestamp` and `timestamp > ?` compare the strings, so rows
//     with different offsets sort and filter by wall clock, not by instant: a
//     -03:00 row looks three hours later than it is.
//   - a bound time.Time cannot be compared against a column whose rows use a
//     different offset, which is why the readers grew expression wrappers that
//     defeat the index.
//   - clients that read last_message_time / last_read_time straight out of a
//     tool result saw two different offsets in the same response (issue #270).
//
// The fix is to format the string here: UTC, seconds resolution, explicit
// "+00:00" offset. Every row then has the same width and the same offset, so
// lexicographic order is chronological order and a bound value is directly
// comparable. migrateCanonicalTimestamps rewrites existing rows once.

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// dbTimeLayout is the canonical on-disk spelling. Values are always rendered
// after .UTC(), so the "-07:00" token always comes out as "+00:00"; it is
// spelled as a layout token rather than a literal so a caller that forgets the
// .UTC() produces a wrong-but-parseable value instead of a lie.
const dbTimeLayout = "2006-01-02 15:04:05-07:00"

// dbTime renders t for storage in any TIMESTAMP column of messages.db.
// Sub-second precision is dropped on purpose: WhatsApp timestamps have
// one-second resolution, and a fixed-width string is what makes the plain
// string comparison in SQL a chronological one.
func dbTime(t time.Time) string {
	return t.UTC().Format(dbTimeLayout)
}

// dbTimeLayouts are every spelling a bridge release (or its SQLite driver) has
// ever written into a TIMESTAMP column, canonical form first. They are listed
// literally rather than imported from the driver because the driver's exported
// list lives in a cgo file and is an implementation detail.
var dbTimeLayouts = []string{
	dbTimeLayout,                              // canonical
	"2006-01-02 15:04:05.999999999-07:00",     // driver write form with `_time_format=sqlite`
	"2006-01-02T15:04:05.999999999-07:00",     // same, RFC3339-ish `T` separator
	"2006-01-02 15:04:05 -0700 MST",           // Go time.Time.String(), the modernc default
	"2006-01-02 15:04:05.999999999 -0700 MST", // same, with fractional seconds
	time.RFC3339,
	"2006-01-02 15:04:05.999999999", // no offset; SQLite reads these as UTC
	"2006-01-02T15:04:05.999999999",
}

// parseDBTime reads any spelling dbTimeLayouts covers back into a time.Time.
func parseDBTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	// time.Time.String() appends a monotonic reading (" m=+0.001") for values
	// derived from time.Now(); it is not part of any layout.
	if i := strings.Index(s, " m="); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range dbTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

// anchorTime converts a scanned timestamp column into a time.Time. The type
// depends on the query: the driver parses a column declared TIMESTAMP into a
// time.Time on scan, while an expression over one (MAX(...), a CAST) has no
// declared type and comes back as the raw string. A zero time means the value
// could not be read.
func anchorTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case []byte:
		return anchorTime(string(t))
	case string:
		if parsed, err := parseDBTime(t); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// messagesDBUserVersion is the schema version stamped on messages.db once every
// time column holds the canonical spelling. Bump it (and add the matching
// rewrite) when a future migration has to touch existing rows again.
const messagesDBUserVersion = 1

// canonicalTimeGlob matches exactly what dbTime produces, so rows already in
// the canonical spelling are skipped by the migration without being parsed.
// GLOB treats '+', '-' and ':' literally.
const canonicalTimeGlob = "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]+00:00"

// canonicalTimeColumns are the TIMESTAMP columns the bridge writes.
// chats.ephemeral_setting_timestamp is deliberately absent: it is an INTEGER
// of WhatsApp seconds, not a time string.
var canonicalTimeColumns = []struct{ table, column string }{
	{"messages", "timestamp"},
	{"messages", "deleted_at"},
	{"chats", "last_message_time"},
	{"chats", "last_read_time"},
	{"calls", "timestamp"},
	{"calls", "ended_at"},
	{"polls", "created_at"},
	{"poll_votes", "voted_at"},
}

// migrateCanonicalTimestamps rewrites every non-canonical value in
// canonicalTimeColumns into the canonical UTC spelling and stamps
// PRAGMA user_version. It is idempotent: a store already at
// messagesDBUserVersion is left alone, and re-running the rewrite on canonical
// rows changes nothing.
//
// The version is stamped only when every value was converted. A value nothing
// can parse would otherwise stay in the store forever, sorting and filtering
// wrong on the strength of one startup warning; leaving the store unstamped
// costs a scan per boot and keeps saying so until the row is fixed.
//
// Known gap: the stamp is trusted, so rows written by an *older* binary after
// this migration ran (an image pinned back to a previous release for a day,
// then rolled forward) are never revisited. Re-running the rewrite means
// clearing the stamp by hand: PRAGMA user_version = 0 with the bridge stopped.
func migrateCanonicalTimestamps(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("failed to read messages.db user_version: %w", err)
	}
	if version >= messagesDBUserVersion {
		return nil
	}

	skipped := 0
	for _, col := range canonicalTimeColumns {
		rewritten, columnSkipped, err := rewriteToCanonicalTime(db, col.table, col.column)
		if err != nil {
			return fmt.Errorf("failed to normalise %s.%s: %w", col.table, col.column, err)
		}
		skipped += columnSkipped
		if rewritten > 0 {
			bridgeLog.Infof("Timestamp migration: rewrote %d %s.%s value(s) to UTC", rewritten, col.table, col.column)
		}
	}

	if skipped > 0 {
		bridgeLog.Warnf("Timestamp migration: %d value(s) could not be parsed and stay in their old spelling; "+
			"time bounds and ordering are wrong for those rows until they are fixed or deleted", skipped)
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", messagesDBUserVersion)); err != nil {
		return fmt.Errorf("failed to stamp messages.db user_version: %w", err)
	}
	return nil
}

// canonicalTimeChunk bounds how many rows one migration transaction holds, so
// a store with hundreds of thousands of messages does not need all of them in
// memory (or in one write transaction) at once.
const canonicalTimeChunk = 5000

// rewriteToCanonicalTime normalises one column, paging by rowid so the scan
// always makes progress even when a value cannot be parsed. It returns how
// many values it rewrote and how many it had to skip; unparseable values are
// logged and left untouched — an archive row is worth more than a tidy column.
func rewriteToCanonicalTime(db *sql.DB, table, column string) (rewritten, skipped int, err error) {
	// The table/column names come from canonicalTimeColumns, never from input.
	selectSQL := fmt.Sprintf(
		`SELECT rowid, CAST(%[1]s AS TEXT) FROM %[2]s
		  WHERE rowid > ? AND %[1]s IS NOT NULL AND CAST(%[1]s AS TEXT) NOT GLOB ?
		  ORDER BY rowid LIMIT %[3]d`, column, table, canonicalTimeChunk)
	updateSQL := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, column)

	cursor := int64(math.MinInt64) // no rowid can sort before this
	for {
		batch, err := pendingTimeRows(db, selectSQL, cursor)
		if err != nil {
			return rewritten, skipped, err
		}
		if len(batch) == 0 {
			return rewritten, skipped, nil
		}
		cursor = batch[len(batch)-1].rowid

		tx, err := db.Begin()
		if err != nil {
			return rewritten, skipped, err
		}
		stmt, err := tx.Prepare(updateSQL)
		if err != nil {
			_ = tx.Rollback()
			return rewritten, skipped, err
		}
		for _, row := range batch {
			parsed, parseErr := parseDBTime(row.value)
			if parseErr != nil {
				bridgeLog.Warnf("Timestamp migration: leaving %s.%s rowid=%d unchanged: %v", table, column, row.rowid, parseErr)
				skipped++
				continue
			}
			if _, err := stmt.Exec(dbTime(parsed), row.rowid); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return rewritten, skipped, err
			}
			rewritten++
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			return rewritten, skipped, err
		}
	}
}

// pendingTimeRow is one non-canonical value waiting to be rewritten.
type pendingTimeRow struct {
	rowid int64
	value string
}

// pendingTimeRows reads one page of non-canonical values. The rows are fully
// consumed before the caller opens its write transaction: SQLite holds a read
// lock for as long as a statement is stepping.
func pendingTimeRows(db *sql.DB, selectSQL string, cursor int64) ([]pendingTimeRow, error) {
	rows, err := db.Query(selectSQL, cursor, canonicalTimeGlob)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var batch []pendingTimeRow
	for rows.Next() {
		var row pendingTimeRow
		if err := rows.Scan(&row.rowid, &row.value); err != nil {
			return nil, err
		}
		batch = append(batch, row)
	}
	return batch, rows.Err()
}
