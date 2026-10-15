package main

// Chat name resolution.
//
// Names come, in order, from: the chats table (already resolved), the
// history-sync conversation payload, the group metadata from WhatsApp
// (network, groups only), the whatsmeow contact store, and finally the JID
// itself. Group metadata is the expensive step: right after pairing a
// history sync hands us hundreds of conversations, and fetching group info
// for each one is a burst of round trips that also delays message
// processing. So:
//
//   - history sync never calls the network; a group without a name in the
//     payload gets the "Group <id>" placeholder and is fixed lazily;
//   - live messages may fetch group info, but a failed lookup is remembered
//     for groupInfoRetryAfter so a flaky group does not trigger a fetch per
//     message;
//   - resolved names are cached in memory for chatNameTTL and refreshed from
//     the chats table afterwards; a GroupInfo name change updates both at
//     once (events.go), so renames show up without a restart. Failed group
//     lookups are pruned as they expire so neither map grows without bound.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	groupInfoRetryAfter = 10 * time.Minute
	// chatNameTTL bounds how long an in-memory name is trusted before the
	// chats table is consulted again.
	chatNameTTL    = 24 * time.Hour
	contactMissTTL = 30 * time.Second
)

// groupInfoLookup fetches group metadata; nil means "no network available"
// (tests, or before the client is connected).
type groupInfoLookup func(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

// chatNameCache remembers resolved names and failed group lookups.
type chatNameCache struct {
	mu          sync.Mutex
	names       map[string]cachedName
	groupErr    map[string]time.Time
	contactMiss map[string]time.Time
	now         func() time.Time // injectable clock for tests
}

type cachedName struct {
	name    string
	expires time.Time
}

func newChatNameCache() *chatNameCache {
	return &chatNameCache{names: map[string]cachedName{}, groupErr: map[string]time.Time{}, contactMiss: map[string]time.Time{}, now: time.Now}
}

func (c *chatNameCache) get(chatJID string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.names[chatJID]
	if !ok {
		return "", false
	}
	if c.now().After(entry.expires) {
		delete(c.names, chatJID)
		return "", false
	}
	return entry.name, true
}

func (c *chatNameCache) put(chatJID, name string) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	c.names[chatJID] = cachedName{name: name, expires: c.now().Add(chatNameTTL)}
	c.mu.Unlock()
}

// invalidate forgets a cached name and any remembered lookup failure, so the
// next resolution goes back to the chats table (or the network).
func (c *chatNameCache) invalidate(chatJID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.names, chatJID)
	delete(c.groupErr, chatJID)
	delete(c.contactMiss, chatJID)
	c.mu.Unlock()
}

// size reports cached names and remembered failures (tests, health).
func (c *chatNameCache) size() (names, failures int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.names), len(c.groupErr)
}

// Contact misses are short-lived and bounded; contact events invalidate them.
func (c *chatNameCache) contactLookupAllowed(chatJID string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires, ok := c.contactMiss[chatJID]
	return !ok || !c.now().Before(expires)
}

func (c *chatNameCache) rememberContactMiss(chatJID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for jid, expires := range c.contactMiss {
		if !now.Before(expires) {
			delete(c.contactMiss, jid)
		}
	}
	if len(c.contactMiss) >= contactNameBatchSize {
		for jid := range c.contactMiss {
			delete(c.contactMiss, jid)
			break
		}
	}
	c.contactMiss[chatJID] = now.Add(contactMissTTL)
}

func (c *chatNameCache) groupLookupAllowed(chatJID string, now time.Time) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.groupErr[chatJID]
	return !ok || now.Sub(last) >= groupInfoRetryAfter
}

func (c *chatNameCache) rememberGroupFailure(chatJID string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	// Prune entries whose retry window has passed so the map stays bounded by
	// the number of groups that failed recently, not ever.
	for jid, last := range c.groupErr {
		if now.Sub(last) >= groupInfoRetryAfter {
			delete(c.groupErr, jid)
		}
	}
	c.groupErr[chatJID] = now
	c.mu.Unlock()
}

// RenameChat records a new display name (group subject change) in the chats
// table and the cache, without touching last_message_time.
func (store *MessageStore) RenameChat(chatJID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if _, err := store.db.Exec(`UPDATE chats SET name = ? WHERE jid = ?`, name, chatJID); err != nil {
		return err
	}
	store.names.put(chatJID, name)
	return nil
}

// RenamePlaceholderChat updates only the name inspected by the caller. A
// concurrent real rename wins, and contact metadata never advances activity.
func (store *MessageStore) RenamePlaceholderChat(chatJID, expected, name string) error {
	result, err := store.db.Exec(`UPDATE chats SET name = ? WHERE jid = ? AND COALESCE(name, '') = ?`, name, chatJID, expected)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		store.names.put(chatJID, name)
	} else {
		store.names.invalidate(chatJID)
	}
	return nil
}

// refreshContactChatName consumes the already-local contact store and the
// event's name. No network request or chat creation is needed. Refresh both
// stored phone/LID rows when the SDK confirms the alternate identity.
func (b *Bridge) refreshContactChatName(jid, hint types.JID, eventName string) {
	if b.Store == nil || b.Store.db == nil {
		return
	}
	primary, err := normalizedUserJID(jid.String())
	if err != nil || (primary.Server != types.DefaultUserServer && primary.Server != types.HiddenUserServer) {
		return
	}
	targets := map[string]types.JID{}
	add := func(candidate types.JID) {
		normal, err := normalizedUserJID(candidate.String())
		if err == nil && (normal.Server == types.DefaultUserServer || normal.Server == types.HiddenUserServer) {
			normal = normal.ToNonAD()
			targets[normal.String()] = normal
		}
	}
	add(primary)
	add(hint)
	client := b.currentClient()
	if alt, err := lookupAltJID(context.Background(), client, primary.ToNonAD()); err == nil {
		add(alt)
	}
	self := ownUsers(client)
	twins := make([]types.JID, 0, len(targets))
	for _, target := range targets {
		twins = append(twins, target)
	}
	for chatJID, target := range targets {
		b.storeLive("contact name", "", chatJID, func() error {
			var existing string
			if err := b.Store.db.QueryRow(`SELECT COALESCE(name, '') FROM chats WHERE jid = ?`, chatJID).Scan(&existing); err != nil {
				if err == sql.ErrNoRows {
					return nil
				}
				return err
			}
			if strings.TrimSpace(existing) != "" && !isChatFallbackName(strings.TrimSpace(existing), target, self, twins...) {
				return nil
			}
			b.Store.names.invalidate(chatJID)
			name := GetChatName(client, b.Store, target, chatJID, nil, "", false, b.Log)
			if strings.TrimSpace(name) == "" || isChatFallbackName(name, target, self, twins...) {
				name = strings.TrimSpace(eventName)
			}
			if name == "" || isChatFallbackName(name, target, self, twins...) {
				return nil
			}
			return b.Store.RenamePlaceholderChat(chatJID, existing, name)
		})
	}
}

// EnsureChat makes sure the chat row exists, which a message row needs before
// it can reference it, and records the resolved name. It never touches
// last_message_time: that moves only when a row of the chat is actually
// written, so a message the bridge does not store cannot make the chat look
// active at a time nothing in it accounts for (issue #531).
func (store *MessageStore) EnsureChat(chatJID, name string) error {
	return store.StoreChat(chatJID, name, time.Time{})
}

// conversationName extracts the name a history-sync conversation carries.
func conversationName(conversation *waHistorySync.Conversation) string {
	if conversation == nil {
		return ""
	}
	if name := strings.TrimSpace(conversation.GetDisplayName()); name != "" {
		return name
	}
	return strings.TrimSpace(conversation.GetName())
}

func placeholderGroupName(jid types.JID) string {
	return fmt.Sprintf("Group %s", jid.User)
}

// selfUsers are the user parts our own account goes by: the phone number and,
// once the session has one, the LID. Both are empty before pairing, and then
// nothing matches.
type selfUsers struct{ phone, lid string }

func (s selfUsers) has(user string) bool {
	return user != "" && (user == s.phone || user == s.lid)
}

// ownUsers reads them off the whatsmeow store, the way /api/me does.
func ownUsers(client *whatsmeow.Client) selfUsers {
	phone, lid := clientIdentity(client)(context.Background())
	return selfUsers{phone: phone.User, lid: lid.User}
}

// GetChatName resolves the display name for a chat. conversation is the
// history-sync payload (nil for live messages); allowNetwork permits a group
// metadata fetch through store.groupInfo. sender is the last-resort name for
// chats that are not groups, unless the sender is us: a chat we start with an
// unknown number (or a broadcast list we post to) would be named after our own
// number, which reads as a real name and never heals (issue #448).
func GetChatName(client *whatsmeow.Client, store *MessageStore, jid types.JID, chatJID string, conversation *waHistorySync.Conversation, sender string, allowNetwork bool, logger waLog.Logger) string {
	return getChatNameContext(context.Background(), client, store, jid, chatJID, conversation, sender, allowNetwork, logger)
}

func getChatNameContext(ctx context.Context, client *whatsmeow.Client, store *MessageStore, jid types.JID, chatJID string, conversation *waHistorySync.Conversation, sender string, allowNetwork bool, logger waLog.Logger) string {
	if store != nil {
		if name, ok := store.names.get(chatJID); ok {
			return name
		}
	}

	var self selfUsers
	if jid.Server != types.GroupServer {
		self = ownUsers(client)
	}

	var contact *localContactIdentity
	lookupContact := func(archived string, forceName bool) localContactIdentity {
		if store != nil && !store.names.contactLookupAllowed(chatJID) {
			return localContactIdentity{Name: archived, needsName: true}
		}
		account := ""
		if client != nil && client.Store != nil && client.Store.ID != nil {
			account = client.Store.ID.String()
		}
		value := store.resolveLocalContactNames(ctx, []localContactIdentity{{JID: chatJID, Name: archived, forceName: forceName}}, account, client, logger)[chatJID]
		if store != nil && !value.resolved && ctx.Err() == nil {
			store.names.rememberContactMiss(chatJID)
		}
		return value
	}
	// Main's historical chat fallbacks also heal groups and misplaced self
	// labels. The shared contact-placeholder rule is strictly direct-only.
	if store != nil && store.db != nil {
		var existing string
		if err := store.db.QueryRowContext(ctx, "SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existing); err == nil {
			existing = strings.TrimSpace(existing)
			direct := jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer
			if existing != "" && direct && localPhoneDigits(existing) != "" {
				value := lookupContact(existing, isChatFallbackName(existing, jid, self))
				contact = &value
				if !value.needsName || value.resolved {
					store.names.put(chatJID, value.Name)
					return value.Name
				}
				// A formatted archived number is still more readable than a
				// sender/LID fallback. Only replace it with a resolved name.
				if existing != jid.User && existing != placeholderGroupName(jid) && !self.has(existing) {
					return existing
				}
			} else if existing != "" && !isChatFallbackName(existing, jid, self) {
				store.names.put(chatJID, existing)
				return existing
			}
		}
	}

	var name string
	if jid.Server == types.GroupServer {
		name = conversationName(conversation)
		if name == "" && allowNetwork && store != nil && store.groupInfo != nil && store.names.groupLookupAllowed(chatJID, time.Now()) {
			info, err := store.groupInfo(context.Background(), jid)
			if err == nil && strings.TrimSpace(info.Name) != "" {
				name = strings.TrimSpace(info.Name)
			} else {
				store.names.rememberGroupFailure(chatJID, time.Now())
				if err != nil {
					logger.Debugf("Group info lookup failed for %s: %v", chatJID, err)
				}
			}
		}
		if name == "" {
			// Placeholder: not cached, so a later live message can still resolve it.
			return placeholderGroupName(jid)
		}
	} else {
		if contact == nil {
			value := lookupContact("", true)
			contact = &value
		}
		if contact.resolved {
			name = contact.Name
		}
		if name == "" {
			// Sender/user fallbacks are placeholders too: do not cache them.
			if sender != "" && !self.has(sender) {
				return sender
			}
			if self.has(jid.User) {
				// The chat with ourselves, whichever namespace it is filed
				// under, goes by our number as it always did.
				return self.phone
			}
			return jid.User
		}
	}
	if store != nil {
		store.names.put(chatJID, name)
	}
	return name
}

// Chat history has additional fallbacks that main has always healed (#448).
// They are not contact placeholders: groups and misplaced self labels keep
// this rule, while contact lookup uses the single direct phone/LID predicate.
func isChatFallbackName(name string, jid types.JID, self selfUsers, twins ...types.JID) bool {
	if isPlaceholderName(name, jid, twins...) || name == placeholderGroupName(jid) || name == jid.User {
		return true
	}
	return jid.Server != types.GroupServer && self.has(name) && !self.has(jid.User)
}
