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
	"errors"
	"fmt"
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
	// groupRosterGoneRetryAfter is the wait after WhatsApp answered that this
	// account is not in the group, or that the group no longer exists. That
	// answer does not change by itself: being added back clears it at once
	// (handleEvent), and a weekly try covers a missed notification (#746).
	groupRosterGoneRetryAfter = 7 * 24 * time.Hour
)

// resolveGroupRosterSync parses groupRosterSyncEnv. Zero disables the pass;
// a negative, non-numeric or unrepresentable duration is an error so main()
// fails fast rather than running with a setting the operator did not write.
func resolveGroupRosterSync(value string) (time.Duration, error) {
	return resolveHoursEnv(groupRosterSyncEnv, value, groupRosterSyncInterval, 0)
}

// groupRosterSyncSummary renders the setting for the startup log.
func groupRosterSyncSummary(interval time.Duration) string {
	if interval <= 0 {
		return "off"
	}
	return everyHoursSummary(interval)
}

// rosterFailures remembers the groups whose last refresh failed. Without it a
// group this account was removed from — its chat row stays, and its cached
// roster was dropped, so it is permanently "stale" and sorts first — would
// consume a slot of every batch forever and push real work past the cap.
type rosterFailures struct {
	mu sync.Mutex
	// waits holds, per group, when its backoff began and when it ends.
	waits map[string]rosterWait
	// back holds when the account was last seen in the group again, so an
	// answer to a fetch sent before that cannot put it back in the long wait.
	back map[string]time.Time
}

func newRosterFailures() *rosterFailures {
	return &rosterFailures{waits: map[string]rosterWait{}, back: map[string]time.Time{}}
}

type rosterWait struct{ since, until time.Time }

// recent reports whether jid is still backed off at now.
func (f *rosterFailures) recent(jid string, now time.Time) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	wait, ok := f.waits[jid]
	if ok && !now.Before(wait.until) {
		delete(f.waits, jid)
		return false
	}
	return ok
}

// note backs jid off after a failure that may pass on its own.
func (f *rosterFailures) note(jid string, now time.Time) {
	f.wait(jid, now, now.Add(groupRosterRetryAfter))
}

// noteGone backs jid off after WhatsApp answered that this account is not in
// it, unless the account was seen back in the group after asked, the moment
// the question went out.
func (f *rosterFailures) noteGone(jid string, asked, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	back, ok := f.back[jid]
	if ok && !back.Before(asked) {
		f.mu.Unlock()
		return
	}
	delete(f.back, jid)
	f.mu.Unlock()
	f.wait(jid, now, now.Add(groupRosterGoneRetryAfter))
}

func (f *rosterFailures) wait(jid string, since, until time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits[jid] = rosterWait{since: since, until: until}
}

// forget ends jid's backoff after a fetch succeeded or the account was seen
// in the group again at now.
func (f *rosterFailures) forget(jid string, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.waits, jid)
	f.back[jid] = now
}

// forgetIfAfter ends jid's backoff when evidence dated at, such as a message
// this account sent there, is newer than the backoff: a message queued before
// a departure and delivered after it must not reopen the group.
func (f *rosterFailures) forgetIfAfter(jid string, at, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	wait, ok := f.waits[jid]
	f.mu.Unlock()
	if ok && at.After(wait.since) {
		f.forget(jid, now)
	}
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
	written, err := b.Store.ReplaceGroupRoster(groupJID, b.resolveRosterRows(groupMemberRows(members)), at)
	if err == nil && written > 0 {
		// WhatsApp just answered with this group's members: we are in it,
		// whichever caller asked (#746).
		b.rosterFailures.forget(groupJID, time.Now())
	}
	return written, err
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
		if pn := resolveUserJID(b.currentClient(), lid, types.EmptyJID); pn.Server == types.DefaultUserServer {
			rows[i].Phone, rows[i].User = pn.User, pn.User
		}
	}
	return rows
}

// recordGroupRoster is the rosterRecorder /api/group/members is wired to
// (rest.go): caching the roster must never change what the caller gets back,
// so a failure is a log line and nothing more.
func (b *Bridge) recordGroupRoster(groupJID string, members []GroupMember, at time.Time, info *types.GroupInfo) {
	if info == nil {
		// Subject updates fetched these participants before SetGroupTopic.
		// That pre-update metadata must not replace the subsequent topic event.
		if b.Store != nil && b.Policy.effective(b.ctx).Allows(groupJID) {
			if _, err := b.storeGroupRoster(groupJID, members, at); err != nil {
				b.Log.Warnf("Failed to cache the roster of %s: %v", groupJID, err)
			}
		}
		return
	}
	if _, err := b.storeFetchedGroupInfo(groupJID, info, at, members); err != nil {
		b.Log.Warnf("Failed to cache the roster of %s: %v", groupJID, err)
	}
}

// storeFetchedGroupInfo applies a fresh policy after network I/O. Both scheduled
// refreshes and live roster requests seed settings even without change events.
func (b *Bridge) storeFetchedGroupInfo(group string, info *types.GroupInfo, at time.Time, resolved ...[]GroupMember) (int, error) {
	if b.Store == nil || !b.Policy.effective(b.ctx).Allows(group) {
		return 0, nil
	}
	if info == nil {
		return 0, fmt.Errorf("group info for %s was nil", group)
	}
	if err := b.storeGroupSettings(group, map[string]any{
		"announce": info.IsAnnounce, "locked": info.IsLocked,
		"join_approval_required": info.IsJoinApprovalRequired, "topic": info.Topic,
	}, time.Now()); err != nil {
		b.Log.Warnf("Failed to store group settings for %s: %v", group, err)
	}
	members := buildGroupMembers(info, nil, nil).Members
	if len(resolved) > 0 {
		members = resolved[0]
	} // Keep the HTTP response's resolved contact names.
	return b.storeGroupRoster(group, members, at)
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
	b.applyGroupParticipantChangesWithPolicy(b.Policy, v)
}

func (b *Bridge) applyGroupParticipantChangesWithPolicy(policy chatPolicyResolver, v *events.GroupInfo) {
	group := v.JID.String()
	if len(v.Join) > 0 && b.leftGroup(v.Join) {
		// Added back: the roster is ours to read again, also for a group the
		// allow-list excludes now and may include later (#746).
		b.rosterFailures.forget(group, time.Now())
	}
	// A group the operator excluded gets no cached membership at all, on any
	// feed: the allow-list is about which conversations this deployment may
	// hold, not only which ones it may write to.
	if b.Store == nil || !policy.effective(b.ctx).Allows(group) {
		return
	}
	// Stamped with the local clock, not v.Timestamp. last_seen means "we
	// confirmed this membership then", and a notification that spent a minute
	// in transit would otherwise land *behind* a roster fetched in the
	// meantime — whose DELETE drops every row older than its own stamp, taking
	// the member who just joined with it.
	now := time.Now()

	if len(v.Join) > 0 {
		if err := b.Store.AddGroupMembers(group, groupMemberEventRows(b.currentClient(), v.Join), now); err != nil {
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
			// departure blocks both that feed and the sync for a week (#746).
			b.rosterFailures.noteGone(group, now, now)
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
		if err := b.Store.RemoveGroupMembers(group, groupMemberEventRows(b.currentClient(), v.Leave), leftAt); err != nil {
			b.Log.Warnf("Failed to record departures from %s: %v", group, err)
		}
	}
	if len(v.Promote) > 0 {
		if err := b.Store.SetGroupMemberAdmin(group, groupMemberEventRows(b.currentClient(), v.Promote), true); err != nil {
			b.Log.Warnf("Failed to record promotions in %s: %v", group, err)
		}
	}
	if len(v.Demote) > 0 {
		if err := b.Store.SetGroupMemberAdmin(group, groupMemberEventRows(b.currentClient(), v.Demote), false); err != nil {
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
	if b.currentClient() == nil || b.currentClient().Store == nil {
		return false
	}
	selves := map[string]struct{}{}
	if id := b.currentClient().Store.ID; id != nil {
		selves[id.ToNonAD().User] = struct{}{}
	}
	if lid := b.currentClient().Store.LID; !lid.IsEmpty() {
		selves[lid.ToNonAD().User] = struct{}{}
	}
	if len(selves) == 0 {
		return false
	}
	for _, jid := range leaving {
		if _, ok := selves[jid.ToNonAD().User]; ok {
			return true
		}
		if _, ok := selves[resolveUserJID(b.currentClient(), jid, types.EmptyJID).User]; ok {
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
func (b *Bridge) noteGroupSender(policy chatPolicyResolver, chatJID string, sender types.JID, now time.Time) {
	if b.Store == nil || !isGroupJID(chatJID) || !policy.effective(b.ctx).Allows(chatJID) {
		return
	}
	if b.leftGroup([]types.JID{sender}) {
		// This account wrote in the group after the backoff began, so it is
		// a member again (#746).
		b.rosterFailures.forgetIfAfter(chatJID, now, time.Now())
	}
	if b.rosterFailures.recent(chatJID, now) {
		return
	}
	row, ok := groupMemberRowFromJID(b.currentClient(), sender)
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
	policy := b.Policy.effective(ctx) // One settings read for the pass, not one per group.
	for _, jid := range stale {
		if ctx.Err() != nil || (b.Connected != nil && !b.Connected()) {
			return refreshed, true
		}
		if !policy.Allows(jid) || b.rosterFailures.recent(jid, now) {
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
	asked := time.Now()
	written, err := b.fetchGroupRoster(ctx, jid, parsed)
	if errors.Is(err, whatsmeow.ErrNotInGroup) || errors.Is(err, whatsmeow.ErrGroupNotFound) {
		// A group this account left, or one that was deleted: its chat stays
		// in the archive, so it is stale forever. Expected, not a warning,
		// and not worth asking WhatsApp again every six hours (#746).
		b.Log.Debugf("Group roster sync: skipping %s for %s: %v", jid, groupRosterGoneRetryAfter, err)
		b.rosterFailures.noteGone(jid, asked, now)
		return false
	}
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
	return true // storeGroupRoster ended the backoff
}

// fetchGroupRoster does no backoff accounting: event-triggered refreshes
// must not suppress the scheduled roster pass or message-derived membership.
func (b *Bridge) fetchGroupRoster(ctx context.Context, jid string, parsed types.JID) (int, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, groupRosterFetchTimeout)
	defer cancel()
	at := time.Now() // Participant sweep must predate the request.
	b.clientGate.RLock()
	if b.operatorLogout.Load() {
		b.clientGate.RUnlock()
		return 0, fmt.Errorf("operator logout in progress")
	}
	info, err := b.Store.groupInfo(fetchCtx, parsed)
	b.clientGate.RUnlock()
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, fmt.Errorf("group info response was nil")
	}
	return b.storeFetchedGroupInfo(jid, info, at)
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

// applyGroupSettings retains fields present in an event using the local
// observation clock, shared with fetched snapshots; the last observation wins.
func (b *Bridge) applyGroupSettings(policy chatPolicyResolver, v *events.GroupInfo) {
	if b.Store == nil || !policy.effective(b.ctx).Allows(v.JID.String()) {
		return
	}
	values := map[string]any{}
	if v.Announce != nil {
		values["announce"] = v.Announce.IsAnnounce
	}
	if v.Locked != nil {
		values["locked"] = v.Locked.IsLocked
	}
	if v.MembershipApprovalMode != nil && b.Store.groupInfo != nil {
		// The SDK decoder cannot distinguish on/off. Refresh the full roster
		// off the event dispatcher, with a bounded queue and one worker.
		b.requestGroupSettingsRefresh(v.JID)
	}
	if v.Topic != nil {
		topic := v.Topic.Topic
		if v.Topic.TopicDeleted {
			topic = ""
		}
		values["topic"] = topic
	}
	if len(values) == 0 {
		return
	}
	if err := b.storeGroupSettings(v.JID.String(), values, time.Now()); err != nil {
		b.Log.Warnf("Failed to store group settings for %s: %v", v.JID, err)
	}
}

// requestGroupSettingsRefresh never waits on network I/O or a full queue.
func (b *Bridge) requestGroupSettingsRefresh(jid types.JID) {
	if b.Connected != nil && !b.Connected() {
		return
	}
	b.groupSettingsRefreshOnce.Do(func() {
		b.groupSettingsPending = make(map[types.JID]bool)
		b.groupSettingsRefresh = make(chan types.JID, groupRosterSyncBatch)
		go func() {
			for {
				select {
				case <-b.ctx.Done():
					return
				case group := <-b.groupSettingsRefresh:
					// Deduplicate queued requests only. A notification received
					// during this fetch queues one follow-up observation.
					b.groupSettingsPendingMu.Lock()
					delete(b.groupSettingsPending, group)
					b.groupSettingsPendingMu.Unlock()
					if (b.Connected == nil || b.Connected()) && b.Policy.effective(b.ctx).Allows(group.String()) {
						if _, err := b.fetchGroupRoster(b.ctx, group.String(), group); err != nil {
							b.Log.Warnf("Group settings refresh: %s: %v", group, err)
						}
					}
					select {
					case <-b.ctx.Done():
						return
					case <-time.After(groupRosterSyncPace):
					}
				}
			}
		}()
	})
	b.groupSettingsPendingMu.Lock()
	defer b.groupSettingsPendingMu.Unlock()
	if b.groupSettingsPending[jid] {
		return
	}
	select {
	case b.groupSettingsRefresh <- jid:
		b.groupSettingsPending[jid] = true
	default:
		b.Log.Warnf("Group settings refresh queue full; skipping %s", jid)
	}
}

func (b *Bridge) storeGroupSettings(group string, values map[string]any, at time.Time) error {
	tx, err := b.Store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for field, value := range values {
		// field is one of the fixed internal keys above, never event text.
		column := "group_" + field
		//nolint:gosec // Column names are fixed internal keys; all values are bound parameters.
		query := fmt.Sprintf(`UPDATE chats SET %[1]s=?, %[1]s_updated_at=?
            WHERE jid=? AND (%[1]s_updated_at IS NULL OR ? >= %[1]s_updated_at)`, column)
		if _, err = tx.Exec(query, value, dbTime(at), group, dbTime(at)); err != nil {
			break
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	return err
}
