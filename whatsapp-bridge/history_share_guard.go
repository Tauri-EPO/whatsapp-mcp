package main

import (
	"database/sql"
	"errors"
)

// Called inside the canonical IMMEDIATE transaction. Peer bundles may add a
// missing key, but may not replace either an older row or one from this import.
func (b *messageBatch) historyRowExists(id, chat string) (exists bool, err error) {
	err = b.write(func() error {
		var present int
		queryErr := b.tx.QueryRow("SELECT 1 FROM messages WHERE id=? AND chat_jid=?", id, chat).Scan(&present)
		if errors.Is(queryErr, sql.ErrNoRows) {
			return nil
		}
		exists = queryErr == nil
		return queryErr
	})
	return exists, err
}

// Rows and activity commit together, including a last chunk before shutdown
// or expiry. Only the keys inserted by this transaction contribute activity.
func (b *messageBatch) storePeerHistoryActivity(chat string, keys map[string]struct{}) error {
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	return b.write(func() error {
		stamp, present, err := maxMessageTimestampWith(b.tx, chat, ids)
		if err != nil || !present {
			return err
		}
		return storeChatWith(b.tx, chat, "", stamp)
	})
}
