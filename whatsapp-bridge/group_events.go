package main

// The two feeds that keep group_members fresh without anyone asking:
// participant changes pushed by WhatsApp (events.GroupInfo) and a paced
// background refresh of the rosters nobody has fetched lately.
//
// Both write through the store methods in group_members_store.go; neither is
// required for correctness, they only shorten how long a membership answer can
// be stale. See that file's header for the whole picture.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// groupRosterSyncEnv sets how stale a cached roster may get, in hours.
// 0 turns the background pass off; the table then only grows from
// /api/group/members, group events and group messages.
const groupRosterSyncEnv = "WHATSAPP_GROUP_ROSTER_SYNC_HOURS"

const (
	// groupRosterSyncInterval: the default for groupRosterSyncEnv.
	groupRosterSyncInterval = 6 * time.Hour
	// groupRosterSyncStartDelay: give the connection (and a pair-time history
	// sync) time to settle before the first pass fetches anything.
	groupRosterSyncStartDelay = 2 * time.Minute
	// groupRosterSyncRetryDelay: how soon to look again when a pass found the
	// WhatsApp socket down (still pairing, or reconnecting), or stopped at the
	// batch cap with stale groups left over.
	groupRosterSyncRetryDelay = 5 * time.Minute
	// groupRosterSyncPace: at most one GetGroupInfo per second, so a store
	// with hundreds of groups never looks like a burst to WhatsApp.
	groupRosterSyncPace = time.Second
	// groupRosterSyncBatch caps one pass; at the pace above, a full pass over
	// this many groups takes a few minutes, after which the loop sleeps for a
	// whole interval.
	groupRosterSyncBatch = 200
	// groupRosterFetchTimeout bounds one GetGroupInfo round trip.
	groupRosterFetchTimeout = 30 * time.Second
	// groupRosterRetryAfter is how long a group whose roster could not be
	// fetched (or was dropped because we left it) is left alone: it neither
	// consumes a slot of the sync batch nor accepts message-derived rows.
	groupRosterRetryAfter = 6 * time.Hour
)

// resolveGroupRosterSync parses groupRosterSyncEnv. Zero disables the pass;
// a negative or non-numeric value is an error so main() fails fast rather than
// silently running with a default the operator did not write.
func resolveGroupRosterSync(value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return groupRosterSyncInterval, nil
	}
	hours, err := strconv.Atoi(v)
	if err != nil || hours < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a non-negative number of hours (0 disables)", groupRosterSyncEnv, value)
	}
	return time.Duration(hours) * time.Hour, nil
}

// groupRosterSyncSummary renders the setting for the startup log.
func groupRosterSyncSummary(interval time.Duration) string {
	if interval <= 0 {
		return "off"
	}
	return fmt.Sprintf("every %d h", int(interval.Hours()))
}

// rosterFailures remembers the groups whose last refresh failed. Without it a
// group this account was removed from — its chat row stays, and its cached
// roster was dropped, so it is permanently "stale" and sorts first — would
// consume a slot of every batch forever and push real work past the cap.
type rosterFailures struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newRosterFailures() *rosterFailures { return &rosterFailures{last: map[string]time.Time{}} }

// recent reports whether jid failed within the last `within`.
func (f *rosterFailures) recent(jid string, now time.Time, within time.Duration) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.last[jid]
	return ok && now.Sub(at) < within
}

func (f *rosterFailures) note(jid string, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[jid] = now
}

func (f *rosterFailures) forget(jid string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.last, jid)
}

// storeGroupRoster caches one freshly fetched participant list and reports how
// many rows it wrote. `at` must be read *before* the fetch: it is the stamp
// ReplaceGroupRoster sweeps with, so a member who joined while the request was
// in flight (stored by the event feed with a later stamp) survives instead of
// being deleted by a snapshot taken before they arrived.
func (b *Bridge) storeGroupRoster(groupJID string, members []GroupMember, at time.Time) (int, error) {
	if b.Store == nil {
		return 0, nil
	}
	return b.Store.ReplaceGroupRoster(groupJID, b.resolveRosterRows(groupMemberRows(members)), at)
}

// resolveRosterRows re-keys the members the group IQ addressed by LID only.
// The IQ's PhoneNumber attribute is often absent for such participants, while
// the event and message feeds resolve them through whatsmeow's LID store — so
// without this the same person is keyed two different ways depending on which
// feed saw them first, and get_contact_chats counts them twice.
func (b *Bridge) resolveRosterRows(rows []groupMemberRow) []groupMemberRow {
	for i := range rows {
		if rows[i].Phone != "" || rows[i].LID == "" {
			continue
		}
		lid := types.JID{User: rows[i].LID, Server: types.HiddenUserServer}
		if pn := resolveUserJID(b.Client, lid, types.EmptyJID); pn.Server == types.DefaultUserServer {
			rows[i].Phone, rows[i].User = pn.User, pn.User
		}
	}
	return rows
}

// recordGroupRoster is the rosterRecorder /api/group/members is wired to
// (rest.go): caching the roster must never change what the caller gets back,
// so a failure is a log line and nothing more.
func (b *Bridge) recordGroupRoster(groupJID string, members []GroupMember, at time.Time) {
	if _, err := b.storeGroupRoster(groupJID, members, at); err != nil {
		b.Log.Warnf("Failed to cache the roster of %s: %v", groupJID, err)
	}
}

// groupMemberRowFromJID turns a participant JID from a group event into a table
// row, filling both address forms when the LID store knows the mapping.
func groupMemberRowFromJID(client *whatsmeow.Client, jid types.JID) (groupMemberRow, bool) {
	jid = jid.ToNonAD()
	resolved := resolveUserJID(client, jid, types.EmptyJID)
	phone, lid := "", ""
	for _, form := range []types.JID{jid, resolved} {
		switch form.Server {
		case types.DefaultUserServer:
			phone = form.User
		case types.HiddenUserServer:
			lid = form.User
		}
	}
	return newGroupMemberRow(resolved.User, phone, lid)
}

// groupMemberEventRows maps the JIDs of one event list to table rows.
func groupMemberEventRows(client *whatsmeow.Client, jids []types.JID) []groupMemberRow {
	rows := make([]groupMemberRow, 0, len(jids))
	for _, jid := range jids {
		if row, ok := groupMemberRowFromJID(client, jid); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

// applyGroupParticipantChanges records the Join/Leave/Promote/Demote lists of
// an events.GroupInfo. Every failure is a warning: a missed delta costs
// freshness until the next roster refresh, never a stored message.
func (b *Bridge) applyGroupParticipantChanges(v *events.GroupInfo) {
	group := v.JID.String()
	// A group the operator excluded gets no cached membership at all, on any
	// feed: the allow-list is about which conversations this deployment may
	// hold, not only which ones it may write to.
	if b.Store == nil || !b.Policy.Allows(group) {
		return
	}
	// Stamped with the local clock, not v.Timestamp. last_seen means "we
	// confirmed this membership then", and a notification that spent a minute
	// in transit would otherwise land *behind* a roster fetched in the
	// meantime — whose DELETE drops every row older than its own stamp, taking
	// the member who just joined with it.
	now := time.Now()

	if len(v.Join) > 0 {
		if err := b.Store.AddGroupMembers(group, groupMemberEventRows(b.Client, v.Join), now); err != nil {
			b.Log.Warnf("Failed to record joins in %s: %v", group, err)
		}
	}
	if len(v.Leave) > 0 {
		// Our own departure ends the group as a source of truth: the roster can
		// never be refreshed again, so keeping the snapshot would answer "yes,
		// they are a member" about a group we can no longer see.
		if b.leftGroup(v.Leave) {
			if err := b.Store.DropGroupRoster(group); err != nil {
				b.Log.Warnf("Failed to drop the cached roster of %s: %v", group, err)
			}
			// Messages queued for the group before the removal are still
			// delivered afterwards; without this the message feed would put
			// rows straight back, and they could never be reconciled because
			// GetGroupInfo now fails for the group. Recording the drop as a
			// failure blocks both that feed and the next sync batch.
			b.rosterFailures.note(group, now)
			return
		}
		// v.Timestamp, not `now`: WhatsApp re-delivers unacked notifications
		// after a reconnect, and a departure from before the last roster
		// refresh is stale news about a member the roster has since confirmed.
		// RemoveGroupMembers refuses to delete rows newer than this.
		leftAt := v.Timestamp
		if leftAt.IsZero() {
			leftAt = now
		}
		if err := b.Store.RemoveGroupMembers(group, groupMemberEventRows(b.Client, v.Leave), leftAt); err != nil {
			b.Log.Warnf("Failed to record departures from %s: %v", group, err)
		}
	}
	if len(v.Promote) > 0 {
		if err := b.Store.SetGroupMemberAdmin(group, groupMemberEventRows(b.Client, v.Promote), true); err != nil {
			b.Log.Warnf("Failed to record promotions in %s: %v", group, err)
		}
	}
	if len(v.Demote) > 0 {
		if err := b.Store.SetGroupMemberAdmin(group, groupMemberEventRows(b.Client, v.Demote), false); err != nil {
			b.Log.Warnf("Failed to record demotions in %s: %v", group, err)
		}
	}
}

// leftGroup reports whether a departure list includes this account. Our phone
// JID, our own LID and the LID store's mapping are all checked, because
// WhatsApp may address the removal by either form.
//
// If none of the three recognises it — an unmapped LID we have never been told
// is ours — the departure is treated as somebody else's and the roster is kept.
// Nothing else drops it: a failing refresh only backs the group off. That stale
// roster then keeps answering "yes, they are a member" of a group we can no
// longer see, until the group is deleted from the archive by hand.
func (b *Bridge) leftGroup(leaving []types.JID) bool {
	if b.Client == nil || b.Client.Store == nil {
		return false
	}
	selves := map[string]struct{}{}
	if id := b.Client.Store.ID; id != nil {
		selves[id.ToNonAD().User] = struct{}{}
	}
	if lid := b.Client.Store.LID; !lid.IsEmpty() {
		selves[lid.ToNonAD().User] = struct{}{}
	}
	if len(selves) == 0 {
		return false
	}
	for _, jid := range leaving {
		if _, ok := selves[jid.ToNonAD().User]; ok {
			return true
		}
		if _, ok := selves[resolveUserJID(b.Client, jid, types.EmptyJID).User]; ok {
			return true
		}
	}
	return false
}

// noteGroupSender records the sender of a group message when group_members has
// no row for them yet, so a group nobody has ever fetched still answers "who
// talks here". Cheap: one guarded INSERT per group message.
//
// Groups whose roster we recently failed to fetch — including the one we were
// just removed from — are skipped: a row written for such a group can never be
// reconciled against a real roster again.
func (b *Bridge) noteGroupSender(chatJID string, sender types.JID, now time.Time) {
	if b.Store == nil || !isGroupJID(chatJID) || !b.Policy.Allows(chatJID) {
		return
	}
	if b.rosterFailures.recent(chatJID, now, groupRosterRetryAfter) {
		return
	}
	row, ok := groupMemberRowFromJID(b.Client, sender)
	if !ok {
		return
	}
	if err := b.Store.NoteGroupParticipant(chatJID, row, now); err != nil {
		b.Log.Warnf("Failed to note group participant %s in %s: %v", row.User, chatJID, err)
	}
}

// isGroupJID reports whether a stored chat JID addresses a group.
func isGroupJID(chatJID string) bool {
	parsed, err := types.ParseJID(chatJID)
	return err == nil && parsed.Server == types.GroupServer
}

// syncGroupRosters refreshes at most limit stale rosters, waiting pace between
// fetches. It returns how many it refreshed and whether stale groups were left
// over, which is what tells the loop to come back soon instead of sleeping a
// whole interval. It stops early when the context is cancelled or the WhatsApp
// connection drops.
//
// Two kinds of group are skipped without spending the budget: those the chat
// allow-list excludes (a roster is conversation data like any other) and those
// whose last fetch failed recently. Neither can ever gain a roster row, so both
// stay in the stale list on every pass; counting them against the cap would let
// enough of them starve the groups that can actually be refreshed.
func (b *Bridge) syncGroupRosters(ctx context.Context, now time.Time, limit int, pace time.Duration) (refreshed int, more bool) {
	if b.Store == nil || b.Store.groupInfo == nil || b.GroupRosterSync <= 0 {
		return 0, false
	}
	stale, err := b.Store.StaleGroupRosters(now.Add(-b.GroupRosterSync))
	if err != nil {
		b.Log.Warnf("Group roster sync: could not list stale groups: %v", err)
		return 0, false
	}
	attempted := 0
	for _, jid := range stale {
		if ctx.Err() != nil || (b.Connected != nil && !b.Connected()) {
			return refreshed, true
		}
		if !b.Policy.Allows(jid) || b.rosterFailures.recent(jid, now, groupRosterRetryAfter) {
			continue
		}
		parsed, err := types.ParseJID(jid)
		if err != nil {
			continue
		}
		if attempted >= limit {
			return refreshed, true
		}
		attempted++
		if b.refreshGroupRoster(ctx, jid, parsed, now) {
			refreshed++
		}
		// Pace every attempt, not only the successful ones: a store full of
		// groups the account was removed from fails fast, and skipping the
		// wait there is exactly the burst this delay exists to prevent.
		if pace > 0 {
			select {
			case <-time.After(pace):
			case <-ctx.Done():
				return refreshed, true
			}
		}
	}
	return refreshed, false
}

// refreshGroupRoster fetches one group's participants and stores them. It
// reports success only when a roster row was actually written: a response with
// no usable participant leaves the group with no source="roster" row, so it
// stays stale forever and has to be backed off like any other failure.
func (b *Bridge) refreshGroupRoster(ctx context.Context, jid string, parsed types.JID, now time.Time) bool {
	fetchCtx, cancel := context.WithTimeout(ctx, groupRosterFetchTimeout)
	defer cancel()
	// Read before the request: see storeGroupRoster on why the sweep stamp
	// must predate anything the event feed can write while it is in flight.
	at := time.Now()
	info, err := b.Store.groupInfo(fetchCtx, parsed)
	if err != nil {
		// Left groups and transient failures both land here; neither is worth
		// an operator-visible line once every six hours. The failure is
		// remembered so the group does not consume a slot of the next batch.
		b.Log.Debugf("Group roster sync: %s: %v", jid, err)
		b.rosterFailures.note(jid, now)
		return false
	}
	written, err := b.storeGroupRoster(jid, buildGroupMembers(info, nil).Members, at)
	if err != nil {
		b.Log.Warnf("Group roster sync: failed to store %s: %v", jid, err)
		b.rosterFailures.note(jid, now)
		return false
	}
	if written == 0 {
		b.Log.Debugf("Group roster sync: %s returned no usable participant", jid)
		b.rosterFailures.note(jid, now)
		return false
	}
	b.rosterFailures.forget(jid)
	return true
}

// runGroupRosterSync runs a pass groupRosterSyncStartDelay after startup and
// then every GroupRosterSync until b.ctx is cancelled (Shutdown).
// GroupRosterSync <= 0 disables it.
//
// Two situations shorten the wait to groupRosterSyncRetryDelay:
//
//   - the socket was down (a container still showing its QR code two minutes
//     in would otherwise leave the cache empty for the rest of the day);
//   - the pass stopped at the batch cap. Sleeping a whole interval there would
//     make the *next* cutoff fall just after the groups this pass stamped, so
//     the same most-active groups would be refetched every time and the ones
//     past the cap would never get a roster at all.
func (b *Bridge) runGroupRosterSync() {
	if b.GroupRosterSync <= 0 {
		return
	}
	delay := groupRosterSyncStartDelay
	for {
		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return
		}
		if b.Connected != nil && !b.Connected() {
			delay = groupRosterSyncRetryDelay
			continue
		}
		refreshed, more := b.syncGroupRosters(b.ctx, time.Now(), groupRosterSyncBatch, groupRosterSyncPace)
		if refreshed > 0 {
			b.Log.Infof("Group roster sync: refreshed %d group roster(s)", refreshed)
		}
		delay = b.GroupRosterSync
		if more {
			delay = groupRosterSyncRetryDelay
		}
	}
}
