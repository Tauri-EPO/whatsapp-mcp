package main

// Who a message addressed: messages.mentions.
//
// WhatsApp puts the mentioned accounts in ContextInfo.MentionedJID and renders
// them in the text as "@<user>" — usually the LID user ("@158883943301358"),
// which reads like a phone number. extractMentionedJIDs has pulled that list
// out for a long time, but only the webhook ever saw it, so "who was addressed
// in this group" was not answerable from messages.db: an agent had to paste its
// own LID into a full-text search and hope (issue #290).
//
// The column stores the bare user parts, comma-separated, exactly as they
// arrived — the LID user for a LID mention, the phone user for a phone one. No
// LID→phone lookup happens on the write path: the reader knows both forms of
// its own identity (GET /api/me, me.go) and matches either, which is the same
// answer without a store round trip per mention during a history-sync burst.
// A message with no mentions is written with no value at all, so the column
// costs nothing on the archive; the backfill below writes an empty string
// instead, which is how it remembers that it has already read that row.

import (
	"database/sql"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// mentionsColumn renders MentionedJID entries for the mentions column: bare
// user parts, de-duplicated, order preserved. Empty when nothing was mentioned,
// which the callers store as NULL.
func mentionsColumn(jids []string) string {
	if len(jids) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(jids))
	users := make([]string, 0, len(jids))
	for _, jid := range jids {
		user := bareSenderUser(jid)
		// An AD JID carries the device in the user part ("5511…:12").
		if i := strings.IndexByte(user, ':'); i >= 0 {
			user = user[:i]
		}
		if user == "" || seen[user] {
			continue
		}
		seen[user] = true
		users = append(users, user)
	}
	return strings.Join(users, ",")
}

// SetMentions records the mentioned users of a stored message. Called only
// when there is something to store, so it never clears a previous value.
func (store *MessageStore) SetMentions(messageID, chatJID, mentions string) error {
	return setMentionsWith(store.db, messageID, chatJID, mentions)
}

func setMentionsWith(ex sqlExecer, messageID, chatJID, mentions string) error {
	_, err := ex.Exec(`UPDATE messages SET mentions = ? WHERE id = ? AND chat_jid = ?`, mentions, messageID, chatJID)
	return err
}

// mentionTextPattern matches a mention as WhatsApp renders it in the text: an
// "@" followed by the mentioned user's digits. Five digits is the shortest
// national number in use; the bound keeps "@1" style handles out.
var mentionTextPattern = regexp.MustCompile(`@(\d{5,})`)

// mentionsFromText recovers the mentions of an old row from its text. It is a
// best effort by construction — a message that merely types a number after an
// "@" is indistinguishable from one that mentioned it — but the false
// positives are exactly the strings an agent would have had to search for by
// hand. The tool docs say so; nothing downstream can tell a recovered value
// from a real one.
func mentionsFromText(content string) string {
	if !strings.ContainsRune(content, '@') {
		return ""
	}
	matches := mentionTextPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return ""
	}
	users := make([]string, 0, len(matches))
	for _, m := range matches {
		users = append(users, m[1])
	}
	return mentionsColumn(users)
}

// migrateMentionsBackfill fills messages.mentions for rows written before the
// column existed.
func migrateMentionsBackfill(db *sql.DB) error {
	filled, err := backfillMentionsFromContent(db)
	if err != nil {
		return fmt.Errorf("failed to backfill messages.mentions: %w", err)
	}
	if filled > 0 {
		bridgeLog.Infof("Mentions migration: recovered mentions for %d message(s) from the message text", filled)
	}
	return nil
}

// mentionsBackfillChunk bounds how many rows one backfill transaction holds.
const mentionsBackfillChunk = 5000

// backfillMentionsFromContent scans the rows that could carry a mention and
// have not been scanned yet, fills the ones whose text has one and marks the
// rest with an empty string. That mark is what makes the pass idempotent: the
// next boot only sees the rows stored since this one, so there is no schema
// stamp to keep in step with the other migrations (and no way for one
// migration's permanent failure to pin another's). Paged by rowid so a large
// archive never sits in memory, or in one write transaction, at once.
func backfillMentionsFromContent(db *sql.DB) (int, error) {
	selectSQL := fmt.Sprintf(`SELECT rowid, content FROM messages
		  WHERE rowid > ? AND mentions IS NULL AND content LIKE '%%@%%'
		  ORDER BY rowid LIMIT %d`, mentionsBackfillChunk)
	const updateSQL = `UPDATE messages SET mentions = ? WHERE rowid = ?`

	filled := 0
	cursor := int64(math.MinInt64) // no rowid can sort before this
	for {
		batch, err := pendingMentionRows(db, selectSQL, cursor)
		if err != nil {
			return filled, err
		}
		if len(batch) == 0 {
			return filled, nil
		}
		cursor = batch[len(batch)-1].rowid

		tx, err := db.Begin()
		if err != nil {
			return filled, err
		}
		stmt, err := tx.Prepare(updateSQL)
		if err != nil {
			_ = tx.Rollback()
			return filled, err
		}
		for _, row := range batch {
			mentions := mentionsFromText(row.content)
			if _, err := stmt.Exec(mentions, row.rowid); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				return filled, err
			}
			if mentions != "" {
				filled++
			}
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			return filled, err
		}
	}
}

// pendingMentionRow is one candidate row waiting to be scanned.
type pendingMentionRow struct {
	rowid   int64
	content string
}

// pendingMentionRows reads one page of candidates. The rows are fully consumed
// before the caller opens its write transaction: SQLite holds a read lock for
// as long as a statement is stepping.
func pendingMentionRows(db *sql.DB, selectSQL string, cursor int64) ([]pendingMentionRow, error) {
	rows, err := db.Query(selectSQL, cursor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var batch []pendingMentionRow
	for rows.Next() {
		var row pendingMentionRow
		var content sql.NullString
		if err := rows.Scan(&row.rowid, &content); err != nil {
			return nil, err
		}
		row.content = content.String
		batch = append(batch, row)
	}
	return batch, rows.Err()
}
