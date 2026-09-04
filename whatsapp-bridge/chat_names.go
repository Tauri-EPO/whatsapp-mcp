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
//   - resolved names are cached in memory for the life of the process.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const groupInfoRetryAfter = 10 * time.Minute

// groupInfoLookup fetches group metadata; nil means "no network available"
// (tests, or before the client is connected).
type groupInfoLookup func(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

// chatNameCache remembers resolved names and failed group lookups.
type chatNameCache struct {
	mu       sync.Mutex
	names    map[string]string
	groupErr map[string]time.Time
}

func newChatNameCache() *chatNameCache {
	return &chatNameCache{names: map[string]string{}, groupErr: map[string]time.Time{}}
}

func (c *chatNameCache) get(chatJID string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	name, ok := c.names[chatJID]
	return name, ok
}

func (c *chatNameCache) put(chatJID, name string) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	c.names[chatJID] = name
	c.mu.Unlock()
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
	c.groupErr[chatJID] = now
	c.mu.Unlock()
}

// conversationName extracts the name a history-sync conversation carries.
func conversationName(conversation *waProto.Conversation) string {
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

// GetChatName resolves the display name for a chat. conversation is the
// history-sync payload (nil for live messages); allowNetwork permits a group
// metadata fetch through store.groupInfo. sender is the last-resort name for
// direct chats.
func GetChatName(client *whatsmeow.Client, store *MessageStore, jid types.JID, chatJID string, conversation *waProto.Conversation, sender string, allowNetwork bool, logger waLog.Logger) string {
	if store != nil {
		if name, ok := store.names.get(chatJID); ok {
			return name
		}
	}

	// Already resolved in a previous run.
	if store != nil && store.db != nil {
		var existing string
		if err := store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existing); err == nil {
			existing = strings.TrimSpace(existing)
			if existing != "" && !isPlaceholderName(existing, jid) {
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
		if client != nil && client.Store != nil && client.Store.Contacts != nil {
			if contact, err := client.Store.Contacts.GetContact(context.Background(), jid); err == nil && strings.TrimSpace(contact.FullName) != "" {
				name = strings.TrimSpace(contact.FullName)
			}
		}
		if name == "" {
			name = lookupLocalContactName(client, store, chatJID, logger)
		}
		if name == "" {
			// Sender/user fallbacks are placeholders too: do not cache them.
			if sender != "" {
				return sender
			}
			return jid.User
		}
	}
	if store != nil {
		store.names.put(chatJID, name)
	}
	return name
}

// isPlaceholderName reports whether a stored name is one of our fallbacks
// (group placeholder or the bare user part), i.e. worth trying to improve.
func isPlaceholderName(name string, jid types.JID) bool {
	return name == placeholderGroupName(jid) || name == jid.User
}
