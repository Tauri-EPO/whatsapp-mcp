package main

import (
	"crypto/sha256"
	"encoding/json"
)

type historyMessageVersion [sha256.Size]byte

// This transient version belongs to one import, not to persisted schema. Read
// the whole row under the IMMEDIATE transaction so a live/phone/edit change
// between chunks revokes a peer replay's permission to replace that row.
func (b *messageBatch) historyRowVersion(id, chat string) (version historyMessageVersion, exists bool, err error) {
	err = b.write(func() error {
		rows, queryErr := b.tx.Query("SELECT * FROM messages WHERE id=? AND chat_jid=?", id, chat)
		if queryErr != nil {
			return queryErr
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return rows.Err()
		}
		columns, columnErr := rows.Columns()
		if columnErr != nil {
			return columnErr
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if scanErr := rows.Scan(targets...); scanErr != nil {
			return scanErr
		}
		encoded, encodeErr := json.Marshal(values)
		if encodeErr != nil {
			return encodeErr
		}
		version, exists = sha256.Sum256(encoded), true
		return rows.Err()
	})
	return version, exists, err
}
