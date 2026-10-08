package main

// What the bridge does when a row cannot be written.
//
// A write that fails loses data, so it is an ERROR that names the row (kind,
// message ID, chat; never the content) and a count on /metrics, for a live
// message, a reaction, a poll vote and a history row alike (issue #520).
//
// The realistic cause is transient: a history sync writes one transaction per
// conversation, and a live insert that waits longer than busy_timeout for the
// write lock comes back SQLITE_BUSY although it would succeed a moment later.
// Live writes are therefore tried again while the database is busy, a bounded
// number of times (issue #519). Any other error is final at once.
//
// The bound matters because these writes run on whatsmeow's event path, and
// every attempt may itself wait the busy timeout: a message that meets a lock
// held for longer than all of them still ends up dropped, after having held
// the events behind it for several such waits. The retry narrows the window;
// it does not remove the cause, which is how long a history-sync batch holds
// the write lock.

import (
	"context"
	"errors"
	"time"
)

// SQLite primary result codes for "somebody else holds the lock".
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// isBusyError reports whether err is SQLITE_BUSY or SQLITE_LOCKED, in any of
// their extended forms (the primary code is the low byte).
func isBusyError(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	primary := coded.Code() & 0xff
	return primary == sqliteBusy || primary == sqliteLocked
}

// defaultStoreRetryDelays is how long a live write waits before each new
// attempt on a busy database; its length is the number of retries. A function
// rather than a package variable, like the other Bridge timings (issue #382).
func defaultStoreRetryDelays() []time.Duration {
	return []time.Duration{200 * time.Millisecond, time.Second}
}

// retryBusy runs write, and again after each of b.StoreRetryDelays for as
// long as the database is busy. It returns the error of the last attempt.
func (b *Bridge) retryBusy(write func() error) error {
	return retryBusyWithWait(write, b.StoreRetryDelays, b.waitStoreRetry)
}

// The same bounded loop for event/history writes and outbound persistence.
// write must contain database effects only and be safe to repeat.
func retryBusyWithWait(write func() error, delays []time.Duration, wait func(time.Duration) bool) error {
	err := write()
	for _, delay := range delays {
		if err == nil || !isBusyError(err) || !wait(delay) {
			break
		}
		err = write()
	}
	return err
}

// Outbound persistence runs after WhatsApp already accepted the send. Retry
// only these local upserts; never send the WhatsApp message a second time.
func (store *MessageStore) retryBusy(write func() error) error {
	wait := store.storeRetryWait
	if wait == nil {
		wait = func(delay time.Duration) bool { return sleepContext(context.Background(), delay) }
	}
	return retryBusyWithWait(write, defaultStoreRetryDelays(), wait)
}

// storeLive runs one write under retryBusy and reports whether it is in the
// store; a write that is given up is counted and logged once. write has to be
// safe to run again (the bridge's inserts are upserts).
func (b *Bridge) storeLive(kind, messageID, chatJID string, write func() error) bool {
	if err := b.retryBusy(write); err != nil {
		b.noteStoreFailure(kind, messageID, chatJID, err)
		return false
	}
	return true
}

// waitStoreRetry sleeps before a retry; false means the bridge is shutting
// down and the write is not tried again.
func (b *Bridge) waitStoreRetry(delay time.Duration) bool {
	if b.storeRetryWait != nil {
		return b.storeRetryWait(delay)
	}
	return b.sleep(delay)
}

// noteStoreFailure records a row that is lost for good: one ERROR naming it,
// one count on /metrics.
func (b *Bridge) noteStoreFailure(kind, messageID, chatJID string, err error) {
	b.metrics.storeFailures.Add(1)
	b.Log.Errorf("Failed to store %s %s in %s: %v", kind, messageID, chatJID, err)
}
