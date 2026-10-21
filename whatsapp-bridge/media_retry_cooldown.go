package main

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// A media retry is a request the sender's phone has to act on, and for a
// message this account sent that phone is the owner's own: every retry wakes
// it to look for the file. A failed retry is therefore remembered per message,
// and until the cooldown passes every caller (the ingest worker's next pass,
// an agent asking again) gets the last answer back instead of another request
// to the phone. Before this the worker asked the owner's phone about the same
// eight voice notes roughly a thousand times a day (issue #743).
const (
	// The phone answered without the file. The next receipt carries the same
	// identity and gets the same answer, so a day-scale wait loses nothing.
	mediaRetryAnsweredCooldown = 7 * 24 * time.Hour
	// Nothing came back (the phone offline, the receipt not sent, or the
	// download after a successful answer failing): worth another try sooner.
	mediaRetryUnansweredCooldown = time.Hour
	// Bounds the memory; an evicted message can be asked about once more.
	mediaRetryCooldownMax = 4096
)

// phoneRetryAnswer marks an error the phone itself sent back to a media retry,
// as opposed to one that never reached it or never came back.
type phoneRetryAnswer struct{ err error }

func (e phoneRetryAnswer) Error() string { return e.err.Error() }
func (e phoneRetryAnswer) Unwrap() error { return e.err }

// mediaRetryBackoffError is a failed media retry and when the phone may next
// be asked. It wraps the failure, so errMediaUnavailable keeps its meaning.
type mediaRetryBackoffError struct {
	until time.Time
	err   error
}

func (e *mediaRetryBackoffError) Error() string {
	if errors.Is(e.err, errMediaUnavailable) {
		return e.err.Error() // final already: no later date would change it
	}
	return fmt.Sprintf("%v; the sender's phone will not be asked again before %s (UTC)",
		e.err, e.until.UTC().Format("2006-01-02 15:04"))
}

func (e *mediaRetryBackoffError) Unwrap() error { return e.err }

type mediaRetryCooldownKey struct{ chat, id string }

type mediaRetryCooldownEntry struct {
	until time.Time
	err   error
}

type mediaRetryCooldown struct {
	mu      sync.Mutex
	entries map[mediaRetryCooldownKey]mediaRetryCooldownEntry
}

func newMediaRetryCooldown() *mediaRetryCooldown {
	return &mediaRetryCooldown{entries: map[mediaRetryCooldownKey]mediaRetryCooldownEntry{}}
}

// active returns the remembered failure while (chat, id) is cooling down.
func (c *mediaRetryCooldown) active(chat, id string, now time.Time) (*mediaRetryBackoffError, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := mediaRetryCooldownKey{chat, id}
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !now.Before(entry.until) {
		delete(c.entries, key)
		return nil, false
	}
	return &mediaRetryBackoffError{until: entry.until, err: entry.err}, true
}

// note remembers a failed retry and returns it as a backoff error.
func (c *mediaRetryCooldown) note(chat, id string, err error, now time.Time) *mediaRetryBackoffError {
	wait := mediaRetryUnansweredCooldown
	var answered phoneRetryAnswer
	if errors.As(err, &answered) || errors.Is(err, errMediaUnavailable) {
		wait = mediaRetryAnsweredCooldown
	}
	backoff := &mediaRetryBackoffError{until: now.Add(wait), err: err}
	if c == nil {
		return backoff
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := mediaRetryCooldownKey{chat, id}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= mediaRetryCooldownMax {
		c.evictLocked(now)
	}
	c.entries[key] = mediaRetryCooldownEntry{until: backoff.until, err: err}
	return backoff
}

// forget drops (chat, id), after a retry brought the file.
func (c *mediaRetryCooldown) forget(chat, id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, mediaRetryCooldownKey{chat, id})
}

// evictLocked drops the expired entries, or failing that the one due soonest.
func (c *mediaRetryCooldown) evictLocked(now time.Time) {
	var soonest mediaRetryCooldownKey
	var soonestAt time.Time
	for key, entry := range c.entries {
		if !now.Before(entry.until) {
			delete(c.entries, key)
			continue
		}
		if soonestAt.IsZero() || entry.until.Before(soonestAt) {
			soonest, soonestAt = key, entry.until
		}
	}
	if len(c.entries) >= mediaRetryCooldownMax {
		delete(c.entries, soonest)
	}
}
