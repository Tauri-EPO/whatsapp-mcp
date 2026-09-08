package main

// group_members: the membership table behind "which groups is this person in?"
//
// messages.db only knows who has *spoken*, so a contact who belongs to a group
// but never posted there is invisible to a join over messages (issue #288).
// This table caches the rosters themselves. Four feeds, cheapest first:
//
//   - /api/group/members — every successful GetGroupInfo replaces that group's
//     roster (source "roster"); the only feed that can also *remove* a member;
//   - events.GroupInfo Join/Leave/Promote/Demote — incremental deltas as they
//     happen (source "event"), see group_events.go;
//   - a group message from a participant with no row yet — a minimal row
//     (source "message"), so a group nobody has fetched still learns who is
//     active in it;
//   - runGroupRosterSync — a paced background refresh of the groups whose
//     roster has not been confirmed for Bridge.GroupRosterSync.
//
// Every address column holds the *bare* user part (no "@server"), the same
// spelling messages.sender uses since #281, so the MCP server can match a
// contact's aliases against user/phone/lid with one IN list.
//
// last_seen means "membership last confirmed", not "last active": the message
// feed inserts but never bumps it, because roster staleness is measured from
// the roster rows and a chatty group would otherwise never be refreshed.

import (
	"database/sql"
	"strings"
	"time"
)

// Where a row came from. Only roster rows are proof that the whole group was
// looked at, so only they drive the staleness query.
const (
	groupMemberSourceRoster  = "roster"
	groupMemberSourceEvent   = "event"
	groupMemberSourceMessage = "message"
)

const groupMembersSchema = `
	CREATE TABLE IF NOT EXISTS group_members (
		group_jid TEXT NOT NULL,
		user TEXT NOT NULL,
		lid TEXT,
		phone TEXT,
		name TEXT,
		is_admin BOOLEAN NOT NULL DEFAULT 0,
		is_super_admin BOOLEAN NOT NULL DEFAULT 0,
		first_seen TIMESTAMP,
		last_seen TIMESTAMP,
		source TEXT,
		PRIMARY KEY (group_jid, user)
	);
	-- get_contact_chats looks a contact up by whichever address form it holds.
	CREATE INDEX IF NOT EXISTS idx_group_members_user ON group_members(user);
	CREATE INDEX IF NOT EXISTS idx_group_members_phone ON group_members(phone);
	CREATE INDEX IF NOT EXISTS idx_group_members_lid ON group_members(lid);
`

// groupMemberRow is one participant as the table stores it.
type groupMemberRow struct {
	User         string // primary key part: the phone user part when known, else the LID one
	Phone        string
	LID          string
	Name         string
	IsAdmin      bool
	IsSuperAdmin bool
}

// bareUser strips the "@server" from a JID string; a value that is already a
// bare user part passes through unchanged.
func bareUser(jid string) string {
	return strings.SplitN(strings.TrimSpace(jid), "@", 2)[0]
}

// newGroupMemberRow builds a row from the three address forms, picking the
// phone user part as the key when there is one so a member keeps the same
// spelling messages.sender uses. ok=false when no address form is usable.
func newGroupMemberRow(jid, phone, lid string) (groupMemberRow, bool) {
	row := groupMemberRow{Phone: bareUser(phone), LID: bareUser(lid)}
	switch {
	case row.Phone != "":
		row.User = row.Phone
	case row.LID != "":
		row.User = row.LID
	default:
		row.User = bareUser(jid)
	}
	return row, row.User != ""
}

// groupMemberRows converts the /api/group/members shape into table rows,
// dropping participants with no usable address.
func groupMemberRows(members []GroupMember) []groupMemberRow {
	rows := make([]groupMemberRow, 0, len(members))
	for _, m := range members {
		row, ok := newGroupMemberRow(m.JID, m.PhoneNumber, m.LID)
		if !ok {
			continue
		}
		row.Name = strings.TrimSpace(m.Name)
		row.IsAdmin, row.IsSuperAdmin = m.IsAdmin, m.IsSuperAdmin
		rows = append(rows, row)
	}
	return rows
}

// upsertGroupMemberSQL writes a member, keeping first_seen and any address
// form or name the new row does not carry. Admin flags and source come from
// the incoming row: only feeds that know them use this statement.
const upsertGroupMemberSQL = `
	INSERT INTO group_members
		(group_jid, user, lid, phone, name, is_admin, is_super_admin, first_seen, last_seen, source)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(group_jid, user) DO UPDATE SET
		lid = COALESCE(NULLIF(excluded.lid, ''), group_members.lid),
		phone = COALESCE(NULLIF(excluded.phone, ''), group_members.phone),
		name = COALESCE(NULLIF(excluded.name, ''), group_members.name),
		is_admin = excluded.is_admin,
		is_super_admin = excluded.is_super_admin,
		last_seen = excluded.last_seen,
		source = excluded.source`

func upsertGroupMember(stmt *sql.Stmt, groupJID string, row groupMemberRow, source, stamp string) error {
	_, err := stmt.Exec(groupJID, row.User, row.LID, row.Phone, row.Name,
		row.IsAdmin, row.IsSuperAdmin, stamp, stamp, source)
	return err
}

// ReplaceGroupRoster makes the table match the roster WhatsApp just returned:
// every member is written (first_seen preserved) and rows the roster no longer
// lists are dropped. An empty roster is ignored rather than read as "the group
// is empty" — GetGroupInfo never returns one for a group we are in, so an
// empty slice means something went wrong upstream and deleting the cached
// membership would lose real information. Returns the rows written.
func (store *MessageStore) ReplaceGroupRoster(groupJID string, rows []groupMemberRow, now time.Time) (int, error) {
	if store == nil || store.db == nil || groupJID == "" || len(rows) == 0 {
		return 0, nil
	}
	stamp := dbTime(now)
	tx, err := store.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(upsertGroupMemberSQL)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, row := range rows {
		if err := upsertGroupMember(stmt, groupJID, row, groupMemberSourceRoster, stamp); err != nil {
			_ = stmt.Close()
			return 0, err
		}
		written++
	}
	if err := stmt.Close(); err != nil {
		return 0, err
	}
	// Everything just written carries `stamp`; anything older is a member the
	// roster dropped. Comparing the strings is chronological because dbTime is
	// fixed-width UTC (store_time.go).
	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_jid = ? AND (last_seen IS NULL OR last_seen < ?)`,
		groupJID, stamp); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// addGroupMemberSQL is the join feed's write. It differs from the roster's in
// two places, both because a Join notification carries less information than it
// looks like it does:
//
//   - the admin columns are left alone. The rows built from a Join always say
//     "not an admin", and WhatsApp re-delivers unacked notifications after a
//     reconnect, so applying them would silently demote a member the roster or
//     a Promote had already flagged.
//   - `source` is left alone on an existing row. StaleGroupRosters reads
//     freshness from the source="roster" rows, and WhatsApp replays the whole
//     participant list as Joins when this account is re-added to a group —
//     downgrading every row would make a freshly fetched group read as never
//     rostered.
const addGroupMemberSQL = `
	INSERT INTO group_members
		(group_jid, user, lid, phone, name, is_admin, is_super_admin, first_seen, last_seen, source)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(group_jid, user) DO UPDATE SET
		lid = COALESCE(NULLIF(excluded.lid, ''), group_members.lid),
		phone = COALESCE(NULLIF(excluded.phone, ''), group_members.phone),
		name = COALESCE(NULLIF(excluded.name, ''), group_members.name),
		last_seen = excluded.last_seen`

// AddGroupMembers records participants that joined (events.GroupInfo.Join).
// Unlike the other two write paths this one keys strictly on the primary key:
// a join is new membership, so there is normally nothing to match against. If
// the LID store learns a mapping between a roster fetch and a re-join, the
// member can briefly hold two rows; the next roster replace drops the one the
// roster does not list — the background pass, or a list_group_members call when
// WHATSAPP_GROUP_ROSTER_SYNC_HOURS=0 has turned that pass off.
func (store *MessageStore) AddGroupMembers(groupJID string, rows []groupMemberRow, now time.Time) error {
	if store == nil || store.db == nil || groupJID == "" || len(rows) == 0 {
		return nil
	}
	stamp := dbTime(now)
	stmt, err := store.db.Prepare(addGroupMemberSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, row := range rows {
		if err := upsertGroupMember(stmt, groupJID, row, groupMemberSourceEvent, stamp); err != nil {
			return err
		}
	}
	return nil
}

// DropGroupRoster forgets a group's whole cached membership. Used when this
// account leaves or is removed from the group: from that moment the roster can
// never be refreshed again, and a frozen snapshot would keep answering "yes,
// they are a member" about a group we cannot see.
func (store *MessageStore) DropGroupRoster(groupJID string) error {
	if store == nil || store.db == nil || groupJID == "" {
		return nil
	}
	_, err := store.db.Exec(`DELETE FROM group_members WHERE group_jid = ?`, groupJID)
	return err
}

// memberMatchSQL matches a stored row against any address form of an incoming
// participant. The key alone is not enough: the roster feed learns a phone
// number from the group IQ while the event and message feeds learn it from
// whatsmeow's LID store, and the two can disagree about a member for as long
// as the store has no mapping — so a departure addressed by phone must still
// find a row that was cached under a LID. The guards against the empty string
// make an address form we do not have match nothing rather than everything.
const memberMatchSQL = `(user = ? OR (phone <> '' AND phone = ?) OR (lid <> '' AND lid = ?))`

// groupLeaveClockSkew is how far a row's last_seen may run ahead of a
// departure and still be deleted. leftAt comes from WhatsApp's clock while
// last_seen comes from this host's, so a host running a few seconds fast would
// otherwise refuse every departure that follows a roster fetch closely. A
// re-delivered notification is hours old, well outside this grace.
const groupLeaveClockSkew = 5 * time.Minute

// RemoveGroupMembers drops participants that left (events.GroupInfo.Leave).
// leftAt is when the departure happened; a row confirmed well after that is
// left alone, because WhatsApp re-delivers unacked notifications after a
// reconnect and an old departure must not remove a member a later roster has
// since listed. A row with no last_seen at all predates the stamp by
// definition.
func (store *MessageStore) RemoveGroupMembers(groupJID string, rows []groupMemberRow, leftAt time.Time) error {
	if store == nil || store.db == nil || groupJID == "" || len(rows) == 0 {
		return nil
	}
	stamp := dbTime(leftAt.Add(groupLeaveClockSkew))
	stmt, err := store.db.Prepare(
		`DELETE FROM group_members WHERE group_jid = ? AND ` + memberMatchSQL +
			` AND (last_seen IS NULL OR last_seen <= ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, row := range rows {
		if row.User == "" {
			continue
		}
		if _, err := stmt.Exec(groupJID, row.User, row.Phone, row.LID, stamp); err != nil {
			return err
		}
	}
	return nil
}

// SetGroupMemberAdmin applies a promotion or demotion. A user with no row yet
// is left alone: the next roster refresh brings them in with the right flags,
// and inventing a row from a flag change alone would record a membership we
// have never actually seen.
//
// last_seen is deliberately untouched. It is what StaleGroupRosters measures
// roster freshness from, so bumping it on a flag change would make a group
// with regular admin churn look permanently fresh and starve the background
// refresh — the same trap NoteGroupParticipant avoids.
func (store *MessageStore) SetGroupMemberAdmin(groupJID string, rows []groupMemberRow, isAdmin bool) error {
	if store == nil || store.db == nil || groupJID == "" || len(rows) == 0 {
		return nil
	}
	// A demotion clears super-admin too; a promotion leaves it as it was,
	// because "promoted to admin" says nothing about group ownership.
	stmt, err := store.db.Prepare(
		`UPDATE group_members
		 SET is_admin = ?, is_super_admin = CASE WHEN ? THEN is_super_admin ELSE 0 END
		 WHERE group_jid = ? AND ` + memberMatchSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, row := range rows {
		if row.User == "" {
			continue
		}
		if _, err := stmt.Exec(isAdmin, isAdmin, groupJID, row.User, row.Phone, row.LID); err != nil {
			return err
		}
	}
	return nil
}

// NoteGroupParticipant records a sender seen in a group that has no row for
// them yet. It never updates an existing row: a message says "this account is
// in the group", which a row already says, and bumping last_seen here would
// make a chatty group look freshly rostered and starve runGroupRosterSync.
//
// "Has a row" is decided by memberMatchSQL rather than by the primary key, so
// a member the roster cached under a LID does not gain a second, phone-keyed
// row the first time they post.
func (store *MessageStore) NoteGroupParticipant(groupJID string, row groupMemberRow, now time.Time) error {
	if store == nil || store.db == nil || groupJID == "" || row.User == "" {
		return nil
	}
	stamp := dbTime(now)
	_, err := store.db.Exec(`
		INSERT INTO group_members
			(group_jid, user, lid, phone, name, is_admin, is_super_admin, first_seen, last_seen, source)
		SELECT ?, ?, ?, ?, '', 0, 0, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM group_members WHERE group_jid = ? AND `+memberMatchSQL+`
		)`,
		groupJID, row.User, row.LID, row.Phone, stamp, stamp, groupMemberSourceMessage,
		groupJID, row.User, row.Phone, row.LID)
	return err
}

// StaleGroupRosters lists the group chats whose roster has never been fetched,
// or was last fetched before cutoff, most recently active first so the groups
// an agent is likely to ask about are refreshed first.
//
// It is deliberately unlimited. The caller drops the groups the chat
// allow-list excludes and only then caps how many it fetches; capping here
// instead would let a deployment with more excluded-but-active group chats
// than the batch size hide the allow-listed ones behind them forever. The
// result is one string per group chat in the store — a few hundred at most.
func (store *MessageStore) StaleGroupRosters(cutoff time.Time) ([]string, error) {
	if store == nil || store.db == nil {
		return nil, nil
	}
	rows, err := store.db.Query(`
		SELECT c.jid
		FROM chats c
		LEFT JOIN (
			SELECT group_jid, MAX(last_seen) AS refreshed
			FROM group_members WHERE source = ? GROUP BY group_jid
		) r ON r.group_jid = c.jid
		WHERE c.jid LIKE '%@g.us'
		  AND (r.refreshed IS NULL OR r.refreshed < ?)
		ORDER BY c.last_message_time DESC, c.jid ASC`, groupMemberSourceRoster, dbTime(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		jids = append(jids, jid)
	}
	return jids, rows.Err()
}
