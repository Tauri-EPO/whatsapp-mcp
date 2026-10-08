package main

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	recipientCacheLimit = 256
	recipientCacheTTL   = time.Hour
)

type recipientNumberEntry struct {
	jid     types.JID
	expires time.Time
}

// The zero value is ready to use. Each Bridge owns its cache; a reconnect or
// logout clears it, and an in-flight answer from before that clear stays out.
type recipientNumberCache struct {
	mu         sync.Mutex
	entries    map[string]recipientNumberEntry
	generation uint64
	now        func() time.Time // nil = time.Now; set before use in tests
}

func (c *recipientNumberCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *recipientNumberCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.generation++
}

// Only the registry answer is cached. registeredRecipient still checks the
// current allow-list after every hit; its caller checks the typed number first.
func (b *Bridge) queryRegisteredNumber(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(phones) != 1 {
		return b.IsOnWhatsApp(ctx, phones)
	}
	c := &b.recipientNumbers
	key := phones[0]
	c.mu.Lock()
	generation := c.generation
	entry, ok := c.entries[key]
	if ok && c.clock().Before(entry.expires) {
		c.mu.Unlock()
		return []types.IsOnWhatsAppResponse{{Query: key, IsIn: true, JID: entry.jid, PhoneNumber: entry.jid}}, nil
	}
	delete(c.entries, key)
	c.mu.Unlock()

	answers, err := b.IsOnWhatsApp(ctx, phones)
	// Match canonicalRecipientJID: the first positive answer wins, even if
	// whatsmeow could not persist its LID and also returned an error. An answer
	// naming only a LID is not a known registered phone number and is not cached.
	for _, answer := range answers {
		if !answer.IsIn {
			continue
		}
		jid := registeredPhoneJID(answer)
		if !jid.IsEmpty() && isPhoneDigits(jid.User) && ctx.Err() == nil {
			c.mu.Lock()
			if c.generation == generation {
				if c.entries == nil {
					c.entries = make(map[string]recipientNumberEntry)
				}
				if _, exists := c.entries[key]; !exists && len(c.entries) >= recipientCacheLimit {
					var oldest string
					var expires time.Time
					for candidate, value := range c.entries {
						if oldest == "" || value.expires.Before(expires) {
							oldest, expires = candidate, value.expires
						}
					}
					delete(c.entries, oldest)
				}
				c.entries[key] = recipientNumberEntry{jid: jid, expires: c.clock().Add(recipientCacheTTL)}
			}
			c.mu.Unlock()
		}
		break
	}
	return answers, err
}
