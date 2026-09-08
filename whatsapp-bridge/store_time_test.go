package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zone database for the container images without tzdata
)

// dbTime always renders UTC with an explicit +00:00 offset and a fixed width,
// whatever location the caller's time.Time carries. That is what makes the
// plain string comparison SQLite does on a TEXT column a comparison of
// instants.
func TestDBTimeIsCanonicalUTC(t *testing.T) {
	sao, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 9, 7, 20, 10, 8, 123456789, time.UTC)

	for _, loc := range []*time.Location{time.UTC, sao, kolkata} {
		if got := dbTime(instant.In(loc)); got != "2026-09-07 20:10:08+00:00" {
			t.Errorf("dbTime in %s = %q", loc, got)
		}
	}
}

// Every spelling a previous release (or its driver) wrote must still read back
// as the same instant, so the migration and the readers never lose a row.
func TestParseDBTimeAcceptsLegacySpellings(t *testing.T) {
	want := time.Date(2026, 9, 4, 20, 13, 9, 0, time.UTC)
	cases := map[string]string{
		"canonical":          "2026-09-04 20:13:09+00:00",
		"driver write form":  "2026-09-04 17:13:09.000000000-03:00",
		"driver T separator": "2026-09-04T17:13:09-03:00",
		"go String()":        "2026-09-04 17:13:09 -0300 -03",
		"go String() nanos":  "2026-09-04 20:13:09.000000000 +0000 UTC",
		"go String() mono":   "2026-09-04 20:13:09 +0000 UTC m=+0.001234567",
		"rfc3339":            "2026-09-04T20:13:09Z",
		"no offset":          "2026-09-04 20:13:09",
		"padded":             "  2026-09-04 20:13:09+00:00  ",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseDBTime(raw)
			if err != nil {
				t.Fatalf("parseDBTime(%q): %v", raw, err)
			}
			if !got.Equal(want) {
				t.Fatalf("parseDBTime(%q) = %v, want %v", raw, got, want)
			}
			if round := dbTime(got); round != "2026-09-04 20:13:09+00:00" {
				t.Fatalf("dbTime round-trip of %q = %q", raw, round)
			}
		})
	}

	for _, bad := range []string{"", "   ", "not a time", "1757000000"} {
		if _, err := parseDBTime(bad); err == nil {
			t.Errorf("parseDBTime(%q) accepted a non-timestamp", bad)
		}
	}
}

// The point of the canonical spelling: ordering the strings orders the
// instants. The same two instants spelled with their local offsets sort the
// wrong way round, which is the bug behind issue #270.
func TestCanonicalTimeSortsByInstant(t *testing.T) {
	sao, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	earlier := time.Date(2026, 9, 7, 19, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	if dbTime(earlier) >= dbTime(later) {
		t.Fatalf("canonical spellings sort wrong: %q vs %q", dbTime(earlier), dbTime(later))
	}
	legacyEarlier := earlier.Format("2006-01-02 15:04:05.999999999-07:00")
	legacyLater := later.In(sao).Format("2006-01-02 15:04:05.999999999-07:00")
	if legacyEarlier < legacyLater {
		t.Fatal("expected the mixed-offset spellings to sort by wall clock, not by instant")
	}
}

// countNonCanonical reports how many non-NULL values of a column are not in
// the canonical spelling.
func countNonCanonical(t *testing.T, db *sql.DB, table, column string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE `+column+` IS NOT NULL AND CAST(`+column+` AS TEXT) NOT GLOB ?`,
		canonicalTimeGlob,
	).Scan(&n); err != nil {
		t.Fatalf("count %s.%s: %v", table, column, err)
	}
	return n
}

// Every column the bridge writes comes out in the canonical spelling even when
// the value handed in carries a non-UTC offset, and MarkCallTerminated still
// computes the duration from it (julianday() has to read the offset back).
func TestBridgeWritesCanonicalTimestamps(t *testing.T) {
	sao, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}

	ms := newTestMessageStore(t)
	const chat = "5511999999999@s.whatsapp.net"
	base := time.Date(2026, 9, 7, 20, 10, 8, 0, time.UTC).In(sao)

	if err := ms.StoreChat(chat, "Alice", base); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkChatRead(chat, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("M1", chat, "5511999999999", "hi", base, false, "", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkMessageDeleted("M1", chat, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreCallOffer("C1", chat, chat, base, false, "voice", false); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkCallAnswered("C1", chat); err != nil {
		t.Fatal(err)
	}
	if err := ms.MarkCallTerminated("C1", chat, "hangup", base.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ms.StorePoll("P1", chat, &pollCreation{Question: "Q", Options: []string{"a", "b"}, SelectableCount: 1}, base); err != nil {
		t.Fatal(err)
	}
	if err := ms.StorePollVote("P1", chat, "5511999999999", []string{"a"}, base); err != nil {
		t.Fatal(err)
	}

	for _, col := range canonicalTimeColumns {
		if n := countNonCanonical(t, ms.db, col.table, col.column); n != 0 {
			t.Errorf("%s.%s: %d value(s) not in the canonical spelling", col.table, col.column, n)
		}
	}

	var storedTS string
	if err := ms.db.QueryRow(`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'M1'`).Scan(&storedTS); err != nil {
		t.Fatal(err)
	}
	if storedTS != "2026-09-07 20:10:08+00:00" {
		t.Errorf("messages.timestamp = %q", storedTS)
	}

	var duration int
	if err := ms.db.QueryRow(`SELECT duration_sec FROM calls WHERE call_id = 'C1'`).Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if duration != 90 {
		t.Errorf("duration_sec = %d, want 90 (julianday could not read the canonical spelling)", duration)
	}
}

// seedLegacyRow writes a raw string straight into a TIMESTAMP column,
// bypassing the store methods, to reproduce what earlier releases left behind.
func seedLegacyRow(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seed %q: %v", query, err)
	}
}

// A store written by earlier releases holds a mix of offsets and Go's
// time.Time.String() form. The migration rewrites every one of them to the
// canonical UTC spelling without moving the instant, leaves a value it cannot
// parse alone, stamps user_version and does nothing on the second run.
func TestMigrateCanonicalTimestamps(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	t.Chdir(t.TempDir())
	rec := installRecordingLogger(t)

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	db := ms.db

	const chat = "5511999999999@s.whatsapp.net"
	seedLegacyRow(t, db, `INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, 'Alice', ?, ?)`,
		chat, "2026-09-04 17:13:09.123456-03:00", "2026-09-04 17:00:00 -0300 -03")
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at)
		VALUES ('M1', ?, 's', 'a', ?, 0, ?)`, chat, "2026-09-04 17:13:09.123456-03:00", "2026-09-04 18:00:00 -0300 -03")
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('M2', ?, 's', 'b', ?, 0)`, chat, "2026-09-04 19:00:00+00:00") // already canonical
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('M3', ?, 's', 'c', ?, 0)`, chat, "2026-09-04T21:30:00Z")
	seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('M4', ?, 's', 'broken', 'not a time', 0)`, chat)
	seedLegacyRow(t, db, `INSERT INTO calls (call_id, chat_jid, from_jid, timestamp, is_from_me, call_type, is_group, result, ended_at)
		VALUES ('C1', ?, ?, ?, 0, 'voice', 0, 'ended', ?)`,
		chat, chat, "2026-09-04 17:13:09 -0300 -03", "2026-09-04 17:14:39.5-03:00")
	seedLegacyRow(t, db, `INSERT INTO polls (message_id, chat_jid, question, options_json, selectable_count, created_at)
		VALUES ('P1', ?, 'Q', '["a"]', 1, ?)`, chat, "2026-09-04 17:13:09 -0300 -03")
	seedLegacyRow(t, db, `INSERT INTO poll_votes (poll_message_id, chat_jid, voter, selected_json, voted_at)
		VALUES ('P1', ?, 'v', '["a"]', ?)`, chat, "2026-09-04 17:20:00.5-03:00")

	// NewMessageStore already stamped user_version on the empty store; the rows
	// above are what an upgrade from an older release actually finds.
	if _, err := db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	if err := migrateCanonicalTimestamps(db); err != nil {
		t.Fatalf("migrateCanonicalTimestamps: %v", err)
	}

	for _, col := range canonicalTimeColumns {
		want := 0
		if col.table == "messages" && col.column == "timestamp" {
			want = 1 // the unparseable M4 row is left as it is
		}
		if n := countNonCanonical(t, db, col.table, col.column); n != want {
			t.Errorf("%s.%s: %d non-canonical value(s) after the migration, want %d", col.table, col.column, n, want)
		}
	}

	// The instant is preserved, not the wall clock: 17:13:09-03:00 is 20:13:09 UTC.
	for _, tc := range []struct{ query, want string }{
		{`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'M1'`, "2026-09-04 20:13:09+00:00"},
		{`SELECT CAST(deleted_at AS TEXT) FROM messages WHERE id = 'M1'`, "2026-09-04 21:00:00+00:00"},
		{`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'M2'`, "2026-09-04 19:00:00+00:00"},
		{`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'M3'`, "2026-09-04 21:30:00+00:00"},
		{`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'M4'`, "not a time"},
		{`SELECT CAST(last_message_time AS TEXT) FROM chats WHERE jid = '` + chat + `'`, "2026-09-04 20:13:09+00:00"},
		{`SELECT CAST(last_read_time AS TEXT) FROM chats WHERE jid = '` + chat + `'`, "2026-09-04 20:00:00+00:00"},
		{`SELECT CAST(timestamp AS TEXT) FROM calls WHERE call_id = 'C1'`, "2026-09-04 20:13:09+00:00"},
		{`SELECT CAST(ended_at AS TEXT) FROM calls WHERE call_id = 'C1'`, "2026-09-04 20:14:39+00:00"},
		{`SELECT CAST(created_at AS TEXT) FROM polls WHERE message_id = 'P1'`, "2026-09-04 20:13:09+00:00"},
		{`SELECT CAST(voted_at AS TEXT) FROM poll_votes WHERE poll_message_id = 'P1'`, "2026-09-04 20:20:00+00:00"},
	} {
		var got string
		if err := db.QueryRow(tc.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.query, got, tc.want)
		}
	}

	logged := rec.String()
	if !strings.Contains(logged, "rewrote 2 messages.timestamp value(s)") {
		t.Errorf("expected the migration to log the per-column count, got:\n%s", logged)
	}
	if !strings.Contains(logged, `rowid=4 unchanged`) {
		t.Errorf("expected a warning for the unparseable row, got:\n%s", logged)
	}
	if !strings.Contains(logged, "1 value(s) could not be parsed") {
		t.Errorf("expected the migration to say the store is not fully converted, got:\n%s", logged)
	}

	// The unparseable row keeps the store unstamped, so a later release still
	// gets a chance at it.
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Errorf("user_version = %d, want 0 while a value is still unconverted", version)
	}

	// Idempotent: a second pass over the same rows changes nothing.
	for _, col := range canonicalTimeColumns {
		n, skipped, err := rewriteToCanonicalTime(db, col.table, col.column)
		if err != nil {
			t.Fatalf("re-run %s.%s: %v", col.table, col.column, err)
		}
		if n != 0 {
			t.Errorf("re-run rewrote %d %s.%s value(s)", n, col.table, col.column)
		}
		want := 0
		if col.table == "messages" && col.column == "timestamp" {
			want = 1
		}
		if skipped != want {
			t.Errorf("re-run skipped %d %s.%s value(s), want %d", skipped, col.table, col.column, want)
		}
	}

	// Once the bad row is gone the migration completes and stamps the version.
	if _, err := db.Exec(`DELETE FROM messages WHERE id = 'M4'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCanonicalTimestamps(db); err != nil {
		t.Fatalf("migrateCanonicalTimestamps (second run): %v", err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != messagesDBUserVersion {
		t.Errorf("user_version = %d, want %d", version, messagesDBUserVersion)
	}

	// And the ordering the whole change is about: the M-rows now come out in
	// chronological order under a plain ORDER BY.
	rows, err := db.Query(`SELECT id FROM messages WHERE id != 'M4' ORDER BY timestamp`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "M2" || order[1] != "M1" || order[2] != "M3" {
		t.Errorf("ORDER BY timestamp gave %v, want [M2 M1 M3]", order)
	}
}

// An operator who pins the image back to a release without the migration, then
// rolls forward, leaves driver-formatted rows behind a stamped store. The
// startup probe finds them and re-runs the rewrite for that column, whatever
// legacy spelling the older bridge used, and says so in the log.
func TestMigrateCanonicalTimestampsRepairsRolledBackRows(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	t.Chdir(t.TempDir())
	rec := installRecordingLogger(t)

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()
	db := ms.db

	// NewMessageStore stamps the empty store; these rows land after the stamp.
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != messagesDBUserVersion {
		t.Fatalf("a fresh store should be stamped, user_version = %d", version)
	}

	const chat = "5511999999999@s.whatsapp.net"
	seedLegacyRow(t, db, `INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, 'Alice', ?, ?)`,
		chat, "2026-09-04 17:13:09.123456-03:00", "2026-09-04 17:13:09 -0300 -03")
	// One row per legacy spelling parseDBTime knows, all the same instant.
	legacy := []string{
		"2026-09-04 17:13:09.123456-03:00",       // driver write form
		"2026-09-04T17:13:09.123456-03:00",       // driver write form, T separator
		"2026-09-04 17:13:09 -0300 -03",          // Go time.Time.String()
		"2026-09-04 17:13:09.123456 -0300 -03",   // same, fractional seconds
		"2026-09-04T20:13:09Z",                   // RFC3339
		"2026-09-04 20:13:09.123456",             // no offset, read as UTC
		"2026-09-04T20:13:09.123456",             // same, T separator
		"2026-09-04 20:13:09 +0000 UTC m=+0.001", // String() with a monotonic reading
	}
	for i, raw := range legacy {
		seedLegacyRow(t, db, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
			VALUES (?, ?, 's', 'a', ?, 0)`, fmt.Sprintf("M%d", i), chat, raw)
	}
	seedLegacyRow(t, db, `INSERT INTO calls (call_id, chat_jid, from_jid, timestamp, is_from_me, call_type, is_group, result, ended_at)
		VALUES ('C1', ?, ?, ?, 0, 'voice', 0, 'ended', ?)`,
		chat, chat, "2026-09-04 17:13:09 -0300 -03", "2026-09-04 17:14:39.5-03:00")

	if err := migrateCanonicalTimestamps(db); err != nil {
		t.Fatalf("migrateCanonicalTimestamps: %v", err)
	}

	for _, col := range canonicalTimeColumns {
		if n := countNonCanonical(t, db, col.table, col.column); n != 0 {
			t.Errorf("%s.%s: %d value(s) left in a legacy spelling after the repair", col.table, col.column, n)
		}
	}
	for i := range legacy {
		var got string
		id := fmt.Sprintf("M%d", i)
		if err := db.QueryRow(`SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != "2026-09-04 20:13:09+00:00" {
			t.Errorf("%s (%q) repaired to %q", id, legacy[i], got)
		}
	}

	logged := rec.String()
	if !strings.Contains(logged, "repaired 8 messages.timestamp value(s) written by an older bridge") {
		t.Errorf("expected the repair to log the per-column count, got:\n%s", logged)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != messagesDBUserVersion {
		t.Errorf("user_version = %d after the repair, want %d", version, messagesDBUserVersion)
	}

	// Idempotent: the next start probes, finds nothing and rewrites nothing.
	if err := migrateCanonicalTimestamps(db); err != nil {
		t.Fatalf("migrateCanonicalTimestamps (second run): %v", err)
	}
	if second := strings.TrimPrefix(rec.String(), logged); strings.Contains(second, "Timestamp migration") {
		t.Errorf("a repaired store should be silent on the next start, got:\n%s", second)
	}
}

// The probe reads one column and stops at the first hit, so where the column is
// indexed SQLite answers it from the index instead of the table. This is the
// cost the bridge pays on every start; the plan is asserted so a future index
// change that turns it into a table scan is visible.
func TestLegacyTimeProbeQueryPlan(t *testing.T) {
	t.Setenv(storeDirEnv, t.TempDir())
	t.Chdir(t.TempDir())
	installRecordingLogger(t)

	ms, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	defer func() { _ = ms.Close() }()

	// messages is the table that grows without bound, so both of its time
	// columns must be answered from an index; the rest are small enough to read.
	indexed := map[string]bool{
		"messages.timestamp":      true,
		"messages.deleted_at":     true,
		"chats.last_message_time": true,
		"calls.timestamp":         true,
	}
	for _, col := range canonicalTimeColumns {
		name := col.table + "." + col.column
		var id, parent, notUsed int
		var detail string
		row := ms.db.QueryRow("EXPLAIN QUERY PLAN "+legacyTimeProbeSQL(col.table, col.column), canonicalTimeGlob)
		if err := row.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN %s: %v", name, err)
		}
		t.Logf("%s: %s", name, detail)
		if indexed[name] && !strings.Contains(detail, "INDEX") {
			t.Errorf("%s probe no longer uses an index: %s", name, detail)
		}
	}
}
