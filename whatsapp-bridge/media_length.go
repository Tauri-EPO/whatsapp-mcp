package main

import (
	"database/sql"
	"fmt"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

const mediaLengthDBVersion = 2

func mediaLengthValue(length sql.NullInt64) (uint64, error) {
	if !length.Valid {
		return 0, nil
	}
	if length.Int64 < 0 {
		return 0, fmt.Errorf("invalid stored media length")
	}
	return uint64(length.Int64), nil
}

// Legacy zeroes have no presence bit: conservatively mark them undeclared once.
// The transaction stamps the same write, so a later known empty file stays zero.
func migrateUndeclaredMediaLengths(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version >= mediaLengthDBVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version >= mediaLengthDBVersion {
		return nil
	}
	if _, err := tx.Exec("UPDATE messages SET file_length = NULL WHERE file_length = 0"); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", mediaLengthDBVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func mediaLengthDeclared(msg *waE2E.Message) bool {
	_, part := mediaPartOf(msg)
	switch media := part.(type) {
	case *waE2E.ImageMessage:
		return media.FileLength != nil
	case *waE2E.VideoMessage:
		return media.FileLength != nil
	case *waE2E.AudioMessage:
		return media.FileLength != nil
	case *waE2E.DocumentMessage:
		return media.FileLength != nil
	case *waE2E.StickerMessage:
		return media.FileLength != nil
	}
	return false
}
