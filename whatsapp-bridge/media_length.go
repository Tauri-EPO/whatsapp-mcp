package main

import (
	"database/sql"
	"fmt"
	"math"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

const undeclaredMediaLengthsMigration = "undeclared_media_lengths_v1"

func storedMediaLength(length uint64) any {
	if length > uint64(math.MaxInt64) {
		return nil
	}
	return length
}

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
	var changed int64
	applied, err := applyNamedMigration(db, undeclaredMediaLengthsMigration, func(tx *sql.Tx) error {
		// Old non-media zeroes stay zero: rewriting them would amplify WAL
		// for no useful conversion, as readers do not expose their length.
		result, err := tx.Exec("UPDATE messages SET file_length = NULL WHERE file_length = 0 AND media_type IN ('image','video','audio','document','sticker')")
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	if err != nil || !applied {
		return err
	}
	if changed > 0 {
		bridgeLog.Infof("Media length migration: marked %d legacy media length(s) undeclared", changed)
	} else {
		bridgeLog.Debugf("Media length migration: marked 0 legacy media length(s) undeclared")
	}
	return nil
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
