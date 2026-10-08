package main

import (
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func newMigrationTestStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Setenv(storeDirEnv, t.TempDir())
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	return ms
}

func TestMigrationMarkersAreIndependent(t *testing.T) {
	for _, legacyVersion := range []int{0, 3} {
		t.Run(map[int]string{0: "incomplete timestamp", 3: "future timestamp migration"}[legacyVersion], func(t *testing.T) {
			ms := newMigrationTestStore(t)
			seedLegacyRow(t, ms.db, "INSERT INTO chats(jid,name) VALUES (?, 'Alice')", mediaTestChat)
			ts := "2026-09-04 10:00:00+00:00"
			if legacyVersion == 0 {
				ts = "not a time"
			}
			seedLegacyRow(t, ms.db, "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,file_length) VALUES ('LEGACY1', ?, 'x', 'caption', ?, 0, 'image', 0)", mediaTestChat, ts)
			seedLegacyRow(t, ms.db, "DELETE FROM schema_migrations")
			if legacyVersion == 3 {
				seedLegacyRow(t, ms.db, "PRAGMA user_version=3")
				if err := recordMigration(ms.db, "canonical_timestamps_v3"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensureMessageStoreSchema(ms.db); err != nil {
				t.Fatal(err)
			}
			var version int
			if err := ms.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != legacyVersion {
				t.Fatalf("legacy version=%d err=%v", version, err)
			}
			applied, err := migrationApplied(ms.db, canonicalTimestampsMigration)
			if err != nil || applied != (legacyVersion == 3) {
				t.Fatalf("timestamp applied=%v err=%v", applied, err)
			}
			if applied, err := migrationApplied(ms.db, undeclaredMediaLengthsMigration); err != nil || !applied {
				t.Fatalf("length applied=%v err=%v", applied, err)
			}
			var length sql.NullInt64
			if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='LEGACY1'").Scan(&length); err != nil || length.Valid {
				t.Fatalf("legacy media=%v err=%v", length, err)
			}
			if legacyVersion == 0 {
				seedLegacyRow(t, ms.db, "UPDATE messages SET timestamp='2026-09-04 10:00:00+00:00' WHERE id='LEGACY1'")
				if err := ensureMessageStoreSchema(ms.db); err != nil {
					t.Fatal(err)
				}
				if applied, err := migrationApplied(ms.db, canonicalTimestampsMigration); err != nil || !applied {
					t.Fatalf("timestamp repair not recorded: %v %v", applied, err)
				}
			}
		})
	}
}

func TestLegacyLengthMigrationOnlyRewritesMediaAndLogsCount(t *testing.T) {
	rec := installRecordingLogger(t)
	ms := newMigrationTestStore(t)
	seedLegacyRow(t, ms.db, "INSERT INTO chats(jid,name) VALUES (?, 'Alice')", mediaTestChat)
	seedLegacyRow(t, ms.db, "DELETE FROM schema_migrations WHERE name=?", undeclaredMediaLengthsMigration)
	for _, kind := range []string{"image", "video", "audio", "document", "sticker", "", "reaction", "poll_vote"} {
		seedLegacyRow(t, ms.db, "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,file_length) VALUES (?, ?, 'x', 'caption', '2026-09-04 10:00:00+00:00', 0, ?, 0)", "OLD"+kind, mediaTestChat, kind)
	}
	if err := migrateUndeclaredMediaLengths(ms.db); err != nil {
		t.Fatal(err)
	}
	var nulls, zeroes int
	if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE file_length IS NULL").Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if err := ms.db.QueryRow("SELECT count(*) FROM messages WHERE file_length=0").Scan(&zeroes); err != nil {
		t.Fatal(err)
	}
	if nulls != 5 || zeroes != 3 || !strings.Contains(rec.String(), "marked 5 legacy media length(s) undeclared") {
		t.Fatalf("nulls=%d zeroes=%d log=%s", nulls, zeroes, rec.String())
	}
}

func TestNewNonMediaLengthIsNull(t *testing.T) {
	for _, batch := range []bool{false, true} {
		ms := newTestMessageStore(t)
		seedLegacyRow(t, ms.db, "INSERT INTO chats(jid,name) VALUES (?, 'Alice')", mediaTestChat)
		replayWriter(t, ms, batch, func(w messageWriter) error {
			return w.StoreMessage("TEXT1", mediaTestChat, "x", "text", time.Now(), false, "", "", "", nil, nil, nil, uint64(0), "")
		})
		var length sql.NullInt64
		if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='TEXT1'").Scan(&length); err != nil || length.Valid {
			t.Fatalf("non-media=%v err=%v", length, err)
		}
	}
}

func TestUnrepresentableDeclaredLengthKeepsTheArchivedMessage(t *testing.T) {
	for _, batch := range []bool{false, true} {
		ms := newTestMessageStore(t)
		seedLegacyRow(t, ms.db, "INSERT INTO chats(jid,name) VALUES (?, 'Alice')", mediaTestChat)
		for _, length := range []uint64{uint64(math.MaxInt64) + 1, math.MaxUint64} {
			doc := fixtureDocument()
			doc.FileLength = proto.Uint64(length)
			ex := extractMessage(&waE2E.Message{DocumentMessage: doc}, time.Now(), "HUGE1")
			replayWriter(t, ms, batch, func(w messageWriter) error {
				return persistMessage(w, "HUGE1", mediaTestChat, "x", time.Now(), false, ex, false, testLogger())
			})
			var stored sql.NullInt64
			if err := ms.db.QueryRow("SELECT file_length FROM messages WHERE id='HUGE1'").Scan(&stored); err != nil || stored.Valid {
				t.Fatalf("declared=%d stored=%v err=%v", length, stored, err)
			}
		}
	}
}
