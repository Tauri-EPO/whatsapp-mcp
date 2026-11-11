package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type businessClient interface {
	GetUserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error)
	GetBusinessProfile(context.Context, types.JID) (*types.BusinessProfile, error)
}

type businessResult struct {
	Success           bool     `json:"success"`
	JID               string   `json:"jid"`
	IsBusiness        *bool    `json:"is_business"`
	VerifiedName      string   `json:"verified_name,omitempty"`
	Categories        []string `json:"categories"`
	Email             string   `json:"email,omitempty"`
	Address           string   `json:"address,omitempty"`
	Website           string   `json:"website,omitempty"`
	VerifiedNameError string   `json:"verified_name_error,omitempty"`
	ProfileError      string   `json:"profile_error,omitempty"`
	RateLimited       bool     `json:"rate_limited"`
	Cached            bool     `json:"cached"`
}

type businessEntry struct {
	result  businessResult
	expires time.Time
}
type businessCache struct {
	mu      sync.Mutex
	entries map[types.JID]businessEntry
	now     func() time.Time
	client  businessClient
}

func (c *businessCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Only answers are cached, never policy decisions. Partial answers retain their
// errors on cache hits so a rate limit cannot turn an unknown into a person.
func (c *businessCache) lookup(ctx context.Context, client businessClient, jid types.JID) businessResult {
	c.mu.Lock()
	// A replaced session must never reuse metadata queried by another account.
	if c.client != client {
		c.entries = nil
		c.client = client
	}
	entry, ok := c.entries[jid]
	if ok && c.clock().Before(entry.expires) {
		c.mu.Unlock()
		entry.result.Cached = true
		return entry.result
	}
	c.mu.Unlock()
	result := businessResult{Success: true, JID: jid.String(), Categories: []string{}}
	info, userErr := client.GetUserInfo(ctx, []types.JID{jid})
	if userErr != nil {
		result.VerifiedNameError = "verified name lookup failed"
		var iq *whatsmeow.IQError
		if errors.As(userErr, &iq) && iq.Code == 429 {
			result.RateLimited = true
			result.VerifiedNameError = "verified name lookup rate limited (429)"
		}
	} else if user, exists := info[jid]; exists {
		if user.VerifiedName != nil {
			business := true
			result.IsBusiness = &business
			result.VerifiedName = user.VerifiedName.Details.GetVerifiedName()
		}
	}
	// This independent query still runs after usync refuses the verified name.
	profile, err := client.GetBusinessProfile(ctx, jid)
	if err != nil {
		result.ProfileError = "business profile lookup failed"
		if errors.Is(err, whatsmeow.ErrIQNotFound) && result.IsBusiness == nil && userErr == nil {
			business := false
			result.IsBusiness = &business
		}
	} else if profile != nil {
		result.Email, result.Address = profile.Email, profile.Address
		result.Website = profile.ProfileOptions["website"]
		for _, category := range profile.Categories {
			result.Categories = append(result.Categories, category.Name)
		}
		// WhatsApp also returns a non-nil empty profile for personal accounts.
		if len(result.Categories) > 0 || result.Email != "" || result.Address != "" || result.Website != "" {
			business := true
			result.IsBusiness = &business
		} else if result.IsBusiness == nil && userErr == nil {
			business := false
			result.IsBusiness = &business
		}
	} else if userErr == nil && result.IsBusiness == nil {
		business := false
		result.IsBusiness = &business
	}
	if ctx.Err() == nil && (result.IsBusiness != nil || result.RateLimited) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.client != client {
			return result
		}
		if c.entries == nil {
			c.entries = map[types.JID]businessEntry{}
		}
		if len(c.entries) >= 256 {
			var oldest types.JID
			var expires time.Time
			for key, value := range c.entries {
				if expires.IsZero() || value.expires.Before(expires) {
					oldest, expires = key, value.expires
				}
			}
			delete(c.entries, oldest)
		}
		ttl := time.Hour
		if result.RateLimited || result.VerifiedNameError != "" || result.ProfileError != "" || result.IsBusiness == nil {
			ttl = time.Minute
		}
		c.entries[jid] = businessEntry{result: result, expires: c.clock().Add(ttl)}
	}
	return result
}

func handleCheckBusiness(client businessClient, cache *businessCache, policy chatPolicyResolver, connected func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jid, ok := authorizeChat(w, policy.effective(r.Context()), r.URL.Query().Get("jid"), false)
		if !ok {
			return
		}
		if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
			writeError(w, 400, "jid must be a phone or LID user JID")
			return
		}
		if !connected() {
			writeError(w, 503, "WhatsApp client is not connected")
			return
		}
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		writeJSON(w, 200, cache.lookup(ctx, client, jid))
	}
}
