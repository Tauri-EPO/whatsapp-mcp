package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const testGroupJID = "120363000000000001@g.us"

// storedMember is one group_members row, read back for assertions.
type storedMember struct {
	lid, phone, name, source, firstSeen, lastSeen string
	isAdmin, isSuper                              bool
}

func readMembers(t *testing.T, store *MessageStore, groupJID string) map[string]storedMember {
	t.Helper()
	// The timestamps are CAST to TEXT so the driver hands back the stored
	// spelling instead of parsing the TIMESTAMP column into a time.Time.
	rows, err := store.db.Query(
		`SELECT user, COALESCE(lid, ''), COALESCE(phone, ''), COALESCE(name, ''),
		        is_admin, is_super_admin, CAST(first_seen AS TEXT), CAST(last_seen AS TEXT),
		        COALESCE(source, '')
		 FROM group_members WHERE group_jid = ?`, groupJID)
	if err != nil {
		t.Fatalf("query group_members: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]storedMember{}
	for rows.Next() {
		var user string
		var m storedMember
		if err := rows.Scan(&user, &m.lid, &m.phone, &m.name, &m.isAdmin, &m.isSuper, &m.firstSeen, &m.lastSeen, &m.source); err != nil {
			t.Fatalf("scan group_members: %v", err)
		}
		out[user] = m
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate group_members: %v", err)
	}
	return out
}

func TestReplaceGroupRosterUpsertsAndDropsMissing(t *testing.T) {
	store := newTestMessageStore(t)
	first := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999", Name: "Enrico", IsAdmin: true, IsSuperAdmin: true},
		{JID: "777@lid", PhoneNumber: "5511888888888", LID: "777@lid", Name: "Ana"},
		{JID: "888@lid", LID: "888@lid"},
	}), first); err != nil {
		t.Fatalf("first roster: %v", err)
	}

	members := readMembers(t, store, testGroupJID)
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
	// The phone user part is the key when there is one, so a member keeps the
	// same spelling messages.sender uses; a LID-only member keys on the LID.
	ana, ok := members["5511888888888"]
	if !ok {
		t.Fatalf("ana keyed by phone missing: %+v", members)
	}
	if ana.lid != "777" || ana.phone != "5511888888888" || ana.name != "Ana" {
		t.Fatalf("ana = %+v", ana)
	}
	if _, ok := members["888"]; !ok {
		t.Fatalf("LID-only member missing: %+v", members)
	}
	if !members["5511999999999"].isSuper || members["5511999999999"].source != groupMemberSourceRoster {
		t.Fatalf("owner = %+v", members["5511999999999"])
	}

	// Second roster: Ana lost her admin flag and the LID-only member left.
	second := first.Add(time.Hour)
	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999", Name: "Enrico", IsAdmin: true, IsSuperAdmin: true},
		{JID: "777@lid", PhoneNumber: "5511888888888", LID: "777@lid", IsAdmin: true},
	}), second); err != nil {
		t.Fatalf("second roster: %v", err)
	}

	members = readMembers(t, store, testGroupJID)
	if len(members) != 2 {
		t.Fatalf("expected the departed member to be dropped, got %+v", members)
	}
	ana = members["5511888888888"]
	if !ana.isAdmin {
		t.Fatalf("ana should be admin now: %+v", ana)
	}
	if ana.name != "Ana" {
		t.Fatalf("a roster without a name must not erase the stored one: %+v", ana)
	}
	if ana.firstSeen != dbTime(first) || ana.lastSeen != dbTime(second) {
		t.Fatalf("first_seen must survive a refresh, last_seen must move: %+v", ana)
	}
}

func TestReplaceGroupRosterIgnoresEmptyRoster(t *testing.T) {
	store := newTestMessageStore(t)
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999"},
	}), now); err != nil {
		t.Fatalf("seed roster: %v", err)
	}
	if _, err := store.ReplaceGroupRoster(testGroupJID, nil, now.Add(time.Hour)); err != nil {
		t.Fatalf("empty roster: %v", err)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 1 {
		t.Fatalf("an empty roster must not wipe the cached membership, got %d rows", got)
	}
}

func TestApplyGroupParticipantChanges(t *testing.T) {
	store := newTestMessageStore(t)
	client := newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{
		{User: "777", Server: types.HiddenUserServer}: {User: "5511888888888", Server: types.DefaultUserServer},
	}})
	b := testBridge(t, client, store, testLogger())
	// Event rows are stamped with the local clock (see applyGroupParticipantChanges),
	// so the event timestamps have to sit around it for the Leave ordering guard.
	now := time.Now()

	// Join: a LID the store can map is recorded under its phone user part,
	// with both address forms filled in.
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: now,
		Join: []types.JID{
			{User: "777", Server: types.HiddenUserServer},
			{User: "5511777777777", Server: types.DefaultUserServer},
		},
	})
	members := readMembers(t, store, testGroupJID)
	joined, ok := members["5511888888888"]
	if !ok {
		t.Fatalf("LID join not resolved to its phone: %+v", members)
	}
	if joined.lid != "777" || joined.phone != "5511888888888" || joined.source != groupMemberSourceEvent {
		t.Fatalf("joined = %+v", joined)
	}
	if _, ok := members["5511777777777"]; !ok {
		t.Fatalf("phone join missing: %+v", members)
	}

	// Promote then demote the same member; a demotion also clears super-admin.
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: now.Add(time.Minute),
		Promote:   []types.JID{{User: "777", Server: types.HiddenUserServer}},
	})
	promoted := readMembers(t, store, testGroupJID)["5511888888888"]
	if !promoted.isAdmin {
		t.Fatalf("promotion not applied")
	}
	// A flag change is not a roster refresh: last_seen must not move, or a
	// group with admin churn would never look stale to StaleGroupRosters.
	if promoted.lastSeen != joined.lastSeen {
		t.Fatalf("promotion moved last_seen: %q -> %q", joined.lastSeen, promoted.lastSeen)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: now.Add(2 * time.Minute),
		Demote:    []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	if readMembers(t, store, testGroupJID)["5511888888888"].isAdmin {
		t.Fatalf("demotion not applied")
	}

	// Leave removes the row.
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: now.Add(3 * time.Minute),
		Leave:     []types.JID{{User: "777", Server: types.HiddenUserServer}},
	})
	if _, ok := readMembers(t, store, testGroupJID)["5511888888888"]; ok {
		t.Fatalf("departure not applied")
	}
}

// WhatsApp re-delivers unacked notifications after a reconnect. A departure
// from before the last roster refresh is stale news: the roster has since
// listed the member, so the delete must not fire.
func TestReplayedLeaveDoesNotDropAConfirmedMember(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	rosterAt := time.Now()

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), rosterAt); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: rosterAt.Add(-time.Hour),
		Leave:     []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	if _, ok := readMembers(t, store, testGroupJID)["5511888888888"]; !ok {
		t.Fatalf("a replayed departure dropped a member the roster had just confirmed")
	}

	// A departure that really is newer than the roster still applies.
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: rosterAt.Add(time.Hour),
		Leave:     []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	if _, ok := readMembers(t, store, testGroupJID)["5511888888888"]; ok {
		t.Fatalf("a current departure was not applied")
	}
}

// The allow-list decides which conversations this deployment may hold at all,
// so no feed may cache a roster row for an excluded group.
func TestGroupMembershipFeedsRespectTheAllowList(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.Policy = parseChatPolicy("120363000000000009@g.us")

	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: time.Now(),
		Join:      []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	b.noteGroupSender(testGroupJID, types.JID{User: "5511777777777", Server: types.DefaultUserServer}, time.Now())

	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("an excluded group gained %d cached member(s)", got)
	}
}

func TestResolveGroupRosterSync(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", groupRosterSyncInterval, false},
		{" 12 ", 12 * time.Hour, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"six", 0, true},
	}
	for _, tc := range cases {
		got, err := resolveGroupRosterSync(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("resolveGroupRosterSync(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Fatalf("resolveGroupRosterSync(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if s := groupRosterSyncSummary(0); s != "off" {
		t.Fatalf("summary(0) = %q", s)
	}
	if s := groupRosterSyncSummary(6 * time.Hour); s != "every 6 h" {
		t.Fatalf("summary(6h) = %q", s)
	}
}

// WhatsApp re-delivers unacked notifications after a reconnect, so a Join the
// bridge already applied can arrive twice. The rows it builds always say "not
// an admin", so applying that would silently demote a member the roster or a
// Promote had flagged.
func TestReplayedJoinKeepsAdminFlags(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	group := types.JID{User: "120363000000000001", Server: types.GroupServer}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999", IsAdmin: true, IsSuperAdmin: true},
	}), now); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       group,
		Timestamp: now.Add(time.Minute),
		Join:      []types.JID{{User: "5511999999999", Server: types.DefaultUserServer}},
	})
	member := readMembers(t, store, testGroupJID)["5511999999999"]
	if !member.isAdmin || !member.isSuper {
		t.Fatalf("a replayed join cleared the admin flags: %+v", member)
	}
}

// Once this account leaves, the roster can never be refreshed again, so a
// frozen snapshot would keep answering "yes, they are a member".
func TestOwnDepartureDropsTheWholeRoster(t *testing.T) {
	store := newTestMessageStore(t)
	self := types.JID{User: "5511999999999", Server: types.DefaultUserServer}
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), store, testLogger())
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999"},
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), now); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: now.Add(time.Minute),
		Leave:     []types.JID{self},
	})
	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("leaving the group left %d cached member(s)", got)
	}
}

func TestSetGroupMemberAdminIgnoresUnknownUser(t *testing.T) {
	store := newTestMessageStore(t)
	row, _ := newGroupMemberRow("", "5511999999999", "")
	if err := store.SetGroupMemberAdmin(testGroupJID, []groupMemberRow{row}, true); err != nil {
		t.Fatalf("promote unknown: %v", err)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("a flag change must not invent a membership, got %d rows", got)
	}
}

// A member cached under a LID must still be found when an event, a message or
// a later roster addresses them by phone (and the other way round): the roster
// IQ and whatsmeow's LID store do not always agree on which forms are known.
func TestGroupMemberRowsMatchOnAnyAddressForm(t *testing.T) {
	store := newTestMessageStore(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	// The roster knew only the LID, so the row is keyed on it.
	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "777@lid", LID: "777@lid", Name: "Ana"},
	}), now); err != nil {
		t.Fatalf("roster: %v", err)
	}
	if _, ok := readMembers(t, store, testGroupJID)["777"]; !ok {
		t.Fatalf("expected a LID-keyed row")
	}

	// A message whose sender the LID store now resolves must not add a second row.
	byPhone, _ := newGroupMemberRow("", "5511888888888", "777@lid")
	if err := store.NoteGroupParticipant(testGroupJID, byPhone, now.Add(time.Hour)); err != nil {
		t.Fatalf("note: %v", err)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 1 {
		t.Fatalf("a resolved sender duplicated the LID-keyed row: %+v", readMembers(t, store, testGroupJID))
	}

	// A promotion addressed by phone must reach the LID-keyed row...
	if err := store.SetGroupMemberAdmin(testGroupJID, []groupMemberRow{byPhone}, true); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !readMembers(t, store, testGroupJID)["777"].isAdmin {
		t.Fatalf("promotion by phone did not reach the LID-keyed row")
	}
	// ...and so must a departure.
	if err := store.RemoveGroupMembers(testGroupJID, []groupMemberRow{byPhone}, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("departure by phone left %d rows", got)
	}
}

// The roster feed resolves LID-only participants through the same LID store
// the event and message feeds use, so every feed keys a member the same way.
func TestStoreGroupRosterResolvesLIDOnlyMembers(t *testing.T) {
	store := newTestMessageStore(t)
	client := newTestClient(&mockLIDStore{pnByLID: map[types.JID]types.JID{
		{User: "888", Server: types.HiddenUserServer}: {User: "5511777777777", Server: types.DefaultUserServer},
	}})
	b := testBridge(t, client, store, testLogger())
	if _, err := b.storeGroupRoster(testGroupJID, buildGroupMembers(fakeGroup(), nil, nil).Members, time.Now()); err != nil {
		t.Fatalf("store roster: %v", err)
	}
	members := readMembers(t, store, testGroupJID)
	anon, ok := members["5511777777777"]
	if !ok {
		t.Fatalf("LID-only member not re-keyed on its phone: %+v", members)
	}
	if anon.lid != "888" || anon.phone != "5511777777777" {
		t.Fatalf("anon = %+v", anon)
	}
}

func TestNoteGroupParticipantOnlyFillsGaps(t *testing.T) {
	store := newTestMessageStore(t)
	first := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)

	row, ok := newGroupMemberRow("", "5511888888888", "777@lid")
	if !ok {
		t.Fatalf("row not built")
	}
	if err := store.NoteGroupParticipant(testGroupJID, row, first); err != nil {
		t.Fatalf("note participant: %v", err)
	}
	noted := readMembers(t, store, testGroupJID)["5511888888888"]
	if noted.source != groupMemberSourceMessage || noted.lid != "777" {
		t.Fatalf("noted = %+v", noted)
	}

	// A roster row must win, and a later message must not move last_seen —
	// that stamp is what StaleGroupRosters measures roster freshness from.
	rosterAt := first.Add(time.Hour)
	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "777@lid", PhoneNumber: "5511888888888", LID: "777@lid", Name: "Ana", IsAdmin: true},
	}), rosterAt); err != nil {
		t.Fatalf("roster: %v", err)
	}
	if err := store.NoteGroupParticipant(testGroupJID, row, rosterAt.Add(time.Hour)); err != nil {
		t.Fatalf("second note: %v", err)
	}
	after := readMembers(t, store, testGroupJID)["5511888888888"]
	if after.source != groupMemberSourceRoster || !after.isAdmin || after.name != "Ana" {
		t.Fatalf("a message must not downgrade a roster row: %+v", after)
	}
	if after.lastSeen != dbTime(rosterAt) {
		t.Fatalf("last_seen moved on a message: %+v", after)
	}
	if after.firstSeen != dbTime(first) {
		t.Fatalf("first_seen should date from the message: %+v", after)
	}
}

func TestHandleMessageNotesGroupSender(t *testing.T) {
	store := newTestMessageStore(t)
	client := newTestClient(&mockLIDStore{})
	b := testBridge(t, client, store, testLogger())

	group := types.JID{User: "120363000000000001", Server: types.GroupServer}
	sender := types.JID{User: "5511888888888", Server: types.DefaultUserServer}
	msg := buildTextMessage(group, sender, types.EmptyJID, types.EmptyJID, false, "oi")
	msg.Info.IsGroup = true
	b.handleMessage(msg)

	if _, ok := readMembers(t, store, testGroupJID)["5511888888888"]; !ok {
		t.Fatalf("a group message should record its sender as a member")
	}

	// A DM must not create a membership row anywhere.
	dm := buildTextMessage(sender, sender, types.EmptyJID, types.EmptyJID, false, "oi")
	dm.Info.ID = "test-msg-002"
	b.handleMessage(dm)
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM group_members`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected only the group row, got %d", rows)
	}
}

func TestStaleGroupRosters(t *testing.T) {
	store := newTestMessageStore(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	seed := func(jid string, lastMessage time.Time) {
		if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
			jid, jid, dbTime(lastMessage)); err != nil {
			t.Fatalf("seed chat: %v", err)
		}
	}
	seed(testGroupJID, now.Add(-time.Minute))
	seed("120363000000000002@g.us", now.Add(-time.Hour))
	seed("5511999999999@s.whatsapp.net", now)

	// Never fetched: both groups are stale, most recently active first, and
	// the DM is not a group.
	stale, err := store.StaleGroupRosters(now.Add(-6 * time.Hour))
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if len(stale) != 2 || stale[0] != testGroupJID {
		t.Fatalf("stale = %v", stale)
	}

	// A fresh roster takes its group out of the list...
	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511999999999@s.whatsapp.net", PhoneNumber: "5511999999999"},
	}), now); err != nil {
		t.Fatalf("roster: %v", err)
	}
	stale, err = store.StaleGroupRosters(now.Add(-6 * time.Hour))
	if err != nil {
		t.Fatalf("stale after roster: %v", err)
	}
	if len(stale) != 1 || stale[0] != "120363000000000002@g.us" {
		t.Fatalf("stale after roster = %v", stale)
	}

	// ...but a message-derived row does not: nobody has seen the whole group.
	row, _ := newGroupMemberRow("", "5511777777777", "")
	if err := store.NoteGroupParticipant("120363000000000002@g.us", row, now); err != nil {
		t.Fatalf("note: %v", err)
	}
	stale, err = store.StaleGroupRosters(now.Add(-6 * time.Hour))
	if err != nil {
		t.Fatalf("stale after note: %v", err)
	}
	if len(stale) != 1 || stale[0] != "120363000000000002@g.us" {
		t.Fatalf("a message row must not count as a roster refresh: %v", stale)
	}
}

func TestSyncGroupRosters(t *testing.T) {
	store := newTestMessageStore(t)
	for _, jid := range []string{testGroupJID, "120363000000000002@g.us"} {
		if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
			jid, jid, dbTime(time.Now())); err != nil {
			t.Fatalf("seed chat: %v", err)
		}
	}

	var fetched []string
	store.groupInfo = func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		fetched = append(fetched, jid.String())
		if jid.User == "120363000000000002" {
			return nil, errors.New("not a member any more")
		}
		return fakeGroup(), nil
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.GroupRosterSync = groupRosterSyncInterval
	b.Connected = func() bool { return true }

	// pace 0: the test must not sleep. The failing group is fetched but not
	// counted, and it stays stale so the next pass retries it.
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 1 {
		t.Fatalf("refreshed = %d (fetched %v)", n, fetched)
	}
	if len(fetched) != 2 {
		t.Fatalf("fetched = %v", fetched)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 3 {
		t.Fatalf("roster not stored, %d rows", got)
	}

	// Second pass: the refreshed group is no longer stale.
	fetched = nil
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 0 {
		t.Fatalf("second pass refreshed = %d", n)
	}
	if len(fetched) != 1 || fetched[0] != "120363000000000002@g.us" {
		t.Fatalf("second pass fetched = %v", fetched)
	}

	// The pace applies to failed fetches too, so a store full of groups the
	// account was removed from cannot turn into a burst of IQs.
	started := time.Now()
	b.syncGroupRosters(context.Background(), time.Now(), 10, 20*time.Millisecond)
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("a failing fetch skipped the pace: %s", elapsed)
	}
}

func TestSyncGroupRostersRespectsPolicyAndConnection(t *testing.T) {
	store := newTestMessageStore(t)
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
		testGroupJID, "Obra", dbTime(time.Now())); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	fetches := 0
	store.groupInfo = func(_ context.Context, _ types.JID) (*types.GroupInfo, error) {
		fetches++
		return fakeGroup(), nil
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.GroupRosterSync = groupRosterSyncInterval
	b.Connected = func() bool { return true }

	// A group outside WHATSAPP_ALLOWED_CHATS is never fetched.
	b.Policy = parseChatPolicy("5511999999999")
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 0 || fetches != 0 {
		t.Fatalf("policy ignored: refreshed=%d fetches=%d", n, fetches)
	}

	// And an excluded group does not consume the batch: with a budget of one,
	// the allow-listed group behind it must still be reached. Excluded groups
	// can never gain a roster row, so they stay stale on every pass.
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
		"120363000000000002@g.us", "Allowed", dbTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("seed allowed chat: %v", err)
	}
	b.Policy = parseChatPolicy("120363000000000002@g.us")
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 1, 0); n != 1 || fetches != 1 {
		t.Fatalf("excluded group ate the batch: refreshed=%d fetches=%d", n, fetches)
	}
	fetches = 0

	// Neither is anything while the WhatsApp socket is down.
	b.Policy = parseChatPolicy("")
	b.Connected = func() bool { return false }
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 0 || fetches != 0 {
		t.Fatalf("disconnected sync ran: refreshed=%d fetches=%d", n, fetches)
	}

	// A cancelled context stops before the first fetch.
	b.Connected = func() bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, _ := b.syncGroupRosters(ctx, time.Now(), 10, 0); n != 0 || fetches != 0 {
		t.Fatalf("cancelled sync ran: refreshed=%d fetches=%d", n, fetches)
	}

	// And disabling the pass is a Bridge field, not an env var.
	b.GroupRosterSync = 0
	if n, _ := b.syncGroupRosters(context.Background(), time.Now(), 10, 0); n != 0 || fetches != 0 {
		t.Fatalf("disabled sync ran: refreshed=%d fetches=%d", n, fetches)
	}
}

func TestHandleGroupMembersCachesRoster(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	fetch := func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		if jid.User == "404" {
			return nil, errors.New("not a member")
		}
		return fakeGroup(), nil
	}
	h := handleGroupMembers(fetch, nil, nil, parseChatPolicy("*@g.us"), b.recordGroupRoster)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/group/members?jid="+testGroupJID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp GroupMembersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Members) != 3 {
		t.Fatalf("members = %d", len(resp.Members))
	}
	if _, err := parseDBTime(resp.FetchedAt); err != nil {
		t.Fatalf("fetched_at = %q: %v", resp.FetchedAt, err)
	}
	if got := len(readMembers(t, store, testGroupJID)); got != 3 {
		t.Fatalf("roster not cached, %d rows", got)
	}

	// A failed fetch caches nothing.
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/group/members?jid=404@g.us", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("error status = %d", rec.Code)
	}
	if got := len(readMembers(t, store, "404@g.us")); got != 0 {
		t.Fatalf("failed fetch cached %d rows", got)
	}

	// Neither does a request the allow-list denies.
	denied := handleGroupMembers(fetch, nil, nil, parseChatPolicy("5511999999999"), b.recordGroupRoster)
	rec = httptest.NewRecorder()
	denied(rec, httptest.NewRequest(http.MethodGet, "/api/group/members?jid=120363000000000009@g.us", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("policy status = %d", rec.Code)
	}
	if got := len(readMembers(t, store, "120363000000000009@g.us")); got != 0 {
		t.Fatalf("denied request cached %d rows", got)
	}
}

// A group whose fetch keeps failing (the account was removed, but the chat row
// and its LIKE-matching JID stay) must not consume a slot of every batch, and a
// pass that stops at the cap must say so — the loop uses that to come back soon
// instead of sleeping a whole interval and re-stamping the same groups.
func TestSyncGroupRostersBacksOffFailuresAndReportsLeftovers(t *testing.T) {
	store := newTestMessageStore(t)
	for _, jid := range []string{"120363000000000001@g.us", "120363000000000002@g.us"} {
		if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
			jid, jid, dbTime(time.Now())); err != nil {
			t.Fatalf("seed chat: %v", err)
		}
	}
	var fetched []string
	store.groupInfo = func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		fetched = append(fetched, jid.String())
		if jid.User == "120363000000000001" {
			return nil, errors.New("not a member any more")
		}
		return fakeGroup(), nil
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.GroupRosterSync = groupRosterSyncInterval
	b.Connected = func() bool { return true }
	b.rosterFailures = newRosterFailures()

	now := time.Now()
	// Budget of one: the first group fails and spends it, so the second is
	// left over.
	refreshed, more := b.syncGroupRosters(context.Background(), now, 1, 0)
	if refreshed != 0 || !more {
		t.Fatalf("first pass: refreshed=%d more=%v (fetched %v)", refreshed, more, fetched)
	}

	// Second pass: the failure is remembered, so the budget goes to the group
	// that can actually be refreshed and nothing is left over.
	fetched = nil
	refreshed, more = b.syncGroupRosters(context.Background(), now.Add(time.Minute), 1, 0)
	if refreshed != 1 || more {
		t.Fatalf("second pass: refreshed=%d more=%v (fetched %v)", refreshed, more, fetched)
	}
	if len(fetched) != 1 || fetched[0] != "120363000000000002@g.us" {
		t.Fatalf("second pass fetched = %v", fetched)
	}

	// The backoff expires after one interval.
	fetched = nil
	if _, _ = b.syncGroupRosters(context.Background(), now.Add(2*groupRosterSyncInterval), 5, 0); len(fetched) == 0 {
		t.Fatalf("the failing group was never retried")
	}
}

// A response with no usable participant writes no source="roster" row, so the
// group stays stale forever; it has to be backed off like any other failure
// rather than reported as refreshed.
func TestEmptyRosterResponseCountsAsAFailure(t *testing.T) {
	store := newTestMessageStore(t)
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`,
		testGroupJID, "Obra", dbTime(time.Now())); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	fetches := 0
	store.groupInfo = func(_ context.Context, _ types.JID) (*types.GroupInfo, error) {
		fetches++
		return &types.GroupInfo{JID: types.JID{User: "120363000000000001", Server: types.GroupServer}}, nil
	}
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	b.GroupRosterSync = groupRosterSyncInterval
	b.Connected = func() bool { return true }
	b.rosterFailures = newRosterFailures()

	now := time.Now()
	if refreshed, _ := b.syncGroupRosters(context.Background(), now, 10, 0); refreshed != 0 {
		t.Fatalf("an empty roster was reported as refreshed")
	}
	// Backed off: the next pass does not spend a fetch on it.
	if _, _ = b.syncGroupRosters(context.Background(), now.Add(time.Minute), 10, 0); fetches != 1 {
		t.Fatalf("the empty-roster group was refetched: fetches=%d", fetches)
	}
}

// WhatsApp replays the whole participant list as Joins when this account is
// re-added to a group. Downgrading source to "event" would make a group that
// was just fetched read as never rostered.
func TestJoinDoesNotDowngradeARosterRow(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	at := time.Now()

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), at); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: at,
		Join:      []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	if got := readMembers(t, store, testGroupJID)["5511888888888"].source; got != groupMemberSourceRoster {
		t.Fatalf("source downgraded to %q", got)
	}
	stale, err := store.StaleGroupRosters(at.Add(-time.Hour))
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("the group reads as never rostered: %v", stale)
	}
}

// The departure stamp comes from WhatsApp's clock and last_seen from this
// host's, so a host running slightly fast must not refuse a real departure.
func TestDepartureToleratesSmallClockSkew(t *testing.T) {
	store := newTestMessageStore(t)
	b := testBridge(t, newTestClient(&mockLIDStore{}), store, testLogger())
	at := time.Now()

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), at); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: at.Add(-3 * time.Second), // server clock a hair behind ours
		Leave:     []types.JID{{User: "5511888888888", Server: types.DefaultUserServer}},
	})
	if _, ok := readMembers(t, store, testGroupJID)["5511888888888"]; ok {
		t.Fatalf("a few seconds of clock skew blocked a real departure")
	}
}

// Messages queued for a group are still delivered after we are removed from
// it. The message feed must not put rows back that can never be reconciled.
func TestMessagesAfterOurDepartureDoNotResurrectTheRoster(t *testing.T) {
	store := newTestMessageStore(t)
	self := types.JID{User: "5511999999999", Server: types.DefaultUserServer}
	b := testBridge(t, newTestClientWithSelf(&mockLIDStore{}, self), store, testLogger())
	b.rosterFailures = newRosterFailures()
	at := time.Now()

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), at); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: at,
		Leave:     []types.JID{self},
	})
	b.noteGroupSender(testGroupJID, types.JID{User: "5511888888888", Server: types.DefaultUserServer}, at.Add(time.Second))

	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("a late message resurrected %d row(s) of a group we left", got)
	}
}

// The removal may be addressed to our own LID rather than our phone JID.
func TestOwnDepartureRecognisedByLID(t *testing.T) {
	store := newTestMessageStore(t)
	self := types.JID{User: "5511999999999", Server: types.DefaultUserServer}
	client := newTestClientWithSelf(&mockLIDStore{}, self)
	client.Store.LID = types.JID{User: "999", Server: types.HiddenUserServer}
	b := testBridge(t, client, store, testLogger())
	at := time.Now()

	if _, err := store.ReplaceGroupRoster(testGroupJID, groupMemberRows([]GroupMember{
		{JID: "5511888888888@s.whatsapp.net", PhoneNumber: "5511888888888"},
	}), at); err != nil {
		t.Fatalf("roster: %v", err)
	}
	b.applyGroupParticipantChanges(&events.GroupInfo{
		JID:       types.JID{User: "120363000000000001", Server: types.GroupServer},
		Timestamp: at,
		Leave:     []types.JID{{User: "999", Server: types.HiddenUserServer}},
	})
	if got := len(readMembers(t, store, testGroupJID)); got != 0 {
		t.Fatalf("a departure addressed to our LID was read as somebody else's: %d row(s) left", got)
	}
}
